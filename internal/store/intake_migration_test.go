package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"openfactory/internal/intake"
	"openfactory/internal/model"
)

// Freeze the schema-3 source shape independently of intake.Revision. Adding an
// optional internal association must not silently rehash ordinary old contracts.
func intakeMigrationLegacyRevision(t *testing.T, f model.Feature) string {
	t.Helper()
	source := struct {
		ID, Project, Title, Kind, Risk string
		Priority                       int
		DependsOn                      []string
		CompletionTarget               string
		Checks                         []string
		Acceptance                     []model.Acceptance
		Body                           string
		DiscoveryStepID                string                     `json:",omitempty"`
		ProgramEvidenceGate            *model.ProgramEvidenceGate `json:",omitempty"`
	}{f.ID, f.Project, f.Title, f.Kind, f.Risk, f.Priority, f.DependsOn, f.CompletionTarget, f.Checks, f.Acceptance, f.Body, f.DiscoveryStepID, f.ProgramEvidenceGate}
	raw, err := json.Marshal(source)
	if err != nil {
		t.Fatal(err)
	}
	return intake.SHA256(raw)
}

func TestIntakeMigrationThreeToFourPreservesHistoricalAuthorityAndBudgets(t *testing.T) {
	ctx := context.Background()
	name := filepath.Join(t.TempDir(), "schema-three.sqlite")
	db, err := sql.Open("sqlite", name)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	// Construct schema 3 directly: this must exercise a real upgrade, not reopen
	// a schema-4 database with only its PRAGMA version artificially downgraded.
	_, err = db.ExecContext(ctx, `
CREATE TABLE features(id TEXT PRIMARY KEY,data BLOB NOT NULL);
CREATE TABLE revisions(feature_id TEXT NOT NULL REFERENCES features(id),revision TEXT NOT NULL,data BLOB NOT NULL,PRIMARY KEY(feature_id,revision));
CREATE TABLE attempts(id TEXT PRIMARY KEY,feature_id TEXT NOT NULL REFERENCES features(id),active INTEGER NOT NULL,revision TEXT NOT NULL,policy_hash TEXT NOT NULL,data BLOB NOT NULL);
CREATE UNIQUE INDEX one_active_attempt ON attempts(feature_id) WHERE active=1;
CREATE TABLE calls(id TEXT PRIMARY KEY,attempt_id TEXT NOT NULL REFERENCES attempts(id),stage TEXT NOT NULL,dispatch_key TEXT NOT NULL,generation INTEGER NOT NULL DEFAULT 0,day TEXT NOT NULL,UNIQUE(attempt_id,dispatch_key));
CREATE INDEX calls_by_day ON calls(day);
CREATE TABLE sessions(attempt_id TEXT NOT NULL REFERENCES attempts(id),thread_id TEXT NOT NULL,stage TEXT NOT NULL,dispatch_key TEXT NOT NULL DEFAULT '',environment_id TEXT NOT NULL DEFAULT '',created_at TEXT NOT NULL DEFAULT '',PRIMARY KEY(attempt_id,thread_id));
CREATE TABLE events(seq INTEGER PRIMARY KEY AUTOINCREMENT,id TEXT UNIQUE NOT NULL,feature_id TEXT NOT NULL,kind TEXT NOT NULL,detail TEXT NOT NULL,created_at TEXT NOT NULL);
CREATE TABLE evidence(id TEXT PRIMARY KEY,feature_id TEXT NOT NULL REFERENCES features(id),attempt_id TEXT NOT NULL REFERENCES attempts(id),revision TEXT NOT NULL,policy_hash TEXT NOT NULL,data BLOB NOT NULL);
CREATE TABLE settings(key TEXT PRIMARY KEY,value TEXT NOT NULL);
CREATE TABLE legacy_integration_recovery(attempt_id TEXT PRIMARY KEY REFERENCES attempts(id));
CREATE TABLE integration_requests(seq INTEGER PRIMARY KEY AUTOINCREMENT,attempt_id TEXT UNIQUE NOT NULL REFERENCES attempts(id),revision TEXT NOT NULL,policy_hash TEXT NOT NULL,resources BLOB NOT NULL,state TEXT NOT NULL,token TEXT NOT NULL DEFAULT '');
CREATE TABLE integration_owners(resource TEXT PRIMARY KEY,attempt_id TEXT NOT NULL REFERENCES attempts(id),token TEXT NOT NULL);
CREATE TABLE checks(attempt_id TEXT NOT NULL REFERENCES attempts(id),name TEXT NOT NULL,generation INTEGER NOT NULL,sha TEXT NOT NULL,state TEXT NOT NULL,detail TEXT NOT NULL,started_at TEXT NOT NULL,updated_at TEXT NOT NULL,PRIMARY KEY(attempt_id,name,generation,sha));
`+programSchema+programResearchSchema+programExperimentSchema+`PRAGMA user_version=3;`)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2025, 6, 7, 8, 9, 10, 0, time.UTC)
	day := time.Now().UTC().Format("2006-01-02")
	policy := "historical-policy"
	accepted := feature("historical-accepted")
	accepted.Revision = intakeMigrationLegacyRevision(t, accepted)
	accepted.PolicyHash, accepted.State, accepted.Receipt = policy, "accepted", "integrated:historical-commit"
	accepted.Attempts, accepted.CreatedAt, accepted.UpdatedAt = 1, at, at
	ready := feature("historical-ready")
	ready.Revision = intakeMigrationLegacyRevision(t, ready)
	ready.PolicyHash, ready.State, ready.CreatedAt, ready.UpdatedAt = policy, "ready", at, at
	marshal := func(value any) []byte {
		t.Helper()
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := db.ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	acceptedRaw, readyRaw := marshal(accepted), marshal(ready)
	for _, f := range []model.Feature{accepted, ready} {
		raw := marshal(f)
		exec(`INSERT INTO features(id,data) VALUES(?,?)`, f.ID, raw)
		exec(`INSERT INTO revisions(feature_id,revision,data) VALUES(?,?,?)`, f.ID, f.Revision, raw)
	}
	attempt := model.Attempt{ID: "historical-attempt", FeatureID: accepted.ID, Stage: "integrated", State: "accepted",
		ThreadID: "historical-thread", EnvironmentID: "historical-environment", DispatchKey: "historical-dispatch",
		Workspace: "/historical/worktree", BaseSHA: "base", CandidateSHA: "candidate", CandidateGeneration: 2,
		StartedAt: at, StageStartedAt: at, UpdatedAt: at, Deadline: at.Add(time.Hour)}
	attemptRaw := marshal(attempt)
	exec(`INSERT INTO attempts(id,feature_id,active,revision,policy_hash,data) VALUES(?,?,0,?,?,?)`, attempt.ID, accepted.ID, accepted.Revision, policy, attemptRaw)
	exec(`INSERT INTO calls(id,attempt_id,stage,dispatch_key,generation,day) VALUES(?,?,?,?,?,?)`, "historical-call", attempt.ID, "build", attempt.DispatchKey, attempt.CandidateGeneration, day)
	exec(`INSERT INTO sessions(attempt_id,thread_id,stage,dispatch_key,environment_id,created_at) VALUES(?,?,?,?,?,?)`, attempt.ID, attempt.ThreadID, "build", attempt.DispatchKey, attempt.EnvironmentID, at.Format(time.RFC3339Nano))
	exec(`INSERT INTO events(id,feature_id,kind,detail,created_at) VALUES(?,?,?,?,?)`, "historical-acceptance", accepted.ID, "integrated", accepted.Receipt, at.Format(time.RFC3339Nano))
	evidence := model.Evidence{ID: "historical-evidence", FeatureID: accepted.ID, AttemptID: attempt.ID, Kind: "unit", CandidateSHA: attempt.CandidateSHA, CandidateGeneration: attempt.CandidateGeneration, Passed: true, Detail: "historical passed check", CreatedAt: at}
	evidenceRaw := marshal(evidence)
	exec(`INSERT INTO evidence(id,feature_id,attempt_id,revision,policy_hash,data) VALUES(?,?,?,?,?,?)`, evidence.ID, accepted.ID, attempt.ID, accepted.Revision, policy, evidenceRaw)

	spec := model.ProgramSpec{ID: "historical-program", Project: "demo", Title: "Historical program", Objective: "Retain lifetime accounting and old local authority",
		Disclosure: model.ProgramDisclosure{ModelProviderIDs: []string{"test"}}, Limits: model.ProgramLimits{MaxIterations: 3, MaxModelCalls: 20, MaxSteps: 32, MaxExperiments: 8}}
	revision := model.ProgramRevision{ProgramID: spec.ID, Number: 1, Spec: spec, SeedManifestSHA: "", CreatedAt: at, Reason: "import"}
	revision.RevisionHash = programHash(struct {
		ProgramID       string
		Number          int
		Spec            model.ProgramSpec
		SeedManifestSHA string
	}{spec.ID, 1, spec, ""})
	program := model.Program{ID: spec.ID, Project: spec.Project, Title: spec.Title, CurrentRevision: 1, State: model.ProgramActive, Iteration: 2, CreatedAt: at, UpdatedAt: at}
	budget := model.ProgramBudget{ModelCalls: 7, Steps: 9, Experiments: 2, Searches: 3, Sources: 4, CaptureBytes: 1234, ReservedCaptureBytes: 128}
	programRaw, revisionRaw, budgetRaw := marshal(program), marshal(revision), marshal(budget)
	// The old representation has ONLY Actor. Do not serialize the new Audit
	// struct's zero value and accidentally create a schema-4-looking fixture.
	grantRaw := marshal(struct {
		ProgramID    string    `json:"program_id"`
		RevisionHash string    `json:"revision_hash"`
		PolicyHash   string    `json:"policy_hash"`
		Actor        string    `json:"actor"`
		CreatedAt    time.Time `json:"created_at"`
	}{spec.ID, revision.RevisionHash, policy, "legacy-cli-operator", at})
	exec(`INSERT INTO programs(id,data) VALUES(?,?)`, spec.ID, programRaw)
	exec(`INSERT INTO program_revisions(program_id,number,revision_hash,data) VALUES(?,1,?,?)`, spec.ID, revision.RevisionHash, revisionRaw)
	exec(`INSERT INTO program_budgets(program_id,data) VALUES(?,?)`, spec.ID, budgetRaw)
	exec(`INSERT INTO program_grants(program_id,revision_hash,subject_hash,policy_hash,data) VALUES(?,?,'',?,?)`, spec.ID, revision.RevisionHash, policy, grantRaw)
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := Open(name)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	var version int
	if err = s.db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil || version != 4 {
		t.Fatalf("upgrade version: %d %v", version, err)
	}
	for _, row := range []struct {
		name, query string
		args        []any
		want        []byte
	}{
		{"accepted feature", `SELECT data FROM features WHERE id=?`, []any{accepted.ID}, acceptedRaw},
		{"ready feature", `SELECT data FROM features WHERE id=?`, []any{ready.ID}, readyRaw},
		{"accepted immutable source", `SELECT data FROM revisions WHERE feature_id=? AND revision=?`, []any{accepted.ID, accepted.Revision}, acceptedRaw},
		{"accepted attempt", `SELECT data FROM attempts WHERE id=?`, []any{attempt.ID}, attemptRaw},
		{"accepted evidence", `SELECT data FROM evidence WHERE id=?`, []any{evidence.ID}, evidenceRaw},
		{"program", `SELECT data FROM programs WHERE id=?`, []any{spec.ID}, programRaw},
		{"program revision", `SELECT data FROM program_revisions WHERE program_id=? AND number=1`, []any{spec.ID}, revisionRaw},
		{"lifetime budget", `SELECT data FROM program_budgets WHERE program_id=?`, []any{spec.ID}, budgetRaw},
		{"legacy grant", `SELECT data FROM program_grants WHERE program_id=?`, []any{spec.ID}, grantRaw},
	} {
		var got []byte
		if err = s.db.QueryRowContext(ctx, row.query, row.args...).Scan(&got); err != nil || !bytes.Equal(got, row.want) {
			t.Fatalf("migration rewrote %s: got %s, want %s: %v", row.name, got, row.want, err)
		}
	}
	for _, f := range []model.Feature{accepted, ready} {
		loaded, err := s.Get(ctx, f.ID)
		if err != nil || loaded.Revision != f.Revision || intake.Revision(loaded) != f.Revision || loaded.IntakeStepID != "" {
			t.Fatalf("ordinary historical revision/hash changed: %+v %v", loaded, err)
		}
	}
	retainedAccepted, err := s.Get(ctx, accepted.ID)
	if err != nil || !DependencySatisfied(retainedAccepted) {
		t.Fatalf("migration lost its ordinary integrated receipt: %+v %v", retainedAccepted, err)
	}
	commandTestCount(t, s, `SELECT count(*) FROM attempts WHERE id=? AND active=0 AND revision=? AND policy_hash=?`, 1, attempt.ID, accepted.Revision, policy)
	commandTestCount(t, s, `SELECT count(*) FROM calls WHERE id=? AND attempt_id=? AND stage='build' AND dispatch_key=? AND generation=2 AND day=?`, 1, "historical-call", attempt.ID, attempt.DispatchKey, day)
	commandTestCount(t, s, `SELECT count(*) FROM sessions WHERE attempt_id=? AND thread_id=? AND dispatch_key=? AND environment_id=?`, 1, attempt.ID, attempt.ThreadID, attempt.DispatchKey, attempt.EnvironmentID)
	commandTestCount(t, s, `SELECT count(*) FROM events WHERE id='historical-acceptance' AND detail=?`, 1, accepted.Receipt)
	var grant model.ProgramGrant
	var retainedGrant []byte
	if err = s.db.QueryRowContext(ctx, `SELECT data FROM program_grants WHERE program_id=?`, spec.ID).Scan(&retainedGrant); err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(retainedGrant, &grant); err != nil || grant.Actor != "legacy-cli-operator" || grant.Audit.Kind != "" || grant.Audit.OperatorID != "" || grant.Audit.ApprovalID != "" {
		t.Fatalf("historical local actor became verified phone evidence: %+v %v", grant, err)
	}
	for _, table := range []string{"intake_operators", "intake_credentials", "intake_pairings", "intake_browser_sessions", "intake_auth_challenges", "intake_reviews", "intake_approval_receipts", "intake_commands", "intake_audits"} {
		commandTestCount(t, s, `SELECT count(*) FROM `+table, 0)
	}
	// Preservation is behavioral: the old UTC-day reservation still prevents a
	// new dispatch, while old program revision authority and all counters survive.
	a, err := s.Claim(ctx, ready.ID, policy, 3, 1, 1, 3600)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ReserveCall(ctx, a, "build", a.ID+":build", 1); !errors.Is(err, ErrWaiting) {
		t.Fatalf("migration reset the global daily call allowance: %v", err)
	}
	commandTestCount(t, s, `SELECT count(*) FROM calls`, 1)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	p, r, err := s.programAuthorityTx(ctx, tx, spec.ID, revision.Number, policy)
	if err != nil || p.Iteration != program.Iteration || r.RevisionHash != revision.RevisionHash {
		t.Fatalf("migration revoked historical exact program grant: %+v %+v %v", p, r, err)
	}
	retained, err := getProgramBudgetTx(ctx, tx, spec.ID)
	if err != nil || retained != budget {
		t.Fatalf("migration reset program lifetime reservations: %+v %v", retained, err)
	}
}
