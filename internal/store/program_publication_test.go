package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"

	"openfactory/internal/model"
)

func TestPublicationDependenciesResolveWholeBatch(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	a, b := feature("publication-a"), feature("publication-b")
	a.DependsOn = []string{b.ID}
	if err = publicationDependenciesTx(ctx, tx, map[string]model.Feature{a.ID: a, b.ID: b}); err != nil {
		t.Fatal(err)
	}
	b.DependsOn = []string{a.ID}
	if err = publicationDependenciesTx(ctx, tx, map[string]model.Feature{a.ID: a, b.ID: b}); err == nil {
		t.Fatal("accepted batch cycle")
	}
	b.DependsOn = []string{"missing"}
	if err = publicationDependenciesTx(ctx, tx, map[string]model.Feature{a.ID: a, b.ID: b}); err == nil {
		t.Fatal("accepted unresolved batch edge")
	}
}

func TestPublicationDependenciesIncludeExistingGraph(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	old := feature("publication-existing")
	old.DependsOn = []string{"publication-new"}
	if err := put(ctx, s.db, old); err != nil {
		t.Fatal(err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	f := feature("publication-new")
	f.DependsOn = []string{old.ID}
	if err = publicationDependenciesTx(ctx, tx, map[string]model.Feature{f.ID: f}); err == nil {
		t.Fatal("accepted cycle through existing feature")
	}
}

func publicationAuthorityFixture(t *testing.T, s *Store, revision int, grant bool) {
	t.Helper()
	ctx := context.Background()
	spec := model.ProgramSpec{
		ID:          "publication-program",
		Project:     "demo",
		Title:       "Evaluated publication",
		Objective:   "Publish only the exact evaluated implementation contracts",
		Scope:       []string{"Preserve inherited operator questions and require complete requirement coverage"},
		NonGoals:    []string{"Search public sources or disclose private material"},
		Constraints: []string{"Bind publication and approval to the current source revision"},
		Disclosure: model.ProgramDisclosure{
			ModelProviderIDs:        []string{"provider"},
			PrivateMaterialToModels: false,
			PublicSearchAllowed:     false,
		},
		Limits: model.ProgramLimits{MaxIterations: 2, MaxModelCalls: 20, MaxSteps: 32, MaxExperiments: 8},
		Implementation: model.ProgramImplementationPolicy{
			PermittedFeatureKinds:      []string{"feature", "fix", "maintenance"},
			PermittedCompletionTargets: []string{"reviewed"},
		},
	}
	for n := 1; n <= revision; n++ {
		r, err := s.ImportProgram(ctx, spec, model.ProgramBundle{})
		if err != nil {
			t.Fatal(err)
		}
		if n < revision || grant {
			if err = s.ApproveProgram(ctx, spec.ID, r.RevisionHash, "policy", model.TrustedLocalAudit()); err != nil {
				t.Fatal(err)
			}
		}
		spec.Objective += "; retain the revised source"
	}
}

func TestPublicationRequiresCurrentRevisionGrant(t *testing.T) {
	s := openTest(t)
	publicationAuthorityFixture(t, s, 2, false)
	ctx := context.Background()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	m, err := programManifestTx(ctx, tx, "publication-program", 2, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	_, err = s.PublishProgramProposals(ctx, "publication-program", m.SHA, model.Config{PolicyHash: "policy"})
	var domain *model.ProgramError
	if !errors.As(err, &domain) || domain.Code != "approval_required" {
		t.Fatalf("publication did not require approval of the current source revision: %v", err)
	}
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM features`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("rejected publication created features")
	}
}

func TestPublicationMissingGateRollsBack(t *testing.T) {
	s := openTest(t)
	publicationAuthorityFixture(t, s, 1, true)
	ctx := context.Background()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	m, err := programManifestTx(ctx, tx, "publication-program", 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	_, err = s.PublishProgramProposals(ctx, "publication-program", m.SHA, model.Config{PolicyHash: "policy"})
	var domain *model.ProgramError
	if !errors.As(err, &domain) || domain.Code != "evidence_invalid" || domain.Message != "accepted exact contract evaluation and requirement coverage required" {
		t.Fatalf("publication did not reach the missing evaluation gate: %v", err)
	}
	var n int
	for _, table := range []string{"features", "revisions", "program_feature_links"} {
		if err = s.db.QueryRowContext(ctx, `SELECT count(*) FROM `+table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatalf("rejected publication mutated %s", table)
		}
	}
	p, err := programLoad(ctx, s.db, "publication-program")
	if err != nil {
		t.Fatal(err)
	}
	if p.State != model.ProgramActive {
		t.Fatal("failed publication advanced program")
	}
}

func TestPublicationManifestBoundToRevision(t *testing.T) {
	s := openTest(t)
	publicationAuthorityFixture(t, s, 2, true)
	ctx := context.Background()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	m, err := programManifestTx(ctx, tx, "publication-program", 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err = s.PublishProgramProposals(ctx, "publication-program", m.SHA, model.Config{PolicyHash: "policy"}); err != sql.ErrNoRows {
		t.Fatalf("stale revision manifest accepted: %v", err)
	}
	var n int
	for _, table := range []string{"features", "revisions", "program_feature_links"} {
		if err = s.db.QueryRowContext(ctx, `SELECT count(*) FROM `+table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatalf("stale publication mutated %s", table)
		}
	}
	if err = s.db.QueryRowContext(ctx, `SELECT count(*) FROM program_grants WHERE program_id='publication-program'`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("stale publication changed exact revision grants: %d %v", n, err)
	}
	p, err := programLoad(ctx, s.db, "publication-program")
	if err != nil {
		t.Fatal(err)
	}
	if p.CurrentRevision != 2 || p.State != model.ProgramActive {
		t.Fatal("stale publication changed current Program authority")
	}
}

func TestPublicationCollisionRollsBackBatchAndExactReplay(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	existing := feature("publication-collision")
	existing.Revision = "immutable"
	if err := put(ctx, s.db, existing); err != nil {
		t.Fatal(err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	first := feature("publication-first")
	if err = put(ctx, tx, first); err != nil {
		t.Fatal(err)
	}
	link := model.ProgramFeatureLink{ProgramID: "publication-program", Revision: 1, FeatureID: existing.ID, ManifestSHA: "manifest", ContractArtifactID: "contract"}
	if _, err = publicationReplayTx(ctx, tx, existing, link); err == nil {
		t.Fatal("accepted existing feature without publication link")
	}
	if err = tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Get(ctx, first.ID); err != sql.ErrNoRows {
		t.Fatalf("partial batch survived rollback: %v", err)
	}
	raw, _ := json.Marshal(link)
	if _, err = s.db.ExecContext(ctx, `INSERT INTO program_feature_links(program_id,feature_id,data) VALUES(?,?,?)`, link.ProgramID, link.FeatureID, raw); err != nil {
		t.Fatal(err)
	}
	tx, err = s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = publicationReplayTx(ctx, tx, existing, link); err != nil {
		t.Fatal(err)
	}
	different := existing
	different.Revision = "different"
	if _, err = publicationReplayTx(ctx, tx, different, link); err == nil {
		t.Fatal("accepted different immutable revision")
	}
	changed := link
	changed.Revision++
	if _, err = publicationReplayTx(ctx, tx, existing, changed); err == nil {
		t.Fatal("accepted different publication revision")
	}
}

func TestMalformedProposalBatchIsTypedAndDoesNotBlockOrdinaryClaims(t *testing.T) {
	for _, malformed := range []string{"feature-id", "acceptance-id", "missing-dependency", "cycle"} {
		t.Run(malformed, func(t *testing.T) {
			s := openTest(t)
			ctx := context.Background()
			a, b := feature("publication-domain-a"), feature("publication-domain-b")
			switch malformed {
			case "feature-id":
				a.ID = "unsafe feature"
			case "acceptance-id":
				a.Acceptance[0].ID = "unsafe acceptance"
			case "missing-dependency":
				a.DependsOn = []string{"absent"}
			case "cycle":
				a.DependsOn = []string{b.ID}
				b.DependsOn = []string{a.ID}
			}
			tx, err := s.db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			err = validateProgramProposalBatchTx(ctx, tx, map[string]model.Feature{a.ID: a, b.ID: b})
			var domain *model.ProgramError
			if !errors.As(err, &domain) || domain.Code != "invalid_contract" {
				t.Fatalf("expected typed contract failure, got %v", err)
			}
			if err = tx.Rollback(); err != nil {
				t.Fatal(err)
			}
			var count int
			if err = s.db.QueryRowContext(ctx, `SELECT count(*) FROM features`).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if count != 0 {
				t.Fatal("malformed batch persisted partial features")
			}
			f := approved(t, s, "healthy-after-malformed")
			attempt := claim(t, s, f.ID)
			if attempt.FeatureID != f.ID {
				t.Fatal("ordinary claim was affected by malformed program contract")
			}
		})
	}
}

func TestPublicationRejectsDiscoveryCodeDependencies(t *testing.T) {
	for _, reserved := range []bool{false, true} {
		s := openTest(t)
		ctx := context.Background()
		discovery := feature("analysis-with-ordinary-id")
		discovery.State = "reviewed"
		if reserved {
			discovery.ID = "openfactory-discovery-reviewed"
		} else {
			discovery.DiscoveryStepID = "server-analysis-step"
		}
		if err := put(ctx, s.db, discovery); err != nil {
			t.Fatal(err)
		}
		product := feature("product-dependent")
		product.DependsOn = []string{discovery.ID}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		err = publicationDependenciesTx(ctx, tx, map[string]model.Feature{product.ID: product})
		var domain *model.ProgramError
		if !errors.As(err, &domain) || domain.Code != "invalid_contract" {
			t.Fatalf("reviewed discovery dependency accepted (reserved=%v): %v", reserved, err)
		}
		if err = tx.Rollback(); err != nil {
			t.Fatal(err)
		}
		if _, err = s.Get(ctx, product.ID); err != sql.ErrNoRows {
			t.Fatalf("rejected proposal persisted: %v", err)
		}
		ordinary := feature("ordinary-code-dependency")
		ordinary.State = "integrated"
		if err = put(ctx, s.db, ordinary); err != nil {
			t.Fatal(err)
		}
		product.DependsOn = []string{ordinary.ID}
		tx, err = s.db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err = validateProgramProposalBatchTx(ctx, tx, map[string]model.Feature{product.ID: product}); err != nil {
			t.Fatalf("ordinary dependency rejected: %v", err)
		}
		if err = tx.Rollback(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPublicationCurrentManifestRetainsHistoricalQuestionRevision(t *testing.T) {
	s := openTest(t)
	publicationAuthorityFixture(t, s, 2, true)
	ctx := context.Background()
	payload := []byte(`{"questions":[{"id":"durability","text":"What durability is required?","decisive":true,"affected_ids":[],"route":"operator"}]}`)
	blob, err := s.blobs.Put(ctx, payload)
	if err != nil {
		t.Fatal(err)
	}
	a := model.ProgramArtifact{ID: "historical-question", ProgramID: "publication-program", Revision: 1, Kind: model.ArtifactQuestions, SchemaVersion: 1, ContentSHA: blob.SHA}
	raw, _ := json.Marshal(a)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO program_artifacts(id,program_id,revision,kind,content_sha,data) VALUES(?,?,?,?,?,?)`, a.ID, a.ProgramID, a.Revision, a.Kind, a.ContentSHA, raw); err != nil {
		t.Fatal(err)
	}
	ref := model.ProgramInputRef{Kind: a.Kind, ID: a.ID, SHA: a.ContentSHA, Revision: 1}
	m, err := programManifestTx(ctx, tx, a.ProgramID, 2, []model.ProgramInputRef{ref})
	if err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	_, err = s.PublishProgramProposals(ctx, a.ProgramID, m.SHA, model.Config{PolicyHash: "policy"})
	var domain *model.ProgramError
	if !errors.As(err, &domain) || domain.Message != "accepted exact contract evaluation and requirement coverage required" {
		t.Fatalf("current publication rejected explicitly inherited evidence before evaluation gate: %v", err)
	}
	if !publicationArtifactMatches(a.ProgramID, 2, a, ref) {
		t.Fatal("exact inherited revision rejected")
	}
	ref.Revision = 2
	if publicationArtifactMatches(a.ProgramID, 2, a, ref) {
		t.Fatal("history may not be relabeled as current")
	}
	ref.Revision = 1
	if publicationArtifactMatches(a.ProgramID, 0, a, ref) {
		t.Fatal("future evidence accepted")
	}
	stored, _, err := s.ProgramArtifactPayload(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Revision != 1 {
		t.Fatal("publication relabeled historical evidence")
	}
}
