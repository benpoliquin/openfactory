package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"openfactory/internal/model"
)

func requireProgramErrorClassification(t *testing.T, err error, domain bool) {
	t.Helper()
	if err == nil {
		t.Fatal("expected error")
	}
	var target *model.ProgramError
	if errors.As(err, &target) != domain {
		t.Fatalf("domain error = %v, want %v: %T: %v", target != nil, domain, err, err)
	}
}

func TestProgramCitationStorageErrorClassification(t *testing.T) {
	for _, mode := range []string{"missing", "dropped table", "malformed JSON"} {
		t.Run(mode, func(t *testing.T) {
			s, id := programReportFixture(t)
			ctx := context.Background()
			switch mode {
			case "dropped table":
				if _, err := s.db.ExecContext(ctx, `DROP TABLE program_sources`); err != nil {
					t.Fatal(err)
				}
			case "malformed JSON":
				if _, err := s.db.ExecContext(ctx, `INSERT INTO program_sources(id,program_id,revision,data) VALUES(?,?,?,?)`, "source", id, 1, []byte(`{`)); err != nil {
					t.Fatal(err)
				}
			}
			tx, err := s.db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			err = s.verifyProgramCitationTx(ctx, tx, model.ProgramStep{ProgramID: id, Revision: 1}, model.SourceCitation{SourceID: "source", TextSHA: "text", Quote: "quote"})
			requireProgramErrorClassification(t, err, mode == "missing")
		})
	}
}

func TestProgramReferenceMissingVersusStorageErrors(t *testing.T) {
	for _, kind := range []string{"program_revision", "source", "answer", "experiment_receipt", model.ArtifactClaims} {
		t.Run(kind, func(t *testing.T) {
			s, id := programReportFixture(t)
			ctx := context.Background()
			tx, err := s.db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			ref := model.ProgramInputRef{Kind: kind, ID: "missing", SHA: "missing"}
			requireProgramErrorClassification(t, programValidateRefTx(ctx, tx, id, 1, ref), true)
			if err = tx.Rollback(); err != nil {
				t.Fatal(err)
			}
			requireProgramErrorClassification(t, programValidateRefTx(ctx, tx, id, 1, ref), false)
		})
	}
}

func TestProgramDiscoveryGateStorageErrorClassification(t *testing.T) {
	for _, mode := range []string{"missing", "dropped table", "malformed JSON"} {
		t.Run(mode, func(t *testing.T) {
			s, id := programReportFixture(t)
			ctx := context.Background()
			switch mode {
			case "dropped table":
				if _, err := s.db.ExecContext(ctx, `DROP TABLE program_steps`); err != nil {
					t.Fatal(err)
				}
			case "malformed JSON":
				if _, err := s.db.ExecContext(ctx, `INSERT INTO program_steps(id,program_id,revision,feature_id,data) VALUES(?,?,?,?,?)`, "step", id, 1, "feature", []byte(`{`)); err != nil {
					t.Fatal(err)
				}
			}
			tx, err := s.db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			err = s.programFeatureGateTx(ctx, tx, model.Feature{DiscoveryStepID: "step"}, "policy")
			requireProgramErrorClassification(t, err, mode == "missing")
		})
	}
}

func TestProgramManifestCorruptionIsNotDomainWait(t *testing.T) {
	s, id := programReportFixture(t)
	ctx := context.Background()
	for _, data := range []string{`{`, `{"sha":"wrong","inputs":[]}`} {
		if _, err := s.db.ExecContext(ctx, `INSERT OR REPLACE INTO program_manifests(sha,program_id,revision,data) VALUES(?,?,?,?)`, "manifest", id, 1, []byte(data)); err != nil {
			t.Fatal(err)
		}
		_, _, _, err := programManifestLoadTx(ctx, s.db, "manifest")
		requireProgramErrorClassification(t, err, false)
	}
	_, _, _, err := programManifestLoadTx(ctx, s.db, "unknown")
	requireProgramErrorClassification(t, err, true)
	if err = s.db.Close(); err != nil {
		t.Fatal(err)
	}
	_, _, _, err = programManifestLoadTx(ctx, s.db, "manifest")
	requireProgramErrorClassification(t, err, false)
	if errors.Is(err, sql.ErrNoRows) {
		t.Fatal("closed database unexpectedly classified as missing manifest")
	}
}

func TestProgramArtifactPersistedJSONIsFatal(t *testing.T) {
	s, id := programReportFixture(t)
	ctx := context.Background()
	if _, err := s.db.ExecContext(ctx, `INSERT INTO program_artifacts(id,program_id,revision,kind,content_sha,data) VALUES(?,?,?,?,?,?)`, "artifact", id, 1, model.ArtifactClaims, "sha", []byte(`{`)); err != nil {
		t.Fatal(err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	err = programValidateRefTx(ctx, tx, id, 1, model.ProgramInputRef{Kind: model.ArtifactClaims, ID: "artifact", SHA: "sha"})
	requireProgramErrorClassification(t, err, false)
}
