package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"openfactory/internal/intake"
	"openfactory/internal/model"
)

// These fixtures install trusted persistence state, not synthetic WebAuthn proof.
// Cryptographic ceremony verification belongs to operatorauth's library tests.
func intakeReviewsFixture(t *testing.T) (*Store, model.Config, model.Operator, model.BrowserSession) {
	t.Helper()
	s := openTest(t)
	now := time.Now().UTC()
	cfg := model.Config{PolicyHash: strings.Repeat("a", 64), AstraModel: "provider/astra", Intake: model.IntakePolicy{Enabled: true}, Projects: map[string]model.Project{"demo": {Trusted: true, Checks: map[string]model.Command{"unit": {}}}}, Discovery: model.DiscoveryPolicy{BundleMaxFiles: 8, BundleMaxFileBytes: 4096, BundleMaxTotalBytes: 8192}}
	op := model.Operator{ID: "reviews-operator", Label: "Review operator", Enabled: true, ProjectScope: []string{"demo"}, UserHandle: []byte("reviews-user-handle")}
	if err := s.CreateOperator(context.Background(), op); err != nil {
		t.Fatal(err)
	}
	credential := model.OperatorCredential{ID: "reviews-credential", OperatorID: op.ID, CredentialData: json.RawMessage(`{}`), CreatedAt: now, ExpiresAt: now.Add(24 * time.Hour)}
	raw, _ := json.Marshal(credential)
	if _, err := s.db.Exec(`INSERT INTO intake_credentials(id,operator_id,data) VALUES(?,?,?)`, credential.ID, op.ID, raw); err != nil {
		t.Fatal(err)
	}
	session := model.BrowserSession{ID: "reviews-session", TokenHash: strings.Repeat("b", 64), CSRFHash: strings.Repeat("c", 64), OperatorID: op.ID, CredentialID: credential.ID, CreatedAt: now, LastUsedAt: now, ExpiresAt: now.Add(time.Hour), AbsoluteExpiresAt: now.Add(24 * time.Hour)}
	if err := s.CreateBrowserSession(context.Background(), session); err != nil {
		t.Fatal(err)
	}
	return s, cfg, op, session
}
func intakeReviewsCommand(t *testing.T, action string, payload any) model.TypedCommand {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return model.TypedCommand{CommandID: "10000000-0000-4000-8000-000000000001", Action: action, Payload: raw}
}
func intakeReviewsReconstruct(t *testing.T, s *Store, session model.BrowserSession, command model.TypedCommand, cfg model.Config, template model.IntakeReview) (model.IntakeReview, error) {
	t.Helper()
	ctx := context.Background()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	current, op, credential, err := intakeAuthenticateTx(ctx, tx, session.TokenHash, "", "", time.Now().UTC())
	if err != nil {
		return model.IntakeReview{}, err
	}
	return s.reviewIntakeCommandTx(ctx, tx, command, cfg, current, op, credential, &template)
}
func intakeReviewsCode(t *testing.T, err error, code string) {
	t.Helper()
	var e *model.ProgramError
	if !errors.As(err, &e) || e.Code != code {
		t.Fatalf("error=%v want code %s", err, code)
	}
}

func TestIntakeReviewsExactFeatureClaimsAndTemplate(t *testing.T) {
	s, cfg, _, session := intakeReviewsFixture(t)
	ctx := context.Background()
	f, err := s.ImportProposal(ctx, feature("reviews-feature"))
	if err != nil {
		t.Fatal(err)
	}
	command := intakeReviewsCommand(t, model.ActionFeatureApprove, model.SubjectCommandPayload{Kind: "feature", ID: f.ID, Revision: f.Revision})
	review, err := s.CreateIntakeReview(ctx, session.TokenHash, command, cfg)
	if err != nil {
		t.Fatal(err)
	}
	source, err := intake.Export(f)
	if err != nil {
		t.Fatal(err)
	}
	if review.CanonicalSource != string(source) || review.Feature == nil || review.Claims.CanonicalPayloadSHA != intake.SHA256(source) || review.Claims.SubjectRevision != f.Revision {
		t.Fatalf("review did not bind exact feature: %+v", review)
	}
	if review.Claims.ChallengeID == "" || review.Claims.ExpiresAt.Sub(review.Claims.IssuedAt) != 2*time.Minute || review.Claims.InstallationID == "" {
		t.Fatalf("missing immutable review identity: %+v", review.Claims)
	}
	sha, _ := intakeReviewSHA(review.Claims)
	if sha != review.ClaimsSHA {
		t.Fatal("claims digest is not canonical JSON SHA256")
	}
	current, err := intakeReviewsReconstruct(t, s, session, command, cfg, review)
	if err != nil {
		t.Fatal(err)
	}
	if current.ClaimsSHA != review.ClaimsSHA || current.ID != review.ID || current.Claims.ChallengeID != review.Claims.ChallengeID || !current.CreatedAt.Equal(review.CreatedAt) {
		t.Fatal("revalidation changed reviewed clocks or random identities")
	}
	stored, err := s.Get(ctx, f.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.State != "proposed" || stored.PolicyHash != "" {
		t.Fatal("review creation granted feature execution authority")
	}
	next, err := s.CreateIntakeReview(ctx, session.TokenHash, command, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if next.Claims.ChallengeID == review.Claims.ChallengeID || next.ID == review.ID {
		t.Fatal("independent review reused identity")
	}
	changed := cfg
	changed.PolicyHash = strings.Repeat("d", 64)
	current, err = intakeReviewsReconstruct(t, s, session, command, changed, review)
	if err != nil {
		t.Fatal(err)
	}
	if current.ClaimsSHA == review.ClaimsSHA {
		t.Fatal("policy change retained exact review claims")
	}
	replacement := feature(f.ID)
	replacement.Body = "A genuinely different immutable feature."
	if _, err = s.Import(ctx, replacement); err != nil {
		t.Fatal(err)
	}
	_, err = intakeReviewsReconstruct(t, s, session, command, cfg, review)
	intakeReviewsCode(t, err, "stale_revision")
}

func intakeReviewsDraftFixture(t *testing.T, s *Store, cfg model.Config, op model.Operator) (model.IntakeRequest, model.IntakeDraft) {
	t.Helper()
	ctx := context.Background()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	r := model.IntakeRequest{ID: "reviews-request", OperatorID: op.ID, Project: "demo", State: model.IntakeReviewReady, ContentVersion: 1, CreationPolicySHA: cfg.PolicyHash, MaxDraftDispatches: 12}
	if err = intakePutRequestTx(ctx, tx, r); err != nil {
		t.Fatal(err)
	}
	if err = intakeAppendTurnTx(ctx, tx, model.IntakeTurn{RequestID: r.ID, Role: "user", Text: "Draft exact bounded work."}); err != nil {
		t.Fatal(err)
	}
	r.ContextManifest, err = s.intakeManifestTx(ctx, tx, cfg, r, nil)
	if err != nil {
		t.Fatal(err)
	}
	source, err := intake.Export(feature("reviews-draft-subject"))
	if err != nil {
		t.Fatal(err)
	}
	source = append(source, '\n')
	canonical, err := s.intakeCanonicalTx(ctx, tx, "feature", model.IntakeFormatFeature, source, r, "", cfg)
	if err != nil {
		t.Fatal(err)
	}
	d := intakeDraftFromCanonical(r, canonical, "", r.ContextManifest.SHA, "")
	if err = s.intakeInsertDraftTx(ctx, tx, &r, &d, true); err != nil {
		t.Fatal(err)
	}
	if err = intakePutRequestTx(ctx, tx, r); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return r, d
}
func TestIntakeReviewsRequestProgressDoesNotChangeExactConsent(t *testing.T) {
	s, cfg, op, session := intakeReviewsFixture(t)
	ctx := context.Background()
	r, d := intakeReviewsDraftFixture(t, s, cfg, op)
	command := intakeReviewsCommand(t, model.ActionIntakeApproveQueue, model.IntakeApprovePayload{DraftRevision: d.Revision, SourceSHA: d.SourceSHA, DomainRevision: d.DomainRevision})
	command.RequestID = r.ID
	command.ExpectedContentVersion = &r.ContentVersion
	review, err := s.CreateIntakeReview(ctx, session.TokenHash, command, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if review.CanonicalSource != d.CanonicalSource || review.Claims.InputManifestSHA != r.ContextManifest.SHA || review.Claims.CanonicalPayloadSHA != d.CanonicalSHA {
		t.Fatal("review lost exact source or context")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = intakeEventTx(ctx, tx, &r, "progress", "worker status advanced without content edit"); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	current, err := intakeReviewsReconstruct(t, s, session, command, cfg, review)
	if err != nil {
		t.Fatal(err)
	}
	if current.ClaimsSHA != review.ClaimsSHA {
		t.Fatal("progress invalidated unchanged review")
	}
	d.CanonicalSource += "\n"
	raw, _ := json.Marshal(d)
	if _, err = s.db.ExecContext(ctx, `UPDATE intake_drafts SET data=? WHERE request_id=? AND revision=?`, raw, r.ID, d.Revision); err != nil {
		t.Fatal(err)
	}
	_, err = intakeReviewsReconstruct(t, s, session, command, cfg, review)
	intakeReviewsCode(t, err, "stale_revision")
}
func TestIntakeReviewsSubjectScopeAndLinkedOwner(t *testing.T) {
	s, cfg, op, session := intakeReviewsFixture(t)
	ctx := context.Background()
	f, err := s.ImportProposal(ctx, feature("reviews-owned-feature"))
	if err != nil {
		t.Fatal(err)
	}
	other := model.Operator{ID: "reviews-other", Label: "Other", Enabled: true, ProjectScope: []string{"demo"}, UserHandle: []byte("other")}
	if err = s.CreateOperator(ctx, other); err != nil {
		t.Fatal(err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	request := model.IntakeRequest{ID: "reviews-owner-request", OperatorID: op.ID, Project: "demo"}
	if err = intakePutRequestTx(ctx, tx, request); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	link := model.IntakeLink{RequestID: request.ID, DraftRevision: 1, SubjectKind: "feature", SubjectID: f.ID, SubjectRevision: f.Revision}
	raw, _ := json.Marshal(link)
	if _, err = tx.ExecContext(ctx, `INSERT INTO intake_links(request_id,draft_revision,data) VALUES(?,?,?)`, request.ID, 1, raw); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err = s.IntakeSubject(ctx, other.ID, "feature", f.ID); !errors.Is(err, ErrProjectForbidden) {
		t.Fatalf("another operator read owned subject: %v", err)
	}
	if _, err = s.IntakeSubjectArtifacts(ctx, other.ID, "feature", f.ID, "", 10); !errors.Is(err, ErrProjectForbidden) {
		t.Fatalf("another operator read artifact namespace: %v", err)
	}
	if _, err = s.IntakeSubject(ctx, op.ID, "plan", f.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("subject kind confusion: %v", err)
	}
	command := intakeReviewsCommand(t, model.ActionFeatureApprove, model.SubjectCommandPayload{Kind: "feature", ID: f.ID, Revision: f.Revision})
	if err = s.RevokeOperator(ctx, op.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.CreateIntakeReview(ctx, session.TokenHash, command, cfg); !errors.Is(err, ErrAuthRequired) {
		t.Fatalf("revoked actor reviewed authority: %v", err)
	}
}
func TestIntakeReviewsProgramPredictionBindsNextNumberAndBundle(t *testing.T) {
	s, cfg, op, _ := intakeReviewsFixture(t)
	ctx := context.Background()
	old := model.ProgramSpec{ID: "reviews-program", Project: "demo", Title: "Program", Objective: "Old objective", Disclosure: model.ProgramDisclosure{ModelProviderIDs: []string{"provider"}}}
	first, err := s.ImportProgram(ctx, old, model.ProgramBundle{})
	if err != nil {
		t.Fatal(err)
	}
	bundle := intakeBundlesFinalized(t, s, cfg, op.ID, []byte("private source\n"))
	spec := old
	spec.Objective = "Revised exact objective"
	spec.Seeds = []model.ProgramSeed{{ID: "private", Kind: "bundle", Path: "inputs/private.md"}}
	if err = intake.NormalizeProgramSpec(&spec, cfg); err != nil {
		t.Fatal(err)
	}
	source, _ := json.Marshal(spec)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	request := model.IntakeRequest{OperatorID: op.ID, Project: "demo", ContextManifest: model.IntakeManifest{Disclosure: model.IntakeDisclosure{BundleManifestSHAs: []string{bundle.ManifestSHA}}}}
	canonical, err := s.intakeCanonicalTx(ctx, tx, "program", model.IntakeFormatProgram, source, request, bundle.ManifestSHA, cfg)
	if err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	p, err := programLoad(ctx, tx, old.ID)
	tx.Rollback()
	if err != nil {
		t.Fatal(err)
	}
	if p.CurrentRevision != first.Number {
		t.Fatal("predicting a review mutated program")
	}
	expected := intake.ProgramRevisionHash(old.ID, first.Number+1, spec, bundle.ManifestSHA)
	if canonical.DomainRevision != expected {
		t.Fatal("predicted revision omitted next number or seed manifest")
	}
	imported, err := s.ImportProgram(ctx, spec, bundle)
	if err != nil {
		t.Fatal(err)
	}
	if imported.RevisionHash != canonical.DomainRevision {
		t.Fatal("review and import use different domain revision algorithms")
	}
	if imported.Spec.Disclosure.PrivateMaterialToModels {
		t.Fatal("manual import invented Program model disclosure")
	}
	p, err = programLoad(ctx, s.db, old.ID)
	if err != nil {
		t.Fatal(err)
	}
	if p.State != model.ProgramDraft {
		t.Fatal("predicting and importing a manual revision granted execution authority")
	}
	var grants, steps int
	if err = s.db.QueryRowContext(ctx, `SELECT count(*) FROM program_grants WHERE program_id=?`, old.ID).Scan(&grants); err != nil {
		t.Fatal(err)
	}
	if err = s.db.QueryRowContext(ctx, `SELECT count(*) FROM program_steps WHERE program_id=?`, old.ID).Scan(&steps); err != nil {
		t.Fatal(err)
	}
	if grants != 0 || steps != 0 {
		t.Fatal("manual revision prediction or import authorized model work")
	}
}

func intakeReviewsArtifactFixture(t *testing.T, s *Store, cfg model.Config, bundle model.ProgramBundle) (model.ProgramRevision, model.ProgramArtifact, model.ExperimentContract) {
	t.Helper()
	ctx := context.Background()
	spec := model.ProgramSpec{ID: "reviews-experiment-program", Project: "demo", Title: "Experiment", Objective: "Measure exact behavior", Disclosure: model.ProgramDisclosure{ModelProviderIDs: []string{"provider"}}}
	revision, err := s.ImportProgram(ctx, spec, model.ProgramBundle{})
	if err != nil {
		t.Fatal(err)
	}
	contract := model.ExperimentContract{ID: "bounded-measurement", Hypothesis: "Ordering is stable", BundleManifestSHA: bundle.ManifestSHA, BundleFiles: []string{"inputs/private.md"}}
	raw, _ := json.Marshal(model.ExperimentContractsPayload{Experiments: []model.ExperimentContract{contract}})
	ref, err := s.blobs.Put(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	manifest, err := programManifestTx(ctx, tx, revision.ProgramID, revision.Number, []model.ProgramInputRef{{Kind: "program_revision", ID: revision.RevisionHash, SHA: revision.RevisionHash, Revision: revision.Number}})
	if err != nil {
		t.Fatal(err)
	}
	step := model.ProgramStep{ID: "reviews-experiment-step", ProgramID: revision.ProgramID, Revision: revision.Number, Kind: model.StepArchitecture, FeatureID: "openfactory-discovery-reviews-experiment-step", State: "completed", InputManifestSHA: manifest.SHA, OutputManifestSHA: manifest.SHA}
	data, _ := json.Marshal(step)
	if _, err = tx.ExecContext(ctx, `INSERT INTO program_steps(id,program_id,revision,feature_id,data) VALUES(?,?,?,?,?)`, step.ID, step.ProgramID, step.Revision, step.FeatureID, data); err != nil {
		t.Fatal(err)
	}
	artifact := model.ProgramArtifact{ID: "reviews-experiment-artifact", ProgramID: revision.ProgramID, Revision: revision.Number, Kind: model.ArtifactExperimentContracts, ContentSHA: ref.SHA, InputManifestSHA: manifest.SHA, ProducerStepID: step.ID}
	data, _ = json.Marshal(artifact)
	if _, err = tx.ExecContext(ctx, `INSERT INTO program_artifacts(id,program_id,revision,kind,content_sha,data) VALUES(?,?,?,?,?,?)`, artifact.ID, artifact.ProgramID, artifact.Revision, artifact.Kind, artifact.ContentSHA, data); err != nil {
		t.Fatal(err)
	}
	data, _ = json.Marshal(cfg.Discovery)
	if _, err = tx.ExecContext(ctx, `INSERT INTO program_policies(program_id,revision,policy_hash,data) VALUES(?,?,?,?)`, revision.ProgramID, revision.Number, cfg.PolicyHash, data); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return revision, artifact, contract
}
func TestIntakeReviewsExperimentUsesExactArtifactSubjectAndCurrentPolicy(t *testing.T) {
	s, cfg, op, session := intakeReviewsFixture(t)
	ctx := context.Background()
	cfg.Discovery.ExperimentProfiles = map[string]model.ExperimentProfile{"bounded": {Argv: []string{"measure"}, ImageIdentity: "sha256:frozen"}}
	cfg.Discovery.ExperimentLimits = model.ExperimentLimits{WallSeconds: 30, MaxOutputBytes: 1024}
	bundle := intakeBundlesFinalized(t, s, cfg, op.ID, []byte("frozen input\n"))
	r, a, c := intakeReviewsArtifactFixture(t, s, cfg, bundle)
	command := intakeReviewsCommand(t, model.ActionProgramApproveExperiment, model.ProgramArtifactCommandPayload{ProgramID: r.ProgramID, Revision: r.RevisionHash, ArtifactID: a.ID, ArtifactKind: a.Kind, ArtifactSHA: a.ContentSHA, SubjectSHA: programHash(c)})
	review, err := s.CreateIntakeReview(ctx, session.TokenHash, command, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if review.Claims.CanonicalPayloadSHA != programHash(c) || review.Claims.CanonicalPayloadSHA == a.ContentSHA || review.Claims.ExecutionPolicySHA != model.ExperimentPolicySHA(cfg.Discovery.ExperimentProfiles, cfg.Discovery.ExperimentLimits) {
		t.Fatal("experiment authority confused artifact and selected contract or omitted current execution policy")
	}
	if len(review.Claims.Evidence) < 2 || review.Claims.InputManifestSHA != a.InputManifestSHA {
		t.Fatal("review omitted exact artifact/evidence closure")
	}
	detail, err := s.IntakeSubjectArtifact(ctx, op.ID, "program", r.ProgramID, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(detail.ExperimentSubjects) != 1 || detail.ExperimentSubjects[0].SubjectSHA != programHash(c) || detail.CanonicalSource != string(detail.Payload) {
		t.Fatal("browser did not receive canonical server-computed experiment identity")
	}
	cfg.Discovery.ExperimentProfiles = map[string]model.ExperimentProfile{"bounded": {Argv: []string{"different-command"}, ImageIdentity: "sha256:frozen"}}
	_, err = intakeReviewsReconstruct(t, s, session, command, cfg, review)
	intakeReviewsCode(t, err, "stale_policy")
	if _, err = s.IntakeSubjectArtifact(ctx, op.ID, "feature", r.ProgramID, a.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("artifact namespace confused subject kind: %v", err)
	}
}

func TestIntakeReviewsManualImportNeedsNoModelDisclosureAndPreflightsGlobalCollision(t *testing.T) {
	s, cfg, op, session := intakeReviewsFixture(t)
	ctx := context.Background()
	r, d := intakeReviewsDraftFixture(t, s, cfg, op)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	r.ContextManifest = model.IntakeManifest{}
	d.InputManifestSHA = ""
	if err = intakePutRequestTx(ctx, tx, r); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	raw, _ := json.Marshal(d)
	if _, err = tx.ExecContext(ctx, `UPDATE intake_drafts SET data=? WHERE request_id=? AND revision=?`, raw, r.ID, d.Revision); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	command := intakeReviewsCommand(t, model.ActionIntakeApproveQueue, model.IntakeApprovePayload{DraftRevision: d.Revision, SourceSHA: d.SourceSHA, DomainRevision: d.DomainRevision})
	command.RequestID = r.ID
	command.ExpectedContentVersion = &r.ContentVersion
	review, err := s.CreateIntakeReview(ctx, session.TokenHash, command, cfg)
	if err != nil {
		t.Fatalf("manual canonical import improperly required model consent: %v", err)
	}
	if review.Claims.InputManifestSHA != "" {
		t.Fatal("manual source invented model input consent")
	}
	collision := feature("reviews-draft-subject")
	collision.Project = "another-project"
	if _, err = s.Import(ctx, collision); err != nil {
		t.Fatal(err)
	}
	_, err = s.CreateIntakeReview(ctx, session.TokenHash, command, cfg)
	intakeReviewsCode(t, err, "stale_subject")
	var proofs int
	if err = s.db.QueryRow(`SELECT count(*) FROM intake_auth_challenges`).Scan(&proofs); err != nil {
		t.Fatal(err)
	}
	if proofs != 0 {
		t.Fatal("collision preflight created an authority challenge")
	}
}

func TestIntakeReviewsCommittedBundleCanonicalizationSurvivesActorRevocation(t *testing.T) {
	for _, revoked := range []string{"operator", "credential"} {
		t.Run(revoked, func(t *testing.T) {
			s, cfg, op, session := intakeReviewsFixture(t)
			ctx := context.Background()
			bundle := intakeBundlesFinalized(t, s, cfg, op.ID, []byte("committed private source"))
			spec := model.ProgramSpec{ID: "reviews-revoked-program", Project: "demo", Title: "Historical drafting", Objective: "Preserve bounded committed drafting", Seeds: []model.ProgramSeed{{ID: "private", Kind: "bundle", Path: "inputs/private.md"}}, Disclosure: model.ProgramDisclosure{ModelProviderIDs: []string{"provider"}}}
			source, err := json.Marshal(spec)
			if err != nil {
				t.Fatal(err)
			}
			// Use the actual saved-context producer and consent transaction. A
			// bundle-only manifest is not a committed drafting authorization.
			r := manifestDraft(t, s, session, cfg, commandTestCreateRequest(t, s, op, session, cfg), []string{bundle.ManifestSHA})
			manifest, err := json.Marshal(r.ContextManifest)
			if err != nil {
				t.Fatal(err)
			}
			var grant []byte
			if err = s.db.QueryRowContext(ctx, `SELECT data FROM intake_draft_authorizations WHERE request_id=? AND content_version=?`, r.ID, r.ContentVersion).Scan(&grant); err != nil {
				t.Fatal(err)
			}
			if revoked == "operator" {
				err = s.RevokeOperator(ctx, op.ID)
			} else {
				err = s.RevokeCredential(ctx, session.CredentialID)
			}
			if err != nil {
				t.Fatal(err)
			}
			step, attempt := lifecycleStart(t, s, cfg, r)
			input, turns, _, err := s.IntakeStepInputs(ctx, step.ID)
			if err != nil || len(turns) != 1 {
				t.Fatalf("committed context was unreadable after %s revocation: %v", revoked, err)
			}
			retained, err := json.Marshal(input.ContextManifest)
			if err != nil || !bytes.Equal(retained, manifest) {
				t.Fatalf("revocation changed exact committed manifest: %s %v", retained, err)
			}
			kinds := map[string]bool{}
			for _, ref := range input.ContextManifest.Inputs {
				kinds[ref.Kind] = true
			}
			if !kinds["turn"] || !kinds["bundle"] || !kinds["capabilities"] || input.ContextManifest.Disclosure.ContextSHA == "" {
				t.Fatal("committed context omitted exact turn, bundle, capabilities or root identity")
			}
			tx, err := s.db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			historical, err := s.intakeCanonicalCommittedTx(ctx, tx, "program", model.IntakeFormatProgram, source, input, cfg)
			if err != nil {
				tx.Rollback()
				t.Fatalf("committed model work lost authority after %s revocation: %v", revoked, err)
			}
			if historical.Program == nil || historical.Program.Disclosure.PrivateMaterialToModels || historical.DomainRevision != intake.ProgramRevisionHash(spec.ID, 1, *historical.Program, bundle.ManifestSHA) {
				tx.Rollback()
				t.Fatal("committed intake context changed Program execution disclosure or historical revision identity")
			}
			if revoked == "operator" {
				if _, err = s.intakeCanonicalTx(ctx, tx, "program", model.IntakeFormatProgram, source, input, bundle.ManifestSHA, cfg); !errors.Is(err, ErrAuthRequired) {
					tx.Rollback()
					t.Fatalf("revoked actor acquired NEW seed authority: %v", err)
				}
			}
			tx.Rollback()
			output, err := json.Marshal(map[string]any{"type": "draft", "message": "Exact committed Program", "route": "program", "format": model.IntakeFormatProgram, "canonical_source": string(source), "assumptions": []string{}, "unresolved_risks": []string{}})
			if err != nil {
				t.Fatal(err)
			}
			lifecycleResult(t, s, cfg, step, attempt, string(output))
			r = manifestCurrent(t, s, r.ID)
			draft, err := s.IntakeDraft(ctx, r.ID, r.LatestDraftRevision)
			if err != nil || r.State != model.IntakeReviewReady || len(draft.Findings) != 0 || draft.CanonicalSource != string(source) || draft.SourceSHA != intake.SHA256(source) || draft.DomainRevision != historical.DomainRevision || draft.InputManifestSHA != input.ContextManifest.SHA || draft.BundleManifestSHA != bundle.ManifestSHA {
				t.Fatalf("committed Program lost exact source/context/revision after revocation: %+v %v", draft, err)
			}
			retained, err = json.Marshal(r.ContextManifest)
			if err != nil || !bytes.Equal(retained, manifest) {
				t.Fatalf("settlement rewrote committed context: %s %v", retained, err)
			}
			var retainedGrant []byte
			if err = s.db.QueryRowContext(ctx, `SELECT data FROM intake_draft_authorizations WHERE request_id=? AND content_version=?`, r.ID, r.ContentVersion).Scan(&retainedGrant); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(retainedGrant, grant) {
				t.Fatal("revocation or settlement rewrote the exact original drafting grant")
			}
			command := lifecycleCommand(model.ActionIntakeApproveQueue, r, model.IntakeApprovePayload{DraftRevision: draft.Revision, SourceSHA: draft.SourceSHA, DomainRevision: draft.DomainRevision})
			if _, err = s.CreateIntakeReview(ctx, session.TokenHash, command, cfg); !errors.Is(err, ErrAuthRequired) {
				t.Fatalf("revoked credential acquired NEW review authority: %v", err)
			}
			command = lifecycleCommand(model.ActionRequestCreate, model.IntakeRequest{}, model.RequestCreatePayload{Project: "demo", Text: "New authority is forbidden", ClientMessageID: lifecycleUUID()})
			if _, err = s.ExecuteCommand(ctx, session.TokenHash, session.CSRFHash, command, cfg, nil); !errors.Is(err, ErrAuthRequired) {
				t.Fatalf("revoked credential acquired NEW command authority: %v", err)
			}
		})
	}
}

func TestIntakeReviewsCommittedBundleRejectsUncommittedOriginalManifest(t *testing.T) {
	s, cfg, op, _ := intakeReviewsFixture(t)
	ctx := context.Background()
	bundle := intakeBundlesFinalized(t, s, cfg, op.ID, []byte("committed private source"))
	spec := model.ProgramSpec{ID: "reviews-revoked-program", Project: "demo", Title: "Historical drafting", Objective: "Preserve bounded committed drafting", Seeds: []model.ProgramSeed{{ID: "private", Kind: "bundle", Path: "inputs/private.md"}}, Disclosure: model.ProgramDisclosure{ModelProviderIDs: []string{"provider"}}}
	source, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	// Preserve the original incomplete fixture as a negative regression: it
	// has no ContextSHA, capabilities, saved request or committed consent grant.
	manifest := model.IntakeManifest{RequestID: "committed-request", ContentVersion: 1, Project: "demo", PolicySHA: cfg.PolicyHash, Disclosure: model.IntakeDisclosure{PolicySHA: cfg.PolicyHash, PrivateMaterialToModels: true, ModelProviderIDs: []string{cfg.AstraModel}, BundleManifestSHAs: []string{bundle.ManifestSHA}}}
	for _, f := range bundle.Files {
		manifest.Inputs = append(manifest.Inputs, model.IntakeInputRef{Kind: "bundle", ID: bundle.ManifestSHA + "/" + f.Path, SHA: f.ContentSHA, Bytes: f.Bytes})
	}
	manifest.SHA = programHash(manifest)
	r := model.IntakeRequest{ID: manifest.RequestID, OperatorID: op.ID, Project: "demo", ContentVersion: 1, ContextManifest: manifest}
	if err = s.RevokeOperator(ctx, op.ID); err != nil {
		t.Fatal(err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	_, err = s.intakeCanonicalCommittedTx(ctx, tx, "program", model.IntakeFormatProgram, source, r, cfg)
	intakeCommittedIntegrityError(t, err)
}

func TestIntakeReviewsManualProgramSeedSelectionIsNotModelDisclosure(t *testing.T) {
	s, cfg, op, session := intakeReviewsFixture(t)
	ctx := context.Background()
	bundle := intakeBundlesFinalized(t, s, cfg, op.ID, []byte("human-selected seed"))
	spec := model.ProgramSpec{ID: "reviews-manual-program", Project: "demo", Title: "Manual program", Objective: "Import an exact human contract", Seeds: []model.ProgramSeed{{ID: "seed", Kind: "bundle", Path: "inputs/private.md"}}, Disclosure: model.ProgramDisclosure{ModelProviderIDs: []string{"provider"}}}
	source, _ := json.Marshal(spec)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	r := model.IntakeRequest{ID: "reviews-manual-program-request", OperatorID: op.ID, Project: "demo", State: model.IntakeReviewReady, ContentVersion: 1, CreationPolicySHA: cfg.PolicyHash, MaxDraftDispatches: 12}
	if err = intakePutRequestTx(ctx, tx, r); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	canonical, err := s.intakeCanonicalTx(ctx, tx, "program", model.IntakeFormatProgram, source, r, bundle.ManifestSHA, cfg)
	if err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	draft := intakeDraftFromCanonical(r, canonical, "", "", "")
	draft.PolicySHA = cfg.PolicyHash
	draft.BundleManifestSHA = bundle.ManifestSHA
	if err = s.intakeInsertDraftTx(ctx, tx, &r, &draft, true); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err = intakePutRequestTx(ctx, tx, r); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	command := intakeReviewsCommand(t, model.ActionIntakeApproveQueue, model.IntakeApprovePayload{DraftRevision: draft.Revision, SourceSHA: draft.SourceSHA, DomainRevision: draft.DomainRevision})
	command.RequestID = r.ID
	command.ExpectedContentVersion = &r.ContentVersion
	_, err = s.CreateIntakeReview(ctx, session.TokenHash, command, cfg)
	intakeReviewsCode(t, err, "disclosure_required")
	if canonical.Program == nil || canonical.Program.Disclosure.PrivateMaterialToModels {
		t.Fatal("manual seed selection fabricated Program model disclosure")
	}
	if draft.BundleManifestSHA != bundle.ManifestSHA || draft.InputManifestSHA != "" || draft.DomainRevision != intake.ProgramRevisionHash(spec.ID, 1, *canonical.Program, bundle.ManifestSHA) {
		t.Fatal("manual draft lost exact bundle or fabricated model context")
	}
	var challenges, grants, programs int
	if err = s.db.QueryRowContext(ctx, `SELECT count(*) FROM intake_auth_challenges`).Scan(&challenges); err != nil {
		t.Fatal(err)
	}
	if err = s.db.QueryRowContext(ctx, `SELECT count(*) FROM program_grants`).Scan(&grants); err != nil {
		t.Fatal(err)
	}
	if err = s.db.QueryRowContext(ctx, `SELECT count(*) FROM programs`).Scan(&programs); err != nil {
		t.Fatal(err)
	}
	if challenges != 0 || grants != 0 || programs != 0 {
		t.Fatal("missing execution disclosure created an unusable challenge or partial handoff")
	}
	stored, err := s.IntakeRequest(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.LifetimeDispatches != 0 || stored.ContextManifest.SHA != "" {
		t.Fatal("human seed selection silently consumed model disclosure or dispatch")
	}
}
