package research

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"openfactory/internal/model"
)

var fullCommit = regexp.MustCompile(`^[0-9a-f]{40}$`)
var githubName = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

// githubLocation derives every disclosed path/ref from an already authorized
// seed or lead URL. It never appends free model input to a public request.
func githubLocation(raw string) (repository, ref, path string, err error) {
	u, e := PublicURL(raw)
	if e != nil {
		err = e
		return
	}
	if u.RawQuery != "" {
		err = rejected("source_rejected")
		return
	}
	parts := strings.Split(strings.TrimPrefix(u.Path, "/"), "/")
	if u.Hostname() == "github.com" {
		if len(parts) < 5 || parts[2] != "blob" {
			err = rejected("source_rejected")
			return
		}
		parts = append(parts[:2], parts[3:]...)
	} else if u.Hostname() != "raw.githubusercontent.com" {
		err = rejected("source_rejected")
		return
	}
	if len(parts) < 4 || !githubName.MatchString(parts[0]) || !githubName.MatchString(parts[1]) || parts[0] == "." || parts[0] == ".." || parts[1] == "." || parts[1] == ".." {
		err = rejected("source_rejected")
		return
	}
	for _, part := range parts[2:] {
		if part == "" || part == "." || part == ".." || strings.ContainsAny(part, "\\\x00\r\n") {
			err = rejected("source_rejected")
			return
		}
	}
	repository = parts[0] + "/" + parts[1]
	ref = parts[2]
	path = strings.Join(parts[3:], "/")
	return
}
func escapedPath(path string) string {
	parts := strings.Split(path, "/")
	for i := range parts {
		parts[i] = url.PathEscape(parts[i])
	}
	return strings.Join(parts, "/")
}
func (s *Service) github(ctx context.Context, a model.ResearchAuthorization) (model.ResearchOperationResult, error) {
	repository, ref, path, err := githubLocation(a.URL)
	if err != nil {
		return model.ResearchOperationResult{}, err
	}
	req := a.Request
	if req.Repository != "" && req.Repository != repository || req.Ref != "" && req.Ref != ref || req.Path != "" && req.Path != path {
		return model.ResearchOperationResult{}, rejected("source_rejected")
	}
	apiURL := "https://api.github.com/repos/" + repository + "/commits/" + url.PathEscape(ref)
	resolution, err := s.fetcher(a).Fetch(ctx, apiURL)
	observed := int64(len(resolution.Body))
	partial := model.ResearchOperationResult{ObservedBytes: observed}
	if err != nil {
		return partial, err
	}
	if resolution.Status != 200 {
		return partial, statusError(resolution.Status)
	}
	if resolution.FinalURL != apiURL || len(resolution.RedirectChain) != 0 {
		return partial, rejected("source_rejected")
	}
	var commit struct {
		SHA string `json:"sha"`
	}
	if json.Unmarshal(resolution.Body, &commit) != nil || !fullCommit.MatchString(commit.SHA) {
		return partial, rejected("source_rejected")
	}
	if fullCommit.MatchString(ref) && ref != commit.SHA {
		return partial, rejected("source_rejected")
	}
	// Preserve the complete resolution response as its own immutable CAS blob.
	resolutionBlob, err := s.blobs.Put(ctx, resolution.Body)
	if err != nil {
		return partial, err
	}
	pinned := "https://raw.githubusercontent.com/" + repository + "/" + commit.SHA + "/" + escapedPath(path)
	allowance := a.MaxCaptureBytes
	if allowance <= 0 || allowance > MaxBodyBytes {
		allowance = MaxBodyBytes
	}
	allowance -= observed
	if allowance <= 0 {
		return partial, rejected("source_too_large")
	}
	rawFetcher := s.fetcher(a)
	rawFetcher.MaxBytes = allowance
	fetched, err := rawFetcher.Fetch(ctx, pinned)
	partial.ObservedBytes += int64(len(fetched.Body))
	if err != nil {
		return partial, err
	}
	if fetched.Status != 200 {
		return partial, statusError(fetched.Status)
	}
	if fetched.FinalURL != pinned || len(fetched.RedirectChain) != 0 {
		return partial, rejected("source_rejected")
	}
	// raw.githubusercontent.com serves many source extensions as text/plain.
	// Its actual complete bytes, not API snippets, define the Git blob identity.
	result, err := s.captureResult(ctx, a, fetched)
	result.ObservedBytes += observed
	if err != nil {
		return result, err
	}
	hash := sha1.New()
	fmt.Fprintf(hash, "blob %d\x00", len(fetched.Body))
	hash.Write(fetched.Body)
	result.Source.GitHub = &model.SourceGitHub{Repository: repository, Commit: commit.SHA, Path: path, BlobSHA: hex.EncodeToString(hash.Sum(nil)), BlobIdentityScope: "computed_git_blob_sha1_from_complete_public_raw_body", CommitResolutionURL: resolution.FinalURL, CommitResolutionSHA256: resolutionBlob.SHA}
	result.Source.Kind = "github"
	return result, nil
}
