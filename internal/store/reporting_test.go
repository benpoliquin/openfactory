package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"

	"openfactory/internal/model"
)

func reportingConfig() model.Config {
	return model.Config{PolicyHash: "policy", MaxAttempts: 3, MaxConcurrent: 4, MaxCallsPerDay: 20, Projects: map[string]model.Project{"demo": {MaxConcurrent: 2}, "other": {}}}
}

func reportingFeature(t *testing.T, s *Store, id, project, state string) model.Feature {
	t.Helper()
	f := feature(id)
	f.Project = project
	f.State = state
	f.Revision = "revision-" + id
	f.PolicyHash = "policy"
	f.CreatedAt = time.Now().UTC()
	if err := put(context.Background(), s.db, f); err != nil {
		t.Fatal(err)
	}
	stored, err := s.Get(context.Background(), f.ID)
	if err != nil {
		t.Fatal(err)
	}
	return stored
}

func reportingAttempt(t *testing.T, s *Store, f model.Feature, id string, active bool, generation int, sha string) model.Attempt {
	t.Helper()
	a := model.Attempt{ID: id, FeatureID: f.ID, State: f.State, Stage: "checks", CandidateGeneration: generation, CandidateSHA: sha, BaseSHA: "base", StartedAt: time.Now().UTC()}
	reportingInsertAttempt(t, s, f, a, active)
	return a
}

func reportingInsertAttempt(t *testing.T, s *Store, f model.Feature, a model.Attempt, active bool) {
	t.Helper()
	data, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`INSERT INTO attempts(id,feature_id,active,revision,policy_hash,data) VALUES(?,?,?,?,?,?)`, a.ID, f.ID, active, f.Revision, f.PolicyHash, data); err != nil {
		t.Fatal(err)
	}
}

func reportingEvidence(t *testing.T, s *Store, f model.Feature, a model.Attempt, id, detail string, generation int, sha string) {
	t.Helper()
	e := model.Evidence{ID: id, FeatureID: f.ID, AttemptID: a.ID, CandidateGeneration: generation, CandidateSHA: sha, Kind: "check:unit", Passed: true, Detail: detail, CreatedAt: time.Now().UTC()}
	data, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`INSERT INTO evidence(id,feature_id,attempt_id,revision,policy_hash,data) VALUES(?,?,?,?,?,?)`, id, f.ID, a.ID, f.Revision, f.PolicyHash, data); err != nil {
		t.Fatal(err)
	}
}

func TestReportingStatusFiltersAndStablePagination(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	cfg := reportingConfig()
	for _, id := range []string{"alpha", "bravo", "charlie"} {
		reportingFeature(t, s, id, "demo", "ready")
	}
	reportingFeature(t, s, "delta", "other", "ready")
	reportingFeature(t, s, "echo", "demo", "blocked")
	q := model.StatusQuery{Project: "demo", State: "ready", Limit: 2}
	first, err := s.Status(ctx, q, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Features) != 2 || first.Features[0].Feature.ID != "alpha" || first.Features[1].Feature.ID != "bravo" || first.NextCursor == "" {
		t.Fatalf("first page: %+v", first)
	}
	// New append-only history is excluded from this traversal's high-water mark.
	reportingFeature(t, s, "zulu", "demo", "ready")
	q.After = first.NextCursor
	second, err := s.Status(ctx, q, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Features) != 1 || second.Features[0].Feature.ID != "charlie" || second.NextCursor != "" {
		t.Fatalf("second page: %+v", second)
	}
	if first.Limits.Projects[0].Name != "demo" || first.Limits.Projects[0].Limit != 2 || first.Limits.Projects[1].Name != "other" || first.Limits.Projects[1].Limit != 1 {
		t.Fatalf("project limits: %+v", first.Limits)
	}
	if first.Calls.Limit != cfg.MaxCallsPerDay || first.Calls.Day != first.ObservedAt.Format("2006-01-02") {
		t.Fatalf("calls: %+v", first.Calls)
	}
	for _, invalid := range []model.StatusQuery{
		{Project: "unknown"}, {State: "invented"}, {Limit: -1}, {Limit: 501}, {After: "garbage"},
		{After: first.NextCursor, Project: "other", State: "ready"},
		{After: first.NextCursor, Project: "demo", State: "blocked"},
	} {
		if _, err = s.Status(ctx, invalid, cfg); !errors.Is(err, ErrInvalidQuery) {
			t.Fatalf("query %+v: %v", invalid, err)
		}
	}
}

func TestReportingDefaultLimitAndEmptyArrays(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	cfg := reportingConfig()
	empty, err := s.Status(ctx, model.StatusQuery{}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if empty.Features == nil || len(empty.Features) != 0 || empty.Limits.Projects == nil {
		t.Fatalf("empty snapshot: %+v", empty)
	}
	for i := range 103 {
		reportingFeature(t, s, fmt.Sprintf("item-%03d", i), "demo", "proposed")
	}
	page, err := s.Status(ctx, model.StatusQuery{}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Features) != 100 || page.NextCursor == "" {
		t.Fatalf("default page length=%d cursor=%q", len(page.Features), page.NextCursor)
	}
	all, err := s.Status(ctx, model.StatusQuery{Limit: 500}, cfg)
	if err != nil || len(all.Features) != 103 {
		t.Fatalf("max page: length=%d error=%v", len(all.Features), err)
	}
	detail, err := s.FeatureStatus(ctx, "item-000", cfg)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Attempts == nil || detail.Sessions == nil || detail.Checks == nil || detail.CurrentEvidence == nil || detail.Integration.Resources == nil || detail.Feature.Acceptance == nil || detail.Feature.DependsOn == nil || detail.Dependencies == nil || detail.Timeline == nil {
		t.Fatalf("null collections: %+v", detail)
	}
	for _, endpoint := range []string{"feature", "events", "evidence"} {
		var e error
		switch endpoint {
		case "feature":
			_, e = s.FeatureStatus(ctx, "absent", cfg)
		case "events":
			_, e = s.EventPage(ctx, "absent", model.PageQuery{})
		case "evidence":
			_, e = s.EvidencePage(ctx, "absent", model.PageQuery{})
		}
		if !errors.Is(e, ErrNotFound) {
			t.Fatalf("%s missing: %v", endpoint, e)
		}
	}
	events, err := s.EventPage(ctx, "item-000", model.PageQuery{})
	if err != nil || events.Items == nil || len(events.Items) != 0 {
		t.Fatalf("empty events: %+v %v", events, err)
	}
	evidence, err := s.EvidencePage(ctx, "item-000", model.PageQuery{})
	if err != nil || evidence.Items == nil || len(evidence.Items) != 0 {
		t.Fatalf("empty evidence: %+v %v", evidence, err)
	}
}

func TestReportingHistoryPaginationUsesDurableOrderAndScope(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	f := reportingFeature(t, s, "work", "demo", "running")
	a := reportingAttempt(t, s, f, "attempt", true, 1, "candidate")
	reportingFeature(t, s, "other-work", "demo", "ready")
	// IDs and times deliberately disagree with insertion order.
	for i, id := range []string{"z", "a", "m", "b", "q"} {
		at := time.Date(2026, 1, 10-i, 0, 0, 0, 0, time.UTC).Format(time.RFC3339Nano)
		if _, err := s.db.Exec(`INSERT INTO events(id,feature_id,kind,detail,created_at) VALUES(?,?,?,?,?)`, id, f.ID, "checks", "detail", at); err != nil {
			t.Fatal(err)
		}
		reportingEvidence(t, s, f, a, id, "detail", 1, "candidate")
	}
	first, err := s.EventPage(ctx, f.ID, model.PageQuery{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	efirst, err := s.EvidencePage(ctx, f.ID, model.PageQuery{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if first.Items[0].ID != "q" || first.Items[1].ID != "b" || efirst.Items[0].ID != "q" || efirst.Items[1].ID != "b" {
		t.Fatal("history is not newest-first durable sequence order")
	}
	if err = event(ctx, s.db, f.ID, "checks", "new event"); err != nil {
		t.Fatal(err)
	}
	reportingEvidence(t, s, f, a, "new", "new evidence", 1, "candidate")
	gotEvents := []string{first.Items[0].ID, first.Items[1].ID}
	cursor := first.NextCursor
	for cursor != "" {
		page, e := s.EventPage(ctx, f.ID, model.PageQuery{After: cursor, Limit: 2})
		if e != nil {
			t.Fatal(e)
		}
		for _, item := range page.Items {
			gotEvents = append(gotEvents, item.ID)
		}
		cursor = page.NextCursor
	}
	gotEvidence := []string{efirst.Items[0].ID, efirst.Items[1].ID}
	cursor = efirst.NextCursor
	for cursor != "" {
		page, e := s.EvidencePage(ctx, f.ID, model.PageQuery{After: cursor, Limit: 2})
		if e != nil {
			t.Fatal(e)
		}
		for _, item := range page.Items {
			gotEvidence = append(gotEvidence, item.ID)
		}
		cursor = page.NextCursor
	}
	if strings.Join(gotEvents, ",") != "q,b,m,a,z" || strings.Join(gotEvidence, ",") != "q,b,m,a,z" {
		t.Fatalf("unstable histories events=%v evidence=%v", gotEvents, gotEvidence)
	}
	for _, q := range []model.PageQuery{{Limit: -1}, {Limit: 501}, {After: "bad"}, {After: efirst.NextCursor}} {
		if _, err = s.EventPage(ctx, f.ID, q); !errors.Is(err, ErrInvalidQuery) {
			t.Fatalf("invalid events query %+v: %v", q, err)
		}
	}
	if _, err = s.EventPage(ctx, "other-work", model.PageQuery{After: first.NextCursor}); !errors.Is(err, ErrInvalidQuery) {
		t.Fatalf("cross-feature event cursor: %v", err)
	}
	if _, err = s.EvidencePage(ctx, "other-work", model.PageQuery{After: efirst.NextCursor}); !errors.Is(err, ErrInvalidQuery) {
		t.Fatalf("cross-feature evidence cursor: %v", err)
	}
	for _, q := range []model.PageQuery{{Limit: -1}, {Limit: 501}, {After: first.NextCursor}} {
		if _, err = s.EvidencePage(ctx, f.ID, q); !errors.Is(err, ErrInvalidQuery) {
			t.Fatalf("invalid evidence query %+v: %v", q, err)
		}
	}
}

func TestReportingCurrentEvidenceRequiresLatestAttemptGenerationAndSHA(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	f := reportingFeature(t, s, "work", "demo", "running")
	old := reportingAttempt(t, s, f, "old", false, 2, "same-sha")
	a := reportingAttempt(t, s, f, "current", true, 2, "same-sha")
	reportingEvidence(t, s, f, old, "previous-attempt", "old attempt", 2, "same-sha")
	reportingEvidence(t, s, f, a, "previous-generation", "old generation", 1, "same-sha")
	reportingEvidence(t, s, f, a, "previous-sha", "old sha", 2, "different-sha")
	reportingEvidence(t, s, f, a, "current-proof", "current", 2, "same-sha")
	out, err := s.FeatureStatus(ctx, f.ID, reportingConfig())
	if err != nil {
		t.Fatal(err)
	}
	if len(out.CurrentEvidence) != 1 || out.CurrentEvidence[0].ID != "current-proof" {
		t.Fatalf("stale proof exposed as current: %+v", out.CurrentEvidence)
	}
	all, err := s.EvidencePage(ctx, f.ID, model.PageQuery{})
	if err != nil || len(all.Items) != 4 {
		t.Fatalf("history discarded stale proof: %+v %v", all, err)
	}
	// A reimport's previous authority is history, not current proof.
	f.Revision = "replacement-revision"
	if err = put(ctx, s.db, f); err != nil {
		t.Fatal(err)
	}
	out, err = s.FeatureStatus(ctx, f.ID, reportingConfig())
	if err != nil || len(out.CurrentEvidence) != 0 {
		t.Fatalf("old revision proof: %+v %v", out.CurrentEvidence, err)
	}
}

func TestReportingSanitizesExplicitDTOsAndAllDetailEndpoints(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	cfg := reportingConfig()
	cfg.StateDir = "/private/state"
	cfg.WorkspaceDir = "/private/workspaces"
	cfg.Projects["demo"] = model.Project{MaxConcurrent: 2, Repository: "/private/repository", Auth: model.GitAuth{TokenFile: "/private/auth/token"}}
	f := reportingFeature(t, s, "work", "demo", "running")
	f.Body = "PRIVATE_BODY_SENTINEL"
	f.Acceptance = []model.Acceptance{{ID: "accept", Scenario: "PRIVATE_SCENARIO_SENTINEL", Expected: "PRIVATE_EXPECTED_SENTINEL"}}
	f.Receipt = "https://github.com/org/repo/pull/7?token=PRIVATE_QUERY_SENTINEL#PRIVATE_FRAGMENT_SENTINEL"
	if err := put(ctx, s.db, f); err != nil {
		t.Fatal(err)
	}
	a := model.Attempt{ID: "attempt", FeatureID: f.ID, State: "running", Stage: "checks", CandidateGeneration: 2, CandidateSHA: "candidate", SourceBaseSHA: "source-base", SourceCandidateSHA: "source-candidate", Workspace: "/private/workspaces/attempt", Summary: "PRIVATE_SUMMARY_SENTINEL", Cursor: "PRIVATE_CURSOR_SENTINEL", Fingerprint: "PRIVATE_FINGERPRINT_SENTINEL", IntegrationToken: "PRIVATE_TOKEN_SENTINEL", Wait: &model.WaitReason{Code: "integration_resource", Resource: "git:/private/repository:refs/heads/main", Detail: "waiting for /private/repository"}}
	reportingInsertAttempt(t, s, f, a, true)
	detail := strings.Join([]string{f.Body, f.Acceptance[0].Scenario, f.Acceptance[0].Expected, a.Summary, a.Cursor, a.Fingerprint, a.IntegrationToken, a.Workspace, "/private/auth/token", "\x00\x1b", strings.Repeat("é", 3000)}, " ")
	reportingEvidence(t, s, f, a, "proof", detail, 2, "candidate")
	if err := event(ctx, s.db, f.ID, "checks", detail); err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := s.db.Exec(`INSERT INTO checks(attempt_id,name,generation,sha,state,detail,started_at,updated_at) VALUES(?,?,?,?,?,?,?,?)`, a.ID, "unit", 2, "candidate", "passed", detail, at, at); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO sessions(attempt_id,thread_id,stage,dispatch_key,environment_id,created_at) VALUES(?,?,?,?,?,?)`, a.ID, "thread", "review", "attempt:review:2", "environment", at); err != nil {
		t.Fatal(err)
	}
	resources, _ := json.Marshal([]string{"git:/private/repository:refs/heads/main"})
	if _, err := s.db.Exec(`INSERT INTO integration_requests(attempt_id,revision,policy_hash,resources,state,token) VALUES(?,?,?,?,?,?)`, a.ID, f.Revision, f.PolicyHash, resources, "owned", a.IntegrationToken); err != nil {
		t.Fatal(err)
	}
	out, err := s.FeatureStatus(ctx, f.ID, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if out.Feature.Body != "" || len(out.Feature.Acceptance) != 0 || out.Feature.PolicyHash != "" || out.Integration.Token != "" {
		t.Fatalf("private fields retained: %+v", out)
	}
	if out.Receipt != "https://github.com/org/repo/pull/7" || out.Attempts[0].SourceCandidateSHA != "source-candidate" || out.Sessions[0].DispatchKey != "attempt:review:2" || out.Sessions[0].EnvironmentID != "environment" {
		t.Fatalf("public facts lost: %+v", out)
	}
	if len(out.Integration.Resources) != 1 || !strings.HasPrefix(out.Integration.Resources[0], "resource:") || out.Wait == nil || out.Wait.Resource != out.Integration.Resources[0] {
		t.Fatalf("unsafe/inconsistent resource label: %+v", out)
	}
	events, err := s.EventPage(ctx, f.ID, model.PageQuery{})
	if err != nil {
		t.Fatal(err)
	}
	evidence, err := s.EvidencePage(ctx, f.ID, model.PageQuery{})
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]any{"feature": out, "events": events, "evidence": evidence} {
		data, e := json.Marshal(value)
		if e != nil {
			t.Fatal(e)
		}
		for _, forbidden := range []string{"PRIVATE_", "/private/", "workspace\"", "fingerprint\"", "integration_token\"", "summary\"", "cursor\""} {
			if strings.Contains(string(data), forbidden) {
				t.Fatalf("%s leaks %q: %s", name, forbidden, data)
			}
		}
	}
	for _, value := range []string{out.Checks[0].Detail, out.CurrentEvidence[0].Detail, events.Items[0].Detail, evidence.Items[0].Detail} {
		if len(value) > reportDetailLimit || !utf8.ValidString(value) || strings.IndexFunc(value, unicode.IsControl) >= 0 {
			t.Fatalf("unbounded/invalid detail: bytes=%d", len(value))
		}
	}
}

func TestReportingIdleWaitMatchesSharedAdmissionGate(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	cfg := reportingConfig()
	f := reportingFeature(t, s, "waiting", "demo", "ready")
	busy := reportingFeature(t, s, "busy", "demo", "running")
	reportingAttempt(t, s, busy, "active", true, 1, "sha")
	cfg.Projects["demo"] = model.Project{MaxConcurrent: 1}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	expected, err := s.admissionGate(ctx, tx, f, cfg.PolicyHash, cfg.MaxAttempts, cfg.MaxConcurrent, 1)
	tx.Rollback()
	if err != nil || expected == nil {
		t.Fatalf("gate fixture: %+v %v", expected, err)
	}
	out, err := s.FeatureStatus(ctx, f.ID, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if out.Wait == nil || out.Wait.Code != expected.Code || out.Wait.OwnerID != expected.OwnerID {
		t.Fatalf("report diverges from admission: got=%+v want=%+v", out.Wait, expected)
	}
}

func TestReportingSnapshotRemainsConsistentDuringAtomicTransitions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	writer, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = writer.Exec(`PRAGMA busy_timeout=5000`); err != nil {
		writer.Close()
		t.Fatal(err)
	}
	f := reportingFeature(t, s, "work", "demo", "ready")
	a := reportingAttempt(t, s, f, "attempt", false, 0, "candidate-0")
	stop := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		for generation := 1; ; generation++ {
			select {
			case <-stop:
				done <- nil
				return
			default:
			}
			tx, e := writer.Begin()
			if e != nil {
				done <- e
				return
			}
			currentFeature, currentAttempt := f, a
			active := generation%2 != 0
			currentFeature.State = "ready"
			if active {
				currentFeature.State = "running"
			}
			currentAttempt.State = currentFeature.State
			currentAttempt.CandidateGeneration = generation
			currentAttempt.CandidateSHA = fmt.Sprintf("candidate-%d", generation)
			featureData, _ := json.Marshal(currentFeature)
			attemptData, _ := json.Marshal(currentAttempt)
			_, e = tx.Exec(`UPDATE features SET data=? WHERE id=?`, featureData, f.ID)
			if e == nil {
				_, e = tx.Exec(`UPDATE attempts SET active=?,data=? WHERE id=?`, active, attemptData, a.ID)
			}
			if e == nil {
				e = tx.Commit()
			} else {
				tx.Rollback()
			}
			if e != nil {
				done <- e
				return
			}
		}
	}()
	t.Cleanup(func() {
		close(stop)
		e := <-done
		writer.Close()
		if e != nil {
			t.Errorf("writer: %v", e)
		}
	})
	for range 40 {
		out, e := s.Status(context.Background(), model.StatusQuery{}, reportingConfig())
		if e != nil {
			t.Fatal(e)
		}
		if len(out.Features) != 1 || len(out.Features[0].Attempts) != 1 {
			t.Fatalf("incomplete snapshot: %+v", out)
		}
		entry := out.Features[0]
		attempt := entry.Attempts[0]
		wantActive := 0
		if attempt.Active {
			wantActive = 1
		}
		if out.Limits.Global.Active != wantActive || out.Limits.Projects[0].Active != wantActive || entry.Feature.State != attempt.State || attempt.CandidateSHA != fmt.Sprintf("candidate-%d", attempt.CandidateGeneration) {
			t.Fatalf("mixed transaction state: %+v", out)
		}
	}
}

func reportingWriteFeature(t *testing.T, s *Store, f model.Feature) {
	t.Helper()
	data, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`UPDATE features SET data=? WHERE id=?`, data, f.ID); err != nil {
		t.Fatal(err)
	}
}

func reportingHistoryEvent(t *testing.T, s *Store, featureID, kind, detail string, at time.Time, progress *Progress) {
	t.Helper()
	id := ID()
	if _, err := s.db.Exec(`INSERT INTO events(id,feature_id,kind,detail,created_at) VALUES(?,?,?,?,?)`, id, featureID, kind, detail, at.Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if progress != nil {
		data, err := json.Marshal(progress)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = s.db.Exec(`INSERT INTO event_progress(event_id,data) VALUES(?,?)`, id, data); err != nil {
			t.Fatal(err)
		}
	}
}

func TestReportingPreservesIdentifiersDespitePrivateSubstringCollisions(t *testing.T) {
	s := openTest(t)
	f := reportingFeature(t, s, "add-tests", "demo", "running")
	f.Title = "Add tests for test dispatch"
	// Even an actual private value colliding with an identifier must not break
	// route addressing, SHA association, or structured execution facts.
	f.Body = "tests"
	reportingWriteFeature(t, s, f)
	a := reportingAttempt(t, s, f, "tests-attempt", true, 1, "tests-candidate")
	cfg := reportingConfig()
	cfg.Projects["demo"] = model.Project{MaxConcurrent: 2, Checks: map[string]model.Command{"tests": {Argv: []string{"test"}}}}
	out, err := s.FeatureStatus(context.Background(), f.ID, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if out.Feature.ID != f.ID || out.Attempts[0].ID != a.ID || out.Attempts[0].CandidateSHA != a.CandidateSHA {
		t.Fatalf("private substring changed public identity: %+v", out)
	}
	// Ordinary command words are not secrets and must remain readable in titles.
	f.Body = "unrelated private body"
	reportingWriteFeature(t, s, f)
	out, err = s.FeatureStatus(context.Background(), f.ID, cfg)
	if err != nil || out.Feature.Title != f.Title {
		t.Fatalf("ordinary command word redacted title: %+v %v", out, err)
	}
}

func TestReportingDependenciesAndAcceptedCompletionTimeline(t *testing.T) {
	for _, tc := range []struct {
		target, receipt, stage string
		satisfied, closed      bool
	}{
		{"pull_request", "https://github.com/o/r/pull/42", "waiting_to_merge", false, false},
		{"reviewed", "reviewed:sha", "reviewed", false, true},
		{"integrated", "commit:abc", "integrated", true, true},
		{"staging", "commit:abc", "integrated", true, true},
		{"production", "commit:abc", "integrated", true, true},
		{"integrated", "", "reviewing", false, true},
	} {
		t.Run(tc.target+"/"+tc.receipt, func(t *testing.T) {
			s := openTest(t)
			dep := reportingFeature(t, s, "dependency", "demo", "running")
			dep.CompletionTarget, dep.Receipt = tc.target, tc.receipt
			reportingWriteFeature(t, s, dep)
			attempt := reportingAttempt(t, s, dep, "accepted-attempt", true, 1, "candidate")
			attempt.Stage, attempt.State = "accepted", "accepted"
			if err := s.Save(context.Background(), attempt, "accepted", "target "+tc.target+" proven", tc.receipt, false); err != nil {
				t.Fatal(err)
			}
			child := reportingFeature(t, s, "child", "demo", "proposed")
			child.DependsOn = []string{dep.ID, "missing"}
			reportingWriteFeature(t, s, child)
			out, err := s.FeatureStatus(context.Background(), child.ID, reportingConfig())
			if err != nil {
				t.Fatal(err)
			}
			if len(out.Dependencies) != 2 || out.Dependencies[0].State != "accepted" || out.Dependencies[0].Satisfied != tc.satisfied || out.Dependencies[1].State != "missing" || out.Dependencies[1].Satisfied {
				t.Fatalf("dependency gates: %+v", out.Dependencies)
			}
			accepted, err := s.FeatureStatus(context.Background(), dep.ID, reportingConfig())
			if err != nil {
				t.Fatal(err)
			}
			last := accepted.Timeline[len(accepted.Timeline)-1]
			if last.Stage != tc.stage || last.State != "accepted" || last.StartedAt.IsZero() || last.StartedAt.Before(dep.CreatedAt) || (last.FinishedAt != nil) != tc.closed {
				t.Fatalf("completion timeline: %+v", accepted.Timeline)
			}
		})
	}
}

func TestReportingTimelinePreservesHistoricalTargetAcrossRetryAndReimport(t *testing.T) {
	for _, reset := range []string{"retry_requested", "imported"} {
		t.Run(reset, func(t *testing.T) {
			s := openTest(t)
			f := reportingFeature(t, s, "work", "demo", "ready")
			at := func(n int64) time.Time { return time.Unix(n, 0).UTC() }
			f.CreatedAt, f.UpdatedAt = at(1), at(6)
			if reset == "imported" {
				f.State = "proposed"
			}
			reportingWriteFeature(t, s, f)
			old := f
			old.Revision, old.CompletionTarget = "old-revision", "pull_request"
			data, _ := json.Marshal(old)
			if _, err := s.db.Exec(`INSERT INTO revisions(feature_id,revision,data) VALUES(?,?,?)`, f.ID, old.Revision, data); err != nil {
				t.Fatal(err)
			}
			reportingHistoryEvent(t, s, f.ID, "imported", old.Revision, at(1), nil)
			reportingHistoryEvent(t, s, f.ID, "attempt_started", "old-attempt", at(2), nil)
			reportingHistoryEvent(t, s, f.ID, "accepted", "", at(3), &Progress{State: "accepted", Stage: "accepted", AttemptID: "old-attempt", Revision: old.Revision, CompletionTarget: "pull_request", Receipt: "https://github.com/o/r/pull/1"})
			reportingHistoryEvent(t, s, f.ID, reset, f.Revision, at(5), nil)
			out, err := s.FeatureStatus(context.Background(), f.ID, reportingConfig())
			if err != nil {
				t.Fatal(err)
			}
			foundPR := false
			for _, interval := range out.Timeline {
				if interval.State == "accepted" {
					foundPR = true
					if interval.Stage != "waiting_to_merge" || interval.FinishedAt == nil || !interval.FinishedAt.Equal(at(5)) {
						t.Fatalf("historical PR rewritten by current integrated target: %+v", out.Timeline)
					}
				}
			}
			last := out.Timeline[len(out.Timeline)-1]
			if !foundPR || last.Stage != "queued" || last.AttemptID != "" || last.FinishedAt != nil {
				t.Fatalf("old acceptance leaked after reset: %+v", out.Timeline)
			}
		})
	}
}

func TestReportingTimelineLegacyRevisionAndCancellation(t *testing.T) {
	s := openTest(t)
	at := func(n int64) time.Time { return time.Unix(n, 0).UTC() }
	f := reportingFeature(t, s, "work", "demo", "cancelled")
	f.CreatedAt, f.UpdatedAt = at(1), at(9)
	reportingWriteFeature(t, s, f)
	old := f
	old.Revision, old.CompletionTarget = "legacy", "integrated"
	data, _ := json.Marshal(old)
	if _, err := s.db.Exec(`INSERT INTO revisions(feature_id,revision,data) VALUES(?,?,?)`, f.ID, old.Revision, data); err != nil {
		t.Fatal(err)
	}
	for _, e := range []struct {
		kind, detail string
		second       int64
	}{
		{"imported", "legacy", 1}, {"attempt_started", "old", 2}, {"integrated", "", 3},
		{"imported", f.Revision, 4}, {"approved", "", 5}, {"attempt_started", "new", 6},
		{"cancel_requested", "", 7}, {"cancelled", "", 8},
	} {
		reportingHistoryEvent(t, s, f.ID, e.kind, e.detail, at(e.second), nil)
	}
	out, err := s.FeatureStatus(context.Background(), f.ID, reportingConfig())
	if err != nil {
		t.Fatal(err)
	}
	foundIntegrated, foundCancelling := false, false
	for _, interval := range out.Timeline {
		if interval.Stage == "integrated" {
			foundIntegrated = true
			if interval.AttemptID != "old" || interval.FinishedAt == nil || !interval.FinishedAt.Equal(at(4)) {
				t.Fatalf("legacy integration crossed revision: %+v", out.Timeline)
			}
		}
		if interval.State == "cancelling" {
			foundCancelling = true
			if interval.FinishedAt == nil || !interval.FinishedAt.Equal(at(8)) {
				t.Fatalf("cancellation wait not preserved: %+v", out.Timeline)
			}
		}
	}
	last := out.Timeline[len(out.Timeline)-1]
	if !foundIntegrated || !foundCancelling || last.State != "cancelled" || last.FinishedAt == nil {
		t.Fatalf("legacy/cancel timeline: %+v", out.Timeline)
	}
}

func TestReportingTimelineStreamingBoundAndClockAdjustment(t *testing.T) {
	s := openTest(t)
	f := reportingFeature(t, s, "work", "demo", "running")
	at := time.Unix(1, 0).UTC()
	f.CreatedAt, f.UpdatedAt = at, at.Add(3000*time.Second)
	reportingWriteFeature(t, s, f)
	for i := range 1100 {
		stage := "checks"
		if i%2 == 0 {
			stage = "build_running"
		}
		observed := at.Add(time.Duration(i+2) * time.Second)
		if i == 1099 {
			observed = at
		} // Sequence, not an adjusted clock, wins.
		reportingHistoryEvent(t, s, f.ID, stage, "", observed, &Progress{State: "running", Stage: stage, AttemptID: "attempt", Revision: f.Revision, CompletionTarget: "integrated"})
	}
	out, err := s.FeatureStatus(context.Background(), f.ID, reportingConfig())
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Timeline) != reportTimelineLimit {
		t.Fatalf("timeline not bounded: %d", len(out.Timeline))
	}
	for i, interval := range out.Timeline {
		if interval.FinishedAt != nil && interval.FinishedAt.Before(interval.StartedAt) {
			t.Fatalf("negative interval: %+v", interval)
		}
		if i > 0 && interval.StartedAt.Before(out.Timeline[i-1].StartedAt) {
			t.Fatal("clock correction reordered durable history")
		}
	}
	history, err := s.EventPage(context.Background(), f.ID, model.PageQuery{Limit: 500})
	if err != nil || len(history.Items) != 500 || history.NextCursor == "" {
		t.Fatalf("bounded projection discarded pageable history: %d %v", len(history.Items), err)
	}
}

func TestReportingDetailBoundsAndCompleteSessionsNewestFirst(t *testing.T) {
	s := openTest(t)
	f := reportingFeature(t, s, "work", "demo", "running")
	var a model.Attempt
	for i := range 105 {
		a = reportingAttempt(t, s, f, fmt.Sprintf("attempt-%03d", i), i == 104, 1, "candidate")
	}
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	at := time.Now().UTC().Format(time.RFC3339Nano)
	for i := range 501 {
		id := fmt.Sprintf("item-%03d", i)
		if _, err = tx.Exec(`INSERT INTO sessions(attempt_id,thread_id,stage,dispatch_key,environment_id,created_at) VALUES(?,?,?,?,?,?)`, a.ID, id, "review", id, "environment", at); err != nil {
			t.Fatal(err)
		}
		if _, err = tx.Exec(`INSERT INTO checks(attempt_id,name,generation,sha,state,detail,started_at,updated_at) VALUES(?,?,?,?,?,?,?,?)`, a.ID, id, 1, a.CandidateSHA, "passed", "passed", at, at); err != nil {
			t.Fatal(err)
		}
		data, _ := json.Marshal(model.Evidence{ID: id, FeatureID: f.ID, AttemptID: a.ID, Kind: "check:unit", CandidateSHA: a.CandidateSHA, CandidateGeneration: 1, Passed: true})
		if _, err = tx.Exec(`INSERT INTO evidence(id,feature_id,attempt_id,revision,policy_hash,data) VALUES(?,?,?,?,?,?)`, id, f.ID, a.ID, f.Revision, f.PolicyHash, data); err != nil {
			t.Fatal(err)
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	out, err := s.FeatureStatus(context.Background(), f.ID, reportingConfig())
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Attempts) != 100 || out.Attempts[0].ID != "attempt-104" || out.Attempts[99].ID != "attempt-005" {
		t.Fatalf("attempt history: %+v", out.Attempts)
	}
	if len(out.Sessions) != 501 || out.Sessions[0].ThreadID != "item-500" || out.Sessions[500].ThreadID != "item-000" {
		t.Fatal("known sessions were truncated or not newest-first")
	}
	if len(out.Checks) != 500 || out.Checks[0].Name != "item-500" || len(out.CurrentEvidence) != 500 || out.CurrentEvidence[0].ID != "item-500" {
		t.Fatal("detail histories are unbounded or not newest-first")
	}
	page, err := s.EvidencePage(context.Background(), f.ID, model.PageQuery{Limit: 500})
	if err != nil || len(page.Items) != 500 || page.NextCursor == "" {
		t.Fatalf("first evidence history: %+v %v", page, err)
	}
	older, err := s.EvidencePage(context.Background(), f.ID, model.PageQuery{After: page.NextCursor, Limit: 500})
	if err != nil || len(older.Items) != 1 || older.Items[0].ID != "item-000" {
		t.Fatalf("bounded detail destroyed history: %+v %v", older, err)
	}
}

func TestReportingHistoricalDetailsSanitizeOriginalAuthority(t *testing.T) {
	s := openTest(t)
	f := reportingFeature(t, s, "work", "demo", "running")
	old := f
	old.Revision, old.Body = "old-revision", "PRIVATE_OLD_BODY_SENTINEL"
	data, _ := json.Marshal(old)
	if _, err := s.db.Exec(`INSERT INTO revisions(feature_id,revision,data) VALUES(?,?,?)`, f.ID, old.Revision, data); err != nil {
		t.Fatal(err)
	}
	a := model.Attempt{ID: "old-attempt", FeatureID: f.ID, Stage: "checks", Summary: "PRIVATE_OLD_SUMMARY_SENTINEL", CandidateSHA: "old-sha", CandidateGeneration: 1}
	reportingInsertAttempt(t, s, old, a, false)
	detail := "safe result " + old.Body + " " + a.Summary
	reportingEvidence(t, s, old, a, "old-proof", detail, 1, a.CandidateSHA)
	reportingHistoryEvent(t, s, f.ID, "checks", detail, time.Now().UTC(), &Progress{State: "running", Stage: "checks", AttemptID: a.ID, Revision: old.Revision, CompletionTarget: "integrated"})
	at := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := s.db.Exec(`INSERT INTO checks(attempt_id,name,generation,sha,state,detail,started_at,updated_at) VALUES(?,?,?,?,?,?,?,?)`, a.ID, "unit", 1, a.CandidateSHA, "passed", detail, at, at); err != nil {
		t.Fatal(err)
	}
	for i := range 101 {
		reportingAttempt(t, s, f, fmt.Sprintf("new-%03d", i), i == 100, 2, "new-sha")
	}
	featureStatus, err := s.FeatureStatus(context.Background(), f.ID, reportingConfig())
	if err != nil {
		t.Fatal(err)
	}
	events, err := s.EventPage(context.Background(), f.ID, model.PageQuery{})
	if err != nil {
		t.Fatal(err)
	}
	evidence, err := s.EvidencePage(context.Background(), f.ID, model.PageQuery{})
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{featureStatus.Checks[0].Detail, events.Items[0].Detail, evidence.Items[0].Detail} {
		if strings.Contains(value, "PRIVATE_OLD") || !strings.Contains(value, "safe result") {
			t.Fatalf("historical authority privacy lost outside newest attempts: %q", value)
		}
	}
}
