package store

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"openfactory/internal/model"
)

func TestExternalDiscoveryFeaturesRejected(t *testing.T) {
	ctx := context.Background()
	for _, proposal := range []bool{false, true} {
		for _, reservedID := range []bool{false, true} {
			s := openTest(t)
			f := feature("external-discovery")
			if reservedID {
				f.ID = "openfactory-discovery-forged"
			} else {
				f.DiscoveryStepID = "forged-step"
				f.Checks = nil
			}
			var err error
			if proposal {
				_, err = s.ImportProposal(ctx, f)
			} else {
				_, err = s.Import(ctx, f)
			}
			if err == nil {
				t.Fatalf("accepted controller-owned feature: proposal=%v reservedID=%v", proposal, reservedID)
			}
			if _, err = s.Get(ctx, f.ID); err == nil {
				t.Fatal("rejected import persisted feature")
			}
		}
	}
}

func missingProgramGate() *model.ProgramEvidenceGate {
	return &model.ProgramEvidenceGate{ProgramID: "absent-program", Revision: 1, ManifestSHA: strings.Repeat("a", 64), EvaluationArtifactID: "absent-evaluation"}
}

func TestProgramGateRejectsApprovalAndClaim(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	f := feature("gated-approval")
	f.ProgramEvidenceGate = missingProgramGate()
	f, err := s.Import(ctx, f)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Approve(ctx, f.ID, f.Revision, "policy", model.TrustedLocalAudit()); err == nil {
		t.Fatal("approved feature without current program evidence")
	}
	f, err = s.Get(ctx, f.ID)
	if err != nil {
		t.Fatal(err)
	}
	if f.State != "proposed" || f.PolicyHash != "" {
		t.Fatal("failed approval changed authority")
	}
	// Represent formerly approved evidence that is no longer available.
	f.State, f.PolicyHash = "ready", "policy"
	if err = put(ctx, s.db, f); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Claim(ctx, f.ID, "policy", 3, 2, 1, 3600); !errors.Is(err, ErrWaiting) {
		t.Fatalf("missing evidence must remain queued: %v", err)
	}
	active, err := s.Active(ctx)
	if err != nil || len(active) != 0 {
		t.Fatalf("gate consumed attempt capacity: %v %v", active, err)
	}
}

func TestProgramGateFencesDispatchReplayAndOperations(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	f := approved(t, s, "ordinary-then-gated")
	a := claim(t, s, f.ID)
	if err := s.BeginOperation(ctx, a, "policy"); err != nil {
		t.Fatalf("ordinary operation blocked: %v", err)
	}
	if err := s.ReserveCall(ctx, a, "implement", "dispatch-one", 10); err != nil {
		t.Fatalf("ordinary dispatch blocked: %v", err)
	}
	active, err := s.Active(ctx)
	if err != nil || len(active) != 1 {
		t.Fatalf("active attempt: %v %v", active, err)
	}
	a = active[0]
	f, err = s.Get(ctx, f.ID)
	if err != nil {
		t.Fatal(err)
	}
	f.ProgramEvidenceGate = missingProgramGate()
	if err = put(ctx, s.db, f); err != nil {
		t.Fatal(err)
	}
	if err = s.ReserveCall(ctx, a, "implement", "dispatch-one", 10); err == nil {
		t.Fatal("idempotent replay bypassed evidence fence")
	}
	if err = s.ReserveCall(ctx, a, "review", "dispatch-two", 10); err == nil {
		t.Fatal("new model call bypassed evidence fence")
	}
	if err = s.BeginOperation(ctx, a, "policy"); err == nil {
		t.Fatal("external effect bypassed evidence fence")
	}
	var count int
	if err = s.db.QueryRowContext(ctx, `SELECT count(*) FROM calls`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("rejected call consumed allowance: count=%d err=%v", count, err)
	}
	if err = s.Cancel(ctx, f.ID, model.TrustedLocalAudit()); err != nil {
		t.Fatalf("evidence fence blocked cancellation: %v", err)
	}
}

func TestMemoryStoreArtifactsArePrivateAndRemoved(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	root := s.blobTempDir
	if root == "" || s.blobs == nil {
		s.Close()
		t.Fatal("memory store has no isolated artifact root")
	}
	if _, err = s.blobs.Put(context.Background(), []byte("private artifact")); err != nil {
		s.Close()
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("temporary artifact root retained: %v", err)
	}
}

func TestProgramSessionAuthorityFencesReplay(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	f := approved(t, s, "session-authority")
	a := claim(t, s, f.ID)
	if err := s.ReserveCall(ctx, a, "implement", "session-dispatch", 10); err != nil {
		t.Fatal(err)
	}
	session := model.Session{ThreadID: "ordinary-thread", EnvironmentID: "ordinary-environment"}
	for range 2 {
		if err := s.RecordSession(ctx, a.ID, "implement", "session-dispatch", session); err != nil {
			t.Fatalf("ordinary session/replay rejected: %v", err)
		}
	}
	f, err := s.Get(ctx, f.ID)
	if err != nil {
		t.Fatal(err)
	}
	// A discovery association without its durable input graph cannot establish
	// independence, including when a previous session identity already exists.
	f.DiscoveryStepID = "missing-evaluator-step"
	if err = put(ctx, s.db, f); err != nil {
		t.Fatal(err)
	}
	if err = s.RecordSession(ctx, a.ID, "implement", "session-dispatch", session); err == nil {
		t.Fatal("session replay bypassed discovery independence authority")
	}
	session.ThreadID = "new-unverified-thread"
	if err = s.RecordSession(ctx, a.ID, "implement", "session-dispatch", session); err == nil {
		t.Fatal("session inserted without discovery independence authority")
	}
	var count int
	if err = s.db.QueryRowContext(ctx, `SELECT count(*) FROM sessions WHERE attempt_id=?`, a.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("rejected session persisted: count=%d err=%v", count, err)
	}
}

func TestDiscoveryCannotSatisfyCodeDependencies(t *testing.T) {
	ctx := context.Background()
	for _, reservedID := range []bool{false, true} {
		for _, target := range []string{"reviewed", "integrated"} {
			s := openTest(t)
			parent := feature("discovery-parent")
			parent.Kind = "research"
			parent.State, parent.CompletionTarget, parent.Receipt = "accepted", target, "review-receipt"
			if reservedID {
				parent.ID = "openfactory-discovery-parent"
			} else {
				parent.DiscoveryStepID = "generated-step"
			}
			if err := put(ctx, s.db, parent); err != nil {
				t.Fatal(err)
			}
			child := feature("dependent-product")
			child.DependsOn = []string{parent.ID}
			if _, err := s.Import(ctx, child); err == nil {
				t.Fatalf("import accepted discovery dependency: reserved=%v target=%s", reservedID, target)
			}
			// Older persisted features must also remain fenced, even if the
			// discovery feature falsely advertises an integrated receipt.
			child.State, child.PolicyHash = "ready", "policy"
			if err := put(ctx, s.db, child); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Claim(ctx, child.ID, "policy", 3, 2, 1, 3600); !errors.Is(err, ErrWaiting) {
				t.Fatalf("claim accepted discovery dependency: reserved=%v target=%s err=%v", reservedID, target, err)
			}
		}
	}
	s := openTest(t)
	parent := feature("ordinary-integrated-parent")
	parent.State, parent.Receipt = "accepted", "integrated-receipt"
	if err := put(ctx, s.db, parent); err != nil {
		t.Fatal(err)
	}
	child := feature("ordinary-dependent")
	child.DependsOn = []string{parent.ID}
	child, err := s.Import(ctx, child)
	if err != nil {
		t.Fatalf("ordinary dependency rejected: %v", err)
	}
	if err = s.Approve(ctx, child.ID, child.Revision, "policy", model.TrustedLocalAudit()); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Claim(ctx, child.ID, "policy", 3, 2, 1, 3600); err != nil {
		t.Fatalf("ordinary integrated dependency blocked: %v", err)
	}
}
