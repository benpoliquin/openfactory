package store

import (
	"context"
	"encoding/json"
	"testing"

	"openfactory/internal/model"
)

func TestProgramCompletionSupersessionRequiresExactInputAndRole(t *testing.T) {
	for _, mode := range []string{"same role", "outside input", "cross role"} {
		t.Run(mode, func(t *testing.T) {
			s, cfg, r := programStepsFixture(t)
			ctx := context.Background()
			step := firstProgramStep(t, s, cfg)
			originalManifest := step.InputManifestSHA
			producerKind := model.StepPlanEvaluation
			producer := model.ProgramStep{ID: "prior-evaluator", ProgramID: r.ProgramID, Revision: r.Number, Kind: producerKind, State: "completed", FeatureID: "prior-feature"}
			programReportInsert(t, s, `INSERT INTO program_steps(id,program_id,revision,feature_id,data) VALUES(?,?,?,?,?)`, producer, producer.ID, r.ProgramID, r.Number, producer.FeatureID)
			payload, err := json.Marshal(model.EvaluationPayload{EvaluatedManifestSHA: originalManifest, Verdict: "accept"})
			if err != nil {
				t.Fatal(err)
			}
			blob, err := s.blobs.Put(ctx, payload)
			if err != nil {
				t.Fatal(err)
			}
			prior := model.ProgramArtifact{ID: "prior-evaluation", ProgramID: r.ProgramID, Revision: r.Number, Kind: model.ArtifactEvaluation, ContentSHA: blob.SHA, InputManifestSHA: originalManifest, ProducerStepID: producer.ID, ProducerThreadID: "prior-thread", EvaluationVerdict: "accept", EvaluatedManifestSHA: originalManifest}
			programReportInsert(t, s, `INSERT INTO program_artifacts(id,program_id,revision,kind,content_sha,data) VALUES(?,?,?,?,?,?)`, prior, prior.ID, r.ProgramID, r.Number, prior.Kind, prior.ContentSHA)
			if mode != "outside input" {
				tx, err := s.db.BeginTx(ctx, nil)
				if err != nil {
					t.Fatal(err)
				}
				manifest, err := programManifestTx(ctx, tx, r.ProgramID, r.Number, []model.ProgramInputRef{{Kind: prior.Kind, ID: prior.ID, SHA: prior.ContentSHA, Revision: r.Number}})
				if err != nil {
					tx.Rollback()
					t.Fatal(err)
				}
				if err = tx.Commit(); err != nil {
					t.Fatal(err)
				}
				step.InputManifestSHA = manifest.SHA
			}
			step.Kind = model.StepPlanEvaluation
			if mode == "cross role" {
				step.Kind = model.StepResultEvaluation
			}
			stepJSON, err := json.Marshal(step)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.db.ExecContext(ctx, `UPDATE program_steps SET data=? WHERE id=?`, stepJSON, step.ID); err != nil {
				t.Fatal(err)
			}
			attempt := programAttemptFixture(t, s, step)
			output, err := json.Marshal(model.EvaluationPayload{EvaluatedManifestSHA: step.InputManifestSHA, Verdict: "accept"})
			if err != nil {
				t.Fatal(err)
			}
			err = s.CompleteProgramStep(ctx, attempt, model.ProgramStepResult{Outcome: "completed", Artifacts: []model.ProgramArtifactDraft{{Kind: model.ArtifactEvaluation, Payload: output, SupersedesID: prior.ID}}})
			if mode == "same role" {
				if err != nil {
					t.Fatal(err)
				}
			} else {
				requireProgramErrorClassification(t, err, true)
			}
			var retained int
			if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM program_artifacts WHERE id=?`, prior.ID).Scan(&retained); err != nil {
				t.Fatal(err)
			}
			if retained != 1 {
				t.Fatal("supersession deleted predecessor history")
			}
		})
	}
}
