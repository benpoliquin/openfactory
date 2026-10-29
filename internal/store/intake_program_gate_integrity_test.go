package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"openfactory/internal/model"
)

func TestProgramFeatureGateKeepsPersistedAuthorityCorruptionFatal(t *testing.T) {
	for _, corruption := range []string{"missing-current-revision", "malformed-program-json"} {
		t.Run(corruption, func(t *testing.T) {
			s := openTest(t)
			ctx := context.Background()
			revision, err := s.ImportProgram(ctx, programTxSpec("persisted-program"), model.ProgramBundle{})
			if err != nil {
				t.Fatal(err)
			}
			f := feature("gated-corrupt-authority")
			f.ProgramEvidenceGate = &model.ProgramEvidenceGate{ProgramID: revision.ProgramID, Revision: revision.Number, ManifestSHA: authTestHash("missing-manifest"), EvaluationArtifactID: "missing-evaluation"}
			f, err = s.Import(ctx, f)
			if err != nil {
				t.Fatal(err)
			}
			// An existing approved product can wait for unavailable external evidence,
			// but corruption of a present Program's own authority is not a queue wait.
			f.State, f.PolicyHash = "ready", "policy"
			if err = put(ctx, s.db, f); err != nil {
				t.Fatal(err)
			}
			if corruption == "missing-current-revision" {
				tx, err := s.db.BeginTx(ctx, nil)
				if err != nil {
					t.Fatal(err)
				}
				p, err := programLoad(ctx, tx, revision.ProgramID)
				if err != nil {
					tx.Rollback()
					t.Fatal(err)
				}
				p.CurrentRevision = revision.Number + 1
				if err = programPut(ctx, tx, p); err != nil {
					tx.Rollback()
					t.Fatal(err)
				}
				if err = tx.Commit(); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err = s.db.ExecContext(ctx, `UPDATE programs SET data=? WHERE id=?`, []byte(`{"id":`), revision.ProgramID); err != nil {
					t.Fatal(err)
				}
			}
			_, err = s.Claim(ctx, f.ID, "policy", 3, 2, 1, 3600)
			if err == nil || errors.Is(err, ErrWaiting) {
				t.Fatalf("persisted authority corruption became an ordinary queue wait: %v", err)
			}
			if corruption == "missing-current-revision" && !errors.Is(err, sql.ErrNoRows) {
				t.Fatalf("lost original missing-current-revision integrity error: %v", err)
			}
			active, err := s.Active(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if len(active) != 0 {
				t.Fatal("corrupt authority consumed attempt capacity")
			}
		})
	}
}
