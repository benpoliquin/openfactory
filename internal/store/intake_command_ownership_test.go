package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"openfactory/internal/model"
)

// These fixtures persist the real ownership edges produced by Program
// publication. Commands must honor those edges even when both operators share
// the same enrolled project and the product has no direct intake link.
func commandOwnershipProgram(t *testing.T, s *Store, owner model.Operator, session model.BrowserSession, cfg model.Config) model.ProgramRevision {
	t.Helper()
	ctx := context.Background()
	request := commandTestCreateRequest(t, s, owner, session, cfg)
	revision, err := s.ImportProgram(ctx, programTxSpec("ownership-program"), model.ProgramBundle{})
	if err != nil {
		t.Fatal(err)
	}
	link := model.IntakeLink{RequestID: request.ID, DraftRevision: 1, SubjectKind: "program", SubjectID: revision.ProgramID, SubjectRevision: revision.RevisionHash, CommandID: "ownership-fixture", CreatedAt: time.Now().UTC()}
	raw, err := json.Marshal(link)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.ExecContext(ctx, `INSERT INTO intake_links(request_id,draft_revision,data) VALUES(?,?,?)`, request.ID, link.DraftRevision, raw); err != nil {
		t.Fatal(err)
	}
	return revision
}

func commandOwnershipProduct(t *testing.T, s *Store, program model.ProgramRevision, featureID string) {
	t.Helper()
	link := model.ProgramFeatureLink{ProgramID: program.ProgramID, Revision: program.Number, FeatureID: featureID, ContractArtifactID: "ownership-contract", ManifestSHA: authTestHash("ownership-manifest")}
	raw, err := json.Marshal(link)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.ExecContext(context.Background(), `INSERT INTO program_feature_links(program_id,feature_id,data) VALUES(?,?,?)`, program.ProgramID, featureID, raw); err != nil {
		t.Fatal(err)
	}
}

func TestIntakeCommandOwnershipFollowsPublishedProgram(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	cfg := commandTestConfig()
	now := time.Now().UTC().Add(-time.Second)
	owner, _, ownerSession := authTestActor(t, s, now, "program-owner")
	other, _, otherSession := authTestActor(t, s, now, "same-project-other")
	program := commandOwnershipProgram(t, s, owner, ownerSession, cfg)
	product, err := s.Import(ctx, feature("program-owned-product"))
	if err != nil {
		t.Fatal(err)
	}
	commandOwnershipProduct(t, s, program, product.ID)
	plan, _, question := planningQuestionFixture(t, s, "program-owned-plan", "Keep acknowledgements durable.", "Must acknowledgements survive process loss?")
	commandOwnershipProduct(t, s, program, plan.ID)
	// The planning fixture returns its imported source; capture the actual
	// post-question state before checking that rejected commands preserve it.
	plan, err = s.Get(ctx, plan.ID)
	if err != nil {
		t.Fatal(err)
	}

	commands := []model.TypedCommand{
		commandTestCommand(t, "20000000-0000-4000-8000-000000000001", model.ActionSubjectCancel, model.SubjectCommandPayload{Kind: "feature", ID: product.ID, Revision: product.Revision}),
		commandTestCommand(t, "20000000-0000-4000-8000-000000000002", model.ActionSubjectCancel, model.SubjectCommandPayload{Kind: "plan", ID: plan.ID, Revision: plan.Revision}),
		commandTestCommand(t, "20000000-0000-4000-8000-000000000003", model.ActionPlanningAnswer, model.PlanningAnswerPayload{FeatureID: plan.ID, FeatureRevision: plan.Revision, AttemptID: question.AttemptID, OutputSHA: question.OutputSHA, QuestionID: question.ID, Text: "Acknowledgements must survive process loss."}),
	}
	for _, command := range commands {
		if _, err = s.ExecuteCommand(ctx, otherSession.TokenHash, otherSession.CSRFHash, command, cfg, nil); !errors.Is(err, ErrProjectForbidden) {
			t.Fatalf("same-project operator mutated another operator's published subject with %s: %v", command.Action, err)
		}
		if _, err = s.GetCommandReceipt(ctx, other.ID, command.CommandID); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("rejected command left a receipt: %v", err)
		}
	}
	currentProduct, err := s.Get(ctx, product.ID)
	if err != nil {
		t.Fatal(err)
	}
	currentPlan, err := s.Get(ctx, plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if currentProduct.State != product.State || currentProduct.Revision != product.Revision || currentPlan.State != plan.State || currentPlan.Revision != plan.Revision {
		t.Fatal("foreign cancellation/answer changed existing work")
	}
	var answers int
	if err = s.db.QueryRowContext(ctx, `SELECT count(*) FROM planning_answers WHERE feature_id=?`, plan.ID).Scan(&answers); err != nil {
		t.Fatal(err)
	}
	if answers != 0 {
		t.Fatal("foreign answer created planning history")
	}

	// The same boundary must permit the actual owner, rather than simply block
	// every published product command.
	if _, err = s.ExecuteCommand(ctx, ownerSession.TokenHash, ownerSession.CSRFHash, commands[2], cfg, nil); err != nil {
		t.Fatalf("owner cannot answer its published plan: %v", err)
	}
	updated, err := s.Get(ctx, plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Revision == plan.Revision || updated.State != "proposed" || updated.PolicyHash != "" {
		t.Fatal("owner planning answer did not produce an unapproved revision")
	}
	if _, err = s.ExecuteCommand(ctx, ownerSession.TokenHash, ownerSession.CSRFHash, commands[0], cfg, nil); err != nil {
		t.Fatalf("owner cannot cancel its published product: %v", err)
	}
}

func TestIntakeCommandReceiptRechecksRecursiveOwnership(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	cfg := commandTestConfig()
	now := time.Now().UTC().Add(-time.Second)
	owner, _, ownerSession := authTestActor(t, s, now, "receipt-program-owner")
	other, _, otherSession := authTestActor(t, s, now, "receipt-prior-local-access")
	product, err := s.Import(ctx, feature("formerly-local-product"))
	if err != nil {
		t.Fatal(err)
	}
	cancel := commandTestCommand(t, "30000000-0000-4000-8000-000000000001", model.ActionSubjectCancel, model.SubjectCommandPayload{Kind: "feature", ID: product.ID, Revision: product.Revision})
	if _, err = s.ExecuteCommand(ctx, otherSession.TokenHash, otherSession.CSRFHash, cancel, cfg, nil); err != nil {
		t.Fatal(err)
	}
	program := commandOwnershipProgram(t, s, owner, ownerSession, cfg)
	commandOwnershipProduct(t, s, program, product.ID)
	if _, err = s.GetCommandReceipt(ctx, other.ID, cancel.CommandID); !errors.Is(err, ErrProjectForbidden) {
		t.Fatalf("receipt disclosed after recursive ownership restriction: %v", err)
	}
	if _, err = s.ExecuteCommand(ctx, otherSession.TokenHash, otherSession.CSRFHash, cancel, cfg, nil); !errors.Is(err, ErrProjectForbidden) {
		t.Fatalf("receipt replay bypassed recursive current access: %v", err)
	}
}
