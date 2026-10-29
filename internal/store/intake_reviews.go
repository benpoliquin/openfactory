package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"openfactory/internal/intake"
	"openfactory/internal/model"
)

// intakeSubjectOwnerTx supplements project scope with intake ownership. Trusted
// local subjects have no intake owner; their enrolled project is the boundary.
func intakeSubjectOwnerTx(ctx context.Context, tx *sql.Tx, operatorID, kind, id string) error {
	var denied int
	query := `SELECT count(*) FROM intake_links l JOIN intake_requests r ON r.id=l.request_id WHERE json_extract(l.data,'$.subject_id')=? AND ((?='program' AND json_extract(l.data,'$.subject_kind')='program') OR (?!='program' AND json_extract(l.data,'$.subject_kind') IN ('feature','plan'))) AND r.operator_id!=?`
	if err := tx.QueryRowContext(ctx, query, id, kind, kind, operatorID).Scan(&denied); err != nil {
		return err
	}
	if denied != 0 {
		return ErrProjectForbidden
	}
	if kind != "program" {
		links, err := intakeList[model.ProgramFeatureLink](ctx, tx, `SELECT data FROM program_feature_links WHERE feature_id=?`, id)
		if err != nil {
			return err
		}
		for _, link := range links {
			if err = intakeSubjectOwnerTx(ctx, tx, operatorID, "program", link.ProgramID); err != nil {
				return err
			}
		}
	}
	return nil
}
func intakeSubjectScopeTx(ctx context.Context, tx *sql.Tx, operatorID, kind, id string) (string, error) {
	var project string
	switch kind {
	case "feature", "plan":
		f, err := get(ctx, tx, id)
		if err != nil {
			return "", programReadError(err)
		}
		if f.IntakeStepID != "" || f.DiscoveryStepID != "" || f.Kind == model.IntakeFeatureKind || strings.HasPrefix(f.ID, model.IntakeFeaturePrefix) || strings.HasPrefix(f.ID, "openfactory-discovery-") {
			return "", ErrNotFound
		}
		if kind == "plan" && f.Kind != "plan" || kind == "feature" && f.Kind == "plan" {
			return "", ErrNotFound
		}
		project = f.Project
	case "program":
		p, err := programLoad(ctx, tx, id)
		if err != nil {
			return "", programReadError(err)
		}
		project = p.Project
	default:
		return "", programError("invalid_request", "unsupported subject kind")
	}
	if _, err := intakeAuthLiveOperator(ctx, tx, operatorID, project); err != nil {
		return "", err
	}
	if err := intakeSubjectOwnerTx(ctx, tx, operatorID, kind, id); err != nil {
		return "", err
	}
	return project, nil
}

func (s *Store) intakeDraftBundleTx(ctx context.Context, tx *sql.Tx, r model.IntakeRequest, spec model.ProgramSpec, manifestSHA string) (model.ProgramBundle, error) {
	var bundle model.ProgramBundle
	var err error
	if manifestSHA != "" {
		bundle, err = s.intakeAuthorizeBundleTx(ctx, tx, r.OperatorID, r.Project, manifestSHA)
		if err != nil {
			return bundle, err
		}
	}
	files := map[string]bool{}
	for _, f := range bundle.Files {
		files[f.Path] = true
		if err = s.intakeVerifyBundleFile(ctx, f, false); err != nil {
			return bundle, err
		}
	}
	for _, seed := range spec.Seeds {
		if seed.Kind == "bundle" && !files[seed.Path] {
			return bundle, programError("invalid_contract", "program seed is not a declared bundle member")
		}
	}
	return bundle, nil
}
func intakePredictProgramTx(ctx context.Context, tx *sql.Tx, spec model.ProgramSpec, bundle model.ProgramBundle, now time.Time) (model.ProgramRevision, error) {
	n, sup := 1, 0
	p, err := programLoad(ctx, tx, spec.ID)
	if err == nil {
		if p.Project != spec.Project {
			return model.ProgramRevision{}, programError("invalid_contract", "program project is immutable")
		}
		if p.State == model.ProgramCancelled {
			return model.ProgramRevision{}, programError("stale_subject", "cancelled program cannot be revised")
		}
		old, e := programRevision(ctx, tx, spec.ID, p.CurrentRevision)
		if e != nil {
			return old, e
		}
		if programHash(old.Spec) == programHash(spec) && old.SeedManifestSHA == bundle.ManifestSHA && p.State != model.ProgramBlocked {
			return old, nil
		}
		n, sup = p.CurrentRevision+1, p.CurrentRevision
	} else if !errors.Is(err, sql.ErrNoRows) {
		return model.ProgramRevision{}, err
	}
	out := model.ProgramRevision{ProgramID: spec.ID, Number: n, Spec: spec, SeedManifestSHA: bundle.ManifestSHA, Supersedes: sup, CreatedAt: now, Reason: "import"}
	out.RevisionHash = intake.ProgramRevisionHash(spec.ID, n, spec, bundle.ManifestSHA)
	return out, nil
}

// Draft persistence and approval share the historical domain revision hash. A
// normalized spec digest alone is not a program revision (number+bundle matter).
func (s *Store) intakeCanonicalTx(ctx context.Context, tx *sql.Tx, route, format string, source []byte, r model.IntakeRequest, bundleManifestSHA string, cfg model.Config) (intake.CanonicalDraft, error) {
	canonical, err := intake.ParseCanonical(route, format, source, r.Project, cfg)
	if err != nil {
		return canonical, programError("invalid_contract", err.Error())
	}
	if canonical.Feature != nil && bundleManifestSHA != "" {
		return canonical, programError("invalid_contract", "only program drafts can select a seed bundle")
	}
	if canonical.Feature != nil {
		if err = intakeSubjectOwnerTx(ctx, tx, r.OperatorID, route, canonical.Feature.ID); err != nil {
			return canonical, err
		}
	}
	if canonical.Program != nil {
		bundle, e := s.intakeDraftBundleTx(ctx, tx, r, *canonical.Program, bundleManifestSHA)
		if e != nil {
			return canonical, e
		}
		if _, e = programLoad(ctx, tx, canonical.Program.ID); e == nil {
			if e = intakeSubjectOwnerTx(ctx, tx, r.OperatorID, "program", canonical.Program.ID); e != nil {
				return canonical, e
			}
		} else if !errors.Is(e, sql.ErrNoRows) {
			return canonical, e
		}
		rev, e := intakePredictProgramTx(ctx, tx, *canonical.Program, bundle, time.Time{})
		if e != nil {
			return canonical, e
		}
		canonical.DomainRevision = rev.RevisionHash
	}
	return canonical, nil
}

// Committed drafting authority survives later credential/operator revocation.
// Only its immutable disclosed context is readable here; new reviews/imports use
// intakeCanonicalTx and must independently establish current live ownership.
func (s *Store) intakeCanonicalCommittedTx(ctx context.Context, tx *sql.Tx, route, format string, source []byte, r model.IntakeRequest, cfg model.Config) (intake.CanonicalDraft, error) {
	// Verify persisted authority before interpreting untrusted model source.
	// Even an invalid model draft must not disguise immutable context corruption.
	if _, _, err := s.intakeResolveManifestTx(ctx, tx, r, r.ContextManifest, nil); err != nil {
		return intake.CanonicalDraft{}, err
	}
	canonical, err := intake.ParseCanonical(route, format, source, r.Project, cfg)
	if err != nil {
		return canonical, programError("invalid_contract", err.Error())
	}
	if canonical.Program == nil {
		return canonical, nil
	}
	shas := r.ContextManifest.Disclosure.BundleManifestSHAs
	if len(shas) > 1 {
		return canonical, programError("question_unresolved", "program requires one unambiguous seed bundle")
	}
	var bundle model.ProgramBundle
	if len(shas) == 1 {
		bundle, err = s.blobs.LoadBundle(ctx, shas[0])
		if err != nil {
			return canonical, err
		}
	}
	files := map[string]bool{}
	for _, f := range bundle.Files {
		files[f.Path] = true
	}
	for _, seed := range canonical.Program.Seeds {
		if seed.Kind == "bundle" && !files[seed.Path] {
			return canonical, programError("invalid_contract", "program seed is not a committed bundle member")
		}
	}
	revision, err := intakePredictProgramTx(ctx, tx, *canonical.Program, bundle, time.Time{})
	if err != nil {
		return canonical, err
	}
	canonical.DomainRevision = revision.RevisionHash
	return canonical, nil
}

// The manifest deliberately excludes progress event sequence and request update
// time. Appending a progress event cannot invalidate unchanged human consent.
type intakeReviewRequestContent struct {
	ID                  string               `json:"id"`
	OperatorID          string               `json:"operator_id"`
	Project             string               `json:"project"`
	State               string               `json:"state"`
	ContentVersion      int                  `json:"content_version"`
	LatestDraftRevision int                  `json:"latest_draft_revision"`
	CreationPolicySHA   string               `json:"creation_policy_sha"`
	LifetimeDispatches  int                  `json:"lifetime_dispatches"`
	MaxDraftDispatches  int                  `json:"max_draft_dispatches"`
	Context             model.IntakeManifest `json:"context"`
}
type intakeReviewManifest struct {
	CommandSHA         string                      `json:"command_sha"`
	SourceSHA          string                      `json:"source_sha"`
	CanonicalSHA       string                      `json:"canonical_sha"`
	DomainRevision     string                      `json:"domain_revision"`
	Request            *intakeReviewRequestContent `json:"request,omitempty"`
	Draft              *model.IntakeDraft          `json:"draft,omitempty"`
	Turns              []model.IntakeTurn          `json:"turns,omitempty"`
	Questions          []model.IntakeQuestion      `json:"questions,omitempty"`
	Feature            *model.Feature              `json:"feature,omitempty"`
	Program            *model.Program              `json:"program,omitempty"`
	ProgramRevision    *model.ProgramRevision      `json:"program_revision,omitempty"`
	Answers            []model.QuestionAnswer      `json:"answers,omitempty"`
	SeedManifest       *model.ProgramBundle        `json:"seed_manifest,omitempty"`
	Artifacts          []model.ProgramArtifact     `json:"artifacts,omitempty"`
	PlanningQuestions  []model.PlanningQuestion    `json:"planning_questions,omitempty"`
	PlanningAnswers    []model.PlanningAnswer      `json:"planning_answers,omitempty"`
	SelectedArtifact   *model.ProgramArtifact      `json:"selected_artifact,omitempty"`
	SelectedExperiment *model.ExperimentContract   `json:"selected_experiment,omitempty"`
	Sources            []model.SourceCapture       `json:"sources,omitempty"`
	Evidence           []model.ProgramInputRef     `json:"evidence"`
	BundleManifestSHAs []string                    `json:"bundle_manifest_shas"`
	PolicySHA          string                      `json:"policy_sha"`
	ExecutionPolicySHA string                      `json:"execution_policy_sha,omitempty"`
	ResumeWork         *model.ProgramResumeWork    `json:"resume_work,omitempty"`
}

func intakeReviewBound[T any](v []T) error {
	if len(v) > 4096 {
		return programError("input_limit", "review history exceeds the bounded review limit")
	}
	return nil
}

// Collect the complete validated evidence closure into the claims. A manifest
// digest is not a substitute for the exact sources/artifacts a person reviewed.
func (s *Store) intakeReviewEvidenceTx(ctx context.Context, tx *sql.Tx, programID string, revision int, sha string, m *intakeReviewManifest, seen map[string]bool) error {
	if seen[sha] {
		return nil
	}
	seen[sha] = true
	if len(seen) > 1024 {
		return programError("input_limit", "review evidence graph exceeds limit")
	}
	if err := programManifestValidTx(ctx, tx, programID, revision, sha, map[string]bool{}); err != nil {
		return err
	}
	manifest, _, _, err := programManifestLoadTx(ctx, tx, sha)
	if err != nil {
		return err
	}
	for _, ref := range manifest.Inputs {
		m.Evidence = append(m.Evidence, ref)
		switch ref.Kind {
		case "program_revision", "answer", "experiment_receipt", "experiment", "receipt":
		case "bundle":
			bundle, e := s.blobs.LoadBundle(ctx, ref.SHA)
			if e != nil {
				return e
			}
			for _, f := range bundle.Files {
				if e = s.intakeVerifyBundleFile(ctx, f, false); e != nil {
					return e
				}
			}
			m.BundleManifestSHAs = append(m.BundleManifestSHAs, ref.SHA)
		case "source":
			source, e := intakeLoad[model.SourceCapture](ctx, tx, `SELECT data FROM program_sources WHERE id=? AND program_id=?`, ref.ID, programID)
			if e != nil {
				return e
			}
			if _, e = s.blobs.Read(ctx, source.TextSHA256, 8<<20); e != nil {
				return e
			}
			m.Sources = append(m.Sources, source)
		default:
			a, e := programArtifactTx(ctx, tx, ref.ID)
			if e != nil {
				return e
			}
			if _, e = s.blobs.Read(ctx, a.ContentSHA, 8<<20); e != nil {
				return e
			}
			if a.InputManifestSHA != "" {
				if e = s.intakeReviewEvidenceTx(ctx, tx, programID, revision, a.InputManifestSHA, m, seen); e != nil {
					return e
				}
			}
		}
	}
	return nil
}

func (s *Store) intakeReviewRequestTx(ctx context.Context, tx *sql.Tx, command model.TypedCommand, op model.Operator, cfg model.Config, m *intakeReviewManifest) (model.IntakeRequest, error) {
	r, err := intakeRequestTx(ctx, tx, command.RequestID)
	if err != nil {
		return r, programReadError(err)
	}
	if err = intakeWritable(r, command.ExpectedContentVersion, op.ID); err != nil {
		return r, err
	}
	if _, err = intakeAuthLiveOperator(ctx, tx, op.ID, r.Project); err != nil {
		return r, err
	}
	if p, ok := cfg.Projects[r.Project]; !ok || !p.Trusted {
		return r, ErrProjectForbidden
	}
	requireContext := command.Action == model.ActionIntakeRetry
	if command.Action == model.ActionIntakeApproveQueue {
		draft, e := intakeLoad[model.IntakeDraft](ctx, tx, `SELECT data FROM intake_drafts WHERE request_id=? AND revision=?`, r.ID, r.LatestDraftRevision)
		if e != nil {
			return r, e
		}
		requireContext = draft.StepID != ""
	}
	if r.ContextManifest.SHA != "" {
		if err = intakeManifestIdentity(r.ContextManifest); err != nil {
			return r, err
		}
		if r.ContextManifest.Project != r.Project || r.ContextManifest.RequestID != r.ID {
			return r, errors.New("request context scope differs")
		}
	} else if requireContext || programHash(r.ContextManifest) != programHash(model.IntakeManifest{}) {
		return r, programError("disclosure_required", "committed draft context is missing")
	}
	if requireContext {
		if r.ContextManifest.PolicySHA != cfg.PolicyHash || r.ContextManifest.Disclosure.PolicySHA != cfg.PolicyHash {
			return r, programError("stale_policy", "request disclosure policy changed")
		}
		if !r.ContextManifest.Disclosure.PrivateMaterialToModels {
			return r, programError("disclosure_required", "exact context disclosure is required")
		}
		if len(r.ContextManifest.Disclosure.ModelProviderIDs) != 1 || r.ContextManifest.Disclosure.ModelProviderIDs[0] != cfg.AstraModel {
			return r, programError("stale_policy", "disclosed draft provider changed")
		}
	}
	m.Request = &intakeReviewRequestContent{ID: r.ID, OperatorID: r.OperatorID, Project: r.Project, State: r.State, ContentVersion: r.ContentVersion, LatestDraftRevision: r.LatestDraftRevision, CreationPolicySHA: r.CreationPolicySHA, LifetimeDispatches: r.LifetimeDispatches, MaxDraftDispatches: r.MaxDraftDispatches, Context: r.ContextManifest}
	m.Turns, err = intakeList[model.IntakeTurn](ctx, tx, `SELECT data FROM intake_turns WHERE request_id=? ORDER BY sequence LIMIT 4097`, r.ID)
	if err != nil {
		return r, err
	}
	if err = intakeReviewBound(m.Turns); err != nil {
		return r, err
	}
	m.Questions, err = intakeList[model.IntakeQuestion](ctx, tx, `SELECT data FROM intake_questions WHERE request_id=? AND json_extract(data,'$.content_version')=? ORDER BY id LIMIT 4097`, r.ID, r.ContentVersion)
	if err != nil {
		return r, err
	}
	if err = intakeReviewBound(m.Questions); err != nil {
		return r, err
	}
	for i := range m.Questions {
		err = tx.QueryRowContext(ctx, `SELECT sequence FROM intake_question_answers WHERE question_id=?`, m.Questions[i].ID).Scan(&m.Questions[i].AnsweredBySequence)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return r, err
		}
	}
	// Resolve the identical full safe context used by the model worker. Reviews
	// are new authority, so bundle ownership and capabilities must still be live.
	if r.ContextManifest.SHA != "" {
		if _, _, err = s.intakeResolveManifestTx(ctx, tx, r, r.ContextManifest, &cfg); err != nil {
			return r, err
		}
		m.BundleManifestSHAs = append(m.BundleManifestSHAs, r.ContextManifest.Disclosure.BundleManifestSHAs...)
	}
	return r, nil
}

func (s *Store) intakeSubjectViewTx(ctx context.Context, tx *sql.Tx, operatorID, kind, id string) (model.IntakeSubjectView, error) {
	out := model.IntakeSubjectView{Kind: kind, ID: id, Answers: []model.QuestionAnswer{}, Artifacts: []model.ProgramArtifact{}}
	project, err := intakeSubjectScopeTx(ctx, tx, operatorID, kind, id)
	if err != nil {
		return out, err
	}
	out.Project = project
	if kind != "program" {
		f, e := get(ctx, tx, id)
		if e != nil {
			return out, e
		}
		out.Feature = &f
		out.Revision = f.Revision
		if kind == "plan" {
			out.PlanningQuestions, err = intakeList[model.PlanningQuestion](ctx, tx, `SELECT data FROM planning_questions WHERE feature_id=? ORDER BY rowid LIMIT 4097`, id)
			if err != nil {
				return out, err
			}
			if err = intakeReviewBound(out.PlanningQuestions); err != nil {
				return out, err
			}
			out.PlanningAnswers, err = intakeList[model.PlanningAnswer](ctx, tx, `SELECT data FROM planning_answers WHERE feature_id=? ORDER BY rowid LIMIT 4097`, id)
			if err != nil {
				return out, err
			}
			if err = intakeReviewBound(out.PlanningAnswers); err != nil {
				return out, err
			}
		}
		return out, nil
	}
	p, err := programLoad(ctx, tx, id)
	if err != nil {
		return out, err
	}
	r, err := programRevision(ctx, tx, id, p.CurrentRevision)
	if err != nil {
		return out, err
	}
	out.Program = &p
	out.ProgramRevision = &r
	out.Revision = r.RevisionHash
	out.Answers, err = intakeList[model.QuestionAnswer](ctx, tx, `SELECT data FROM program_answers WHERE program_id=? ORDER BY id LIMIT 4097`, id)
	if err != nil {
		return out, err
	}
	if err = intakeReviewBound(out.Answers); err != nil {
		return out, err
	}
	out.Artifacts, err = intakeList[model.ProgramArtifact](ctx, tx, `SELECT data FROM program_artifacts WHERE program_id=? ORDER BY id LIMIT 4097`, id)
	if err != nil {
		return out, err
	}
	if err = intakeReviewBound(out.Artifacts); err != nil {
		return out, err
	}
	if r.SeedManifestSHA != "" {
		var count int
		if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM program_bundles WHERE program_id=? AND manifest_sha=? AND revision<=?`, id, r.SeedManifestSHA, r.Number).Scan(&count); err != nil {
			return out, err
		}
		if count == 0 {
			return out, programError("evidence_invalid", "program seed bundle ownership is missing")
		}
		bundle, e := s.blobs.LoadBundle(ctx, r.SeedManifestSHA)
		if e != nil {
			return out, e
		}
		out.SeedManifest = &bundle
	}
	return out, nil
}
func (s *Store) IntakeSubject(ctx context.Context, operatorID, kind, id string) (model.IntakeSubjectView, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return model.IntakeSubjectView{}, err
	}
	defer tx.Rollback()
	out, err := s.intakeSubjectViewTx(ctx, tx, operatorID, kind, id)
	if err != nil {
		return out, err
	}
	return out, tx.Commit()
}

func (s *Store) CreateIntakeReview(ctx context.Context, tokenSHA string, command model.TypedCommand, cfg model.Config) (model.IntakeReview, error) {
	var out model.IntakeReview
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	session, op, credential, err := intakeAuthenticateTx(ctx, tx, tokenSHA, "", "", time.Now().UTC())
	if err != nil {
		return out, err
	}
	out, err = s.reviewIntakeCommandTx(ctx, tx, command, cfg, session, op, credential, nil)
	if err != nil {
		return out, err
	}
	if err = intakeSaveReviewTx(ctx, tx, out); err != nil {
		return out, err
	}
	return out, tx.Commit()
}

func (s *Store) reviewIntakeCommandTx(ctx context.Context, tx *sql.Tx, command model.TypedCommand, cfg model.Config, session model.BrowserSession, op model.Operator, credential model.OperatorCredential, template *model.IntakeReview) (model.IntakeReview, error) {
	out := model.IntakeReview{Command: command, Findings: []model.IntakeFinding{}}
	if !cfg.Intake.Enabled {
		return out, programError("unsupported_action", "intake is disabled")
	}
	if !intake.RequiresHumanVerification(command.Action) {
		return out, programError("unsupported_action", "command does not require a human authority review")
	}
	commandRaw, err := intake.CommandCanonical(command)
	if err != nil {
		return out, programError("invalid_request", "invalid command contract")
	}
	out.CommandCanonical = string(commandRaw)
	digest := intake.SHA256(commandRaw)
	payload, err := intake.CommandPayload(command)
	if err != nil {
		return out, err
	}
	now := time.Now().UTC()
	if session.OperatorID != op.ID || session.CredentialID != credential.ID || credential.OperatorID != op.ID {
		return out, ErrAuthRequired
	}
	if _, err = intakeAuthLiveOperator(ctx, tx, op.ID, ""); err != nil {
		return out, err
	}
	if _, err = intakeAuthLiveCredential(ctx, tx, credential.ID, op.ID, now); err != nil {
		return out, err
	}
	claims := model.ReviewClaims{Version: 1, OperatorID: op.ID, CredentialID: credential.ID, Action: command.Action, CommandID: command.CommandID, PolicySHA: cfg.PolicyHash, IssuedAt: now, ExpiresAt: now.Add(2 * time.Minute), Evidence: []model.ProgramInputRef{}, BundleManifestSHAs: []string{}}
	if err = tx.QueryRowContext(ctx, `SELECT value FROM settings WHERE key='installation_id'`).Scan(&claims.InstallationID); err != nil {
		return out, err
	}
	if template == nil {
		out.ID = ID()
		out.CreatedAt = now
		claims.ChallengeID = ID()
	} else {
		previousDigest, e := intake.CommandDigest(template.Command)
		if e != nil || previousDigest != digest || template.Claims.OperatorID != op.ID || template.Claims.CredentialID != credential.ID || template.Claims.InstallationID != claims.InstallationID || template.Claims.CommandID != command.CommandID || template.Claims.Action != command.Action {
			return out, ErrAuthRequired
		}
		if !now.Before(template.Claims.ExpiresAt) {
			return out, ErrChallengeExpired
		}
		out.ID = template.ID
		out.CreatedAt = template.CreatedAt
		claims.ChallengeID = template.Claims.ChallengeID
		claims.IssuedAt = template.Claims.IssuedAt
		claims.ExpiresAt = template.Claims.ExpiresAt
	}
	manifest := intakeReviewManifest{CommandSHA: digest, PolicySHA: cfg.PolicyHash, Evidence: []model.ProgramInputRef{}, BundleManifestSHAs: []string{}}
	if command.RequestID != "" {
		r, e := s.intakeReviewRequestTx(ctx, tx, command, op, cfg, &manifest)
		if e != nil {
			return out, e
		}
		claims.RequestID = r.ID
		claims.ContentVersion = r.ContentVersion
		claims.InputManifestSHA = r.ContextManifest.SHA
		switch command.Action {
		case model.ActionIntakeRetry:
			if r.State != model.IntakeBlocked {
				return out, programError("stale_subject", "request is not blocked")
			}
			if r.ActiveStepID != "" {
				return out, programError("attempt_unsettled", "previous draft dispatch is not settled")
			}
			if r.LifetimeDispatches >= r.MaxDraftDispatches {
				return out, programError("input_limit", "request lifetime draft budget exhausted")
			}
			claims.SubjectKind = "intake"
			claims.SubjectID = r.ID
			raw, e := json.Marshal(manifest.Request)
			if e != nil {
				return out, e
			}
			out.CanonicalPayload = string(raw)
			claims.SubjectRevision = intake.SHA256(raw)
			claims.CanonicalPayloadSHA = claims.SubjectRevision
		case model.ActionIntakeApproveQueue:
			p := payload.(*model.IntakeApprovePayload)
			if r.State != model.IntakeReviewReady || r.ActiveStepID != "" || p.DraftRevision != r.LatestDraftRevision {
				return out, programError("stale_revision", "current settled draft review is required")
			}
			d, e := intakeLoad[model.IntakeDraft](ctx, tx, `SELECT data FROM intake_drafts WHERE request_id=? AND revision=?`, r.ID, p.DraftRevision)
			if e != nil {
				return out, e
			}
			if d.RequestID != r.ID || d.ContentVersion != r.ContentVersion || d.Revision != p.DraftRevision || d.SourceSHA != p.SourceSHA || d.DomainRevision != p.DomainRevision {
				return out, programError("stale_revision", "draft content changed")
			}
			if d.PolicySHA != cfg.PolicyHash {
				return out, programError("stale_policy", "draft policy changed")
			}
			for _, q := range manifest.Questions {
				if q.AnsweredBySequence == 0 {
					return out, programError("question_unresolved", "current consequential question is unanswered")
				}
			}
			canonical, e := intake.ParseCanonical(d.Route, d.Format, []byte(d.CanonicalSource), r.Project, cfg)
			if e != nil {
				return out, programError("invalid_contract", e.Error())
			}
			var reviewedBundle model.ProgramBundle
			var reviewedRevision model.ProgramRevision
			if canonical.Program != nil {
				if e = intakeSubjectOwnerTx(ctx, tx, op.ID, "program", canonical.Program.ID); e != nil {
					return out, e
				}
				reviewedBundle, e = s.intakeDraftBundleTx(ctx, tx, r, *canonical.Program, d.BundleManifestSHA)
				if e != nil {
					return out, e
				}
				revisionTime := claims.IssuedAt
				if template != nil && template.ProgramRevision != nil {
					revisionTime = template.ProgramRevision.CreatedAt
				}
				reviewedRevision, e = intakePredictProgramTx(ctx, tx, *canonical.Program, reviewedBundle, revisionTime)
				if e != nil {
					return out, e
				}
				if e = programRevisionDisclosure(reviewedRevision, reviewedBundle); e != nil {
					return out, e
				}
				canonical.DomainRevision = reviewedRevision.RevisionHash
			} else if d.BundleManifestSHA != "" {
				return out, programError("invalid_contract", "only program drafts can select a seed bundle")
			}
			if canonical.SourceSHA != d.SourceSHA || canonical.CanonicalSHA != d.CanonicalSHA || canonical.DomainRevision != d.DomainRevision {
				return out, programError("stale_revision", "canonical draft changed")
			}
			if d.StepID != "" {
				step, e := intakeStepLoad(ctx, tx, "id", d.StepID)
				if e != nil {
					return out, e
				}
				if step.RequestID != r.ID || step.ContentVersion != r.ContentVersion || step.OutputSHA != d.OutputSHA || step.InputManifestSHA != d.InputManifestSHA || step.Superseded || step.State != "complete" {
					return out, programError("stale_revision", "draft producer binding changed")
				}
				if _, e = s.blobs.Read(ctx, d.OutputSHA, int64(intakeLimits(cfg).MaxOutputBytes)); e != nil {
					return out, e
				}
			}
			manifest.Draft = &d
			manifest.SourceSHA = canonical.SourceSHA
			manifest.CanonicalSHA = canonical.CanonicalSHA
			manifest.DomainRevision = canonical.DomainRevision
			claims.DraftRevision = d.Revision
			claims.SubjectKind = d.Route
			claims.SubjectRevision = d.DomainRevision
			claims.CanonicalPayloadSHA = canonical.CanonicalSHA
			out.CanonicalPayload = canonical.CanonicalPayload
			out.CanonicalSource = d.CanonicalSource
			out.Feature = canonical.Feature
			out.Findings = d.Findings
			if canonical.Feature != nil {
				if e = intakeSubjectOwnerTx(ctx, tx, op.ID, d.Route, canonical.Feature.ID); e != nil {
					return out, e
				}
				claims.SubjectID = canonical.Feature.ID
				manifest.Feature = canonical.Feature
				existing, e := get(ctx, tx, canonical.Feature.ID)
				if e == nil {
					if existing.Project != r.Project || existing.Revision != canonical.Feature.Revision {
						return out, programError("stale_subject", "feature ID already belongs to a different immutable contract")
					}
					if existing.State == "cancelled" || existing.State == "cancelling" {
						return out, programError("stale_subject", "cancelled feature cannot be implicitly resubmitted")
					}
					if _, e = intakeSubjectScopeTx(ctx, tx, op.ID, d.Route, existing.ID); e != nil {
						return out, e
					}
					manifest.Feature = &existing
				} else if !errors.Is(e, sql.ErrNoRows) {
					return out, e
				}
				if e = validateFeatureDependenciesTx(ctx, tx, *canonical.Feature); e != nil {
					return out, programError("invalid_contract", e.Error())
				}
				if e = s.programFeatureGateTx(ctx, tx, *canonical.Feature, cfg.PolicyHash); e != nil {
					return out, e
				}
				if gate := canonical.Feature.ProgramEvidenceGate; gate != nil {
					if _, e = intakeSubjectScopeTx(ctx, tx, op.ID, "program", gate.ProgramID); e != nil {
						return out, e
					}
					if e = s.intakeReviewEvidenceTx(ctx, tx, gate.ProgramID, gate.Revision, gate.ManifestSHA, &manifest, map[string]bool{}); e != nil {
						return out, e
					}
				}
			} else {
				bundle, revision := reviewedBundle, reviewedRevision
				claims.SubjectID = revision.ProgramID
				out.ProgramRevision = &revision
				manifest.ProgramRevision = &revision
				if bundle.ManifestSHA != "" {
					out.SeedManifest = &bundle
					manifest.SeedManifest = &bundle
					manifest.BundleManifestSHAs = append(manifest.BundleManifestSHAs, bundle.ManifestSHA)
					manifest.Evidence = append(manifest.Evidence, model.ProgramInputRef{Kind: "bundle", ID: bundle.ManifestSHA, SHA: bundle.ManifestSHA})
				}
				current, e := programLoad(ctx, tx, revision.ProgramID)
				if e == nil && current.CurrentRevision == revision.Number {
					view, e := s.intakeSubjectViewTx(ctx, tx, op.ID, "program", revision.ProgramID)
					if e != nil {
						return out, e
					}
					out.Answers = view.Answers
					out.Artifacts = view.Artifacts
					manifest.Answers = view.Answers
					manifest.Artifacts = view.Artifacts
					current.CreatedAt = time.Time{}
					current.UpdatedAt = time.Time{}
					manifest.Program = &current
					manifest.Evidence = append(manifest.Evidence, model.ProgramInputRef{Kind: "program_revision", ID: revision.RevisionHash, SHA: revision.RevisionHash, Revision: revision.Number})
					for _, answer := range view.Answers {
						sha := model.ProgramAnswerSHA(answer)
						manifest.Evidence = append(manifest.Evidence, model.ProgramInputRef{Kind: "answer", ID: sha, SHA: sha})
					}
				} else if e != nil && !errors.Is(e, sql.ErrNoRows) {
					return out, e
				}
			}
		default:
			return out, programError("unsupported_action", "unsupported request authority")
		}
	} else {
		kind, id, revision := "", "", ""
		switch p := payload.(type) {
		case *model.SubjectCommandPayload:
			kind, id, revision = p.Kind, p.ID, p.Revision
		case *model.ProgramArtifactCommandPayload:
			kind, id, revision = "program", p.ProgramID, p.Revision
		default:
			return out, programError("unsupported_action", "unsupported authority subject")
		}
		view, e := s.intakeSubjectViewTx(ctx, tx, op.ID, kind, id)
		if e != nil {
			return out, e
		}
		if project, ok := cfg.Projects[view.Project]; !ok || !project.Trusted {
			return out, ErrProjectForbidden
		}
		if view.Revision != revision {
			return out, programError("stale_revision", "subject revision changed")
		}
		claims.SubjectKind = kind
		claims.SubjectID = id
		claims.SubjectRevision = revision
		out.Feature = view.Feature
		out.ProgramRevision = view.ProgramRevision
		out.Answers = view.Answers
		out.SeedManifest = view.SeedManifest
		out.Artifacts = view.Artifacts
		manifest.Feature = view.Feature
		manifest.ProgramRevision = view.ProgramRevision
		manifest.Answers = view.Answers
		manifest.SeedManifest = view.SeedManifest
		manifest.Artifacts = view.Artifacts
		manifest.PlanningQuestions = view.PlanningQuestions
		manifest.PlanningAnswers = view.PlanningAnswers
		if view.Feature != nil {
			source, e := intake.Export(*view.Feature)
			if e != nil {
				return out, e
			}
			canonical, e := intake.ParseCanonical(kind, model.IntakeFormatFeature, source, view.Project, cfg)
			if e != nil {
				return out, e
			}
			if canonical.DomainRevision != view.Feature.Revision {
				return out, programError("evidence_invalid", "feature source identity differs")
			}
			out.CanonicalSource = string(source)
			out.CanonicalPayload = string(source)
			manifest.SourceSHA = intake.SHA256(source)
			manifest.CanonicalSHA = canonical.CanonicalSHA
			manifest.DomainRevision = view.Feature.Revision
			claims.CanonicalPayloadSHA = canonical.CanonicalSHA
			if e = s.programFeatureGateTx(ctx, tx, *view.Feature, cfg.PolicyHash); e != nil {
				return out, e
			}
			if gate := view.Feature.ProgramEvidenceGate; gate != nil {
				if _, e = intakeSubjectScopeTx(ctx, tx, op.ID, "program", gate.ProgramID); e != nil {
					return out, e
				}
				claims.InputManifestSHA = gate.ManifestSHA
				if e = s.intakeReviewEvidenceTx(ctx, tx, gate.ProgramID, gate.Revision, gate.ManifestSHA, &manifest, map[string]bool{}); e != nil {
					return out, e
				}
			}
			if command.Action == model.ActionSubjectRetry {
				if view.Feature.PolicyHash != cfg.PolicyHash {
					return out, programError("stale_policy", "fresh approval under current policy is required")
				}
				if view.Feature.State != "blocked" && view.Feature.State != "cancelled" {
					return out, programError("stale_subject", "only blocked or cancelled subjects can retry")
				}
				var active int
				if e = tx.QueryRowContext(ctx, `SELECT count(*) FROM attempts WHERE feature_id=? AND active=1`, id).Scan(&active); e != nil {
					return out, e
				}
				if active != 0 {
					return out, programError("attempt_unsettled", "feature attempt is not settled")
				}
			}
		} else {
			var disclosedBundle model.ProgramBundle
			if view.SeedManifest != nil {
				disclosedBundle = *view.SeedManifest
			}
			if e = programRevisionDisclosure(*view.ProgramRevision, disclosedBundle); e != nil {
				return out, e
			}
			p := *view.Program
			p.CreatedAt = time.Time{}
			p.UpdatedAt = time.Time{}
			manifest.Program = &p
			source, e := json.Marshal(view.ProgramRevision.Spec)
			if e != nil {
				return out, e
			}
			out.CanonicalSource = string(source)
			out.CanonicalPayload = string(source)
			manifest.SourceSHA = intake.SHA256(source)
			manifest.CanonicalSHA = intake.SHA256(source)
			manifest.DomainRevision = revision
			claims.CanonicalPayloadSHA = manifest.CanonicalSHA
			manifest.Evidence = append(manifest.Evidence, model.ProgramInputRef{Kind: "program_revision", ID: revision, SHA: revision, Revision: view.ProgramRevision.Number})
			if view.SeedManifest != nil {
				manifest.BundleManifestSHAs = append(manifest.BundleManifestSHAs, view.SeedManifest.ManifestSHA)
				for _, f := range view.SeedManifest.Files {
					if e = s.intakeVerifyBundleFile(ctx, f, false); e != nil {
						return out, e
					}
				}
			}
			for _, answer := range view.Answers {
				sha := model.ProgramAnswerSHA(answer)
				manifest.Evidence = append(manifest.Evidence, model.ProgramInputRef{Kind: "answer", ID: sha, SHA: sha})
			}
			if command.Action == model.ActionProgramResume {
				if e = s.intakeResumeReviewTx(ctx, tx, *view.Program, *view.ProgramRevision, cfg, template, &claims, &out, &manifest); e != nil {
					return out, e
				}
			}
			if command.Action == model.ActionProgramApproveArchitecture || command.Action == model.ActionProgramApproveExperiment {
				selection := payload.(*model.ProgramArtifactCommandPayload)
				a, e := programArtifactTx(ctx, tx, selection.ArtifactID)
				if e != nil {
					return out, programReadError(e)
				}
				if a.ProgramID != id || a.Revision != view.ProgramRevision.Number || a.Kind != selection.ArtifactKind || a.ContentSHA != selection.ArtifactSHA {
					return out, programError("stale_subject", "artifact identity or scope changed")
				}
				state, e := programArtifactOutputState(ctx, tx, *view.Program, a)
				if e != nil {
					return out, e
				}
				if !state.CurrentStageOutput {
					return out, programError("stale_subject", "artifact is not the current stage output")
				}
				ref := model.ProgramInputRef{Kind: a.Kind, ID: a.ID, SHA: a.ContentSHA, Revision: a.Revision}
				if e = programValidateRefTx(ctx, tx, id, a.Revision, ref); e != nil {
					return out, e
				}
				if e = programManifestValidTx(ctx, tx, id, a.Revision, a.InputManifestSHA, map[string]bool{}); e != nil {
					return out, e
				}
				raw, e := s.blobs.Read(ctx, a.ContentSHA, 8<<20)
				if e != nil {
					return out, e
				}
				manifest.SelectedArtifact = &a
				out.SelectedArtifactCanonical = string(raw)
				manifest.Evidence = append(manifest.Evidence, ref)
				claims.InputManifestSHA = a.InputManifestSHA
				if e = s.intakeReviewEvidenceTx(ctx, tx, id, a.Revision, a.InputManifestSHA, &manifest, map[string]bool{}); e != nil {
					return out, e
				}
				if command.Action == model.ActionProgramApproveArchitecture {
					if selection.SubjectSHA != a.ContentSHA {
						return out, programError("invalid_contract", "architecture subject hash differs")
					}
					claims.CanonicalPayloadSHA = a.ContentSHA
					out.CanonicalPayload = string(raw)
				} else {
					var contracts model.ExperimentContractsPayload
					if e = model.StrictDecode(raw, &contracts); e != nil {
						return out, e
					}
					for _, contract := range contracts.Experiments {
						material, e := model.ExperimentContractCanonical(contract)
						if e != nil {
							return out, e
						}
						if intake.SHA256(material) == selection.SubjectSHA {
							if manifest.SelectedExperiment != nil {
								return out, programError("invalid_contract", "duplicate experiment identity")
							}
							selected := contract
							manifest.SelectedExperiment = &selected
							out.CanonicalPayload = string(material)
						}
					}
					if manifest.SelectedExperiment == nil || manifest.SelectedExperiment.BundleManifestSHA == "" {
						return out, programError("invalid_contract", "selected frozen experiment is missing")
					}
					if e = programInvalidTx(ctx, tx, id, selection.SubjectSHA); e != nil {
						return out, e
					}
					bundle, e := s.blobs.LoadBundle(ctx, manifest.SelectedExperiment.BundleManifestSHA)
					if e != nil {
						return out, e
					}
					for _, f := range bundle.Files {
						if e = s.intakeVerifyBundleFile(ctx, f, false); e != nil {
							return out, e
						}
					}
					manifest.BundleManifestSHAs = append(manifest.BundleManifestSHAs, bundle.ManifestSHA)
					claims.CanonicalPayloadSHA = selection.SubjectSHA
					claims.ExecutionPolicySHA = model.ExperimentPolicySHA(cfg.Discovery.ExperimentProfiles, cfg.Discovery.ExperimentLimits)
					manifest.ExecutionPolicySHA = claims.ExecutionPolicySHA
					var frozen model.DiscoveryPolicy
					var rawPolicy []byte
					e = tx.QueryRowContext(ctx, `SELECT data FROM program_policies WHERE program_id=? AND revision=? AND policy_hash=?`, id, a.Revision, cfg.PolicyHash).Scan(&rawPolicy)
					if errors.Is(e, sql.ErrNoRows) {
						return out, programError("stale_policy", "experiment has no frozen execution policy")
					}
					if e != nil {
						return out, e
					}
					if e = json.Unmarshal(rawPolicy, &frozen); e != nil {
						return out, e
					}
					if model.ExperimentPolicySHA(frozen.ExperimentProfiles, frozen.ExperimentLimits) != claims.ExecutionPolicySHA {
						return out, programError("stale_policy", "experiment execution policy changed")
					}
				}
			}
		}
	}
	if len(manifest.Evidence) > 4096 {
		return out, programError("input_limit", "review evidence exceeds limit")
	}
	sort.Slice(manifest.Evidence, func(i, j int) bool {
		a, b := manifest.Evidence[i], manifest.Evidence[j]
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		if a.ID != b.ID {
			return a.ID < b.ID
		}
		if a.SHA != b.SHA {
			return a.SHA < b.SHA
		}
		return a.Revision < b.Revision
	})
	evidence := manifest.Evidence[:0]
	for _, ref := range manifest.Evidence {
		if len(evidence) == 0 || evidence[len(evidence)-1] != ref {
			evidence = append(evidence, ref)
		}
	}
	manifest.Evidence = evidence
	sort.Strings(manifest.BundleManifestSHAs)
	bundles := manifest.BundleManifestSHAs[:0]
	for _, sha := range manifest.BundleManifestSHAs {
		if len(bundles) == 0 || bundles[len(bundles)-1] != sha {
			bundles = append(bundles, sha)
		}
	}
	manifest.BundleManifestSHAs = bundles
	claims.Evidence = manifest.Evidence
	claims.BundleManifestSHAs = manifest.BundleManifestSHAs
	manifestRaw, err := json.Marshal(manifest)
	if err != nil {
		return out, err
	}
	out.ManifestCanonical = string(manifestRaw)
	claims.ReviewManifestSHA = intake.SHA256(manifestRaw)
	out.Claims = claims
	claimsRaw, err := json.Marshal(claims)
	if err != nil {
		return out, err
	}
	out.ClaimsCanonical = string(claimsRaw)
	out.ClaimsSHA = intake.SHA256(claimsRaw)
	return out, nil
}

// Legacy persisted reviews may have no material. Once any material is supplied,
// require a complete, exact snapshot rather than accepting partially bound data.
func validateIntakeReviewMaterial(r model.IntakeReview) error {
	if r.ResumeWork == nil && r.ClaimsCanonical == "" && r.ManifestCanonical == "" && r.CommandCanonical == "" && r.CanonicalPayload == "" && r.SelectedArtifactCanonical == "" {
		return nil
	}
	invalid := func() error { return errors.New("review canonical material integrity mismatch") }
	exact := func(raw string, value any) bool {
		b, err := json.Marshal(value)
		return err == nil && bytes.Equal(b, []byte(raw))
	}
	if r.ClaimsCanonical == "" || r.ManifestCanonical == "" || r.CommandCanonical == "" || r.CanonicalPayload == "" ||
		!exact(r.ClaimsCanonical, r.Claims) || intake.SHA256([]byte(r.ClaimsCanonical)) != r.ClaimsSHA ||
		intake.SHA256([]byte(r.ManifestCanonical)) != r.Claims.ReviewManifestSHA ||
		intake.SHA256([]byte(r.CanonicalPayload)) != r.Claims.CanonicalPayloadSHA {
		return invalid()
	}
	commandRaw, err := intake.CommandCanonical(r.Command)
	if err != nil || !bytes.Equal(commandRaw, []byte(r.CommandCanonical)) {
		return invalid()
	}
	var manifest intakeReviewManifest
	if err = model.StrictDecode([]byte(r.ManifestCanonical), &manifest); err != nil || !exact(r.ManifestCanonical, manifest) ||
		manifest.CommandSHA != intake.SHA256(commandRaw) || manifest.PolicySHA != r.Claims.PolicySHA ||
		manifest.ExecutionPolicySHA != r.Claims.ExecutionPolicySHA {
		return invalid()
	}
	same := func(a, b any) bool {
		raw, err := json.Marshal(a)
		return err == nil && exact(string(raw), b)
	}
	if !same(r.ProgramRevision, manifest.ProgramRevision) || !same(r.SeedManifest, manifest.SeedManifest) ||
		((len(r.Answers) != 0 || len(manifest.Answers) != 0) && !same(r.Answers, manifest.Answers)) ||
		((len(r.Artifacts) != 0 || len(manifest.Artifacts) != 0) && !same(r.Artifacts, manifest.Artifacts)) {
		return invalid()
	}
	if err = intakeValidateResumeMaterial(r, manifest); err != nil {
		return err
	}
	if (r.Feature == nil) != (manifest.Feature == nil) {
		return invalid()
	}
	if r.Feature != nil {
		presented, e := intake.Export(*r.Feature)
		bound, boundErr := intake.Export(*manifest.Feature)
		if e != nil || boundErr != nil || !bytes.Equal(presented, bound) {
			return invalid()
		}
	}
	evidence, err := json.Marshal(r.Claims.Evidence)
	if err != nil || !exact(string(evidence), manifest.Evidence) {
		return invalid()
	}
	bundles, err := json.Marshal(r.Claims.BundleManifestSHAs)
	if err != nil || !exact(string(bundles), manifest.BundleManifestSHAs) {
		return invalid()
	}
	if (r.CanonicalSource != "" || manifest.SourceSHA != "") && intake.SHA256([]byte(r.CanonicalSource)) != manifest.SourceSHA {
		return invalid()
	}
	if manifest.SelectedArtifact != nil {
		if r.SelectedArtifactCanonical == "" || intake.SHA256([]byte(r.SelectedArtifactCanonical)) != manifest.SelectedArtifact.ContentSHA {
			return invalid()
		}
		payload, err := intake.CommandPayload(r.Command)
		if err != nil {
			return invalid()
		}
		selection, ok := payload.(*model.ProgramArtifactCommandPayload)
		a := manifest.SelectedArtifact
		resumeExperiment := r.Command.Action == model.ActionProgramResume && r.ResumeWork != nil && r.ResumeWork.StepKind == model.StepExperimentRun
		if resumeExperiment {
			if a.Kind != model.ArtifactExperimentContracts || r.Claims.InputManifestSHA != r.ResumeWork.InputManifestSHA || r.Claims.CanonicalPayloadSHA != r.ResumeWork.SubjectID {
				return invalid()
			}
		} else if !ok || selection.ArtifactID != a.ID || selection.ArtifactKind != a.Kind || selection.ArtifactSHA != a.ContentSHA || selection.SubjectSHA != r.Claims.CanonicalPayloadSHA || r.Claims.InputManifestSHA != a.InputManifestSHA {
			return invalid()
		}
		if manifest.SelectedExperiment != nil {
			material, err := model.ExperimentContractCanonical(*manifest.SelectedExperiment)
			if err != nil || !bytes.Equal(material, []byte(r.CanonicalPayload)) {
				return invalid()
			}
			var envelope model.ExperimentContractsPayload
			if err = model.StrictDecode([]byte(r.SelectedArtifactCanonical), &envelope); err != nil {
				return invalid()
			}
			matches := 0
			for _, contract := range envelope.Experiments {
				material, err := model.ExperimentContractCanonical(contract)
				if err != nil {
					return invalid()
				}
				if bytes.Equal(material, []byte(r.CanonicalPayload)) {
					matches++
				}
			}
			if matches != 1 || (r.Command.Action != model.ActionProgramApproveExperiment && !resumeExperiment) {
				return invalid()
			}
		} else if r.Command.Action != model.ActionProgramApproveArchitecture || r.CanonicalPayload != r.SelectedArtifactCanonical {
			return invalid()
		}
	} else {
		if r.SelectedArtifactCanonical != "" {
			return invalid()
		}
		switch {
		case r.Command.Action == model.ActionIntakeRetry:
			if manifest.Request == nil || !exact(r.CanonicalPayload, manifest.Request) {
				return invalid()
			}
		case r.Feature != nil:
			source, err := intake.Export(*r.Feature)
			if err != nil || !bytes.Equal(source, []byte(r.CanonicalPayload)) {
				return invalid()
			}
		case r.ProgramRevision != nil:
			if !exact(r.CanonicalPayload, r.ProgramRevision.Spec) {
				return invalid()
			}
		default:
			return invalid()
		}
	}
	return nil
}

type IntakeExperimentSubject struct {
	ID         string `json:"id"`
	SubjectSHA string `json:"subject_sha"`
}
type IntakeSubjectArtifactDetail struct {
	ProgramArtifactDetail
	CanonicalSource    string                    `json:"canonical_source"`
	ExperimentSubjects []IntakeExperimentSubject `json:"experiment_subjects,omitempty"`
}

func (s *Store) IntakeSubjectArtifact(ctx context.Context, operatorID, kind, id, artifactID string) (IntakeSubjectArtifactDetail, error) {
	var out IntakeSubjectArtifactDetail
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	if _, err = intakeSubjectScopeTx(ctx, tx, operatorID, kind, id); err != nil {
		return out, err
	}
	if kind != "program" {
		return out, ErrNotFound
	}
	a, err := programArtifactTx(ctx, tx, artifactID)
	if err != nil {
		return out, programReadError(err)
	}
	if a.ProgramID != id {
		return out, ErrNotFound
	}
	out.Artifact = a
	out.Payload, err = s.blobs.Read(ctx, a.ContentSHA, 8<<20)
	if err != nil {
		return out, err
	}
	out.CanonicalSource = string(out.Payload)
	p, err := programLoad(ctx, tx, id)
	if err != nil {
		return out, err
	}
	out.OutputState, err = programArtifactOutputState(ctx, tx, p, a)
	if err != nil {
		return out, err
	}
	out.Snapshot, err = programReadSnapshot(ctx, tx, id)
	if err != nil {
		return out, err
	}
	if a.Kind == model.ArtifactExperimentContracts {
		var payload model.ExperimentContractsPayload
		if err = model.StrictDecode(out.Payload, &payload); err != nil {
			return out, err
		}
		for _, contract := range payload.Experiments {
			material, err := model.ExperimentContractCanonical(contract)
			if err != nil {
				return out, err
			}
			out.ExperimentSubjects = append(out.ExperimentSubjects, IntakeExperimentSubject{ID: contract.ID, SubjectSHA: intake.SHA256(material)})
		}
	}
	return out, tx.Commit()
}
func (s *Store) IntakeSubjectArtifacts(ctx context.Context, operatorID, kind, id, after string, limit int) (ProgramArtifactPage, error) {
	out := ProgramArtifactPage{Items: []model.ProgramArtifact{}, OutputStates: []ProgramArtifactOutputState{}}
	if kind != "program" {
		tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
		if err != nil {
			return out, err
		}
		defer tx.Rollback()
		if _, err = intakeSubjectScopeTx(ctx, tx, operatorID, kind, id); err != nil {
			return out, err
		}
		if after != "" {
			return out, ErrInvalidQuery
		}
		return out, tx.Commit()
	}
	tx, r, c, l, err := programReadStart(ctx, s, id, "intake_artifacts", ProgramQuery{After: after, Limit: limit})
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	if _, err = intakeSubjectScopeTx(ctx, tx, operatorID, kind, id); err != nil {
		return out, err
	}
	out.Snapshot = c.Snapshot
	out.RevisionHash = r.RevisionHash
	items, err := intakeList[model.ProgramArtifact](ctx, tx, `SELECT data FROM program_artifacts WHERE program_id=? AND revision=? AND id>? ORDER BY id LIMIT ?`, id, r.Number, c.After, l+1)
	if err != nil {
		return out, err
	}
	if len(items) > l {
		items = items[:l]
		c.After = items[len(items)-1].ID
		out.NextCursor = programReadEncode(c)
	}
	out.Items = items
	p, err := programLoad(ctx, tx, id)
	if err != nil {
		return out, err
	}
	for _, a := range items {
		state, e := programArtifactOutputState(ctx, tx, p, a)
		if e != nil {
			return out, e
		}
		out.OutputStates = append(out.OutputStates, state)
	}
	return out, tx.Commit()
}

func (s *Store) IntakeSubjectProject(ctx context.Context, operatorID, kind, id string) (string, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	project, err := intakeSubjectScopeTx(ctx, tx, operatorID, kind, id)
	if err != nil {
		return "", err
	}
	return project, tx.Commit()
}
