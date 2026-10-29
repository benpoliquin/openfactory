package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"openfactory/internal/intake"
	"openfactory/internal/model"
)

type planResult struct {
	Features []model.Feature `json:"features"`
	Question string          `json:"question,omitempty"`
}

func (e *Engine) finishPlan(ctx context.Context, a model.Attempt, parent model.Feature) error {
	fp, err := e.repos.Fingerprint(ctx, a.Workspace)
	if err != nil {
		return e.block(ctx, a, err.Error(), false)
	}
	if fp != a.Fingerprint {
		return e.block(ctx, a, "planner modified workspace", false)
	}
	var result planResult
	if err = decodeResult(a.Summary, &result); err != nil {
		return e.block(ctx, a, "planning result: "+err.Error(), false)
	}
	if result.Question != "" {
		question := strings.TrimSpace(result.Question)
		if question == "" {
			return e.db.FailPlanningQuestion(ctx, a, "planner returned a whitespace-only question")
		}
		if err = e.db.RecordPlanningQuestion(ctx, a, question); err != nil {
			var domain *model.ProgramError
			if errors.As(err, &domain) {
				return e.db.FailPlanningQuestion(ctx, a, "planning question: "+domain.Message)
			}
			return err
		}
		return nil
	}
	if len(result.Features) == 0 {
		return e.block(ctx, a, "planner returned no feature proposals", false)
	}
	byID := map[string]model.Feature{}
	for _, f := range result.Features {
		if f.Kind == "plan" || f.Project != parent.Project || f.ID == parent.ID {
			return e.block(ctx, a, "planner proposed nested planning or a different project", false)
		}
		if _, exists := byID[f.ID]; exists {
			return e.block(ctx, a, "duplicate proposal ID "+f.ID, false)
		}
		if err = intake.ValidateExternal(f); err != nil {
			return e.block(ctx, a, "proposal: "+err.Error(), false)
		}
		for _, name := range f.Checks {
			if command, ok := e.cfg.Projects[f.Project].Checks[name]; !ok || len(command.Argv) == 0 {
				return e.block(ctx, a, "proposal uses unapproved check "+name, false)
			}
		}
		// A planner cannot replace an operator's existing source or authority.
		old, err := e.db.Get(ctx, f.ID)
		if err == nil && old.Revision != intake.Revision(f) {
			return e.block(ctx, a, "proposal collides with existing feature "+f.ID, false)
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		byID[f.ID] = f
	}
	visiting := map[string]bool{}
	done := map[string]bool{}
	ordered := []model.Feature{}
	var visit func(string) error
	visit = func(id string) error {
		if done[id] {
			return nil
		}
		if visiting[id] {
			return errors.New("planning dependency cycle")
		}
		f, ok := byID[id]
		if !ok {
			_, err := e.db.Get(ctx, id)
			return err
		}
		visiting[id] = true
		for _, dep := range f.DependsOn {
			if dep == parent.ID {
				return errors.New("proposal cannot depend on its planning-only parent")
			}
			if err := visit(dep); err != nil {
				return fmt.Errorf("dependency %s: %w", dep, err)
			}
		}
		visiting[id] = false
		done[id] = true
		ordered = append(ordered, f)
		return nil
	}
	for _, f := range result.Features {
		if err = visit(f.ID); err != nil {
			return e.block(ctx, a, err.Error(), false)
		}
	}
	// Import is revision-idempotent, making a crash between proposals safe to replay.
	// No call here approves a child or inherits the parent's approval.
	ids := []string{}
	for _, f := range ordered {
		if _, err = e.db.ImportProposal(ctx, f); err != nil {
			return e.block(ctx, a, "proposal import: "+err.Error(), false)
		}
		ids = append(ids, f.ID)
	}
	a.Stage = "accepted"
	a.State = "accepted"
	a.ThreadID = ""
	return e.db.Save(ctx, a, "accepted", "proposed features: "+strings.Join(ids, ", "), "reviewed:plan:"+parent.Revision, false, model.Evidence{Kind: "plan", Passed: true, Detail: a.Summary})
}
