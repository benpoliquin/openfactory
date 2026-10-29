package store

import (
	"context"
	"encoding/json"
	"testing"

	"openfactory/internal/model"
)

func TestIntakeReviewConsentUsesActualUnseededBundleFiles(t *testing.T) {
	for _, hasFiles := range []bool{true, false} {
		name := "empty-bundle"
		if hasFiles {
			name = "unseeded-private-files"
		}
		t.Run(name, func(t *testing.T) {
			s, cfg, op, session := intakeReviewsFixture(t)
			ctx := context.Background()
			var bundle model.ProgramBundle
			var err error
			if hasFiles {
				bundle = intakeBundlesFinalized(t, s, cfg, op.ID, []byte("private attachment not named by any Program seed"))
			} else {
				raw, err := json.Marshal(struct {
					Files []model.ProgramBundleFile `json:"files"`
				}{Files: []model.ProgramBundleFile{}})
				if err != nil {
					t.Fatal(err)
				}
				ref, err := s.blobs.Put(ctx, raw)
				if err != nil {
					t.Fatal(err)
				}
				bundle, err = s.blobs.LoadBundle(ctx, ref.SHA)
				if err != nil {
					t.Fatal(err)
				}
			}
			spec := model.ProgramSpec{ID: "unseeded-review", Project: "demo", Title: "Explicit public research", Objective: "Review exact material before any model use", Seeds: []model.ProgramSeed{{ID: "public-source", Kind: "public_url", URL: "https://github.com/example/project"}}, Disclosure: model.ProgramDisclosure{ModelProviderIDs: []string{"provider"}, PublicSearchAllowed: true, PrivateMaterialToModels: false}}
			revision, err := s.ImportProgram(ctx, spec, bundle)
			if err != nil {
				t.Fatal(err)
			}
			command := intakeReviewsCommand(t, model.ActionProgramApproveRevision, model.SubjectCommandPayload{Kind: "program", ID: spec.ID, Revision: revision.RevisionHash})
			review, reviewErr := s.CreateIntakeReview(ctx, session.TokenHash, command, cfg)
			if hasFiles {
				intakeReviewsCode(t, reviewErr, "disclosure_required")
				err = s.ApproveProgram(ctx, spec.ID, revision.RevisionHash, cfg.PolicyHash, model.TrustedLocalAudit())
				intakeReviewsCode(t, err, "disclosure_required")
			} else {
				if reviewErr != nil {
					t.Fatalf("empty attachment manufactured a private-material consent requirement: %v", reviewErr)
				}
				if review.SeedManifest == nil || review.SeedManifest.ManifestSHA != bundle.ManifestSHA || len(review.SeedManifest.Files) != 0 || review.ProgramRevision == nil || review.ProgramRevision.Spec.Disclosure.PrivateMaterialToModels {
					t.Fatal("empty-bundle review lost exact source or synthesized private consent")
				}
				if err = s.ApproveProgram(ctx, spec.ID, revision.RevisionHash, cfg.PolicyHash, model.TrustedLocalAudit()); err != nil {
					t.Fatal(err)
				}
			}
			current, err := programRevision(ctx, s.db, spec.ID, revision.Number)
			if err != nil {
				t.Fatal(err)
			}
			if current.Spec.Disclosure.PrivateMaterialToModels || current.RevisionHash != revision.RevisionHash {
				t.Fatal("review/approval rewrote the source disclosure")
			}
			var grants, challenges, calls, steps int
			for _, query := range []struct {
				sql string
				out *int
			}{
				{`SELECT count(*) FROM program_grants`, &grants},
				{`SELECT count(*) FROM intake_auth_challenges`, &challenges},
				{`SELECT count(*) FROM calls`, &calls},
				{`SELECT count(*) FROM program_steps`, &steps},
			} {
				if err = s.db.QueryRowContext(ctx, query.sql).Scan(query.out); err != nil {
					t.Fatal(err)
				}
			}
			expectedGrants := 1
			if hasFiles {
				expectedGrants = 0
			}
			if grants != expectedGrants || challenges != 0 || calls != 0 || steps != 0 {
				t.Fatalf("review or revision approval created unintended effects: grants=%d challenges=%d calls=%d steps=%d", grants, challenges, calls, steps)
			}
		})
	}
}
