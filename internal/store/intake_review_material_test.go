package store

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"openfactory/internal/intake"
	"openfactory/internal/model"
)

// Decode the strings exactly as a review consumer does, never re-encode them
// before checking the digest. Semantic comparisons are separate from hashing.
func intakeReviewMaterialBoundary(t *testing.T, review model.IntakeReview) intakeReviewManifest {
	t.Helper()
	if err := validateIntakeReviewMaterial(review); err != nil {
		t.Fatal(err)
	}
	if intake.SHA256([]byte(review.ClaimsCanonical)) != review.ClaimsSHA || intake.SHA256([]byte(review.ManifestCanonical)) != review.Claims.ReviewManifestSHA || intake.SHA256([]byte(review.CanonicalPayload)) != review.Claims.CanonicalPayloadSHA {
		t.Fatal("review material does not reproduce its exact digest")
	}
	var claims model.ReviewClaims
	var manifest intakeReviewManifest
	if err := json.Unmarshal([]byte(review.ClaimsCanonical), &claims); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(review.ManifestCanonical), &manifest); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(claims, review.Claims) || !reflect.DeepEqual(manifest.Evidence, review.Claims.Evidence) || !reflect.DeepEqual(manifest.BundleManifestSHAs, review.Claims.BundleManifestSHAs) {
		t.Fatal("material differs from presented claims or evidence")
	}
	if intake.SHA256([]byte(review.CommandCanonical)) != manifest.CommandSHA {
		t.Fatal("command digest does not bind exact command material")
	}
	var command model.TypedCommand
	if err := json.Unmarshal([]byte(review.CommandCanonical), &command); err != nil {
		t.Fatal(err)
	}
	digest, err := intake.CommandDigest(review.Command)
	if err != nil || digest != manifest.CommandSHA || command.Action != review.Claims.Action || command.CommandID != review.Claims.CommandID {
		t.Fatal("command material differs from presented action")
	}
	if review.CanonicalSource != "" && intake.SHA256([]byte(review.CanonicalSource)) != manifest.SourceSHA {
		t.Fatal("source is not bound to review manifest")
	}
	for _, material := range []string{review.ClaimsCanonical, review.ManifestCanonical, review.CanonicalPayload} {
		if intake.SHA256([]byte(material+" ")) == intake.SHA256([]byte(material)) {
			t.Fatal("changed exact material retained its digest")
		}
	}
	return manifest
}

func TestIntakeReviewMaterialFeatureAndDraftReconstruction(t *testing.T) {
	for _, draft := range []bool{false, true} {
		t.Run(map[bool]string{false: "manual", true: "draft"}[draft], func(t *testing.T) {
			s, cfg, op, session := intakeReviewsFixture(t)
			var command model.TypedCommand
			if draft {
				r, d := intakeReviewsDraftFixture(t, s, cfg, op)
				command = intakeReviewsCommand(t, model.ActionIntakeApproveQueue, model.IntakeApprovePayload{DraftRevision: d.Revision, SourceSHA: d.SourceSHA, DomainRevision: d.DomainRevision})
				command.RequestID = r.ID
				command.ExpectedContentVersion = &r.ContentVersion
			} else {
				f, err := s.ImportProposal(context.Background(), feature("material-feature"))
				if err != nil {
					t.Fatal(err)
				}
				command = intakeReviewsCommand(t, model.ActionFeatureApprove, model.SubjectCommandPayload{Kind: "feature", ID: f.ID, Revision: f.Revision})
			}
			review, err := s.CreateIntakeReview(context.Background(), session.TokenHash, command, cfg)
			if err != nil {
				t.Fatal(err)
			}
			manifest := intakeReviewMaterialBoundary(t, review)
			if manifest.Feature == nil || review.Feature == nil {
				t.Fatal("missing feature")
			}
			canonical, err := intake.Export(*review.Feature)
			if err != nil {
				t.Fatal(err)
			}
			if review.CanonicalPayload != string(canonical) {
				t.Fatal("canonical feature bytes differ")
			}
			current, err := intakeReviewsReconstruct(t, s, session, command, cfg, review)
			if err != nil {
				t.Fatal(err)
			}
			intakeReviewMaterialBoundary(t, current)
			if current.ClaimsCanonical != review.ClaimsCanonical || current.ManifestCanonical != review.ManifestCanonical || current.CanonicalPayload != review.CanonicalPayload {
				t.Fatal("reconstruction changed material")
			}
		})
	}
}

func TestIntakeReviewMaterialExperimentSeparatesEnvelopeAndContract(t *testing.T) {
	s, cfg, op, session := intakeReviewsFixture(t)
	bundle := intakeBundlesFinalized(t, s, cfg, op.ID, []byte("frozen material\n"))
	r, artifact, contract := intakeReviewsArtifactFixture(t, s, cfg, bundle)
	command := intakeReviewsCommand(t, model.ActionProgramApproveExperiment, model.ProgramArtifactCommandPayload{ProgramID: r.ProgramID, Revision: r.RevisionHash, ArtifactID: artifact.ID, ArtifactKind: artifact.Kind, ArtifactSHA: artifact.ContentSHA, SubjectSHA: programHash(contract)})
	review, err := s.CreateIntakeReview(context.Background(), session.TokenHash, command, cfg)
	if err != nil {
		t.Fatal(err)
	}
	manifest := intakeReviewMaterialBoundary(t, review)
	if manifest.SelectedArtifact == nil || manifest.SelectedArtifact.Kind != commandPayloadArtifactKind(command, t) || manifest.SelectedArtifact.ContentSHA != intake.SHA256([]byte(review.SelectedArtifactCanonical)) {
		t.Fatal("selected artifact envelope differs")
	}
	var selected model.ExperimentContract
	if err := json.Unmarshal([]byte(review.CanonicalPayload), &selected); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(selected, contract) || manifest.SelectedExperiment == nil || !reflect.DeepEqual(*manifest.SelectedExperiment, selected) || review.Claims.CanonicalPayloadSHA == artifact.ContentSHA {
		t.Fatal("selected contract confused with artifact envelope")
	}
	if len(review.Claims.Evidence) < 2 || review.Claims.InputManifestSHA != artifact.InputManifestSHA {
		t.Fatal("selected artifact evidence omitted")
	}
	current, err := intakeReviewsReconstruct(t, s, session, command, cfg, review)
	if err != nil {
		t.Fatal(err)
	}
	if current.ManifestCanonical != review.ManifestCanonical || current.SelectedArtifactCanonical != review.SelectedArtifactCanonical || current.CanonicalPayload != review.CanonicalPayload {
		t.Fatal("experiment reconstruction changed material")
	}
}

func commandPayloadArtifactKind(command model.TypedCommand, t *testing.T) string {
	t.Helper()
	var payload model.ProgramArtifactCommandPayload
	if err := json.Unmarshal(command.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	return payload.ArtifactKind
}

func TestIntakeReviewMaterialManualProgram(t *testing.T) {
	s, cfg, _, session := intakeReviewsFixture(t)
	spec := model.ProgramSpec{ID: "material-program", Project: "demo", Title: "Manual program", Objective: "Review exact source", Disclosure: model.ProgramDisclosure{ModelProviderIDs: []string{"provider"}}}
	revision, err := s.ImportProgram(context.Background(), spec, model.ProgramBundle{})
	if err != nil {
		t.Fatal(err)
	}
	command := intakeReviewsCommand(t, model.ActionProgramApproveRevision, model.SubjectCommandPayload{Kind: "program", ID: revision.ProgramID, Revision: revision.RevisionHash})
	review, err := s.CreateIntakeReview(context.Background(), session.TokenHash, command, cfg)
	if err != nil {
		t.Fatal(err)
	}
	manifest := intakeReviewMaterialBoundary(t, review)
	if manifest.ProgramRevision == nil || review.ProgramRevision == nil {
		t.Fatal("missing program revision")
	}
	var parsed model.ProgramSpec
	if err := json.Unmarshal([]byte(review.CanonicalPayload), &parsed); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(parsed, revision.Spec) || review.CanonicalPayload != review.CanonicalSource {
		t.Fatal("program canonical bytes differ from reviewed source")
	}
	current, err := intakeReviewsReconstruct(t, s, session, command, cfg, review)
	if err != nil {
		t.Fatal(err)
	}
	if current.ClaimsCanonical != review.ClaimsCanonical || current.ManifestCanonical != review.ManifestCanonical || current.CanonicalPayload != review.CanonicalPayload {
		t.Fatal("program reconstruction changed material")
	}
}

func TestIntakeReviewMaterialRejectsMutationsAndPartialSnapshots(t *testing.T) {
	s, cfg, _, session := intakeReviewsFixture(t)
	f, err := s.ImportProposal(context.Background(), feature("material-mutations"))
	if err != nil {
		t.Fatal(err)
	}
	command := intakeReviewsCommand(t, model.ActionFeatureApprove, model.SubjectCommandPayload{Kind: "feature", ID: f.ID, Revision: f.Revision})
	review, err := s.CreateIntakeReview(context.Background(), session.TokenHash, command, cfg)
	if err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*model.IntakeReview){
		"claims":       func(r *model.IntakeReview) { r.Claims.SubjectID += "-changed" },
		"claims bytes": func(r *model.IntakeReview) { r.ClaimsCanonical += " " },
		"manifest":     func(r *model.IntakeReview) { r.ManifestCanonical += " " },
		"source":       func(r *model.IntakeReview) { r.CanonicalSource += " " },
		"payload":      func(r *model.IntakeReview) { r.CanonicalPayload += " " },
		"command":      func(r *model.IntakeReview) { r.Command.Action = model.ActionSubjectRetry },
		"partial":      func(r *model.IntakeReview) { r.CommandCanonical = "" },
		"presented feature": func(r *model.IntakeReview) {
			f := *r.Feature
			f.Body += " changed"
			r.Feature = &f
		},
	} {
		t.Run(name, func(t *testing.T) {
			changed := review
			mutate(&changed)
			if validateIntakeReviewMaterial(changed) == nil {
				t.Fatal("changed review material accepted")
			}
		})
	}
	if err := validateIntakeReviewMaterial(model.IntakeReview{}); err != nil {
		t.Fatalf("legacy missing material rejected: %v", err)
	}
}

func TestIntakeReviewMaterialPersistenceAndChallengeFailClosed(t *testing.T) {
	s, cfg, _, session := intakeReviewsFixture(t)
	ctx := context.Background()
	f, err := s.ImportProposal(ctx, feature("material-persistence"))
	if err != nil {
		t.Fatal(err)
	}
	command := intakeReviewsCommand(t, model.ActionFeatureApprove, model.SubjectCommandPayload{Kind: "feature", ID: f.ID, Revision: f.Revision})
	review, err := s.CreateIntakeReview(ctx, session.TokenHash, command, cfg)
	if err != nil {
		t.Fatal(err)
	}
	tampered := review
	tampered.ID = "tampered-material-review"
	tampered.CanonicalPayload += " changed"
	if err = s.SaveIntakeReview(ctx, tampered); err == nil {
		t.Fatal("tampered review persisted")
	}
	var rows int
	if err = s.db.QueryRowContext(ctx, `SELECT count(*) FROM intake_reviews WHERE id=?`, tampered.ID).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("tampered review rows=%d err=%v", rows, err)
	}
	var credentialBefore []byte
	if err = s.db.QueryRowContext(ctx, `SELECT data FROM intake_credentials WHERE id=?`, session.CredentialID).Scan(&credentialBefore); err != nil {
		t.Fatal(err)
	}
	tampered = review
	tampered.CanonicalSource += " changed"
	raw, err := json.Marshal(tampered)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.ExecContext(ctx, `UPDATE intake_reviews SET data=? WHERE id=?`, raw, review.ID); err != nil {
		t.Fatal(err)
	}
	digest, err := intake.CommandDigest(command)
	if err != nil {
		t.Fatal(err)
	}
	challenge := model.AuthChallenge{ID: review.Claims.ChallengeID, Purpose: "command", OperatorID: review.Claims.OperatorID, CredentialID: session.CredentialID, SessionID: session.ID, CommandID: command.CommandID, CommandDigest: digest, ReviewID: review.ID, ClaimDigest: review.ClaimsSHA, SessionData: json.RawMessage(`{}`), CreatedAt: review.Claims.IssuedAt, ExpiresAt: review.Claims.ExpiresAt}
	if err = s.CreateAuthChallenge(ctx, challenge); err == nil {
		t.Fatal("persisted source mutation acquired an authorization challenge")
	}
	for _, table := range []string{"intake_auth_challenges", "intake_approval_receipts"} {
		if err = s.db.QueryRowContext(ctx, `SELECT count(*) FROM `+table).Scan(&rows); err != nil || rows != 0 {
			t.Fatalf("%s rows=%d err=%v", table, rows, err)
		}
	}
	var credentialAfter []byte
	if err = s.db.QueryRowContext(ctx, `SELECT data FROM intake_credentials WHERE id=?`, session.CredentialID).Scan(&credentialAfter); err != nil {
		t.Fatal(err)
	}
	if string(credentialAfter) != string(credentialBefore) {
		t.Fatal("rejected challenge changed credential counter or data")
	}
	stored, err := s.Get(ctx, f.ID)
	if err != nil || stored.State != "proposed" || stored.PolicyHash != "" {
		t.Fatalf("rejected challenge changed authority: state=%s policy=%s err=%v", stored.State, stored.PolicyHash, err)
	}
}
