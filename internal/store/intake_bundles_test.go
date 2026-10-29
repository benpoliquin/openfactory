package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"openfactory/internal/intake"
	"openfactory/internal/model"
)

func intakeBundlesManifest(data []byte) model.BundleUploadManifest {
	return model.BundleUploadManifest{Project: "demo", Files: []model.BundleUploadFile{{ID: "private-file", Path: "inputs/private.md", Bytes: int64(len(data)), ContentSHA: intake.SHA256(data)}}}
}
func intakeBundlesFinalized(t *testing.T, s *Store, cfg model.Config, operatorID string, data []byte) model.ProgramBundle {
	t.Helper()
	ctx := context.Background()
	upload, err := s.CreateBundleUpload(ctx, operatorID, intakeBundlesManifest(data), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.PutBundleUploadFile(ctx, operatorID, upload.ID, "private-file", bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	upload, err = s.FinalizeBundleUpload(ctx, operatorID, upload.ID, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if upload.Bundle == nil {
		t.Fatal("finalized upload has no real bundle")
	}
	return *upload.Bundle
}
func TestIntakeBundlesFinalizeRealCASWithoutImplicitConsent(t *testing.T) {
	s, cfg, op, _ := intakeReviewsFixture(t)
	ctx := context.Background()
	data := []byte("private UTF-8 source: café ☕\n")
	bundle := intakeBundlesFinalized(t, s, cfg, op.ID, data)
	loaded, err := s.blobs.LoadBundle(ctx, bundle.ManifestSHA)
	if err != nil {
		t.Fatal(err)
	}
	if programHash(loaded) != programHash(bundle) || len(loaded.Files) != 1 {
		t.Fatal("upload did not produce existing canonical ProgramBundle")
	}
	raw, err := s.blobs.Read(ctx, loaded.Files[0].ContentSHA, loaded.Files[0].Bytes)
	if err != nil || !bytes.Equal(raw, data) {
		t.Fatalf("actual uploaded bytes differ: %q %v", raw, err)
	}
	var requests, commands, grants int
	if err = s.db.QueryRow(`SELECT count(*) FROM intake_requests`).Scan(&requests); err != nil {
		t.Fatal(err)
	}
	if err = s.db.QueryRow(`SELECT count(*) FROM intake_commands`).Scan(&commands); err != nil {
		t.Fatal(err)
	}
	if err = s.db.QueryRow(`SELECT count(*) FROM program_grants`).Scan(&grants); err != nil {
		t.Fatal(err)
	}
	if requests != 0 || commands != 0 || grants != 0 {
		t.Fatal("private upload implicitly granted model disclosure or domain authority")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	authorized, err := s.intakeAuthorizeBundleTx(ctx, tx, op.ID, "demo", bundle.ManifestSHA)
	tx.Rollback()
	if err != nil || authorized.ManifestSHA != bundle.ManifestSHA {
		t.Fatalf("finalized owner bundle not selectable: %v", err)
	}
}
func TestIntakeBundlesDurablePartialUploadAndFinalize(t *testing.T) {
	s, cfg, op, _ := intakeReviewsFixture(t)
	ctx := context.Background()
	data := []byte("durable partial upload")
	upload, err := s.CreateBundleUpload(ctx, op.ID, intakeBundlesManifest(data), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.FinalizeBundleUpload(ctx, op.ID, upload.ID, cfg); err == nil {
		t.Fatal("incomplete upload finalized")
	}
	if _, err = s.PutBundleUploadFile(ctx, op.ID, upload.ID, "private-file", bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	var databasePath string
	rows, err := s.db.Query(`PRAGMA database_list`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var seq int
		var name, p string
		if err = rows.Scan(&seq, &name, &p); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		if name == "main" {
			databasePath = p
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		t.Fatal(err)
	}
	if databasePath == "" || !filepath.IsAbs(databasePath) {
		t.Fatal("fixture is not a real durable database")
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reopened.Close() })
	resumed, err := reopened.BundleUpload(ctx, op.ID, upload.ID)
	if err != nil || resumed.State != "uploading" {
		t.Fatalf("partial upload did not survive restart: %+v %v", resumed, err)
	}
	finalized, err := reopened.FinalizeBundleUpload(ctx, op.ID, upload.ID, cfg)
	if err != nil {
		t.Fatal(err)
	}
	again, err := reopened.FinalizeBundleUpload(ctx, op.ID, upload.ID, cfg)
	if err != nil || again.Bundle == nil || finalized.Bundle == nil || again.Bundle.ManifestSHA != finalized.Bundle.ManifestSHA {
		t.Fatalf("finalization replay changed bundle: %v", err)
	}
}
func TestIntakeBundlesRejectUnsafeManifestAndLimits(t *testing.T) {
	s, cfg, op, _ := intakeReviewsFixture(t)
	ctx := context.Background()
	data := []byte("source")
	for _, p := range []string{"../outside.md", "/absolute.md", "a/../escape.md", "a//duplicate.md", "a\\escape.md", "C:/absolute.md", "archive.zip", "fake.pdf", "file.md\x00.txt", "."} {
		manifest := intakeBundlesManifest(data)
		manifest.Files[0].Path = p
		if _, err := s.CreateBundleUpload(ctx, op.ID, manifest, cfg); err == nil {
			t.Errorf("unsafe or unsupported raw file accepted: %q", p)
		}
	}
	for _, duplicateID := range []bool{false, true} {
		manifest := intakeBundlesManifest(data)
		other := manifest.Files[0]
		if duplicateID {
			other.Path = "second.md"
		} else {
			other.ID = "second-file"
		}
		manifest.Files = append(manifest.Files, other)
		if _, err := s.CreateBundleUpload(ctx, op.ID, manifest, cfg); err == nil {
			t.Fatal("duplicate file ID or path accepted")
		}
	}
	manifest := intakeBundlesManifest(data)
	manifest.Files[0].Bytes = cfg.Discovery.BundleMaxFileBytes + 1
	_, err := s.CreateBundleUpload(ctx, op.ID, manifest, cfg)
	intakeReviewsCode(t, err, "bundle_limit")
	manifest = intakeBundlesManifest(data)
	manifest.Files[0].ContentSHA = strings.Repeat("A", 64)
	if _, err = s.CreateBundleUpload(ctx, op.ID, manifest, cfg); err == nil {
		t.Fatal("noncanonical content hash accepted")
	}
}

type intakeBundlesCountingReader struct{ count int64 }

func (r *intakeBundlesCountingReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	r.count += int64(len(p))
	return len(p), nil
}

type intakeBundlesByteReader struct{ reader io.Reader }

func (r intakeBundlesByteReader) Read(p []byte) (int, error) {
	if len(p) > 1 {
		p = p[:1]
	}
	return r.reader.Read(p)
}
func TestIntakeBundlesRejectMismatchedUndeclaredAndOversizedContent(t *testing.T) {
	s, cfg, op, _ := intakeReviewsFixture(t)
	ctx := context.Background()
	data := []byte("declared")
	upload, err := s.CreateBundleUpload(ctx, op.ID, intakeBundlesManifest(data), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.PutBundleUploadFile(ctx, op.ID, upload.ID, "undeclared", bytes.NewReader(data)); err == nil {
		t.Fatal("undeclared file accepted")
	}
	if _, err = s.PutBundleUploadFile(ctx, op.ID, upload.ID, "private-file", bytes.NewReader([]byte("modified"))); err == nil {
		t.Fatal("hash-mismatching content accepted")
	}
	counter := &intakeBundlesCountingReader{}
	if _, err = s.PutBundleUploadFile(ctx, op.ID, upload.ID, "private-file", counter); err == nil {
		t.Fatal("oversized stream accepted")
	}
	if counter.count > int64(len(data)+1) {
		t.Fatal("upload consumed beyond declared bytes plus one")
	}
	if _, err = s.MarkBundleUploadFile(ctx, op.ID, upload.ID, "private-file", model.BlobRef{SHA: intake.SHA256(data), Bytes: int64(len(data))}); err == nil {
		t.Fatal("unmaterialized client reference accepted as CAS evidence")
	}
	if _, err = s.PutBundleUploadFile(ctx, op.ID, upload.ID, "private-file", bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	stricter := cfg
	stricter.Discovery.BundleMaxFileBytes = int64(len(data) - 1)
	if _, err = s.FinalizeBundleUpload(ctx, op.ID, upload.ID, stricter); err == nil {
		t.Fatal("current stricter policy ignored by finalization")
	}
	if _, err = s.FinalizeBundleUpload(ctx, op.ID, upload.ID, cfg); err != nil {
		t.Fatal(err)
	}
}
func TestIntakeBundlesStreamingUTF8AndOwnershipRevocation(t *testing.T) {
	s, cfg, op, _ := intakeReviewsFixture(t)
	ctx := context.Background()
	data := []byte("é☕ private")
	upload, err := s.CreateBundleUpload(ctx, op.ID, intakeBundlesManifest(data), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.PutBundleUploadFile(ctx, op.ID, upload.ID, "private-file", intakeBundlesByteReader{bytes.NewReader(data)}); err != nil {
		t.Fatalf("split UTF-8 rune rejected: %v", err)
	}
	invalid := []byte{0xe2, 0x82}
	bad, err := s.CreateBundleUpload(ctx, op.ID, intakeBundlesManifest(invalid), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.PutBundleUploadFile(ctx, op.ID, bad.ID, "private-file", intakeBundlesByteReader{bytes.NewReader(invalid)}); err == nil {
		t.Fatal("incomplete UTF-8 accepted")
	}
	other := model.Operator{ID: "bundle-other", Label: "Other", Enabled: true, ProjectScope: []string{"demo"}, UserHandle: []byte("bundle-other")}
	if err = s.CreateOperator(ctx, other); err != nil {
		t.Fatal(err)
	}
	if _, err = s.BundleUpload(ctx, other.ID, upload.ID); !errors.Is(err, ErrProjectForbidden) {
		t.Fatalf("other owner read upload: %v", err)
	}
	if _, err = s.PutBundleUploadFile(ctx, other.ID, upload.ID, "private-file", bytes.NewReader(data)); !errors.Is(err, ErrProjectForbidden) {
		t.Fatalf("other owner replaced upload: %v", err)
	}
	if _, err = s.FinalizeBundleUpload(ctx, other.ID, upload.ID, cfg); !errors.Is(err, ErrProjectForbidden) {
		t.Fatalf("other owner finalized upload: %v", err)
	}
	if err = s.RevokeOperator(ctx, op.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.FinalizeBundleUpload(ctx, op.ID, upload.ID, cfg); !errors.Is(err, ErrAuthRequired) {
		t.Fatalf("revoked actor finalized upload: %v", err)
	}
}
func TestIntakeBundlesCLISelectionRequiresOwnedProgramAssociation(t *testing.T) {
	s, _, op, _ := intakeReviewsFixture(t)
	ctx := context.Background()
	data := []byte("trusted CLI material")
	ref, err := s.blobs.Put(ctx, data)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(struct {
		Files []model.ProgramBundleFile `json:"files"`
	}{[]model.ProgramBundleFile{{Path: "inputs/private.md", ContentSHA: ref.SHA, Bytes: ref.Bytes}}})
	manifest, err := s.blobs.Put(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := s.blobs.LoadBundle(ctx, manifest.SHA)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.intakeAuthorizeBundleTx(ctx, tx, op.ID, "demo", bundle.ManifestSHA)
	tx.Rollback()
	if !errors.Is(err, ErrProjectForbidden) {
		t.Fatalf("arbitrary CAS hash granted access: %v", err)
	}
	spec := model.ProgramSpec{ID: "cli-bundle-owner", Project: "demo", Title: "CLI selection", Objective: "Use staged explicit source", Disclosure: model.ProgramDisclosure{ModelProviderIDs: []string{"provider"}}}
	if _, err = s.ImportProgram(ctx, spec, bundle); err != nil {
		t.Fatal(err)
	}
	tx, err = s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	selected, err := s.intakeAuthorizeBundleTx(ctx, tx, op.ID, "demo", bundle.ManifestSHA)
	tx.Rollback()
	if err != nil || selected.ManifestSHA != bundle.ManifestSHA {
		t.Fatalf("CLI program-linked bundle could not be selected: %v", err)
	}
}
