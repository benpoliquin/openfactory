package store

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"openfactory/internal/artifacts"
	"openfactory/internal/model"
	"openfactory/internal/research"
)

func researchFixture(t *testing.T, policy model.DiscoveryPolicy) (*Store, model.Attempt, model.Config, model.ResearchCapability) {
	t.Helper()
	return researchSeedFixture(t, policy, "https://example.com/article")
}

func researchSeedFixture(t *testing.T, policy model.DiscoveryPolicy, seedURL string) (*Store, model.Attempt, model.Config, model.ResearchCapability) {
	t.Helper()
	ctx := context.Background()
	s := openTest(t)
	s.blobs = artifacts.New(t.TempDir())
	if _, err := s.db.ExecContext(ctx, programSchema+programResearchSchema); err != nil {
		t.Fatal(err)
	}
	cfg := model.Config{PolicyHash: "research-policy", AstraModel: "provider/research", Discovery: policy}
	spec := model.ProgramSpec{
		ID:          "research-program",
		Project:     "demo",
		Title:       "Public research",
		Objective:   "Capture approved public evidence",
		Scope:       []string{"Search approved public topics and capture exact public seeds or recorded leads"},
		NonGoals:    []string{"Read private sources or authorize product implementation"},
		Constraints: []string{"Keep source provenance and lifetime research limits"},
		Seeds:       []model.ProgramSeed{{ID: "seed", Kind: "public_url", URL: seedURL}},
		Disclosure: model.ProgramDisclosure{
			ModelProviderIDs:        []string{"provider"},
			PrivateMaterialToModels: false,
			PublicSearchAllowed:     true,
			PublicSearchTopics:      []model.ProgramSearchTopic{{ID: "topic", Query: "public reliability evidence"}},
		},
		Limits: model.NormalizeDiscoveryPolicy(policy).Limits,
		Implementation: model.ProgramImplementationPolicy{
			PermittedFeatureKinds:      []string{"research"},
			PermittedCompletionTargets: []string{"reviewed"},
		},
	}
	rev, err := s.ImportProgram(ctx, spec, model.ProgramBundle{})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ApproveProgram(ctx, spec.ID, rev.RevisionHash, cfg.PolicyHash, model.TrustedLocalAudit()); err != nil {
		t.Fatal(err)
	}
	f := feature("research-feature")
	f.DiscoveryStepID = "research-step"
	f.PolicyHash = cfg.PolicyHash
	f.Revision = "feature-revision"
	f.State = "running"
	now := time.Now().UTC()
	a := model.Attempt{ID: "research-attempt", FeatureID: f.ID, State: "running", Stage: "research", Deadline: now.Add(time.Hour), StartedAt: now}
	step := model.ProgramStep{ID: f.DiscoveryStepID, ProgramID: spec.ID, Revision: rev.Number, Kind: model.StepResearch, FeatureID: f.ID, State: "running", Model: cfg.AstraModel}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err = put(ctx, tx, f); err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(a)
	if _, err = tx.ExecContext(ctx, `INSERT INTO attempts(id,feature_id,active,revision,policy_hash,data) VALUES(?,?,1,?,?,?)`, a.ID, f.ID, f.Revision, f.PolicyHash, encoded); err != nil {
		t.Fatal(err)
	}
	encoded, _ = json.Marshal(step)
	if _, err = tx.ExecContext(ctx, `INSERT INTO program_steps(id,program_id,revision,feature_id,data) VALUES(?,?,?,?,?)`, step.ID, spec.ID, rev.Number, f.ID, encoded); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	cap, err := s.IssueResearchCapability(ctx, a, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return s, a, cfg, cap
}

func researchWantCode(t *testing.T, err error, code string) {
	t.Helper()
	var programErr *model.ProgramError
	if !errors.As(err, &programErr) || programErr.Code != code {
		t.Fatalf("want %s, got %v", code, err)
	}
}

func researchBudget(t *testing.T, s *Store) model.ProgramBudget {
	t.Helper()
	var data []byte
	var budget model.ProgramBudget
	if err := s.db.QueryRow(`SELECT data FROM program_budgets WHERE program_id='research-program'`).Scan(&data); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &budget); err != nil {
		t.Fatal(err)
	}
	return budget
}

func researchCapture(t *testing.T, s *Store, body string) model.ResearchOperationResult {
	t.Helper()
	ctx := context.Background()
	extracted, err := research.Extract([]byte(body), "text/html", "https://example.com/article", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := s.blobs.Put(ctx, []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	text, err := s.blobs.Put(ctx, []byte(extracted.Text))
	if err != nil {
		t.Fatal(err)
	}
	result := model.ResearchOperationResult{ObservedBytes: int64(len(body)), Source: &model.SourceCapture{ID: "forged-source", ProgramID: "other-program", Revision: 99, Kind: "capture", RequestedURL: "https://example.com/article", FinalURL: "https://example.com/article", Status: 200, MediaType: "text/html", ByteLength: raw.Bytes, ContentSHA256: raw.SHA, TextSHA256: text.SHA, ExtractorVersion: research.ExtractorVersion, PublisherCanonicalURL: extracted.CanonicalURL}}
	for _, link := range extracted.Links {
		result.Leads = append(result.Leads, model.SearchLead{ID: "forged-lead", URL: link, Origin: "forged-origin"})
	}
	return result
}

func TestResearchCapabilityOpaqueActualAttemptAndScope(t *testing.T) {
	s, a, cfg, cap := researchFixture(t, model.DiscoveryPolicy{})
	ctx := context.Background()
	var stored []byte
	var hash string
	if err := s.db.QueryRow(`SELECT token_hash,data FROM program_capabilities`).Scan(&hash, &stored); err != nil {
		t.Fatal(err)
	}
	if len(cap.Token) != 64 || hash != researchHash(cap.Token) || bytes.Contains(stored, []byte(cap.Token)) {
		t.Fatal("raw capability persisted or invalid digest")
	}
	a.Deadline = time.Now().Add(24 * time.Hour)
	second, err := s.IssueResearchCapability(ctx, a, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !second.Deadline.Equal(cap.Deadline) {
		t.Fatal("caller extended capability deadline")
	}
	if second.Token == cap.Token {
		t.Fatal("capability token reused")
	}
	a.FeatureID = "forged-feature"
	if _, err = s.IssueResearchCapability(ctx, a, cfg); err == nil {
		t.Fatal("forged attempt association accepted")
	}
	requests := []model.ResearchOperationRequest{
		{RequestID: "forged-query", Kind: "search", TopicID: "topic", URL: "https://example.com/?private=secret"},
		{RequestID: "forged-topic", Kind: "search", TopicID: "not-approved"},
		{RequestID: "forged-url", Kind: "capture", SeedID: "seed", URL: "https://example.com/article?private=secret"},
		{RequestID: "forged-lead", Kind: "capture", LeadID: "invented"},
	}
	for _, req := range requests {
		_, err = s.AuthorizeResearch(ctx, cap.Token, req)
		researchWantCode(t, err, "source_rejected")
	}
	if budget := researchBudget(t, s); budget.Searches != 0 || budget.Sources != 0 {
		t.Fatalf("rejected requests consumed reservations: %+v", budget)
	}
}

func TestResearchIdempotencyPendingTerminalAndServerStampedLeads(t *testing.T) {
	s, _, _, cap := researchFixture(t, model.DiscoveryPolicy{Limits: model.ProgramLimits{MaxSearches: 1}})
	ctx := context.Background()
	req := model.ResearchOperationRequest{RequestID: "search-1", Kind: "search", TopicID: "topic"}
	auth, err := s.AuthorizeResearch(ctx, cap.Token, req)
	if err != nil {
		t.Fatal(err)
	}
	if auth.Query != "public reliability evidence" {
		t.Fatalf("unresolved approved topic: %+v", auth)
	}
	_, err = s.AuthorizeResearch(ctx, cap.Token, req)
	researchWantCode(t, err, "operation_uncertain")
	changed := req
	changed.TopicID = "different"
	_, err = s.AuthorizeResearch(ctx, cap.Token, changed)
	researchWantCode(t, err, "idempotency_conflict")
	forged := auth
	forged.Query = "private injected query"
	_, err = s.CompleteResearchOperation(ctx, forged, model.ResearchOperationResult{ErrorCode: "search_unavailable"})
	researchWantCode(t, err, "source_rejected")
	result, err := s.CompleteResearchOperation(ctx, auth, model.ResearchOperationResult{Search: &model.SearchReceipt{ID: "forged", Provider: "parallel_keyless_mcp", Status: "completed", Query: "injected"}, Leads: []model.SearchLead{{ID: "forged", URL: "https://example.org/lead", Origin: "forged"}}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Search.Query != auth.Query || result.Search.ID == "forged" || result.Leads[0].ID == "forged" || result.Leads[0].Origin != result.Search.ID {
		t.Fatal("broker result identities not stamped by controller")
	}
	replay, err := s.AuthorizeResearch(ctx, cap.Token, req)
	if err != nil || replay.Replay == nil || replay.Replay.Search.ID != result.Search.ID {
		t.Fatalf("terminal replay: %+v %v", replay, err)
	}
	if budget := researchBudget(t, s); budget.Searches != 1 {
		t.Fatalf("replay charged a new search: %+v", budget)
	}
	_, err = s.AuthorizeResearch(ctx, cap.Token, model.ResearchOperationRequest{RequestID: "search-2", Kind: "search", TopicID: "topic"})
	researchWantCode(t, err, "budget_exhausted")
	capture, err := s.AuthorizeResearch(ctx, cap.Token, model.ResearchOperationRequest{RequestID: "lead-capture", Kind: "capture", LeadID: result.Leads[0].ID})
	if err != nil || capture.URL != "https://example.org/lead" {
		t.Fatalf("recorded lead resolution: %+v %v", capture, err)
	}
}

func TestResearchLifetimeByteReservationsAndFailedIO(t *testing.T) {
	s, _, _, cap := researchFixture(t, model.DiscoveryPolicy{MaxCaptureBytes: 10, Limits: model.ProgramLimits{MaxSources: 4, MaxTotalCaptureBytes: 22}})
	ctx := context.Background()
	authorize := func(id string) (model.ResearchAuthorization, error) {
		return s.AuthorizeResearch(ctx, cap.Token, model.ResearchOperationRequest{RequestID: id, Kind: "capture", SeedID: "seed"})
	}
	first, err := authorize("first")
	if err != nil {
		t.Fatal(err)
	}
	second, err := authorize("second")
	if err != nil {
		t.Fatal(err)
	}
	if first.MaxCaptureBytes != 10 || second.MaxCaptureBytes != 10 {
		t.Fatal("configured capture limits lost")
	}
	if budget := researchBudget(t, s); budget.ReservedCaptureBytes != 22 || budget.Sources != 2 {
		t.Fatalf("unreserved IO allowance: %+v", budget)
	}
	_, err = authorize("blocked")
	researchWantCode(t, err, "budget_exhausted")
	_, err = s.CompleteResearchOperation(ctx, first, model.ResearchOperationResult{ObservedBytes: 12, ErrorCode: "source_too_large"})
	researchWantCode(t, err, "source_too_large")
	if budget := researchBudget(t, s); budget.ReservedCaptureBytes != 22 {
		t.Fatal("overrun released a live reservation")
	}
	result, err := s.CompleteResearchOperation(ctx, first, model.ResearchOperationResult{ObservedBytes: 4, ErrorCode: "source_rejected"})
	if err != nil || result.ErrorCode != "source_rejected" {
		t.Fatal(err)
	}
	if budget := researchBudget(t, s); budget.CaptureBytes != 4 || budget.ReservedCaptureBytes != 11 || budget.Sources != 2 {
		t.Fatalf("failed capture lost lifetime usage: %+v", budget)
	}
	third, err := authorize("third")
	if err != nil {
		t.Fatal(err)
	}
	if third.MaxCaptureBytes != 6 {
		t.Fatalf("remaining lifetime bytes not clamped: %+v", third)
	}
	if budget := researchBudget(t, s); budget.CaptureBytes+budget.ReservedCaptureBytes != 22 {
		t.Fatalf("aggregate byte overrun: %+v", budget)
	}
}

func TestResearchCompletionRechecksRevisionCancellationPolicyAndGrant(t *testing.T) {
	cases := []struct{ name, sql, code string }{
		{"revision", `UPDATE programs SET data=json_set(data,'$.current_revision',2); INSERT INTO program_revisions(program_id,number,revision_hash,data) SELECT program_id,2,'new-revision',json_set(data,'$.number',2,'$.revision_hash','new-revision') FROM program_revisions WHERE number=1`, "stale_revision"},
		{"cancelled", `UPDATE programs SET data=json_set(data,'$.state','cancelled')`, "approval_required"},
		{"policy", `UPDATE features SET data=json_set(data,'$.policy_hash','changed-policy')`, "policy_changed"},
		{"grant", `DELETE FROM program_grants`, "approval_required"},
		{"invalidation", `INSERT INTO program_invalidations(id,program_id,subject,data) VALUES('invalid','research-program','*','{}')`, "evidence_invalid"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, _, _, cap := researchFixture(t, model.DiscoveryPolicy{})
			ctx := context.Background()
			auth, err := s.AuthorizeResearch(ctx, cap.Token, model.ResearchOperationRequest{RequestID: "capture", Kind: "capture", SeedID: "seed"})
			if err != nil {
				t.Fatal(err)
			}
			result := researchCapture(t, s, "<p>Evidence recorded before authority changed.</p>")
			if _, err = s.db.ExecContext(ctx, tc.sql); err != nil {
				t.Fatal(err)
			}
			_, err = s.CompleteResearchOperation(ctx, auth, result)
			researchWantCode(t, err, tc.code)
			var count int
			if err = s.db.QueryRow(`SELECT count(*) FROM program_sources`).Scan(&count); err != nil || count != 0 {
				t.Fatalf("stale capture accepted: %d %v", count, err)
			}
			budget := researchBudget(t, s)
			if budget.ReservedCaptureBytes != 0 || budget.CaptureBytes != result.ObservedBytes {
				t.Fatalf("rejected capture did not settle observed IO: %+v", budget)
			}
			var terminal []byte
			if err = s.db.QueryRow(`SELECT result FROM program_research_operations WHERE id=?`, auth.ID).Scan(&terminal); err != nil {
				t.Fatal(err)
			}
			var rejected model.ResearchOperationResult
			if err = json.Unmarshal(terminal, &rejected); err != nil || rejected.ErrorCode != tc.code {
				t.Fatalf("rejection not durably terminal: %s %v", terminal, err)
			}
		})
	}
}

func TestResearchSourceCASProvenanceFreshnessInvalidationAndExpiry(t *testing.T) {
	s, _, _, cap := researchFixture(t, model.DiscoveryPolicy{})
	ctx := context.Background()
	auth, err := s.AuthorizeResearch(ctx, cap.Token, model.ResearchOperationRequest{RequestID: "source", Kind: "capture", SeedID: "seed"})
	if err != nil {
		t.Fatal(err)
	}
	result, err := s.CompleteResearchOperation(ctx, auth, researchCapture(t, s, `<p>Actual evidence.</p><a href="/next">Next source</a>`))
	if err != nil {
		t.Fatal(err)
	}
	if result.Source.ID == "forged-source" || result.Source.ProgramID != cap.ProgramID || result.Source.Revision != cap.Revision || !result.Source.Valid || result.Source.FreshUntil == nil {
		t.Fatal("source identity not stamped")
	}
	source, err := s.ResearchSource(ctx, cap.Token, result.Source.ID)
	if err != nil || source.TextSHA256 != result.Source.TextSHA256 {
		t.Fatalf("source lookup: %+v %v", source, err)
	}
	lead, err := s.AuthorizeResearch(ctx, cap.Token, model.ResearchOperationRequest{RequestID: "href", Kind: "capture", LeadID: result.Leads[0].ID})
	if err != nil || lead.URL != "https://example.com/next" {
		t.Fatalf("extracted href lead: %+v %v", lead, err)
	}
	if _, err = s.db.Exec(`INSERT INTO program_invalidations(id,program_id,subject,data) VALUES('source-invalid','research-program',?,'{}')`, source.TextSHA256); err != nil {
		t.Fatal(err)
	}
	_, err = s.ResearchSource(ctx, cap.Token, source.ID)
	researchWantCode(t, err, "evidence_invalid")
	_, err = s.AuthorizeResearch(ctx, cap.Token, model.ResearchOperationRequest{RequestID: "invalid-href", Kind: "capture", LeadID: result.Leads[0].ID})
	researchWantCode(t, err, "evidence_invalid")
	if _, err = s.db.Exec(`DELETE FROM program_invalidations; UPDATE program_sources SET data=json_set(data,'$.fresh_until','2000-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	_, err = s.ResearchSource(ctx, cap.Token, source.ID)
	researchWantCode(t, err, "evidence_invalid")
	_, err = s.AuthorizeResearch(ctx, cap.Token, model.ResearchOperationRequest{RequestID: "expired-href", Kind: "capture", LeadID: result.Leads[0].ID})
	researchWantCode(t, err, "evidence_invalid")
	if _, err = s.db.Exec(`UPDATE attempts SET data=json_set(data,'$.deadline','2000-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	_, err = s.AuthorizeResearch(ctx, cap.Token, model.ResearchOperationRequest{RequestID: "expired", Kind: "capture", SeedID: "seed"})
	researchWantCode(t, err, "source_rejected")
}

func TestResearchRejectsInventedTextLinksAndMissingCAS(t *testing.T) {
	for _, kind := range []string{"text", "link", "missing-body", "wrong-length"} {
		t.Run(kind, func(t *testing.T) {
			s, _, _, cap := researchFixture(t, model.DiscoveryPolicy{})
			ctx := context.Background()
			auth, err := s.AuthorizeResearch(ctx, cap.Token, model.ResearchOperationRequest{RequestID: "forgery", Kind: "capture", SeedID: "seed"})
			if err != nil {
				t.Fatal(err)
			}
			result := researchCapture(t, s, "<p>Actual evidence.</p>")
			switch kind {
			case "text":
				blob, e := s.blobs.Put(ctx, []byte("invented evidence"))
				if e != nil {
					t.Fatal(e)
				}
				result.Source.TextSHA256 = blob.SHA
			case "link":
				result.Leads = []model.SearchLead{{URL: "https://example.com/not-present"}}
			case "missing-body":
				result.Source.ContentSHA256 = strings.Repeat("a", 64)
			case "wrong-length":
				result.Source.ByteLength--
			}
			_, err = s.CompleteResearchOperation(ctx, auth, result)
			researchWantCode(t, err, "source_rejected")
			var count int
			if err = s.db.QueryRow(`SELECT count(*) FROM program_sources`).Scan(&count); err != nil || count != 0 {
				t.Fatalf("forged provenance persisted: %d %v", count, err)
			}
		})
	}
}

func TestResearchPendingAndPolicyLimitsSurviveReopen(t *testing.T) {
	s, _, _, cap := researchFixture(t, model.DiscoveryPolicy{MaxCaptureBytes: 17, Limits: model.ProgramLimits{MaxSources: 1}})
	ctx := context.Background()
	req := model.ResearchOperationRequest{RequestID: "durable-pending", Kind: "capture", SeedID: "seed"}
	auth, err := s.AuthorizeResearch(ctx, cap.Token, req)
	if err != nil {
		t.Fatal(err)
	}
	var sequence int
	var name, path string
	if err = s.db.QueryRow(`PRAGMA database_list`).Scan(&sequence, &name, &path); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reopened.Close() })
	_, err = reopened.AuthorizeResearch(ctx, cap.Token, req)
	researchWantCode(t, err, "operation_uncertain")
	if auth.MaxCaptureBytes != 17 {
		t.Fatalf("capability policy limit not retained: %d", auth.MaxCaptureBytes)
	}
	terminal, err := reopened.CompleteResearchOperation(ctx, auth, model.ResearchOperationResult{ObservedBytes: 3, ErrorCode: "source_rejected"})
	if err != nil {
		t.Fatal(err)
	}
	replay, err := reopened.AuthorizeResearch(ctx, cap.Token, req)
	if err != nil || replay.Replay == nil || replay.Replay.ErrorCode != terminal.ErrorCode {
		t.Fatalf("terminal replay lost after reopen: %+v %v", replay, err)
	}
	_, err = reopened.AuthorizeResearch(ctx, cap.Token, model.ResearchOperationRequest{RequestID: "new-source", Kind: "capture", SeedID: "seed"})
	researchWantCode(t, err, "budget_exhausted")
	if budget := researchBudget(t, reopened); budget.Sources != 1 || budget.CaptureBytes != 3 || budget.ReservedCaptureBytes != 0 {
		t.Fatalf("lifetime ledger changed on reopen: %+v", budget)
	}
}

func TestResearchGitHubCommitResolutionAndWholeBlobIdentity(t *testing.T) {
	for _, forged := range []bool{false, true} {
		t.Run(fmt.Sprintf("forged=%v", forged), func(t *testing.T) {
			approvedURL := "https://github.com/example/public/blob/main/doc.md"
			s, _, _, cap := researchSeedFixture(t, model.DiscoveryPolicy{}, approvedURL)
			ctx := context.Background()
			auth, err := s.AuthorizeResearch(ctx, cap.Token, model.ResearchOperationRequest{RequestID: "github", Kind: "github", SeedID: "seed"})
			if err != nil {
				t.Fatal(err)
			}
			body := []byte("Public pinned source evidence.\n")
			commit := strings.Repeat("a", 40)
			raw, err := s.blobs.Put(ctx, body)
			if err != nil {
				t.Fatal(err)
			}
			resolution, err := s.blobs.Put(ctx, []byte(`{"sha":"`+commit+`"}`))
			if err != nil {
				t.Fatal(err)
			}
			hash := sha1.New()
			fmt.Fprintf(hash, "blob %d\x00", len(body))
			hash.Write(body)
			result := model.ResearchOperationResult{ObservedBytes: raw.Bytes + resolution.Bytes, Source: &model.SourceCapture{Kind: "github", RequestedURL: approvedURL, FinalURL: "https://raw.githubusercontent.com/example/public/" + commit + "/doc.md", Status: 200, MediaType: "text/plain", ByteLength: raw.Bytes, ContentSHA256: raw.SHA, TextSHA256: raw.SHA, ExtractorVersion: research.ExtractorVersion, GitHub: &model.SourceGitHub{Repository: "example/public", Commit: commit, Path: "doc.md", BlobSHA: hex.EncodeToString(hash.Sum(nil)), BlobIdentityScope: "computed_git_blob_sha1_from_complete_public_raw_body", CommitResolutionURL: "https://api.github.com/repos/example/public/commits/main", CommitResolutionSHA256: resolution.SHA}}}
			if forged {
				result.Source.GitHub.Commit = strings.Repeat("b", 40)
			}
			completed, err := s.CompleteResearchOperation(ctx, auth, result)
			if forged {
				researchWantCode(t, err, "source_rejected")
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if completed.Source.GitHub.Commit != commit || completed.Source.ByteLength != raw.Bytes {
				t.Fatalf("pinned whole source identity lost: %+v", completed.Source)
			}
			if budget := researchBudget(t, s); budget.CaptureBytes != raw.Bytes+resolution.Bytes || budget.ReservedCaptureBytes != 0 {
				t.Fatalf("GitHub resolution bytes not settled: %+v", budget)
			}
		})
	}
}
