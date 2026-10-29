package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"openfactory/internal/intake"
	"openfactory/internal/model"
)

func programStepLoad(ctx context.Context, q programQuerier, column, id string) (model.ProgramStep, error) {
	var s model.ProgramStep
	var b []byte
	err := q.QueryRowContext(ctx, `SELECT data FROM program_steps WHERE `+column+`=?`, id).Scan(&b)
	if err == nil {
		err = json.Unmarshal(b, &s)
	}
	return s, err
}
func (s *Store) ProgramStepForFeature(ctx context.Context, id string) (model.ProgramStep, error) {
	return programStepLoad(ctx, s.db, "feature_id", id)
}
func programStepIdentity(v model.ProgramStepSpec) string {
	return programHash([]any{v.ProgramID, v.Revision, v.Iteration, v.Kind, v.SubjectID, v.InputManifestSHA})
}
func programLower(spec, policy int) int {
	if spec > 0 && spec < policy {
		return spec
	}
	return policy
}
func programStepSpec(v model.ProgramStep) model.ProgramStepSpec {
	return model.ProgramStepSpec{ProgramID: v.ProgramID, Revision: v.Revision, Iteration: v.Iteration, Kind: v.Kind, SubjectID: v.SubjectID, InputManifestSHA: v.InputManifestSHA, Model: v.Model}
}
func programPhaseForStep(kind string) string {
	if kind == model.StepExperimentRun {
		return "experiments"
	}
	return kind
}

func (s *Store) EnsureProgramStep(ctx context.Context, v model.ProgramStepSpec, cfg model.Config) (model.ProgramStep, error) {
	var out model.ProgramStep
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	p, r, err := s.programAuthorityTx(ctx, tx, v.ProgramID, v.Revision, cfg.PolicyHash)
	if err != nil {
		return out, err
	}
	id := programStepIdentity(v)
	out, err = programStepLoad(ctx, tx, "id", id)
	if err == nil {
		return out, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return out, err
	}
	items, err := s.programWorkTx(ctx, tx, p, cfg)
	if err != nil {
		return out, err
	}
	allowed := false
	for _, w := range items {
		if w.Step != nil && programStepIdentity(*w.Step) == programStepIdentity(v) && w.Wait == nil {
			allowed = true
		}
	}
	if !allowed {
		return out, programError("approval_required", "step is not current authorized program work")
	}
	if _, ok := cfg.Projects[p.Project]; !ok {
		return out, programError("prerequisite_missing", "program project is not configured")
	}
	selected := cfg.AstraModel
	if selected == "" {
		return out, programError("prerequisite_missing", "discovery analysis model is not configured")
	}
	if v.Model != "" && v.Model != selected {
		return out, programError("invalid_contract", "step model differs from configured discovery role")
	}
	if !programModelAllowed(r.Spec, selected) {
		return out, programError("approval_required", "model provider is not disclosed")
	}
	b, err := getProgramBudgetTx(ctx, tx, p.ID)
	if err != nil {
		return out, err
	}
	limits := model.NormalizeDiscoveryPolicy(cfg.Discovery).Limits
	if b.Steps >= programLower(r.Spec.Limits.MaxSteps, limits.MaxSteps) {
		return out, programError("budget_exhausted", "program step budget exhausted")
	}
	if v.Kind == model.StepExperimentRun && b.Experiments >= programLower(r.Spec.Limits.MaxExperiments, limits.MaxExperiments) {
		return out, programError("budget_exhausted", "program experiment budget exhausted")
	}
	policyBytes, _ := json.Marshal(model.NormalizeDiscoveryPolicy(cfg.Discovery))
	var storedPolicy []byte
	err = tx.QueryRowContext(ctx, `SELECT data FROM program_policies WHERE program_id=? AND revision=? AND policy_hash=?`, p.ID, r.Number, cfg.PolicyHash).Scan(&storedPolicy)
	if err == nil && string(storedPolicy) != string(policyBytes) {
		return out, programError("policy_changed", "discovery policy differs under the same policy hash")
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return out, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO program_policies(program_id,revision,policy_hash,data) VALUES(?,?,?,?)`, p.ID, r.Number, cfg.PolicyHash, policyBytes); err != nil {
		return out, err
	}
	now := time.Now().UTC()
	out = model.ProgramStep{ID: id, ProgramID: p.ID, Revision: v.Revision, Iteration: v.Iteration, Kind: v.Kind, SubjectID: v.SubjectID, InputManifestSHA: v.InputManifestSHA, FeatureID: "openfactory-discovery-" + id, State: "ready", Model: selected, CreatedAt: now, UpdatedAt: now}
	f := model.Feature{ID: out.FeatureID, Project: p.Project, Title: p.Title + " / " + v.Kind, Kind: "research", Risk: "normal", CompletionTarget: "reviewed", DiscoveryStepID: id, PolicyHash: cfg.PolicyHash, State: "ready", CreatedAt: now, Body: "Server-owned discovery step " + id}
	f.Acceptance = []model.Acceptance{{ID: "step-output", Scenario: "Execute " + v.Kind + " against immutable input manifest " + v.InputManifestSHA, Expected: "The controller validates the role's complete typed artifact set, exact evidence references, and producer identity before atomically completing the step."}}
	if v.Kind == model.StepExperimentRun {
		f.Acceptance[0].Expected = "The controller verifies an actual conclusive runner receipt bound to this attempt, frozen experiment contract " + v.SubjectID + ", and approved bundle before atomically completing the step."
	}
	f.Revision = intake.Revision(f)
	p.Iteration = v.Iteration
	p.Phase = programPhaseForStep(v.Kind)
	p.State = model.ProgramActive
	p.Wait = nil
	if err = programPut(ctx, tx, p); err != nil {
		return out, err
	}
	raw, _ := json.Marshal(out)
	if _, err = tx.ExecContext(ctx, `INSERT INTO program_steps(id,program_id,revision,feature_id,data) VALUES(?,?,?,?,?)`, id, p.ID, v.Revision, f.ID, raw); err != nil {
		return out, err
	}
	if err = put(ctx, tx, f); err != nil {
		return out, err
	}
	b.Steps++
	if v.Kind == model.StepExperimentRun {
		b.Experiments++
	}
	if err = putProgramBudgetTx(ctx, tx, p.ID, b); err != nil {
		return out, err
	}
	return out, tx.Commit()
}

func (s *Store) ProgramStepInputs(ctx context.Context, id string) (model.ProgramWorkItem, error) {
	var out model.ProgramWorkItem
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	step, err := programStepLoad(ctx, tx, "id", id)
	if err != nil {
		return out, err
	}
	out.Program, err = programLoad(ctx, tx, step.ProgramID)
	if err != nil {
		return out, err
	}
	out.Revision, err = programRevision(ctx, tx, step.ProgramID, step.Revision)
	if err != nil {
		return out, err
	}
	var raw []byte
	if err = tx.QueryRowContext(ctx, `SELECT data FROM program_manifests WHERE sha=? AND program_id=? AND revision=?`, step.InputManifestSHA, step.ProgramID, step.Revision).Scan(&raw); err != nil {
		return out, err
	}
	if err = json.Unmarshal(raw, &out.Manifest); err != nil {
		return out, err
	}
	v := programStepSpec(step)
	out.Step = &v
	return out, tx.Commit()
}

// RecordProgramWait binds a coordinator wait to the exact revision that produced it.
func (s *Store) RecordProgramWait(ctx context.Context, programID, revisionHash, code, detail string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	p, err := programLoad(ctx, tx, programID)
	if err != nil {
		return err
	}
	r, err := programRevision(ctx, tx, p.ID, p.CurrentRevision)
	if err != nil {
		return err
	}
	if r.RevisionHash != revisionHash {
		return programError("stale_revision", "wait belongs to an obsolete program revision")
	}
	if err = recordProgramWaitTx(ctx, tx, &p, r, code, detail); err != nil {
		return err
	}
	return tx.Commit()
}

func recordProgramWaitTx(ctx context.Context, tx *sql.Tx, p *model.Program, r model.ProgramRevision, code, detail string) error {
	if code == "" {
		return programError("invalid_contract", "program wait code is required")
	}
	if p.State == model.ProgramCancelled || p.State == model.ProgramProposed {
		return programError("stale_revision", "program is no longer accepting coordinator waits")
	}
	changed := p.Wait == nil || p.Wait.Code != code || p.Wait.Detail != detail
	p.Wait = &model.WaitReason{Code: code, Detail: detail}
	if p.State == model.ProgramActive || p.State == model.ProgramWaiting {
		p.State = model.ProgramWaiting
	}
	if err := programPut(ctx, tx, *p); err != nil {
		return err
	}
	if changed {
		payload, err := json.Marshal(struct {
			RevisionHash string `json:"revision_hash"`
			Code         string `json:"code"`
			Detail       string `json:"detail"`
		}{r.RevisionHash, code, detail})
		if err != nil {
			return err
		}
		return programEvent(ctx, tx, p.ID, "waiting", string(payload))
	}
	return nil
}

func (s *Store) ProgramWork(ctx context.Context, cfg model.Config) ([]model.ProgramWorkItem, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT data FROM programs ORDER BY id`)
	if err != nil {
		return nil, err
	}
	var programs []model.Program
	for rows.Next() {
		var b []byte
		var p model.Program
		if err = rows.Scan(&b); err != nil {
			rows.Close()
			return nil, err
		}
		if err = json.Unmarshal(b, &p); err != nil {
			rows.Close()
			return nil, err
		}
		programs = append(programs, p)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	var out []model.ProgramWorkItem
	for _, p := range programs {
		if p.State == model.ProgramCancelled || p.State == model.ProgramProposed {
			continue
		}
		items, e := s.programWorkTx(ctx, tx, p, cfg)
		if e != nil {
			var pe *model.ProgramError
			if !errors.As(e, &pe) {
				return nil, e
			}
			r, loadErr := programRevision(ctx, tx, p.ID, p.CurrentRevision)
			if loadErr != nil {
				return nil, loadErr
			}
			if waitErr := recordProgramWaitTx(ctx, tx, &p, r, pe.Code, pe.Message); waitErr != nil {
				return nil, waitErr
			}
			items = []model.ProgramWorkItem{{Program: p, Revision: r, Wait: p.Wait}}
		}
		out = append(out, items...)
	}
	return out, tx.Commit()
}

// programWorkTx persists the coordinator's derived manifest and wait bookkeeping.
// Review and resume selection use the same read-only graph without these writes.
func (s *Store) programWorkTx(ctx context.Context, tx *sql.Tx, p model.Program, cfg model.Config) ([]model.ProgramWorkItem, error) {
	items, err := s.programWorkSelectionTx(ctx, tx, p, cfg)
	if err != nil {
		return nil, err
	}
	// All pending branches share the one current evidence snapshot.
	if len(items) > 0 && items[0].Manifest.SHA != "" {
		if err = programManifestPutTx(ctx, tx, p.ID, items[0].Revision.Number, items[0].Manifest); err != nil {
			return nil, err
		}
	}
	waitRecorded := false
	for i := range items {
		w := &items[i]
		if w.Wait != nil {
			if !waitRecorded {
				if err = recordProgramWaitTx(ctx, tx, &p, w.Revision, w.Wait.Code, w.Wait.Detail); err != nil {
					return nil, err
				}
				waitRecorded = true
			}
			w.Program = p
			w.Step = nil
		}
	}
	return items, nil
}

// programWorkSelectionTx derives the fixed graph solely from immutable completed
// outputs. It performs no writes, including when current work is blocked.
func (s *Store) programWorkSelectionTx(ctx context.Context, tx *sql.Tx, p model.Program, cfg model.Config) ([]model.ProgramWorkItem, error) {
	r, err := programRevision(ctx, tx, p.ID, p.CurrentRevision)
	if err != nil {
		return nil, err
	}
	w := model.ProgramWorkItem{Program: p, Revision: r}
	wait := func(code, detail string) ([]model.ProgramWorkItem, error) {
		w.Wait = &model.WaitReason{Code: code, Detail: detail}
		return []model.ProgramWorkItem{w}, nil
	}
	if _, _, err = s.programAuthorityTx(ctx, tx, p.ID, p.CurrentRevision, cfg.PolicyHash); err != nil {
		var pe *model.ProgramError
		if errors.As(err, &pe) {
			return wait(pe.Code, pe.Message)
		}
		return nil, err
	}
	refs := []model.ProgramInputRef{{Kind: "program_revision", ID: r.RevisionHash, SHA: r.RevisionHash, Revision: r.Number}}
	if r.SeedManifestSHA != "" {
		refs = append(refs, model.ProgramInputRef{Kind: "bundle", ID: r.SeedManifestSHA, SHA: r.SeedManifestSHA, Revision: r.Number})
	}
	rows, err := tx.QueryContext(ctx, `SELECT data FROM program_artifacts WHERE program_id=? AND revision=? ORDER BY rowid`, p.ID, r.Number)
	if err != nil {
		return nil, err
	}
	var artifacts []model.ProgramArtifact
	for rows.Next() {
		var b []byte
		var a model.ProgramArtifact
		if err = rows.Scan(&b); err != nil {
			rows.Close()
			return nil, err
		}
		if err = json.Unmarshal(b, &a); err != nil {
			rows.Close()
			return nil, err
		}
		artifacts = append(artifacts, a)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	answerRevisions := map[string]bool{r.RevisionHash: true}
	ancestor := r
	for ancestor.Reason == "answer" && ancestor.Supersedes > 0 {
		ancestor, err = programRevision(ctx, tx, p.ID, ancestor.Supersedes)
		if err != nil {
			return nil, err
		}
		answerRevisions[ancestor.RevisionHash] = true
		rr, e := tx.QueryContext(ctx, `SELECT data FROM program_artifacts WHERE program_id=? AND revision=? AND kind=? ORDER BY rowid`, p.ID, ancestor.Number, model.ArtifactQuestions)
		if e != nil {
			return nil, e
		}
		for rr.Next() {
			var b []byte
			var a model.ProgramArtifact
			if e = rr.Scan(&b); e != nil {
				rr.Close()
				return nil, e
			}
			if e = json.Unmarshal(b, &a); e != nil {
				rr.Close()
				return nil, e
			}
			artifacts = append(artifacts, a)
		}
		e = rr.Err()
		rr.Close()
		if e != nil {
			return nil, e
		}
	}
	superseded := map[string]bool{}
	for _, a := range artifacts {
		if a.SupersedesID != "" {
			superseded[a.SupersedesID] = true
		}
	}
	selected := make([]model.ProgramArtifact, 0, len(artifacts))
	for _, a := range artifacts {
		if superseded[a.ID] {
			continue
		}
		ref := model.ProgramInputRef{Kind: a.Kind, ID: a.ID, SHA: a.ContentSHA, Revision: a.Revision}
		e := programValidateRefTx(ctx, tx, p.ID, r.Number, ref)
		if e == nil && a.InputManifestSHA != "" {
			e = programManifestValidTx(ctx, tx, p.ID, r.Number, a.InputManifestSHA, map[string]bool{})
		}
		if e != nil {
			var pe *model.ProgramError
			if errors.As(e, &pe) {
				continue
			}
			return nil, e
		}
		selected = append(selected, a)
		refs = append(refs, ref)
	}
	artifacts = selected
	sourceRows, e := tx.QueryContext(ctx, `SELECT data FROM program_sources WHERE program_id=? AND revision=? ORDER BY id`, p.ID, r.Number)
	if e != nil {
		return nil, e
	}
	var sources []model.SourceCapture
	for sourceRows.Next() {
		var b []byte
		var source model.SourceCapture
		if e = sourceRows.Scan(&b); e != nil {
			sourceRows.Close()
			return nil, e
		}
		if e = json.Unmarshal(b, &source); e != nil {
			sourceRows.Close()
			return nil, e
		}
		sources = append(sources, source)
	}
	e = sourceRows.Err()
	sourceRows.Close()
	if e != nil {
		return nil, e
	}
	for _, source := range sources {
		ref := model.ProgramInputRef{Kind: "source", ID: source.ID, SHA: source.TextSHA256, Revision: source.Revision}
		if e = programValidateRefTx(ctx, tx, p.ID, r.Number, ref); e != nil {
			var pe *model.ProgramError
			if errors.As(e, &pe) {
				continue
			}
			return nil, e
		}
		refs = append(refs, ref)
	}
	// Answers and actual runner receipts are part of the evidence snapshot.
	answered := map[string]bool{}
	for _, table := range []string{"program_answers", "program_experiments"} {
		query := `SELECT data FROM ` + table + ` WHERE program_id=? ORDER BY rowid DESC`
		args := []any{p.ID}
		rr, e := tx.QueryContext(ctx, query, args...)
		if e != nil {
			return nil, e
		}
		for rr.Next() {
			var b []byte
			if e = rr.Scan(&b); e != nil {
				rr.Close()
				return nil, e
			}
			if table == "program_answers" {
				var a model.QuestionAnswer
				if e = json.Unmarshal(b, &a); e != nil {
					rr.Close()
					return nil, e
				}
				key := a.QuestionArtifactSHA + "/" + a.QuestionID
				if !answerRevisions[a.RevisionHash] || answered[key] {
					continue
				}
				answered[key] = true
				refs = append(refs, model.ProgramInputRef{Kind: "answer", ID: model.ProgramAnswerSHA(a), SHA: model.ProgramAnswerSHA(a), Revision: r.Number})
			} else {
				var receipt model.ExperimentReceipt
				if e = json.Unmarshal(b, &receipt); e != nil {
					rr.Close()
					return nil, e
				}
				if receipt.Revision == r.Number && receipt.State == "completed" && receipt.Conclusive {
					refs = append(refs, model.ProgramInputRef{Kind: "experiment_receipt", ID: receipt.RunID, SHA: programHash(receipt), Revision: r.Number})
				}
			}
		}
		e = rr.Err()
		rr.Close()
		if e != nil {
			return nil, e
		}
	}
	manifest := programManifest(refs)
	w.Manifest = manifest
	rows, err = tx.QueryContext(ctx, `SELECT data FROM program_steps WHERE program_id=? AND revision=? ORDER BY rowid`, p.ID, r.Number)
	if err != nil {
		return nil, err
	}
	var steps []model.ProgramStep
	for rows.Next() {
		var b []byte
		var step model.ProgramStep
		if err = rows.Scan(&b); err != nil {
			rows.Close()
			return nil, err
		}
		if err = json.Unmarshal(b, &step); err != nil {
			rows.Close()
			return nil, err
		}
		steps = append(steps, step)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	iteration := p.Iteration
	for _, step := range steps {
		if step.Iteration > iteration {
			iteration = step.Iteration
		}
	}
	makeStep := func(kind, subject string) model.ProgramWorkItem {
		selected := manifest
		v := model.ProgramStepSpec{ProgramID: p.ID, Revision: r.Number, Iteration: iteration, Kind: kind, SubjectID: subject, InputManifestSHA: selected.SHA}
		o := w
		o.Manifest = selected
		o.Step = &v
		return o
	}
	done := func(kind string) *model.ProgramStep {
		for i := len(steps) - 1; i >= 0; i-- {
			if steps[i].Iteration == iteration && steps[i].Kind == kind {
				return &steps[i]
			}
		}
		return nil
	}
	for _, kind := range []string{model.StepResearch, model.StepArchitecture, model.StepPlanEvaluation, model.StepExperimentRun, model.StepResultEvaluation, model.StepContracts, model.StepContractEvaluation} {
		step := done(kind)
		if step != nil && step.State != "completed" && kind != model.StepExperimentRun {
			f, e := get(ctx, tx, step.FeatureID)
			if e != nil {
				return nil, e
			}
			o := w
			v := programStepSpec(*step)
			o.Step = &v
			if f.State == "blocked" || f.State == "cancelled" || step.State == "blocked" {
				o.Wait = &model.WaitReason{Code: "prerequisite_missing", Detail: "program step is blocked: " + f.Reason}
			}
			return []model.ProgramWorkItem{o}, nil
		}
		if step == nil && kind != model.StepExperimentRun {
			return []model.ProgramWorkItem{makeStep(kind, "")}, nil
		}
		if kind == model.StepResearch && step != nil && step.State == "completed" {
			var questions model.QuestionsPayload
			questionSHA := ""
			for _, artifact := range artifacts {
				if artifact.ProducerStepID == step.ID && artifact.Kind == model.ArtifactQuestions {
					raw, e := s.blobs.Read(ctx, artifact.ContentSHA, 8<<20)
					if e != nil {
						return nil, e
					}
					if e = json.Unmarshal(raw, &questions); e != nil {
						return nil, e
					}
					questionSHA = artifact.ContentSHA
				}
			}
			for _, question := range questions.Questions {
				if !question.Decisive || answered[questionSHA+"/"+question.ID] {
					continue
				}
				if question.Route == "operator" {
					return wait("question_unresolved", "decisive operator question requires a version-bound answer")
				}
				if question.Route == "source" {
					passes := 0
					for _, st := range steps {
						if st.Iteration == iteration && st.Kind == model.StepResearch {
							passes++
						}
					}
					if passes < 2 {
						return []model.ProgramWorkItem{makeStep(model.StepResearch, "pass-2")}, nil
					}
					return wait("question_unresolved", "decisive source question remains after two research passes")
				}
			}
		}
		if kind == model.StepPlanEvaluation || kind == model.StepResultEvaluation || kind == model.StepContractEvaluation {
			var evaluation model.EvaluationPayload
			found := false
			for _, a := range artifacts {
				if a.ProducerStepID == step.ID && a.Kind == model.ArtifactEvaluation {
					raw, e := s.blobs.Read(ctx, a.ContentSHA, 8<<20)
					if e != nil {
						return nil, e
					}
					if e = json.Unmarshal(raw, &evaluation); e != nil {
						return nil, e
					}
					found = true
				}
			}
			if !found {
				return nil, programError("evidence_invalid", "completed evaluation has no artifact")
			}
			if err = validateProgramResultVerdictTx(ctx, tx, *step, evaluation, cfg.PolicyHash); err != nil {
				return nil, err
			}
			if evaluation.Verdict != "accept" {
				if evaluation.Verdict == "revise" {
					limit := programLower(r.Spec.Limits.MaxIterations, model.NormalizeDiscoveryPolicy(cfg.Discovery).Limits.MaxIterations)
					var revisions int
					if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM program_steps WHERE program_id=? AND json_extract(data,'$.kind')='research' AND json_extract(data,'$.iteration')>0 AND json_extract(data,'$.subject_id')=''`, p.ID).Scan(&revisions); err != nil {
						return nil, err
					}
					if revisions >= limit {
						return wait("iteration_limit", "program lifetime replanning limit reached")
					}
					iteration++
					return []model.ProgramWorkItem{makeStep(model.StepResearch, "")}, nil
				}
				return wait("question_unresolved", "evaluation requires operator or additional evidence")
			}
		}
		if kind == model.StepExperimentRun {
			var contracts []model.ExperimentContract
			var architecture string
			for _, a := range artifacts {
				if a.Iteration != iteration {
					continue
				}
				if a.Kind == model.ArtifactArchitecture {
					architecture = a.ContentSHA
				}
				if a.Kind == model.ArtifactExperimentContracts {
					raw, e := s.blobs.Read(ctx, a.ContentSHA, 8<<20)
					if e != nil {
						return nil, e
					}
					var payload model.ExperimentContractsPayload
					if e = json.Unmarshal(raw, &payload); e != nil {
						return nil, e
					}
					contracts = payload.Experiments
				}
			}
			approved := func(subject string) bool {
				var n int
				e := tx.QueryRowContext(ctx, `SELECT count(*) FROM program_grants WHERE program_id=? AND revision_hash=? AND subject_hash=? AND policy_hash=?`, p.ID, r.RevisionHash, subject, cfg.PolicyHash).Scan(&n)
				return e == nil && n == 1
			}
			if architecture == "" || !approved(architecture) {
				return wait("approval_required", "exact architecture approval required")
			}
			var pending []model.ProgramWorkItem
			for _, contract := range contracts {
				sha := programHash(contract)
				if !approved(sha) {
					return wait("approval_required", "exact frozen experiment approval required")
				}
				if _, e := s.blobs.LoadBundle(ctx, contract.BundleManifestSHA); e != nil {
					return wait("prerequisite_missing", "frozen experiment bundle is unavailable")
				}
				if _, ok := cfg.Discovery.ExperimentProfiles[contract.Environment.Profile]; !ok {
					return wait("prerequisite_missing", "experiment profile is not configured")
				}
				complete := false
				for i := len(steps) - 1; i >= 0; i-- {
					st := steps[i]
					if st.Iteration != iteration || st.Kind != kind || st.SubjectID != sha {
						continue
					}
					complete = true
					if st.State != "completed" {
						f, e := get(ctx, tx, st.FeatureID)
						if e != nil {
							return nil, e
						}
						o := w
						v := programStepSpec(st)
						o.Step = &v
						if f.State == "blocked" || f.State == "cancelled" || st.State == "blocked" {
							o.Wait = &model.WaitReason{Code: "prerequisite_missing", Detail: "program step is blocked: " + f.Reason}
						}
						pending = append(pending, o)
					}
					break
				}
				if !complete {
					pending = append(pending, makeStep(kind, sha))
				}
			}
			if len(pending) > 0 {
				return pending, nil
			}
		}
	}
	w.Publish = true
	return []model.ProgramWorkItem{w}, nil
}

func expectedProgramArtifacts(kind string) []string {
	switch kind {
	case model.StepResearch:
		return []string{model.ArtifactRequirements, model.ArtifactQuestions, model.ArtifactClaims}
	case model.StepArchitecture:
		return []string{model.ArtifactArchitecture, model.ArtifactExperimentContracts}
	case model.StepPlanEvaluation, model.StepResultEvaluation, model.StepContractEvaluation:
		return []string{model.ArtifactEvaluation}
	case model.StepContracts:
		return []string{model.ArtifactImplementationContracts}
	case model.StepExperimentRun:
		return []string{}
	}
	return nil
}

func (s *Store) CompleteProgramStep(ctx context.Context, a model.Attempt, result model.ProgramStepResult) error {
	if result.Outcome != "completed" {
		return programError("invalid_contract", "step completion requires completed outcome")
	}
	canonical := make([][]byte, len(result.Artifacts))
	shas := make([]string, len(result.Artifacts))
	for i, d := range result.Artifacts {
		b, err := model.CanonicalProgramPayload(d.Kind, d.Payload)
		if err != nil {
			return programError("invalid_contract", err.Error())
		}
		if len(b) > 8<<20 {
			return programError("invalid_contract", "canonical artifact exceeds 8 MiB payload limit")
		}
		canonical[i] = b
		blob, err := s.blobs.Put(ctx, b)
		if err != nil {
			return err
		}
		shas[i] = blob.SHA
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	step, err := programStepLoad(ctx, tx, "feature_id", a.FeatureID)
	if err != nil {
		return err
	}
	f, err := get(ctx, tx, a.FeatureID)
	if err != nil {
		return err
	}
	expected := expectedProgramArtifacts(step.Kind)
	if expected == nil {
		return programError("invalid_contract", "unknown step kind")
	}
	if step.Kind == model.StepArchitecture && step.Iteration > 0 {
		expected = append(expected, model.ArtifactRevision)
	}
	if len(expected) != len(result.Artifacts) {
		return programError("invalid_contract", "step artifact set is incomplete")
	}
	seen := map[string]bool{}
	for _, d := range result.Artifacts {
		ok := false
		for _, k := range expected {
			if d.Kind == k {
				ok = true
			}
		}
		if !ok || seen[d.Kind] {
			return programError("invalid_contract", "unexpected or duplicate artifact kind")
		}
		seen[d.Kind] = true
	}
	var persisted model.Attempt
	var raw []byte
	var active int
	var revision, policy string
	if err = tx.QueryRowContext(ctx, `SELECT data,active,revision,policy_hash FROM attempts WHERE id=? AND feature_id=?`, a.ID, a.FeatureID).Scan(&raw, &active, &revision, &policy); err != nil {
		return err
	}
	if err = json.Unmarshal(raw, &persisted); err != nil {
		return err
	}
	if persisted.ThreadID != a.ThreadID || revision != f.Revision || policy != f.PolicyHash {
		return programError("evidence_invalid", "attempt identity or authority changed")
	}
	refs := make([]model.ProgramInputRef, 0, len(result.Artifacts))
	now := time.Now().UTC()
	artifacts := make([]model.ProgramArtifact, 0, len(result.Artifacts))
	for i, d := range result.Artifacts {
		id := programHash([]string{step.ID, d.Kind, shas[i], d.SupersedesID})
		artifact := model.ProgramArtifact{ID: id, ProgramID: step.ProgramID, Revision: step.Revision, Iteration: step.Iteration, Kind: d.Kind, SchemaVersion: 1, ContentSHA: shas[i], InputManifestSHA: step.InputManifestSHA, ProducerStepID: step.ID, SupersedesID: d.SupersedesID, CreatedAt: now, ProducerAttemptID: a.ID, ProducerThreadID: a.ThreadID}
		if d.Kind == model.ArtifactEvaluation {
			var evaluation model.EvaluationPayload
			if err = json.Unmarshal(canonical[i], &evaluation); err != nil {
				return err
			}
			artifact.EvaluationVerdict = evaluation.Verdict
			artifact.EvaluatedManifestSHA = evaluation.EvaluatedManifestSHA
		}
		artifacts = append(artifacts, artifact)
		refs = append(refs, model.ProgramInputRef{Kind: d.Kind, ID: id, SHA: shas[i], Revision: step.Revision})
	}
	if step.Kind == model.StepExperimentRun {
		var receiptBytes []byte
		if err = tx.QueryRowContext(ctx, `SELECT data FROM program_experiments WHERE attempt_id=? AND json_extract(data,'$.step_id')=? AND json_extract(data,'$.contract_sha')=? AND json_extract(data,'$.state')='completed' AND json_extract(data,'$.conclusive')=1`, a.ID, step.ID, step.SubjectID).Scan(&receiptBytes); err != nil {
			return programError("evidence_invalid", "actual conclusive experiment receipt required")
		}
		var receipt model.ExperimentReceipt
		if err = json.Unmarshal(receiptBytes, &receipt); err != nil {
			return err
		}
		refs = append(refs, model.ProgramInputRef{Kind: "experiment_receipt", ID: receipt.RunID, SHA: programHash(receipt), Revision: step.Revision})
	}
	input, _, _, err := programManifestLoadTx(ctx, tx, step.InputManifestSHA)
	if err != nil {
		return err
	}
	refs = append(refs, input.Inputs...)
	if step.Kind == model.StepResearch {
		if step.State == "completed" {
			previous, _, _, e := programManifestLoadTx(ctx, tx, step.OutputManifestSHA)
			if e != nil {
				return e
			}
			for _, ref := range previous.Inputs {
				if ref.Kind == "source" {
					refs = append(refs, ref)
				}
			}
		} else {
			rows, e := tx.QueryContext(ctx, `SELECT data FROM program_sources WHERE program_id=? AND revision=? ORDER BY id`, step.ProgramID, step.Revision)
			if e != nil {
				return e
			}
			var sources []model.SourceCapture
			for rows.Next() {
				var b []byte
				var source model.SourceCapture
				if e = rows.Scan(&b); e != nil {
					rows.Close()
					return e
				}
				if e = json.Unmarshal(b, &source); e != nil {
					rows.Close()
					return e
				}
				sources = append(sources, source)
			}
			e = rows.Err()
			rows.Close()
			if e != nil {
				return e
			}
			for _, source := range sources {
				ref := model.ProgramInputRef{Kind: "source", ID: source.ID, SHA: source.TextSHA256, Revision: source.Revision}
				if e = programValidateRefTx(ctx, tx, step.ProgramID, step.Revision, ref); e != nil {
					var pe *model.ProgramError
					if errors.As(e, &pe) {
						continue
					}
					return e
				}
				refs = append(refs, ref)
			}
		}
	}
	unique := make([]model.ProgramInputRef, 0, len(refs))
	keys := map[string]model.ProgramInputRef{}
	for _, ref := range refs {
		key := ref.Kind + "/" + ref.ID
		if old, ok := keys[key]; ok {
			if old.SHA != ref.SHA || old.Revision != ref.Revision {
				return programError("evidence_invalid", "conflicting immutable manifest references")
			}
			continue
		}
		keys[key] = ref
		unique = append(unique, ref)
	}
	refs = unique
	manifest, err := programManifestTx(ctx, tx, step.ProgramID, step.Revision, refs)
	if err != nil {
		return err
	}
	outputSHA := manifest.SHA
	if step.State == "completed" {
		if step.OutputManifestSHA == outputSHA {
			return nil
		}
		return programError("idempotency_conflict", "step already completed with different output")
	}
	if active != 1 || persisted.Stage == "uncertain" || f.State == "cancelling" {
		return programError("operation_uncertain", "attempt is not safely completable")
	}
	if err = s.programFeatureGateTx(ctx, tx, f, f.PolicyHash); err != nil {
		return err
	}
	if step.Kind != model.StepExperimentRun && a.ThreadID == "" {
		return programError("evidence_invalid", "analysis completion requires producer thread")
	}
	for i, draft := range result.Artifacts {
		if draft.Kind == model.ArtifactEvaluation {
			var evaluation model.EvaluationPayload
			if err = json.Unmarshal(canonical[i], &evaluation); err != nil {
				return err
			}
			if err = validateProgramResultVerdictTx(ctx, tx, step, evaluation, f.PolicyHash); err != nil {
				return err
			}
		}
	}
	for _, artifact := range artifacts {
		b, _ := json.Marshal(artifact)
		if _, err = tx.ExecContext(ctx, `INSERT INTO program_artifacts(id,program_id,revision,kind,content_sha,data) VALUES(?,?,?,?,?,?)`, artifact.ID, artifact.ProgramID, artifact.Revision, artifact.Kind, artifact.ContentSHA, b); err != nil {
			return err
		}
	}
	for i, d := range result.Artifacts {
		if d.Kind == model.ArtifactEvaluation {
			var e model.EvaluationPayload
			_ = json.Unmarshal(canonical[i], &e)
			if e.EvaluatedManifestSHA != step.InputManifestSHA {
				return programError("evidence_invalid", "evaluation does not cover exact input manifest")
			}
			var count int
			if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM program_artifacts WHERE program_id=? AND json_extract(data,'$.producer_thread_id')=? AND json_extract(data,'$.producer_step_id')!=?`, step.ProgramID, a.ThreadID, step.ID).Scan(&count); err != nil {
				return err
			}
			if count != 0 {
				return programError("evidence_invalid", "evaluator must use an independent fresh thread")
			}
		}
		if err = s.validateProgramPayloadTx(ctx, tx, step, d.Kind, canonical[i]); err != nil {
			return err
		}
	}
	step.State = "completed"
	step.OutputManifestSHA = manifest.SHA
	step.UpdatedAt = now
	raw, _ = json.Marshal(step)
	if _, err = tx.ExecContext(ctx, `UPDATE program_steps SET data=? WHERE id=?`, raw, step.ID); err != nil {
		return err
	}
	persisted.Stage = "done"
	persisted.State = "reviewed"
	persisted.Summary = result.Message
	persisted.UpdatedAt = now
	persisted.Wait = nil
	if err = rememberSession(ctx, tx, persisted, ""); err != nil {
		return err
	}
	raw, _ = json.Marshal(persisted)
	if _, err = tx.ExecContext(ctx, `UPDATE attempts SET active=0,data=? WHERE id=?`, raw, a.ID); err != nil {
		return err
	}
	f.State = "reviewed"
	f.Reason = result.Message
	f.Receipt = manifest.SHA
	if err = put(ctx, tx, f); err != nil {
		return err
	}
	if err = programEvent(ctx, tx, step.ProgramID, "step_completed", step.Kind+":"+strconv.Itoa(step.Iteration)); err != nil {
		return err
	}
	return tx.Commit()
}

// An independent evaluator may explain a falsification, but cannot turn the
// runner's measured failed acceptance predicates into permission to implement.
func validateProgramResultVerdictTx(ctx context.Context, tx *sql.Tx, step model.ProgramStep, evaluation model.EvaluationPayload, policy string) error {
	if step.Kind != model.StepResultEvaluation {
		return nil
	}
	if evaluation.EvaluatedManifestSHA != step.InputManifestSHA {
		return programError("evidence_invalid", "result evaluation does not cover exact experiment inputs")
	}
	manifest, programID, revision, err := programManifestLoadTx(ctx, tx, step.InputManifestSHA)
	if err != nil {
		return err
	}
	if programID != step.ProgramID || revision != step.Revision {
		return programError("evidence_invalid", "result evaluation manifest scope mismatch")
	}
	r, err := programRevision(ctx, tx, step.ProgramID, step.Revision)
	if err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT data FROM program_steps WHERE program_id=? AND revision=? AND json_extract(data,'$.iteration')=? AND json_extract(data,'$.kind')=?`, step.ProgramID, step.Revision, step.Iteration, model.StepExperimentRun)
	if err != nil {
		return err
	}
	expected := map[string]model.ProgramStep{}
	for rows.Next() {
		var raw []byte
		var experiment model.ProgramStep
		if err = rows.Scan(&raw); err != nil {
			rows.Close()
			return err
		}
		if err = json.Unmarshal(raw, &experiment); err != nil {
			rows.Close()
			return err
		}
		expected[experiment.ID] = experiment
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, experiment := range expected {
		if experiment.State != "completed" {
			return programError("evidence_invalid", "current experiment step is incomplete")
		}
		if err = programSubjectApprovedTx(ctx, tx, step.ProgramID, r.RevisionHash, experiment.SubjectID, policy); err != nil {
			return err
		}
	}
	covered := map[string]bool{}
	receipts := 0
	for _, ref := range manifest.Inputs {
		if ref.Kind != "experiment_receipt" && ref.Kind != "experiment" && ref.Kind != "receipt" {
			continue
		}
		if err = programValidateRefTx(ctx, tx, step.ProgramID, step.Revision, ref); err != nil {
			return err
		}
		var raw []byte
		if err = tx.QueryRowContext(ctx, `SELECT data FROM program_experiments WHERE run_id=? AND program_id=?`, ref.ID, step.ProgramID).Scan(&raw); err != nil {
			return err
		}
		var receipt model.ExperimentReceipt
		if err = json.Unmarshal(raw, &receipt); err != nil {
			return err
		}
		experiment, current := expected[receipt.StepID]
		if !current {
			continue
		}
		if receipt.Revision != step.Revision || receipt.ContractSHA != experiment.SubjectID {
			return programError("evidence_invalid", "current experiment receipt contract identity mismatch")
		}
		covered[experiment.ID] = true
		receipts++
		if receipt.State != "completed" || !receipt.Conclusive {
			return programError("evidence_invalid", "result evaluation requires conclusive completed experiment receipts")
		}
		if evaluation.Verdict == "accept" && !receipt.Accepted {
			return programError("evidence_invalid", "falsified experiment acceptance requires plan revision, not implementation contracts")
		}
	}
	if receipts == 0 {
		return programError("evidence_invalid", "result evaluation requires actual experiment receipts")
	}
	if len(covered) != len(expected) {
		return programError("evidence_invalid", "result evaluation is missing current approved experiment receipts")
	}
	return nil
}
