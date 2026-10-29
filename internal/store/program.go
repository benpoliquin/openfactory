package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"openfactory/internal/model"
)

const programSchema = `
CREATE TABLE IF NOT EXISTS programs(id TEXT PRIMARY KEY,data BLOB NOT NULL);
CREATE TABLE IF NOT EXISTS program_revisions(program_id TEXT NOT NULL REFERENCES programs(id),number INTEGER NOT NULL,revision_hash TEXT UNIQUE NOT NULL,data BLOB NOT NULL,PRIMARY KEY(program_id,number));
CREATE TABLE IF NOT EXISTS program_budgets(program_id TEXT PRIMARY KEY REFERENCES programs(id),data BLOB NOT NULL);
CREATE TABLE IF NOT EXISTS program_grants(program_id TEXT NOT NULL,revision_hash TEXT NOT NULL,subject_hash TEXT NOT NULL,policy_hash TEXT NOT NULL,data BLOB NOT NULL,PRIMARY KEY(program_id,revision_hash,subject_hash,policy_hash));
CREATE TABLE IF NOT EXISTS program_steps(id TEXT PRIMARY KEY,program_id TEXT NOT NULL REFERENCES programs(id),revision INTEGER NOT NULL,feature_id TEXT UNIQUE NOT NULL,data BLOB NOT NULL);
CREATE TABLE IF NOT EXISTS program_artifacts(id TEXT PRIMARY KEY,program_id TEXT NOT NULL REFERENCES programs(id),revision INTEGER NOT NULL,kind TEXT NOT NULL,content_sha TEXT NOT NULL,data BLOB NOT NULL);
CREATE TABLE IF NOT EXISTS program_manifests(sha TEXT PRIMARY KEY,program_id TEXT NOT NULL,revision INTEGER NOT NULL,data BLOB NOT NULL);
CREATE TABLE IF NOT EXISTS program_answers(id TEXT PRIMARY KEY,program_id TEXT NOT NULL,data BLOB NOT NULL);
CREATE TABLE IF NOT EXISTS program_invalidations(id TEXT PRIMARY KEY,program_id TEXT NOT NULL,subject TEXT NOT NULL,data BLOB NOT NULL);
CREATE TABLE IF NOT EXISTS program_experiments(run_id TEXT PRIMARY KEY,program_id TEXT NOT NULL,attempt_id TEXT NOT NULL,request_sha TEXT NOT NULL,data BLOB NOT NULL);
CREATE TABLE IF NOT EXISTS program_feature_links(program_id TEXT NOT NULL,feature_id TEXT PRIMARY KEY,data BLOB NOT NULL);
CREATE TABLE IF NOT EXISTS program_events(seq INTEGER PRIMARY KEY AUTOINCREMENT,id TEXT UNIQUE NOT NULL,program_id TEXT NOT NULL,kind TEXT NOT NULL,data BLOB NOT NULL);
CREATE TABLE IF NOT EXISTS program_policies(program_id TEXT NOT NULL,revision INTEGER NOT NULL,policy_hash TEXT NOT NULL,data BLOB NOT NULL,PRIMARY KEY(program_id,revision,policy_hash));
CREATE TABLE IF NOT EXISTS program_bundles(program_id TEXT NOT NULL,manifest_sha TEXT NOT NULL,revision INTEGER NOT NULL,PRIMARY KEY(program_id,manifest_sha));
`

type programQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func programError(code, message string) error {
	return &model.ProgramError{Code: code, Message: message}
}
func programHash(v any) string {
	b, _ := json.Marshal(v)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func programLoad(ctx context.Context, q programQuerier, id string) (model.Program, error) {
	var p model.Program
	var b []byte
	err := q.QueryRowContext(ctx, `SELECT data FROM programs WHERE id=?`, id).Scan(&b)
	if err == nil {
		err = json.Unmarshal(b, &p)
	}
	return p, err
}
func programRevision(ctx context.Context, q programQuerier, id string, n int) (model.ProgramRevision, error) {
	var r model.ProgramRevision
	var b []byte
	err := q.QueryRowContext(ctx, `SELECT data FROM program_revisions WHERE program_id=? AND number=?`, id, n).Scan(&b)
	if err == nil {
		err = json.Unmarshal(b, &r)
	}
	return r, err
}
func programPut(ctx context.Context, tx *sql.Tx, p model.Program) error {
	p.UpdatedAt = time.Now().UTC()
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO programs(id,data) VALUES(?,?) ON CONFLICT(id) DO UPDATE SET data=excluded.data`, p.ID, b)
	return err
}
func programEvent(ctx context.Context, tx *sql.Tx, id, kind, detail string) error {
	e := model.ProgramEvent{ID: ID(), ProgramID: id, Kind: kind, Detail: detail, CreatedAt: time.Now().UTC()}
	b, _ := json.Marshal(e)
	_, err := tx.ExecContext(ctx, `INSERT INTO program_events(id,program_id,kind,data) VALUES(?,?,?,?)`, e.ID, id, kind, b)
	return err
}
func getProgramBudgetTx(ctx context.Context, tx *sql.Tx, id string) (model.ProgramBudget, error) {
	var b model.ProgramBudget
	var raw []byte
	err := tx.QueryRowContext(ctx, `SELECT data FROM program_budgets WHERE program_id=?`, id).Scan(&raw)
	if err == nil {
		err = json.Unmarshal(raw, &b)
	}
	return b, err
}
func putProgramBudgetTx(ctx context.Context, tx *sql.Tx, id string, b model.ProgramBudget) error {
	raw, _ := json.Marshal(b)
	_, err := tx.ExecContext(ctx, `INSERT INTO program_budgets(program_id,data) VALUES(?,?) ON CONFLICT(program_id) DO UPDATE SET data=excluded.data`, id, raw)
	return err
}
func (s *Store) programAuthorityTx(ctx context.Context, tx *sql.Tx, id string, n int, policy string) (model.Program, model.ProgramRevision, error) {
	p, err := programLoad(ctx, tx, id)
	if err != nil {
		return p, model.ProgramRevision{}, err
	}
	r, err := programRevision(ctx, tx, id, p.CurrentRevision)
	if err != nil {
		return p, r, err
	}
	if n != p.CurrentRevision {
		return p, r, programError("stale_revision", "program revision changed")
	}
	if p.State == model.ProgramCancelled || p.State == model.ProgramDraft || p.State == model.ProgramBlocked {
		return p, r, programError("approval_required", "program is not authorized")
	}
	var count int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM program_grants WHERE program_id=? AND revision_hash=? AND subject_hash='' AND policy_hash=?`, id, r.RevisionHash, policy).Scan(&count); err != nil {
		return p, r, err
	}
	if count != 1 {
		return p, r, programError("approval_required", "exact revision and policy approval required")
	}
	// A persisted grant cannot substitute for disclosure of the actual immutable bundle.
	if err = s.programDisclosureTx(ctx, tx, r); err != nil {
		return p, r, err
	}
	return p, r, nil
}

// programRevisionDisclosure also checks predicted revisions against an already
// verified bundle during review, before that revision or its grant is persisted.
func programRevisionDisclosure(revision model.ProgramRevision, bundle model.ProgramBundle) error {
	if bundle.ManifestSHA != revision.SeedManifestSHA {
		return fmt.Errorf("program seed bundle identity mismatch")
	}
	if err := model.ValidateProgramSpec(revision.Spec); err != nil {
		return programError("disclosure_required", err.Error())
	}
	// Every selected member becomes model input, even without an explicit seed.
	if len(bundle.Files) > 0 && !revision.Spec.Disclosure.PrivateMaterialToModels {
		return programError("disclosure_required", "private bundle material requires explicit model disclosure")
	}
	return nil
}

func (s *Store) programDisclosureTx(ctx context.Context, tx *sql.Tx, revision model.ProgramRevision) error {
	var bundle model.ProgramBundle
	if revision.SeedManifestSHA != "" {
		var err error
		bundle, err = s.blobs.LoadBundle(ctx, revision.SeedManifestSHA)
		if err != nil {
			return err
		}
		ref := model.ProgramInputRef{Kind: "bundle", ID: revision.SeedManifestSHA, SHA: revision.SeedManifestSHA, Revision: revision.Number}
		if err = programValidateRefTx(ctx, tx, revision.ProgramID, revision.Number, ref); err != nil {
			return err
		}
	}
	return programRevisionDisclosure(revision, bundle)
}
func programInvalidTx(ctx context.Context, q programQuerier, id, subject string) error {
	var n int
	err := q.QueryRowContext(ctx, `SELECT count(*) FROM program_invalidations WHERE program_id=? AND subject IN (?, '*')`, id, subject).Scan(&n)
	if err != nil {
		return err
	}
	if n > 0 {
		return programError("evidence_invalid", "subject has been invalidated")
	}
	return nil
}

// ImportProgram persists an unapproved proposal, including selected private seeds.
// Model and research disclosure is required separately when execution is approved.
func (s *Store) ImportProgram(ctx context.Context, spec model.ProgramSpec, bundle model.ProgramBundle) (model.ProgramRevision, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.ProgramRevision{}, err
	}
	defer tx.Rollback()
	out, err := s.importProgramTx(ctx, tx, spec, bundle)
	if err != nil {
		return out, err
	}
	return out, tx.Commit()
}

func (s *Store) importProgramTx(ctx context.Context, tx *sql.Tx, spec model.ProgramSpec, bundle model.ProgramBundle) (model.ProgramRevision, error) {
	var out model.ProgramRevision
	if err := model.ValidateProgramDraftSpec(spec); err != nil {
		return out, programError("invalid_contract", err.Error())
	}
	if bundle.ManifestSHA != "" {
		loaded, err := s.blobs.LoadBundle(ctx, bundle.ManifestSHA)
		if err != nil {
			return out, err
		}
		if programHash(loaded) != programHash(bundle) {
			return out, programError("invalid_contract", "bundle manifest mismatch")
		}
	}
	files := map[string]bool{}
	for _, f := range bundle.Files {
		files[f.Path] = true
	}
	for _, seed := range spec.Seeds {
		if seed.Kind == "bundle" && !files[seed.Path] {
			return out, programError("invalid_contract", "seed is not a declared bundle member")
		}
	}
	p, err := programLoad(ctx, tx, spec.ID)
	n := 1
	sup := 0
	if err == nil {
		old, e := programRevision(ctx, tx, p.ID, p.CurrentRevision)
		if e != nil {
			return out, e
		}
		if programHash(old.Spec) == programHash(spec) && old.SeedManifestSHA == bundle.ManifestSHA && p.State != model.ProgramBlocked {
			return old, nil
		}
		if p.State == model.ProgramCancelled {
			return out, programError("invalid_contract", "cancelled program cannot be revised")
		}
		n = p.CurrentRevision + 1
		sup = p.CurrentRevision
	} else if !errors.Is(err, sql.ErrNoRows) {
		return out, err
	} else {
		p = model.Program{ID: spec.ID, Project: spec.Project, Title: spec.Title, CreatedAt: time.Now().UTC()}
	}
	if p.Project != spec.Project {
		return out, programError("invalid_contract", "program project is immutable")
	}
	out = model.ProgramRevision{ProgramID: spec.ID, Number: n, Spec: spec, SeedManifestSHA: bundle.ManifestSHA, Supersedes: sup, CreatedAt: time.Now().UTC(), Reason: "import"}
	out.RevisionHash = programHash(struct {
		ProgramID       string
		Number          int
		Spec            model.ProgramSpec
		SeedManifestSHA string
	}{spec.ID, n, spec, bundle.ManifestSHA})
	p.CurrentRevision = n
	p.State = model.ProgramDraft
	p.Phase = model.StepResearch
	p.Title = spec.Title
	p.Wait = &model.WaitReason{Code: "approval_required", Detail: "exact revision approval required"}
	if err = programPut(ctx, tx, p); err != nil {
		return out, err
	}
	b, _ := json.Marshal(out)
	if _, err = tx.ExecContext(ctx, `INSERT INTO program_revisions(program_id,number,revision_hash,data) VALUES(?,?,?,?)`, spec.ID, n, out.RevisionHash, b); err != nil {
		return out, err
	}
	if sup == 0 {
		if err = putProgramBudgetTx(ctx, tx, spec.ID, model.ProgramBudget{}); err != nil {
			return out, err
		}
	}
	if bundle.ManifestSHA != "" {
		if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO program_bundles(program_id,manifest_sha,revision) VALUES(?,?,?)`, p.ID, bundle.ManifestSHA, n); err != nil {
			return out, err
		}
	}
	if err = programEvent(ctx, tx, p.ID, "imported", out.RevisionHash); err != nil {
		return out, err
	}
	return out, nil
}
func (s *Store) ApproveProgram(ctx context.Context, id, revisionHash, policyHash string, audit model.AuditContext) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = s.approveProgramTx(ctx, tx, id, revisionHash, policyHash, audit); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) approveProgramTx(ctx context.Context, tx *sql.Tx, id, revisionHash, policy string, audit model.AuditContext) error {
	return s.programApproveTx(ctx, tx, id, revisionHash, "", policy, audit)
}

func (s *Store) programApproveTx(ctx context.Context, tx *sql.Tx, id, revisionHash, subject, policy string, audit model.AuditContext) error {
	if err := validateAuditContext(audit); err != nil {
		return err
	}
	if policy == "" {
		return programError("invalid_contract", "policy required")
	}
	p, err := programLoad(ctx, tx, id)
	if err != nil {
		return err
	}
	r, err := programRevision(ctx, tx, id, p.CurrentRevision)
	if err != nil {
		return err
	}
	if r.RevisionHash != revisionHash {
		return programError("stale_revision", "revision hash does not match")
	}
	if p.State == model.ProgramCancelled {
		return programError("approval_required", "program cancelled")
	}
	if p.State == model.ProgramBlocked {
		return programError("approval_required", "blocked evidence requires a new program revision before approval")
	}
	if err = s.programDisclosureTx(ctx, tx, r); err != nil {
		return err
	}
	if subject != "" {
		if err = programInvalidTx(ctx, tx, id, subject); err != nil {
			return err
		}
	}
	g := model.ProgramGrant{ProgramID: id, RevisionHash: revisionHash, SubjectHash: subject, PolicyHash: policy, Actor: programAuditActor(audit), Audit: audit, CreatedAt: time.Now().UTC()}
	b, _ := json.Marshal(g)
	if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO program_grants(program_id,revision_hash,subject_hash,policy_hash,data) VALUES(?,?,?,?,?)`, id, revisionHash, subject, policy, b); err != nil {
		return err
	}
	if subject == "" {
		p.State = model.ProgramActive
		p.Wait = nil
		if err = programPut(ctx, tx, p); err != nil {
			return err
		}
	}
	if err = programEvent(ctx, tx, id, "approved", subject); err != nil {
		return err
	}
	return recordAuditTx(ctx, tx, "program", id, "approved", audit)
}
func (s *Store) ApproveProgramArtifact(ctx context.Context, id, expectedProgramRevisionHash string, kind model.ProgramArtifactKind, subjectHash, policyHash string, audit model.AuditContext) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = s.approveProgramArtifactTx(ctx, tx, id, expectedProgramRevisionHash, kind, subjectHash, policyHash, audit); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) approveProgramArtifactTx(ctx context.Context, tx *sql.Tx, id, expectedProgramRevisionHash string, kind model.ProgramArtifactKind, subjectHash, policyHash string, audit model.AuditContext) error {
	if kind != model.ArtifactArchitecture && kind != model.ArtifactExperimentContracts {
		return programError("invalid_contract", "approval requires architecture or experiment kind")
	}
	if len(subjectHash) != sha256.Size*2 {
		return programError("invalid_contract", "subject hash must be a lowercase SHA256 digest")
	}
	for i := range subjectHash {
		c := subjectHash[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return programError("invalid_contract", "subject hash must be a lowercase SHA256 digest")
		}
	}
	p, err := programLoad(ctx, tx, id)
	if err != nil {
		return err
	}
	r, err := programRevision(ctx, tx, id, p.CurrentRevision)
	if err != nil {
		return err
	}
	if r.RevisionHash != expectedProgramRevisionHash {
		return programError("stale_revision", "revision hash does not match")
	}
	rows, err := tx.QueryContext(ctx, `SELECT data FROM program_artifacts WHERE program_id=? AND revision=? AND kind=?`, id, p.CurrentRevision, kind)
	if err != nil {
		return err
	}
	found := false
	var approvedArtifact model.ProgramArtifact
	for rows.Next() {
		var b []byte
		var a model.ProgramArtifact
		if err = rows.Scan(&b); err != nil {
			break
		}
		if err = json.Unmarshal(b, &a); err != nil {
			break
		}
		if kind == model.ArtifactArchitecture && a.Kind == string(kind) && a.ContentSHA == subjectHash {
			found = true
			approvedArtifact = a
		}
		if kind == model.ArtifactExperimentContracts && a.Kind == string(kind) {
			raw, e := s.blobs.Read(ctx, a.ContentSHA, 8<<20)
			if e != nil {
				err = e
				break
			}
			var payload model.ExperimentContractsPayload
			if e = model.StrictDecode(raw, &payload); e != nil {
				err = e
				break
			}
			for _, c := range payload.Experiments {
				if programHash(c) == subjectHash && c.BundleManifestSHA != "" {
					found = true
					approvedArtifact = a
				}
			}
		}
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return err
	}
	if !found {
		return programError("evidence_invalid", "subject is not current architecture or frozen experiment")
	}
	if err = programValidateRefTx(ctx, tx, id, p.CurrentRevision, model.ProgramInputRef{Kind: string(kind), ID: approvedArtifact.ID, SHA: approvedArtifact.ContentSHA, Revision: p.CurrentRevision}); err != nil {
		return err
	}
	return s.programApproveTx(ctx, tx, id, expectedProgramRevisionHash, subjectHash, policyHash, audit)
}
func (s *Store) ResumeProgram(ctx context.Context, id, revisionHash string, cfg model.Config, audit model.AuditContext) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = s.resumeProgramTx(ctx, tx, id, revisionHash, cfg, audit); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) resumeProgramTx(ctx context.Context, tx *sql.Tx, id, revisionHash string, cfg model.Config, audit model.AuditContext) error {
	if err := validateAuditContext(audit); err != nil {
		return err
	}
	p, err := programLoad(ctx, tx, id)
	if err != nil {
		return err
	}
	r, err := programRevision(ctx, tx, id, p.CurrentRevision)
	if err != nil {
		return err
	}
	if r.RevisionHash != revisionHash {
		return programError("stale_revision", "revision changed")
	}
	selection, err := s.programResumeWorkTx(ctx, tx, p, r, cfg)
	if err != nil {
		return err
	}
	if selection != nil {
		work := selection.Work
		if err = retryFeatureTx(ctx, tx, work.FeatureID, audit); err != nil {
			return err
		}
		step, e := programStepTx(ctx, tx, work.StepID)
		if e != nil {
			return e
		}
		// Keep immutable inputs, outputs and producer history; only the selected
		// failed step becomes schedulable again through ordinary admission.
		step.State = "ready"
		step.UpdatedAt = time.Now().UTC()
		raw, e := json.Marshal(step)
		if e != nil {
			return e
		}
		if _, err = tx.ExecContext(ctx, `UPDATE program_steps SET data=? WHERE id=?`, raw, step.ID); err != nil {
			return err
		}
	}
	p.State = model.ProgramActive
	p.Wait = nil
	if err = programPut(ctx, tx, p); err != nil {
		return err
	}
	return recordAuditTx(ctx, tx, "program", id, "resumed", audit)
}
func (s *Store) CancelProgram(ctx context.Context, id, revisionHash string, audit model.AuditContext) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = s.cancelProgramTx(ctx, tx, id, revisionHash, audit); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) cancelProgramTx(ctx context.Context, tx *sql.Tx, id, revisionHash string, audit model.AuditContext) error {
	if err := validateAuditContext(audit); err != nil {
		return err
	}
	p, err := programLoad(ctx, tx, id)
	if err != nil {
		return err
	}
	r, err := programRevision(ctx, tx, id, p.CurrentRevision)
	if err != nil {
		return err
	}
	if r.RevisionHash != revisionHash {
		return programError("stale_revision", "revision changed")
	}
	p.State = model.ProgramCancelled
	p.Wait = nil
	if err = programPut(ctx, tx, p); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT feature_id FROM program_steps WHERE program_id=?`, id)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var fid string
		if err = rows.Scan(&fid); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, fid)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, fid := range ids {
		f, e := get(ctx, tx, fid)
		if e != nil {
			return e
		}
		var n int
		if e = tx.QueryRowContext(ctx, `SELECT count(*) FROM attempts WHERE feature_id=? AND active=1`, fid).Scan(&n); e != nil {
			return e
		}
		if n > 0 {
			f.State = "cancelling"
		} else if f.State != "delivered" && f.State != "reviewed" {
			f.State = "cancelled"
		}
		if e = put(ctx, tx, f); e != nil {
			return e
		}
	}
	if err = programEvent(ctx, tx, id, "cancelled", programAuditActor(audit)); err != nil {
		return err
	}
	return recordAuditTx(ctx, tx, "program", id, "cancelled", audit)
}

func programAuditActor(audit model.AuditContext) string {
	if audit.Kind == model.AuditTrustedLocal {
		return model.AuditTrustedLocal
	}
	return audit.OperatorID
}
func (s *Store) ProgramArtifactPayload(ctx context.Context, id string) (model.ProgramArtifact, []byte, error) {
	var a model.ProgramArtifact
	var b []byte
	err := s.db.QueryRowContext(ctx, `SELECT data FROM program_artifacts WHERE id=?`, id).Scan(&b)
	if err != nil {
		return a, nil, err
	}
	if err = json.Unmarshal(b, &a); err != nil {
		return a, nil, err
	}
	raw, err := s.blobs.Read(ctx, a.ContentSHA, 8<<20)
	return a, raw, err
}
func (s *Store) ProgramBundle(ctx context.Context, sha string) (model.ProgramBundle, error) {
	return s.blobs.LoadBundle(ctx, sha)
}
func programManifest(inputs []model.ProgramInputRef) model.ProgramManifest {
	sort.Slice(inputs, func(i, j int) bool {
		a, b := inputs[i], inputs[j]
		return fmt.Sprintf("%s/%s/%s/%d", a.Kind, a.ID, a.SHA, a.Revision) < fmt.Sprintf("%s/%s/%s/%d", b.Kind, b.ID, b.SHA, b.Revision)
	})
	m := model.ProgramManifest{Inputs: inputs}
	m.SHA = programHash(inputs)
	return m
}
func programManifestTx(ctx context.Context, tx *sql.Tx, id string, revision int, inputs []model.ProgramInputRef) (model.ProgramManifest, error) {
	m := programManifest(inputs)
	return m, programManifestPutTx(ctx, tx, id, revision, m)
}
func programManifestPutTx(ctx context.Context, tx *sql.Tx, id string, revision int, m model.ProgramManifest) error {
	b, _ := json.Marshal(m)
	_, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO program_manifests(sha,program_id,revision,data) VALUES(?,?,?,?)`, m.SHA, id, revision, b)
	return err
}
func programModelAllowed(spec model.ProgramSpec, modelName string) bool {
	for _, provider := range spec.Disclosure.ModelProviderIDs {
		if provider != "" && (modelName == provider || strings.HasPrefix(modelName, provider+"/") || strings.HasPrefix(modelName, provider+":")) {
			return true
		}
	}
	return false
}
