package gitrepo

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"

	"openfactory/internal/model"
)

var githubName = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

type pull struct {
	URL      string  `json:"html_url"`
	State    string  `json:"state"`
	MergedAt *string `json:"merged_at"`
	Head     struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"base"`
}

func (m *Manager) github(ctx context.Context, p model.Project, method, path string, body any, result any) error {
	if err := safeFile(p.Auth.TokenFile, true); err != nil {
		return fmt.Errorf("GitHub API token: %w", err)
	}
	b, err := os.ReadFile(p.Auth.TokenFile)
	if err != nil {
		return err
	}
	token := strings.TrimSpace(string(b))
	if token == "" || strings.ContainsAny(token, "\r\n") {
		return errors.New("invalid GitHub API token")
	}
	var payload []byte
	if body != nil {
		payload, err = json.Marshal(body)
		if err != nil {
			return err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, m.apiURL+path, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Content-Type", "application/json")
	resp, err := m.client.Do(req)
	if err != nil {
		return fmt.Errorf("GitHub request failed: %s", strings.ReplaceAll(err.Error(), token, "[REDACTED]"))
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, outputLimit+1))
	if err != nil {
		return err
	}
	if len(raw) > outputLimit {
		return errors.New("GitHub response exceeds limit")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("GitHub returned HTTP %d", resp.StatusCode)
	}
	if result != nil {
		return json.Unmarshal(raw, result)
	}
	return nil
}
func (m *Manager) findPR(ctx context.Context, p model.Project, branch, candidate, expectedBase string) (string, error) {
	owner := strings.Split(p.GitHubRepository, "/")[0]
	query := url.Values{"state": {"all"}, "head": {owner + ":" + branch}, "base": {p.Branch}, "per_page": {"100"}}
	var found []pull
	if err := m.github(ctx, p, http.MethodGet, "/repos/"+p.GitHubRepository+"/pulls?"+query.Encode(), nil, &found); err != nil {
		return "", err
	}
	if len(found) >= 100 {
		return "", errors.New("PR lookup is not uniquely bounded")
	}
	var matching []pull
	for _, pr := range found {
		if pr.Head.Ref == branch && pr.Base.Ref == p.Branch {
			matching = append(matching, pr)
		}
	}
	if len(matching) > 1 {
		return "", errors.New("ambiguous existing pull requests")
	}
	if len(matching) == 0 {
		return "", nil
	}
	pr := matching[0]
	if pr.Head.SHA != candidate {
		return "", errors.New("PR head differs from approved candidate")
	}
	if pr.Base.SHA != expectedBase {
		return "", errors.New("PR base differs from the checked target commit")
	}
	if pr.State != "open" && pr.MergedAt == nil {
		return "", errors.New("existing PR was closed without merge")
	}
	if pr.URL == "" {
		return "", errors.New("GitHub returned PR without URL")
	}
	return pr.URL, nil
}
func (m *Manager) PullRequest(ctx context.Context, name string, p model.Project, a model.Attempt, f model.Feature) (string, error) {
	defer m.lockRepository(p)()
	reject := func(err error) (string, error) {
		return "", deliveryError("rejected", err.Error(), "", nil)
	}
	r, err := m.validateCandidate(ctx, name, p, a)
	if err != nil {
		return reject(err)
	}
	if f.Project != name || f.ID != a.FeatureID {
		return reject(errors.New("PR feature identity mismatch"))
	}
	if !githubName.MatchString(p.GitHubRepository) {
		return reject(errors.New("github_repository must be owner/repository"))
	}
	if err = safeFile(p.Auth.TokenFile, true); err != nil {
		return reject(err)
	}
	branch, err := m.remoteHeadOptional(ctx, r, p, r.Branch)
	if err != nil {
		return reject(err)
	}
	if branch != "" && branch != a.CandidateSHA {
		return reject(errors.New("remote candidate branch unexpectedly changed"))
	}
	target, err := m.remoteHead(ctx, r, p, p.Branch)
	if err != nil {
		if branch != "" {
			return "", deliveryError("uncertain", "published PR branch target could not be observed: "+err.Error(), "", nil)
		}
		return reject(err)
	}
	if target != a.BaseSHA {
		if branch != "" {
			return "", deliveryError("uncertain", "PR target moved after candidate publication; do not retry", target, nil)
		}
		return "", deliveryError("target_moved", "PR target moved; renewed checks required", target, nil)
	}
	// Check API state before publishing anything. Once a candidate ref exists,
	// every failed observation is conservative uncertainty, not a safe retry.
	existing, err := m.findPR(ctx, p, r.Branch, a.CandidateSHA, a.BaseSHA)
	if err != nil {
		if branch != "" {
			return "", deliveryError("uncertain", "published PR branch could not be reconciled: "+err.Error(), "", nil)
		}
		return reject(err)
	}
	if existing != "" {
		if branch != a.CandidateSHA {
			return reject(errors.New("existing PR lacks the exact published candidate ref"))
		}
		actualBranch, branchErr := m.remoteHead(ctx, r, p, r.Branch)
		actualTarget, targetErr := m.remoteHead(ctx, r, p, p.Branch)
		if branchErr != nil || actualBranch != a.CandidateSHA || targetErr != nil || actualTarget != a.BaseSHA {
			return "", deliveryError("uncertain", "existing PR refs changed or could not be observed; do not retry", actualTarget, nil)
		}
		return existing, nil
	}
	if branch == "" {
		if _, err = m.publishCandidate(ctx, r, p, r.Branch, a.CandidateSHA, ""); err != nil {
			return "", err
		}
	}
	target, err = m.remoteHead(ctx, r, p, p.Branch)
	if err != nil || target != a.BaseSHA {
		return "", deliveryError("uncertain", "PR target changed or could not be observed after candidate publication; do not retry", target, nil)
	}
	var created pull
	body := map[string]string{"title": f.Title, "head": r.Branch, "base": p.Branch, "body": "OpenFactory feature " + f.ID + "\nAttempt " + a.ID + "\nApproved revision " + f.Revision + "\nCandidate " + a.CandidateSHA + "\nChecked target " + a.BaseSHA}
	createErr := m.github(ctx, p, http.MethodPost, "/repos/"+p.GitHubRepository+"/pulls", body, &created)
	existing, err = m.findPR(ctx, p, r.Branch, a.CandidateSHA, a.BaseSHA)
	// A URL or HTTP acknowledgement is not a receipt until both Git refs and
	// the API's candidate/base identities still match the checked generation.
	actualBranch, branchErr := m.remoteHead(ctx, r, p, r.Branch)
	target, targetErr := m.remoteHead(ctx, r, p, p.Branch)
	if err == nil && existing != "" && branchErr == nil && actualBranch == a.CandidateSHA && targetErr == nil && target == a.BaseSHA {
		return existing, nil
	}
	message := "PR creation outcome uncertain; do not retry"
	if createErr != nil {
		message += ": " + createErr.Error()
	} else if err != nil {
		message += ": " + err.Error()
	}
	return "", deliveryError("uncertain", message, target, nil)
}
