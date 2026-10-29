package store

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"path"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"openfactory/internal/artifacts"
	"openfactory/internal/model"
)

const intakeBundleSchema = `
CREATE TABLE IF NOT EXISTS intake_bundle_uploads(id TEXT PRIMARY KEY,operator_id TEXT NOT NULL REFERENCES intake_operators(id),project TEXT NOT NULL,data BLOB NOT NULL);
CREATE INDEX IF NOT EXISTS intake_bundle_uploads_owner ON intake_bundle_uploads(operator_id,project);
CREATE TABLE IF NOT EXISTS intake_bundle_upload_files(upload_id TEXT NOT NULL REFERENCES intake_bundle_uploads(id),file_id TEXT NOT NULL,content_sha TEXT NOT NULL,bytes INTEGER NOT NULL,PRIMARY KEY(upload_id,file_id));
`

type intakeBundleRecord struct {
	Upload model.BundleUpload `json:"upload"`
	Limits model.BundleLimits `json:"limits"`
}

func intakeBundleLimits(cfg model.Config) model.BundleLimits {
	return model.BundleLimits{MaxFiles: cfg.Discovery.BundleMaxFiles, MaxFileBytes: cfg.Discovery.BundleMaxFileBytes, MaxTotalBytes: cfg.Discovery.BundleMaxTotalBytes}
}
func intakeBundleSHAValid(s string) bool {
	b, err := hex.DecodeString(s)
	return err == nil && len(b) == 32 && strings.ToLower(s) == s
}
func intakeBundlePathValid(p string) bool {
	if p == "" || !utf8.ValidString(p) || len(p) > 4096 || p == "." || path.IsAbs(p) || path.Clean(p) != p || strings.ContainsAny(p, "\\:\x00") {
		return false
	}
	for _, r := range p {
		if unicode.IsControl(r) {
			return false
		}
	}
	for _, part := range strings.Split(p, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	switch strings.ToLower(path.Ext(p)) {
	case ".txt", ".md", ".markdown", ".json", ".csv":
		return true
	}
	return false
}
func intakeValidateBundleManifest(m model.BundleUploadManifest, l model.BundleLimits) error {
	if m.Project == "" || l.MaxFiles <= 0 || l.MaxFileBytes <= 0 || l.MaxTotalBytes <= 0 {
		return programError("invalid_contract", "bundle policy and project are required")
	}
	if len(m.Files) == 0 || len(m.Files) > l.MaxFiles {
		return programError("bundle_limit", "bundle file count exceeds policy")
	}
	ids, paths := map[string]bool{}, map[string]bool{}
	var total int64
	for _, f := range m.Files {
		if f.ID == "" || len(f.ID) > 128 || strings.ContainsAny(f.ID, "/\\\x00") || ids[f.ID] || paths[f.Path] || !intakeBundlePathValid(f.Path) || !intakeBundleSHAValid(f.ContentSHA) {
			return programError("invalid_contract", "invalid, duplicate or unsupported bundle member")
		}
		if f.Bytes < 0 || f.Bytes == 1<<63-1 || f.Bytes > l.MaxFileBytes || f.Bytes > l.MaxTotalBytes-total {
			return programError("bundle_limit", "bundle byte limit exceeded")
		}
		ids[f.ID], paths[f.Path] = true, true
		total += f.Bytes
	}
	return nil
}
func intakeBundleLoadTx(ctx context.Context, tx *sql.Tx, operatorID, id string) (intakeBundleRecord, error) {
	var r intakeBundleRecord
	var owner, project string
	var raw []byte
	if err := tx.QueryRowContext(ctx, `SELECT operator_id,project,data FROM intake_bundle_uploads WHERE id=?`, id).Scan(&owner, &project, &raw); err != nil {
		return r, programReadError(err)
	}
	if _, err := intakeAuthLiveOperator(ctx, tx, operatorID, project); err != nil {
		return r, err
	}
	if owner != operatorID {
		return r, ErrProjectForbidden
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return r, err
	}
	if r.Upload.ID != id || r.Upload.OperatorID != owner || r.Upload.Manifest.Project != project {
		return r, programError("evidence_invalid", "bundle upload identity mismatch")
	}
	if err := intakeValidateBundleManifest(r.Upload.Manifest, r.Limits); err != nil {
		return r, err
	}
	return r, nil
}
func intakeBundleSaveTx(ctx context.Context, tx *sql.Tx, r intakeBundleRecord) error {
	raw, err := json.Marshal(r)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE intake_bundle_uploads SET data=? WHERE id=?`, raw, r.Upload.ID)
	return err
}
func (s *Store) CreateBundleUpload(ctx context.Context, operatorID string, manifest model.BundleUploadManifest, cfg model.Config) (model.BundleUpload, error) {
	var out model.BundleUpload
	if !cfg.Intake.Enabled {
		return out, programError("invalid_contract", "intake is disabled")
	}
	if p, ok := cfg.Projects[manifest.Project]; !ok || !p.Trusted {
		return out, ErrProjectForbidden
	}
	limits := intakeBundleLimits(cfg)
	if err := intakeValidateBundleManifest(manifest, limits); err != nil {
		return out, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	if _, err = intakeAuthLiveOperator(ctx, tx, operatorID, manifest.Project); err != nil {
		return out, err
	}
	out = model.BundleUpload{ID: ID(), OperatorID: operatorID, Manifest: manifest, State: "uploading"}
	raw, err := json.Marshal(intakeBundleRecord{Upload: out, Limits: limits})
	if err != nil {
		return out, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO intake_bundle_uploads(id,operator_id,project,data) VALUES(?,?,?,?)`, out.ID, operatorID, manifest.Project, raw); err != nil {
		return out, err
	}
	return out, tx.Commit()
}
func (s *Store) BundleUpload(ctx context.Context, operatorID, id string) (model.BundleUpload, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return model.BundleUpload{}, err
	}
	defer tx.Rollback()
	r, err := intakeBundleLoadTx(ctx, tx, operatorID, id)
	if err != nil {
		return r.Upload, err
	}
	return r.Upload, tx.Commit()
}
func intakeBundleFile(r intakeBundleRecord, id string) (model.BundleUploadFile, error) {
	for _, f := range r.Upload.Manifest.Files {
		if f.ID == id {
			return f, nil
		}
	}
	return model.BundleUploadFile{}, programError("invalid_contract", "file is not declared in this upload")
}
func (s *Store) intakeMarkBundleFileTx(ctx context.Context, tx *sql.Tx, r intakeBundleRecord, fileID string, ref model.BlobRef) (model.BundleUpload, error) {
	f, err := intakeBundleFile(r, fileID)
	if err != nil {
		return r.Upload, err
	}
	if ref.SHA != f.ContentSHA || ref.Bytes != f.Bytes {
		return r.Upload, programError("evidence_invalid", "uploaded content differs from declared file")
	}
	// A ref is not evidence that its bytes exist. Verify the real CAS object and
	// exact byte count, including on replay and after controller restart.
	if err = s.intakeVerifyBundleFile(ctx, model.ProgramBundleFile{Path: f.Path, ContentSHA: ref.SHA, Bytes: ref.Bytes}, true); err != nil {
		return r.Upload, err
	}
	if r.Upload.State != "uploading" && r.Upload.State != "finalized" {
		return r.Upload, programError("stale_subject", "bundle upload is not writable")
	}
	var sha string
	var size int64
	err = tx.QueryRowContext(ctx, `SELECT content_sha,bytes FROM intake_bundle_upload_files WHERE upload_id=? AND file_id=?`, r.Upload.ID, fileID).Scan(&sha, &size)
	if err == nil {
		if sha != ref.SHA || size != ref.Bytes {
			return r.Upload, programError("evidence_invalid", "recorded file differs")
		}
		return r.Upload, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return r.Upload, err
	}
	if r.Upload.State != "uploading" {
		return r.Upload, programError("stale_subject", "bundle is already finalized")
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO intake_bundle_upload_files(upload_id,file_id,content_sha,bytes) VALUES(?,?,?,?)`, r.Upload.ID, fileID, ref.SHA, ref.Bytes)
	return r.Upload, err
}
func (s *Store) MarkBundleUploadFile(ctx context.Context, operatorID, uploadID, fileID string, ref model.BlobRef) (model.BundleUpload, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.BundleUpload{}, err
	}
	defer tx.Rollback()
	r, err := intakeBundleLoadTx(ctx, tx, operatorID, uploadID)
	if err != nil {
		return r.Upload, err
	}
	out, err := s.intakeMarkBundleFileTx(ctx, tx, r, fileID, ref)
	if err != nil {
		return out, err
	}
	return out, tx.Commit()
}
func (s *Store) PutBundleUploadFile(ctx context.Context, operatorID, uploadID, fileID string, reader io.Reader) (model.BundleUpload, error) {
	// Do not hold the single database connection while waiting for network bytes.
	upload, err := s.BundleUpload(ctx, operatorID, uploadID)
	if err != nil {
		return upload, err
	}
	f, err := intakeBundleFile(intakeBundleRecord{Upload: upload}, fileID)
	if err != nil {
		return upload, err
	}
	if upload.State != "uploading" && upload.State != "finalized" {
		return upload, programError("stale_subject", "bundle upload is not writable")
	}
	if reader == nil {
		return upload, programError("invalid_request", "file body is required")
	}
	validator := &intakeUTF8Writer{}
	ref, err := s.blobs.PutReader(ctx, io.TeeReader(reader, validator), f.ContentSHA, f.Bytes, f.Bytes)
	if err != nil {
		if errors.Is(err, artifacts.ErrContentMismatch) {
			return upload, programError("evidence_invalid", "uploaded file size or hash mismatch")
		}
		if errors.Is(err, artifacts.ErrSizeLimit) {
			return upload, programError("bundle_limit", "uploaded file exceeds byte limit")
		}
		return upload, err
	}
	if err = validator.finish(); err != nil {
		return upload, err
	}
	// Recheck live actor/ownership after streaming; revocation during upload must
	// not publish a file or finalized bundle into the authenticated namespace.
	return s.MarkBundleUploadFile(ctx, operatorID, uploadID, fileID, ref)
}
func (s *Store) FinalizeBundleUpload(ctx context.Context, operatorID, id string, cfg model.Config) (model.BundleUpload, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.BundleUpload{}, err
	}
	defer tx.Rollback()
	r, err := intakeBundleLoadTx(ctx, tx, operatorID, id)
	if err != nil {
		return r.Upload, err
	}
	if !cfg.Intake.Enabled {
		return r.Upload, programError("invalid_contract", "intake is disabled")
	}
	if p, ok := cfg.Projects[r.Upload.Manifest.Project]; !ok || !p.Trusted {
		return r.Upload, ErrProjectForbidden
	}
	if err = intakeValidateBundleManifest(r.Upload.Manifest, intakeBundleLimits(cfg)); err != nil {
		return r.Upload, err
	}
	if r.Upload.State == "finalized" {
		if r.Upload.Bundle == nil {
			return r.Upload, programError("evidence_invalid", "finalized bundle is missing")
		}
		bundle, e := s.blobs.LoadBundle(ctx, r.Upload.Bundle.ManifestSHA)
		if e != nil {
			return r.Upload, e
		}
		if programHash(bundle) != programHash(*r.Upload.Bundle) {
			return r.Upload, programError("evidence_invalid", "finalized bundle identity changed")
		}
		for _, f := range bundle.Files {
			if e = s.intakeVerifyBundleFile(ctx, f, true); e != nil {
				return r.Upload, e
			}
		}
		return r.Upload, tx.Commit()
	}
	if r.Upload.State != "uploading" {
		return r.Upload, programError("stale_subject", "bundle upload is not writable")
	}
	files := make([]model.ProgramBundleFile, 0, len(r.Upload.Manifest.Files))
	for _, f := range r.Upload.Manifest.Files {
		var sha string
		var size int64
		if err = tx.QueryRowContext(ctx, `SELECT content_sha,bytes FROM intake_bundle_upload_files WHERE upload_id=? AND file_id=?`, id, f.ID).Scan(&sha, &size); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return r.Upload, programError("invalid_contract", "bundle upload is incomplete")
			}
			return r.Upload, err
		}
		if sha != f.ContentSHA || size != f.Bytes {
			return r.Upload, programError("evidence_invalid", "bundle member identity changed")
		}
		if e := s.intakeVerifyBundleFile(ctx, model.ProgramBundleFile{Path: f.Path, ContentSHA: sha, Bytes: size}, true); e != nil {
			return r.Upload, e
		}
		files = append(files, model.ProgramBundleFile{Path: f.Path, ContentSHA: sha, Bytes: size})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	// This is the existing ProgramBundle CAS manifest format, not an archive or
	// an intake-only surrogate. LoadBundle independently verifies the format.
	raw, err := json.Marshal(struct {
		Files []model.ProgramBundleFile `json:"files"`
	}{files})
	if err != nil {
		return r.Upload, err
	}
	ref, err := s.blobs.Put(ctx, raw)
	if err != nil {
		return r.Upload, err
	}
	bundle, err := s.blobs.LoadBundle(ctx, ref.SHA)
	if err != nil {
		return r.Upload, err
	}
	r.Upload.State = "finalized"
	r.Upload.Bundle = &bundle
	if err = intakeBundleSaveTx(ctx, tx, r); err != nil {
		return r.Upload, err
	}
	return r.Upload, tx.Commit()
}

// An upload grants ownership only. The caller must separately validate the
// exact disclosure and providers before exposing any private material to models.
func (s *Store) intakeAuthorizeBundleTx(ctx context.Context, tx *sql.Tx, operatorID, project, sha string) (model.ProgramBundle, error) {
	if _, err := intakeAuthLiveOperator(ctx, tx, operatorID, project); err != nil {
		return model.ProgramBundle{}, err
	}
	if !intakeBundleSHAValid(sha) {
		return model.ProgramBundle{}, programError("invalid_contract", "invalid bundle manifest identity")
	}
	var id string
	err := tx.QueryRowContext(ctx, `SELECT id FROM intake_bundle_uploads WHERE operator_id=? AND project=? AND json_extract(data,'$.upload.state')='finalized' AND json_extract(data,'$.upload.bundle.manifest_sha')=? ORDER BY id LIMIT 1`, operatorID, project, sha).Scan(&id)
	if err == nil {
		r, e := intakeBundleLoadTx(ctx, tx, operatorID, id)
		if e != nil {
			return model.ProgramBundle{}, e
		}
		bundle, e := s.blobs.LoadBundle(ctx, sha)
		if e != nil {
			return bundle, e
		}
		if r.Upload.Bundle == nil || programHash(bundle) != programHash(*r.Upload.Bundle) {
			return bundle, programError("evidence_invalid", "bundle manifest differs")
		}
		return bundle, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return model.ProgramBundle{}, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT p.id FROM program_bundles b JOIN programs p ON p.id=b.program_id WHERE b.manifest_sha=? AND json_extract(p.data,'$.project')=? ORDER BY p.id`, sha, project)
	if err != nil {
		return model.ProgramBundle{}, err
	}
	ids := []string{}
	for rows.Next() {
		var p string
		if err = rows.Scan(&p); err != nil {
			rows.Close()
			return model.ProgramBundle{}, err
		}
		ids = append(ids, p)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return model.ProgramBundle{}, err
	}
	for _, programID := range ids {
		if err = intakeSubjectOwnerTx(ctx, tx, operatorID, "program", programID); err == nil {
			return s.blobs.LoadBundle(ctx, sha)
		}
		if !errors.Is(err, ErrProjectForbidden) {
			return model.ProgramBundle{}, err
		}
	}
	return model.ProgramBundle{}, ErrProjectForbidden
}

// The UTF-8 validator retains only a single incomplete rune across streaming
// chunks. File extension denotes a supported raw text format, never extraction.
type intakeUTF8Writer struct {
	pending [utf8.UTFMax]byte
	count   int
}

func (v *intakeUTF8Writer) Write(data []byte) (int, error) {
	size := len(data)
	invalid := func() (int, error) {
		return 0, programError("invalid_contract", "bundle files must contain valid UTF-8 text")
	}
	if v.count > 0 {
		for len(data) > 0 && !utf8.FullRune(v.pending[:v.count]) {
			v.pending[v.count] = data[0]
			v.count++
			data = data[1:]
		}
		if !utf8.FullRune(v.pending[:v.count]) {
			return size, nil
		}
		r, n := utf8.DecodeRune(v.pending[:v.count])
		if r == utf8.RuneError && n == 1 {
			return invalid()
		}
		v.count = 0
	}
	for len(data) > 0 {
		if data[0] < utf8.RuneSelf {
			data = data[1:]
			continue
		}
		if !utf8.FullRune(data) {
			v.count = copy(v.pending[:], data)
			return size, nil
		}
		r, n := utf8.DecodeRune(data)
		if r == utf8.RuneError && n == 1 {
			return invalid()
		}
		data = data[n:]
	}
	return size, nil
}
func (v *intakeUTF8Writer) finish() error {
	if v.count != 0 {
		return programError("invalid_contract", "bundle text ends in an incomplete UTF-8 rune")
	}
	return nil
}
func (s *Store) intakeVerifyBundleFile(ctx context.Context, f model.ProgramBundleFile, text bool) error {
	var writer io.Writer = io.Discard
	var validator *intakeUTF8Writer
	if text {
		validator = &intakeUTF8Writer{}
		writer = validator
	}
	_, err := s.blobs.ReadTo(ctx, f.ContentSHA, f.Bytes, f.Bytes, writer)
	if err != nil {
		return err
	}
	if validator != nil {
		return validator.finish()
	}
	return nil
}
