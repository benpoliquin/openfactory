package store

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"openfactory/internal/intake"
	"openfactory/internal/model"
)

func planningQuestionFixture(t *testing.T, s *Store, id, goal, question string) (model.Feature, model.Attempt, model.PlanningQuestion) {
	t.Helper()
	ctx := context.Background()
	f, err := intake.NewPlan(id, "demo", "Clarify bounded work", goal)
	if err != nil {
		t.Fatal(err)
	}
	f, err = s.Import(ctx, f)
	if err != nil {
		t.Fatal(err)
	}
	return planningQuestionForRevision(t, s, f, question)
}

func planningQuestionForRevision(t *testing.T, s *Store, f model.Feature, question string) (model.Feature, model.Attempt, model.PlanningQuestion) {
	t.Helper()
	ctx := context.Background()
	if err := s.Approve(ctx, f.ID, f.Revision, "policy", model.TrustedLocalAudit()); err != nil {
		t.Fatal(err)
	}
	a, err := s.Claim(ctx, f.ID, "policy", 8, 8, 8, 3600)
	if err != nil {
		t.Fatal(err)
	}
	output, err := json.Marshal(struct {
		Question string `json:"question"`
	}{question})
	if err != nil {
		t.Fatal(err)
	}
	a.Summary = string(output)
	if err = s.RecordPlanningQuestion(ctx, a, question); err != nil {
		t.Fatal(err)
	}
	questions, err := s.PlanningQuestions(ctx, f.ID)
	if err != nil || len(questions) == 0 {
		t.Fatalf("missing durable question: %+v %v", questions, err)
	}
	return f, a, questions[len(questions)-1]
}

func TestPlanningAnswerCreatesUnapprovedRevisionWithCompleteGoalAndLineage(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	goal := "Keep the original exact planning goal; propose work, never approve products."
	f, a, q := planningQuestionFixture(t, s, "planning-lineage", goal, "Must acknowledgements survive a restart?")
	if q.FeatureRevision != f.Revision || q.AttemptID != a.ID || q.OutputSHA != planningOutputSHA(a.Summary) {
		t.Fatalf("question lost exact output binding: %+v", q)
	}
	if err := s.RecordPlanningQuestion(ctx, a, q.Question); err != nil {
		t.Fatalf("record replay failed: %v", err)
	}
	var active int
	if err := s.db.QueryRowContext(ctx, `SELECT active FROM attempts WHERE id=?`, a.ID).Scan(&active); err != nil || active != 0 {
		t.Fatalf("question retained attempt ownership: %d %v", active, err)
	}
	next, err := s.AnswerPlanningQuestion(ctx, model.PlanningAnswer{Question: q, Text: "Yes: retain acknowledged data across restarts.", Audit: model.TrustedLocalAudit()})
	if err != nil {
		t.Fatal(err)
	}
	if next.Revision == f.Revision || next.State != "proposed" || next.PolicyHash != "" || next.Receipt != "" || next.Attempts != 1 {
		t.Fatalf("answer reused authority or reset attempt history: %+v", next)
	}
	for _, exact := range []string{goal, q.Question, "Yes: retain acknowledged data across restarts.", q.FeatureRevision, q.AttemptID, q.OutputSHA, q.ID} {
		if !strings.Contains(next.Body, exact) {
			t.Fatalf("missing original goal/answer lineage %q in %q", exact, next.Body)
		}
	}
	if _, err = intake.Export(next); err != nil {
		t.Fatalf("answered revision cannot be exported canonically: %v", err)
	}
	var original []byte
	if err = s.db.QueryRowContext(ctx, `SELECT data FROM revisions WHERE feature_id=? AND revision=?`, f.ID, f.Revision).Scan(&original); err != nil {
		t.Fatal(err)
	}
	var historical model.Feature
	if err = json.Unmarshal(original, &historical); err != nil || historical.Body != goal {
		t.Fatalf("original goal rewritten: %+v %v", historical, err)
	}
	if _, err = s.Claim(ctx, f.ID, "policy", 8, 8, 8, 3600); err == nil {
		t.Fatal("answer automatically retried the plan")
	}
	_, _, secondQ := planningQuestionForRevision(t, s, next, "Which retention period is required?")
	final, err := s.AnswerPlanningQuestion(ctx, model.PlanningAnswer{Question: secondQ, Text: "Retain the journal for seven days.", Audit: model.TrustedLocalAudit()})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(final.Body, goal) != 1 || !strings.Contains(final.Body, q.ID) || !strings.Contains(final.Body, secondQ.ID) || !strings.Contains(final.Body, "Retain the journal for seven days.") || final.State != "proposed" || final.PolicyHash != "" {
		t.Fatalf("amendment lost history or inherited approval: %+v", final)
	}
	var count int
	if err = s.db.QueryRowContext(ctx, `SELECT count(*) FROM features`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("question/answer published product features: %d %v", count, err)
	}
}

func TestPlanningAnswerRejectsForgedBindingsAndStaleReplay(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	f, _, q := planningQuestionFixture(t, s, "planning-binding", "Preserve source authority.", "Which durable boundary matters?")
	for _, mutate := range []func(*model.PlanningAnswer){
		func(a *model.PlanningAnswer) { a.Question.OutputSHA = programHash("different output") },
		func(a *model.PlanningAnswer) { a.Question.AttemptID = "other-attempt" },
		func(a *model.PlanningAnswer) { a.Question.FeatureRevision = "other-revision" },
		func(a *model.PlanningAnswer) { a.Question.Question = "changed question" },
		func(a *model.PlanningAnswer) { a.Audit = model.AuditContext{} },
		func(a *model.PlanningAnswer) { a.Text = "\n" },
	} {
		answer := model.PlanningAnswer{Question: q, Text: "Use a durable journal.", Audit: model.TrustedLocalAudit()}
		mutate(&answer)
		if _, err := s.AnswerPlanningQuestion(ctx, answer); err == nil {
			t.Fatalf("accepted forged planning answer: %+v", answer)
		}
	}
	answer := model.PlanningAnswer{Question: q, Text: "Use a durable journal.", Audit: model.TrustedLocalAudit()}
	next, err := s.AnswerPlanningQuestion(ctx, answer)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.AnswerPlanningQuestion(ctx, answer)
	answerErrorCode(t, err, "stale_revision")
	current, err := s.Get(ctx, f.ID)
	if err != nil || current.Revision != next.Revision {
		t.Fatalf("stale replay changed source: %+v %v", current, err)
	}
}

func TestPlanningAnswerDoesNotReviveQuestionAfterNewAttempt(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	f, _, old := planningQuestionFixture(t, s, "planning-new-attempt", "Preserve exact attempt authority.", "First question?")
	if err := s.Retry(ctx, f.ID, model.TrustedLocalAudit()); err != nil {
		t.Fatal(err)
	}
	a, err := s.Claim(ctx, f.ID, "policy", 8, 8, 8, 3600)
	if err != nil {
		t.Fatal(err)
	}
	a.Stage, a.State = "blocked", "blocked"
	if err = s.Save(ctx, a, "blocked", "later planning result", "", false); err != nil {
		t.Fatal(err)
	}
	_, err = s.AnswerPlanningQuestion(ctx, model.PlanningAnswer{Question: old, Text: "An obsolete answer.", Audit: model.TrustedLocalAudit()})
	answerErrorCode(t, err, "stale_revision")
}

func TestPlanningAnswerRollsBackNewRevisionAndLineageWithOuterTransaction(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	f, _, q := planningQuestionFixture(t, s, "planning-rollback", "Keep the original goal intact.", "What must remain atomic?")
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	next, err := s.answerPlanningQuestionTx(ctx, tx, model.PlanningAnswer{Question: q, Text: "The answer, new revision, audit and command receipt.", Audit: model.TrustedLocalAudit()})
	if err != nil {
		t.Fatal(err)
	}
	if next.Revision == f.Revision {
		t.Fatal("transaction helper did not create a revision")
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO settings(key,value) VALUES('planning-command-test','receipt')`); err != nil {
		t.Fatal(err)
	}
	if err = tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	current, err := s.Get(ctx, f.ID)
	if err != nil || current.Revision != f.Revision || current.Body != f.Body {
		t.Fatalf("outer rollback retained new source: %+v %v", current, err)
	}
	for _, table := range []string{"planning_answers", "planning_lineage"} {
		var n int
		if err = s.db.QueryRowContext(ctx, `SELECT count(*) FROM `+table+` WHERE feature_id=?`, f.ID).Scan(&n); err != nil || n != 0 {
			t.Fatalf("outer rollback retained %s: %d %v", table, n, err)
		}
	}
	var n int
	if err = s.db.QueryRowContext(ctx, `SELECT count(*) FROM revisions WHERE feature_id=?`, f.ID).Scan(&n); err != nil || n != 1 {
		t.Fatalf("outer rollback retained revision: %d %v", n, err)
	}
	if err = s.db.QueryRowContext(ctx, `SELECT count(*) FROM settings WHERE key='planning-command-test'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("outer rollback retained receipt: %d %v", n, err)
	}
}

func TestPlanningQuestionRollsBackWhenUncertainAttemptCannotSettle(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	f, err := intake.NewPlan("planning-question-uncertain", "demo", "Retain uncertain ownership", "Never free ambiguous worker ownership to ask a question.")
	if err != nil {
		t.Fatal(err)
	}
	f, err = s.Import(ctx, f)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Approve(ctx, f.ID, f.Revision, "policy", model.TrustedLocalAudit()); err != nil {
		t.Fatal(err)
	}
	a, err := s.Claim(ctx, f.ID, "policy", 8, 8, 8, 3600)
	if err != nil {
		t.Fatal(err)
	}
	a.Stage, a.State = "uncertain", "uncertain"
	if err = s.Save(ctx, a, "running", "ownership unknown", "", true); err != nil {
		t.Fatal(err)
	}
	a.Summary = `{"question":"May this uncertain execution be replayed?"}`
	if err = s.RecordPlanningQuestion(ctx, a, "May this uncertain execution be replayed?"); err == nil {
		t.Fatal("uncertain ownership was settled by a planning question")
	}
	questions, err := s.PlanningQuestions(ctx, f.ID)
	if err != nil || len(questions) != 0 {
		t.Fatalf("failed settlement left a durable question: %+v %v", questions, err)
	}
	var active int
	if err = s.db.QueryRowContext(ctx, `SELECT active FROM attempts WHERE id=?`, a.ID).Scan(&active); err != nil || active != 1 {
		t.Fatalf("failed question settlement freed uncertain ownership: %d %v", active, err)
	}
}

func terminalPlanningFixture(t *testing.T, s *Store, id string) (model.Feature, model.Attempt) {
	t.Helper()
	ctx := context.Background()
	f, err := intake.NewPlan(id, "demo", "Settle terminal planning", "Ask questions without granting implementation authority.")
	if err != nil {
		t.Fatal(err)
	}
	f, err = s.Import(ctx, f)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Approve(ctx, f.ID, f.Revision, "policy", model.TrustedLocalAudit()); err != nil {
		t.Fatal(err)
	}
	a, err := s.Claim(ctx, f.ID, "policy", 8, 8, 8, 3600)
	if err != nil {
		t.Fatal(err)
	}
	a.Stage, a.State, a.ThreadID, a.DispatchKey = "plan_running", "running", "terminal-plan-thread", "terminal-plan-dispatch"
	if err = s.Save(ctx, a, "running", "", "", true); err != nil {
		t.Fatal(err)
	}
	a.Cursor, a.Summary = "terminal-output-cursor", `{"question":"   "}`
	return f, a
}

func TestFailPlanningQuestionSettlesOnlyTerminalFailureOrCancellation(t *testing.T) {
	for _, cancel := range []bool{false, true} {
		name := "blocked"
		if cancel {
			name = "cancelled"
		}
		t.Run(name, func(t *testing.T) {
			s := openTest(t)
			ctx := context.Background()
			f, a := terminalPlanningFixture(t, s, "planning-terminal-"+name)
			if cancel {
				if err := s.Cancel(ctx, f.ID, model.TrustedLocalAudit()); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.FailPlanningQuestion(ctx, a, "planning question could not be accepted"); err != nil {
				t.Fatal(err)
			}
			var active int
			var raw []byte
			if err := s.db.QueryRowContext(ctx, `SELECT active,data FROM attempts WHERE id=?`, a.ID).Scan(&active, &raw); err != nil {
				t.Fatal(err)
			}
			var settled model.Attempt
			if err := json.Unmarshal(raw, &settled); err != nil {
				t.Fatal(err)
			}
			if active != 0 || settled.State != name || settled.ThreadID != "" || settled.Summary != a.Summary || settled.Cursor != a.Cursor {
				t.Fatalf("terminal planning failure lost settlement/output binding: active=%d %+v", active, settled)
			}
			current, err := s.Get(ctx, f.ID)
			if err != nil || current.Revision != f.Revision || current.State != name || current.Receipt != "" {
				t.Fatalf("failure changed revision/created success or lost cancellation: %+v %v", current, err)
			}
			var n int
			if err = s.db.QueryRowContext(ctx, `SELECT count(*) FROM sessions WHERE attempt_id=? AND thread_id=? AND dispatch_key=?`, a.ID, a.ThreadID, a.DispatchKey).Scan(&n); err != nil || n != 1 {
				t.Fatalf("terminal worker provenance lost: %d %v", n, err)
			}
			for _, table := range []string{"planning_questions", "planning_answers", "evidence"} {
				if err = s.db.QueryRowContext(ctx, `SELECT count(*) FROM `+table+` WHERE feature_id=?`, f.ID).Scan(&n); err != nil || n != 0 {
					t.Fatalf("terminal failure fabricated %s: %d %v", table, n, err)
				}
			}
		})
	}
}

func TestFailPlanningQuestionRejectsWrongWorkerAndInconsistentAuthority(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	f, a := terminalPlanningFixture(t, s, "planning-terminal-identity")
	for _, mutate := range []func(*model.Attempt){
		func(a *model.Attempt) { a.ThreadID = "different-worker" },
		func(a *model.Attempt) { a.DispatchKey = "different-dispatch" },
		func(a *model.Attempt) { a.Stage = "uncertain" },
	} {
		wrong := a
		mutate(&wrong)
		if err := s.FailPlanningQuestion(ctx, wrong, "terminal failure"); err == nil {
			t.Fatalf("settled wrong or uncertain worker: %+v", wrong)
		}
	}
	// Ordinary imports prohibit replacing an active revision. Simulate corrupt
	// ledger authority directly; the failure helper must not hide it by freeing
	// the old attempt or mutating the newer feature.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	current, err := get(ctx, tx, f.ID)
	if err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	current.Revision, current.Body = "inconsistent-newer-revision", "Do not overwrite this current source."
	if err = put(ctx, tx, current); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err = s.FailPlanningQuestion(ctx, a, "terminal failure"); err == nil {
		t.Fatal("inconsistent active authority was silently settled")
	}
	var active int
	if err = s.db.QueryRowContext(ctx, `SELECT active FROM attempts WHERE id=?`, a.ID).Scan(&active); err != nil || active != 1 {
		t.Fatalf("integrity failure freed active ownership: %d %v", active, err)
	}
	retained, err := s.Get(ctx, f.ID)
	if err != nil || retained.Revision != current.Revision || retained.Body != current.Body || retained.State != "running" {
		t.Fatalf("integrity failure rewrote current feature: %+v %v", retained, err)
	}
}
