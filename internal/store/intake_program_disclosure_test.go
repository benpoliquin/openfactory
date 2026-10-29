package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"openfactory/internal/model"
)

func TestIntakeProgramDraftDisclosureDoesNotGrantExecution(t *testing.T) {
	cases := []struct {
		name       string
		disclosure model.ProgramDisclosure
		noSeeds    bool
	}{
		{name: "private bundle not disclosed", disclosure: model.ProgramDisclosure{ModelProviderIDs: []string{"provider"}}},
		{name: "provider not disclosed", disclosure: model.ProgramDisclosure{PrivateMaterialToModels: true}},
		{name: "public search not disclosed", disclosure: model.ProgramDisclosure{PrivateMaterialToModels: true, ModelProviderIDs: []string{"provider"}, PublicSearchTopics: []model.ProgramSearchTopic{{ID: "topic", Query: "public protocol documentation"}}}},
		{name: "unseeded private bundle not disclosed", noSeeds: true, disclosure: model.ProgramDisclosure{ModelProviderIDs: []string{"provider"}, PublicSearchAllowed: true, PublicSearchTopics: []model.ProgramSearchTopic{{ID: "topic", Query: "public protocol documentation"}}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, cfg, op, session := intakeReviewsFixture(t)
			ctx := context.Background()
			bundle := intakeBundlesFinalized(t, s, cfg, op.ID, []byte("private proposal source"))
			spec := model.ProgramSpec{ID: "disclosure-program", Project: "demo", Title: "Human proposal", Objective: "Preserve a proposal before execution consent", Seeds: []model.ProgramSeed{{ID: "private", Kind: "bundle", Path: "inputs/private.md"}}, Disclosure: tc.disclosure}
			if tc.noSeeds {
				spec.Seeds = nil
			}
			revision, err := s.ImportProgram(ctx, spec, bundle)
			if err != nil {
				t.Fatal(err)
			}
			if revision.SeedManifestSHA != bundle.ManifestSHA || programHash(revision.Spec) != programHash(spec) {
				t.Fatal("draft persistence changed the exact bundle or manufactured disclosure")
			}
			if err = s.ApproveProgram(ctx, spec.ID, revision.RevisionHash, cfg.PolicyHash, model.TrustedLocalAudit()); err == nil {
				t.Fatal("missing disclosure granted Program execution")
			}
			intakeReviewsCode(t, err, "disclosure_required")
			command := intakeReviewsCommand(t, model.ActionProgramApproveRevision, model.SubjectCommandPayload{Kind: "program", ID: spec.ID, Revision: revision.RevisionHash})
			_, err = s.CreateIntakeReview(ctx, session.TokenHash, command, cfg)
			intakeReviewsCode(t, err, "disclosure_required")
			program, err := programLoad(ctx, s.db, spec.ID)
			if err != nil {
				t.Fatal(err)
			}
			if program.State != model.ProgramDraft || program.CurrentRevision != revision.Number {
				t.Fatal("rejected approval changed the saved proposal")
			}
			var grants, steps, challenges int
			if err = s.db.QueryRowContext(ctx, `SELECT count(*) FROM program_grants`).Scan(&grants); err != nil {
				t.Fatal(err)
			}
			if err = s.db.QueryRowContext(ctx, `SELECT count(*) FROM program_steps`).Scan(&steps); err != nil {
				t.Fatal(err)
			}
			if err = s.db.QueryRowContext(ctx, `SELECT count(*) FROM intake_auth_challenges`).Scan(&challenges); err != nil {
				t.Fatal(err)
			}
			var raw []byte
			if err = s.db.QueryRowContext(ctx, `SELECT data FROM program_budgets WHERE program_id=?`, spec.ID).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			var budget model.ProgramBudget
			if err = json.Unmarshal(raw, &budget); err != nil {
				t.Fatal(err)
			}
			if grants != 0 || steps != 0 || challenges != 0 || budget.ModelCalls != 0 {
				t.Fatal("draft persistence or rejected approval created execution authority or work")
			}
			for _, table := range []string{"calls", "program_capabilities", "program_research_operations", "program_sources", "intake_approval_receipts", "intake_commands"} {
				commandTestCount(t, s, `SELECT count(*) FROM `+table, 0)
			}
			commandTestCount(t, s, `SELECT count(*) FROM program_events`, 1)
			spec.Disclosure.PrivateMaterialToModels = true
			spec.Disclosure.ModelProviderIDs = []string{"provider"}
			if len(spec.Disclosure.PublicSearchTopics) != 0 {
				spec.Disclosure.PublicSearchAllowed = true
			}
			approvedRevision, err := s.ImportProgram(ctx, spec, bundle)
			if err != nil {
				t.Fatal(err)
			}
			if approvedRevision.Number != revision.Number+1 || approvedRevision.RevisionHash == revision.RevisionHash {
				t.Fatal("new disclosure did not produce a new exact Program revision")
			}
			command.Payload, err = json.Marshal(model.SubjectCommandPayload{Kind: "program", ID: spec.ID, Revision: approvedRevision.RevisionHash})
			if err != nil {
				t.Fatal(err)
			}
			review, err := s.CreateIntakeReview(ctx, session.TokenHash, command, cfg)
			if err != nil {
				t.Fatal(err)
			}
			if review.ProgramRevision == nil || review.ProgramRevision.RevisionHash != approvedRevision.RevisionHash || review.SeedManifest == nil || review.SeedManifest.ManifestSHA != bundle.ManifestSHA {
				t.Fatal("consented review lost the exact Program revision or selected bundle")
			}
			if err = s.ApproveProgram(ctx, spec.ID, revision.RevisionHash, cfg.PolicyHash, model.TrustedLocalAudit()); err == nil {
				t.Fatal("old nonconsented revision acquired the new revision's authority")
			}
			intakeReviewsCode(t, err, "stale_revision")
			if err = s.ApproveProgram(ctx, spec.ID, approvedRevision.RevisionHash, cfg.PolicyHash, model.TrustedLocalAudit()); err != nil {
				t.Fatal(err)
			}
			if err = s.db.QueryRowContext(ctx, `SELECT count(*) FROM program_grants WHERE program_id=? AND revision_hash=? AND subject_hash='' AND policy_hash=?`, spec.ID, approvedRevision.RevisionHash, cfg.PolicyHash).Scan(&grants); err != nil {
				t.Fatal(err)
			}
			if grants != 1 {
				t.Fatal("explicit disclosure did not grant only the exact revised Program")
			}
			if tc.noSeeds {
				work, e := s.ProgramWork(ctx, cfg)
				if e != nil {
					t.Fatal(e)
				}
				if len(work) != 1 || work[0].Step == nil || work[0].Wait != nil {
					t.Fatalf("explicit revised disclosure did not authorize model work: %+v", work)
				}
				step, e := s.EnsureProgramStep(ctx, *work[0].Step, cfg)
				if e != nil {
					t.Fatal(e)
				}
				a, e := s.Claim(ctx, step.FeatureID, cfg.PolicyHash, 3, 1, 1, 3600)
				if e != nil {
					t.Fatal(e)
				}
				if _, e = s.IssueResearchCapability(ctx, a, cfg); e != nil {
					t.Fatal(e)
				}
				if e = s.ReserveCall(ctx, a, a.Stage, a.ID+":research", 20); e != nil {
					t.Fatal(e)
				}
			}
		})
	}
}

func TestIntakeProgramLegacyGrantCannotBypassDisclosureOrMaskStorageErrors(t *testing.T) {
	s, cfg, op, session := intakeReviewsFixture(t)
	ctx := context.Background()
	bundle := intakeBundlesFinalized(t, s, cfg, op.ID, []byte("private source retained before consent"))
	spec := model.ProgramSpec{ID: "legacy-disclosure-program", Project: "demo", Title: "Legacy grant", Objective: "Do not treat persisted grants as implicit disclosure", Disclosure: model.ProgramDisclosure{ModelProviderIDs: []string{"provider"}, PublicSearchAllowed: true, PublicSearchTopics: []model.ProgramSearchTopic{{ID: "topic", Query: "public protocol documentation"}}}}
	revision, err := s.ImportProgram(ctx, spec, bundle)
	if err != nil {
		t.Fatal(err)
	}
	program, err := programLoad(ctx, s.db, spec.ID)
	if err != nil {
		t.Fatal(err)
	}
	program.State = model.ProgramWaiting
	program.Wait = &model.WaitReason{Code: "approval_required", Detail: "Legacy Program needs explicit private source disclosure"}
	raw, err := json.Marshal(program)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.ExecContext(ctx, `UPDATE programs SET data=? WHERE id=?`, raw, spec.ID); err != nil {
		t.Fatal(err)
	}
	grant := model.ProgramGrant{ProgramID: spec.ID, RevisionHash: revision.RevisionHash, PolicyHash: cfg.PolicyHash, Audit: model.TrustedLocalAudit(), CreatedAt: time.Now().UTC()}
	raw, err = json.Marshal(grant)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.ExecContext(ctx, `INSERT INTO program_grants(program_id,revision_hash,subject_hash,policy_hash,data) VALUES(?,?,'',?,?)`, spec.ID, revision.RevisionHash, cfg.PolicyHash, raw); err != nil {
		t.Fatal(err)
	}
	command := intakeReviewsCommand(t, model.ActionProgramResume, model.SubjectCommandPayload{Kind: "program", ID: spec.ID, Revision: revision.RevisionHash})
	_, err = s.CreateIntakeReview(ctx, session.TokenHash, command, cfg)
	intakeReviewsCode(t, err, "disclosure_required")
	var challenges int
	if err = s.db.QueryRowContext(ctx, `SELECT count(*) FROM intake_auth_challenges`).Scan(&challenges); err != nil {
		t.Fatal(err)
	}
	if challenges != 0 {
		t.Fatal("legacy grant with missing disclosure created an unusable resume challenge")
	}
	current, err := programLoad(ctx, s.db, spec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.State != model.ProgramWaiting || current.Wait == nil || current.CurrentRevision != revision.Number {
		t.Fatal("rejected resume review changed the legacy Program")
	}
	_, err = s.EnsureProgramStep(ctx, model.ProgramStepSpec{ProgramID: spec.ID, Revision: revision.Number, Kind: model.StepResearch}, cfg)
	intakeReviewsCode(t, err, "disclosure_required")
	var steps int
	if err = s.db.QueryRowContext(ctx, `SELECT count(*) FROM program_steps`).Scan(&steps); err != nil {
		t.Fatal(err)
	}
	if steps != 0 {
		t.Fatal("legacy grant queued model work without explicit Program disclosure")
	}
	// Simulate legacy server-owned work and a previously persisted capability.
	// Every model/research entry point must re-read the actual bundle, rather than
	// treating the old grant or capability as substitute source consent.
	step := model.ProgramStep{ID: "legacy-step", ProgramID: spec.ID, Revision: revision.Number, Kind: model.StepResearch, FeatureID: "openfactory-discovery-legacy-step", State: "running", Model: cfg.AstraModel}
	f := model.Feature{ID: step.FeatureID, Project: spec.Project, Title: "Legacy research", Kind: "research", CompletionTarget: "reviewed", DiscoveryStepID: step.ID, Revision: "legacy-feature-revision", PolicyHash: cfg.PolicyHash, State: "running"}
	if err = put(ctx, s.db, f); err != nil {
		t.Fatal(err)
	}
	raw, err = json.Marshal(step)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.ExecContext(ctx, `INSERT INTO program_steps(id,program_id,revision,feature_id,data) VALUES(?,?,?,?,?)`, step.ID, spec.ID, revision.Number, f.ID, raw); err != nil {
		t.Fatal(err)
	}
	a := model.Attempt{ID: "legacy-attempt", FeatureID: f.ID, State: "running", Stage: "research", Deadline: time.Now().UTC().Add(time.Hour)}
	reportingInsertAttempt(t, s, f, a, true)
	rec := researchCapabilityRecord{Capability: model.ResearchCapability{ProgramID: spec.ID, Revision: revision.Number, StepID: step.ID, AttemptID: a.ID, Deadline: a.Deadline}, PolicyHash: cfg.PolicyHash, Policy: researchPolicy(cfg.Discovery)}
	raw, err = json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	token := strings.Repeat("d", 64)
	if _, err = s.db.ExecContext(ctx, `INSERT INTO program_capabilities(token_hash,attempt_id,program_id,revision,policy_hash,deadline,data) VALUES(?,?,?,?,?,?,?)`, researchHash(token), a.ID, spec.ID, revision.Number, cfg.PolicyHash, a.Deadline.Format(time.RFC3339Nano), raw); err != nil {
		t.Fatal(err)
	}
	_, err = s.IssueResearchCapability(ctx, a, cfg)
	intakeReviewsCode(t, err, "disclosure_required")
	err = s.ReserveCall(ctx, a, a.Stage, a.ID+":research", 20)
	intakeReviewsCode(t, err, "disclosure_required")
	err = s.BeginOperation(ctx, a, cfg.PolicyHash)
	intakeReviewsCode(t, err, "disclosure_required")
	_, err = s.AuthorizeResearch(ctx, token, model.ResearchOperationRequest{RequestID: "legacy-search", Kind: "search", TopicID: "topic"})
	intakeReviewsCode(t, err, "disclosure_required")
	for _, table := range []string{"calls", "program_research_operations", "program_sources", "program_searches", "intake_auth_challenges", "intake_approval_receipts", "intake_commands"} {
		commandTestCount(t, s, `SELECT count(*) FROM `+table, 0)
	}
	commandTestCount(t, s, `SELECT count(*) FROM program_capabilities`, 1)
	commandTestCount(t, s, `SELECT count(*) FROM program_steps`, 1)
	if err = s.db.QueryRowContext(ctx, `SELECT data FROM program_budgets WHERE program_id=?`, spec.ID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var budget model.ProgramBudget
	if err = json.Unmarshal(raw, &budget); err != nil || budget != (model.ProgramBudget{}) {
		t.Fatalf("rejected legacy authority changed lifetime counters: %+v %v", budget, err)
	}
	if _, err = s.db.ExecContext(ctx, `DROP TABLE program_grants`); err != nil {
		t.Fatal(err)
	}
	_, err = s.EnsureProgramStep(ctx, model.ProgramStepSpec{ProgramID: spec.ID, Revision: revision.Number, Kind: model.StepResearch}, cfg)
	var authorityError *model.ProgramError
	if err == nil || errors.As(err, &authorityError) {
		t.Fatalf("grant lookup storage failure was replaced with an authority error: %v", err)
	}
}

func TestIntakeProgramEmptyCASBundleDoesNotInventPrivateConsent(t *testing.T) {
	s, cfg, _, session := intakeReviewsFixture(t)
	ctx := context.Background()
	bundle, err := s.blobs.ImportBundle(ctx, t.TempDir(), nil, model.BundleLimits{MaxFiles: 1, MaxFileBytes: 1, MaxTotalBytes: 1})
	if err != nil {
		t.Fatal(err)
	}
	if bundle.ManifestSHA == "" || len(bundle.Files) != 0 {
		t.Fatal("fixture must use a real empty immutable bundle")
	}
	spec := model.ProgramSpec{ID: "empty-bundle-program", Project: "demo", Title: "Public only", Objective: "Allow public research without invented private source consent", Disclosure: model.ProgramDisclosure{ModelProviderIDs: []string{"provider"}, PublicSearchAllowed: true, PublicSearchTopics: []model.ProgramSearchTopic{{ID: "topic", Query: "public protocol documentation"}}}}
	revision, err := s.ImportProgram(ctx, spec, bundle)
	if err != nil {
		t.Fatal(err)
	}
	command := intakeReviewsCommand(t, model.ActionProgramApproveRevision, model.SubjectCommandPayload{Kind: "program", ID: spec.ID, Revision: revision.RevisionHash})
	review, err := s.CreateIntakeReview(ctx, session.TokenHash, command, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if review.ProgramRevision == nil || review.ProgramRevision.Spec.Disclosure.PrivateMaterialToModels || review.SeedManifest == nil || review.SeedManifest.ManifestSHA != bundle.ManifestSHA || len(review.SeedManifest.Files) != 0 {
		t.Fatal("empty bundle review changed private consent or bundle identity")
	}
	if err = s.ApproveProgram(ctx, spec.ID, revision.RevisionHash, cfg.PolicyHash, model.TrustedLocalAudit()); err != nil {
		t.Fatal(err)
	}
	work, err := s.ProgramWork(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(work) != 1 || work[0].Step == nil || work[0].Wait != nil {
		t.Fatalf("empty bundle did not authorize public discovery: %+v", work)
	}
	step, err := s.EnsureProgramStep(ctx, *work[0].Step, cfg)
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.Claim(ctx, step.FeatureID, cfg.PolicyHash, 3, 1, 1, 3600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.IssueResearchCapability(ctx, a, cfg); err != nil {
		t.Fatal(err)
	}
	if err = s.ReserveCall(ctx, a, a.Stage, a.ID+":research", 20); err != nil {
		t.Fatal(err)
	}
	stored, err := programRevision(ctx, s.db, spec.ID, revision.Number)
	if err != nil || stored.Spec.Disclosure.PrivateMaterialToModels || programHash(stored.Spec) != programHash(spec) {
		t.Fatalf("execution manufactured private consent: %+v %v", stored.Spec.Disclosure, err)
	}
}

func TestIntakeProgramBundleAuthorityDoesNotMaskCASCorruption(t *testing.T) {
	s, cfg, op, _ := intakeReviewsFixture(t)
	ctx := context.Background()
	bundle := intakeBundlesFinalized(t, s, cfg, op.ID, []byte("unseeded private source"))
	spec := model.ProgramSpec{ID: "corrupt-bundle-program", Project: "demo", Title: "Corruption is fatal", Objective: "Do not classify storage corruption as absent source consent", Disclosure: model.ProgramDisclosure{ModelProviderIDs: []string{"provider"}}}
	revision, err := s.ImportProgram(ctx, spec, bundle)
	if err != nil {
		t.Fatal(err)
	}
	// A malformed manifest is real CAS content, not a fake member-count cache.
	corrupt, err := s.blobs.Put(ctx, []byte("{"))
	if err != nil {
		t.Fatal(err)
	}
	revision.SeedManifestSHA = corrupt.SHA
	raw, err := json.Marshal(revision)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.ExecContext(ctx, `UPDATE program_revisions SET data=? WHERE program_id=? AND number=?`, raw, spec.ID, revision.Number); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.ExecContext(ctx, `INSERT INTO program_bundles(program_id,manifest_sha,revision) VALUES(?,?,?)`, spec.ID, corrupt.SHA, revision.Number); err != nil {
		t.Fatal(err)
	}
	err = s.ApproveProgram(ctx, spec.ID, revision.RevisionHash, cfg.PolicyHash, model.TrustedLocalAudit())
	var domain *model.ProgramError
	if err == nil || errors.As(err, &domain) {
		t.Fatalf("CAS corruption became a disclosure wait: %v", err)
	}
	commandTestCount(t, s, `SELECT count(*) FROM program_grants`, 0)
	commandTestCount(t, s, `SELECT count(*) FROM program_steps`, 0)
	commandTestCount(t, s, `SELECT count(*) FROM calls`, 0)
	p, err := programLoad(ctx, s.db, spec.ID)
	if err != nil {
		t.Fatal(err)
	}
	p.State = model.ProgramActive
	raw, err = json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.ExecContext(ctx, `UPDATE programs SET data=? WHERE id=?`, raw, spec.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.ExecContext(ctx, `INSERT INTO program_grants(program_id,revision_hash,subject_hash,policy_hash,data) VALUES(?,?,'',?,'{}')`, spec.ID, revision.RevisionHash, cfg.PolicyHash); err != nil {
		t.Fatal(err)
	}
	_, err = s.EnsureProgramStep(ctx, model.ProgramStepSpec{ProgramID: spec.ID, Revision: revision.Number, Kind: model.StepResearch}, cfg)
	if err == nil || errors.As(err, &domain) {
		t.Fatalf("legacy authority masked CAS corruption as a domain denial: %v", err)
	}
	commandTestCount(t, s, `SELECT count(*) FROM program_steps`, 0)
	commandTestCount(t, s, `SELECT count(*) FROM calls`, 0)
}
