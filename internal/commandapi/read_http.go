package commandapi

import (
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"openfactory/internal/model"
	"openfactory/internal/store"
)

func (s *Server) read(w http.ResponseWriter, r *http.Request, p principal) error {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/v1/"), "/")
	switch parts[0] {
	case "session":
		if len(parts) != 1 {
			return notFound()
		}
		view, err := s.sessionView(r, p)
		if err != nil {
			return err
		}
		writeJSON(w, 200, view)
		return nil
	case "projects":
		if len(parts) != 1 {
			return notFound()
		}
		return s.projects(w, p)
	case "requests":
		return s.readRequest(w, r, p, parts)
	case "commands":
		if len(parts) != 2 || !validUUID(parts[1]) {
			return notFound()
		}
		receipt, err := s.db.GetCommandReceipt(r.Context(), p.operator.ID, parts[1])
		if err != nil {
			return err
		}
		writeJSON(w, 200, receipt)
		return nil
	case "reviews":
		if len(parts) != 2 {
			return notFound()
		}
		review, err := s.db.GetIntakeReview(r.Context(), parts[1])
		if err != nil {
			return err
		}
		if review.Claims.OperatorID != p.operator.ID || review.Claims.CredentialID != p.credential.ID {
			return notFound()
		}
		writeJSON(w, 200, review)
		return nil
	case "subjects":
		return s.readSubject(w, r, p, parts)
	case "bundles":
		if len(parts) != 2 {
			return notFound()
		}
		upload, err := s.db.BundleUpload(r.Context(), p.operator.ID, parts[1])
		if err != nil {
			return err
		}
		if err = s.checkProject(p, upload.Manifest.Project); err != nil {
			return err
		}
		writeJSON(w, 200, upload)
		return nil
	}
	return notFound()
}
func (s *Server) projects(w http.ResponseWriter, p principal) error {
	items := []model.IntakeProjectView{}
	policy := s.cfg.Intake
	limits := model.IntakeLimits{MaxTurnBytes: policy.MaxTurnBytes, MaxTurns: policy.MaxTurns, MaxTextBytes: policy.MaxTextBytes, MaxCanonicalBytes: policy.MaxCanonicalBytes, MaxOutputBytes: policy.MaxOutputBytes, MaxDraftCalls: policy.MaxDraftCalls, HistoryDefault: policy.HistoryDefault, HistoryMax: policy.HistoryMax}
	for id, project := range s.cfg.Projects {
		if !project.Trusted || s.checkProject(p, id) != nil {
			continue
		}
		checks := make([]string, 0, len(project.Checks))
		for name := range project.Checks {
			checks = append(checks, name)
		}
		sort.Strings(checks)
		targets := []string{"reviewed", "integrated"}
		if project.GitHubRepository != "" {
			targets = append(targets, "pull_request")
		}
		for name, release := range project.Releases {
			if release.Allowed && release.Qualified {
				targets = append(targets, "release:"+name)
			}
		}
		sort.Strings(targets)
		items = append(items, model.IntakeProjectView{ID: id, Checks: checks, CompletionTargets: targets, ModelProviderIDs: []string{s.cfg.AstraModel}, Limits: limits})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
	writeJSON(w, 200, struct {
		Items     []model.IntakeProjectView `json:"items"`
		PolicySHA string                    `json:"policy_sha"`
	}{items, s.cfg.PolicyHash})
	return nil
}
func (s *Server) historyLimit(r *http.Request) (int, error) {
	value := r.URL.Query().Get("limit")
	if value == "" {
		return s.cfg.Intake.HistoryDefault, nil
	}
	n, err := strconv.Atoi(value)
	if err != nil || n < 1 || n > s.cfg.Intake.HistoryMax {
		return 0, invalidJSON()
	}
	return n, nil
}
func afterSequence(r *http.Request) (int64, error) {
	value := r.URL.Query().Get("after")
	if value == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil || n < 0 {
		return 0, invalidJSON()
	}
	return n, nil
}
func (s *Server) scopedRequest(r *http.Request, p principal, id string) (model.IntakeRequest, error) {
	request, err := s.db.IntakeRequest(r.Context(), id)
	if err != nil {
		return request, err
	}
	if request.OperatorID != p.operator.ID {
		return model.IntakeRequest{}, notFound()
	}
	if err = s.checkProject(p, request.Project); err != nil {
		return model.IntakeRequest{}, err
	}
	return request, nil
}
func (s *Server) readRequest(w http.ResponseWriter, r *http.Request, p principal, parts []string) error {
	if len(parts) == 1 {
		limit, err := s.historyLimit(r)
		if err != nil {
			return err
		}
		project := r.URL.Query().Get("project")
		if project != "" {
			if err = s.checkProject(p, project); err != nil {
				return err
			}
		}
		requests, err := s.db.ListIntakeRequests(r.Context(), p.operator.ID, project, r.URL.Query().Get("after"), limit)
		if err != nil {
			return err
		}
		page := model.IntakeRequestPage{Items: []model.IntakeRequest{}}
		for _, request := range requests {
			if s.checkProject(p, request.Project) == nil {
				page.Items = append(page.Items, request)
			}
		}
		if len(requests) == limit {
			page.Next = requests[len(requests)-1].ID
		}
		writeJSON(w, 200, page)
		return nil
	}
	request, err := s.scopedRequest(r, p, parts[1])
	if err != nil {
		return err
	}
	if len(parts) == 2 {
		questions, err := s.db.IntakeQuestions(r.Context(), request.ID)
		if err != nil {
			return err
		}
		steps, err := s.db.IntakeSteps(r.Context(), request.ID)
		if err != nil {
			return err
		}
		links, err := s.db.IntakeLinks(r.Context(), request.ID)
		if err != nil {
			return err
		}
		view := model.IntakeRequestView{Request: request, Questions: questions, Steps: steps, Links: links}
		if request.LatestDraftRevision > 0 {
			draft, e := s.db.IntakeDraft(r.Context(), request.ID, request.LatestDraftRevision)
			if e != nil {
				return e
			}
			view.Draft = &draft
		}
		writeJSON(w, 200, view)
		return nil
	}
	switch parts[2] {
	case "turns":
		if len(parts) != 3 {
			return notFound()
		}
		limit, err := s.historyLimit(r)
		if err != nil {
			return err
		}
		after, err := afterSequence(r)
		if err != nil {
			return err
		}
		items, err := s.db.IntakeTurns(r.Context(), request.ID, after, limit)
		if err != nil {
			return err
		}
		if items == nil {
			items = []model.IntakeTurn{}
		}
		page := model.IntakeTurnPage{Items: items}
		if len(items) == limit {
			page.Next = items[len(items)-1].Sequence
		}
		writeJSON(w, 200, page)
		return nil
	case "disclosure":
		if len(parts) != 3 {
			return notFound()
		}
		version, offer, err := s.currentDisclosure(r, p, disclosureRequest{Project: request.Project, RequestID: request.ID, BundleManifestSHAs: r.URL.Query()["bundle_manifest_sha"]})
		if err != nil {
			return err
		}
		writeJSON(w, 200, struct {
			ContentVersion int                    `json:"content_version"`
			Disclosure     model.IntakeDisclosure `json:"disclosure"`
		}{version, offer})
		return nil
	case "events":
		if len(parts) != 3 {
			return notFound()
		}
		limit, err := s.historyLimit(r)
		if err != nil {
			return err
		}
		after, err := afterSequence(r)
		if err != nil {
			return err
		}
		items, err := s.db.IntakeEvents(r.Context(), request.ID, after, limit)
		if err != nil {
			return err
		}
		if items == nil {
			items = []model.IntakeEvent{}
		}
		page := model.IntakeEventPage{Items: items}
		if len(items) == limit {
			page.Next = items[len(items)-1].Sequence
		}
		writeJSON(w, 200, page)
		return nil
	case "drafts":
		if len(parts) != 4 && len(parts) != 5 {
			return notFound()
		}
		revision, err := strconv.Atoi(parts[3])
		if err != nil || revision < 1 {
			return notFound()
		}
		draft, err := s.db.IntakeDraft(r.Context(), request.ID, revision)
		if err != nil {
			return err
		}
		if len(parts) == 5 {
			if parts[4] != "download" {
				return notFound()
			}
			download(w, []byte(draft.CanonicalSource), draft.Format == model.IntakeFormatProgram)
			return nil
		}
		writeJSON(w, 200, draft)
		return nil
	}
	return notFound()
}
func (s *Server) readSubject(w http.ResponseWriter, r *http.Request, p principal, parts []string) error {
	if len(parts) < 3 {
		return notFound()
	}
	kind, id := parts[1], parts[2]
	if len(parts) == 3 {
		view, err := s.db.IntakeSubject(r.Context(), p.operator.ID, kind, id)
		if err != nil {
			return err
		}
		if err = s.checkProject(p, view.Project); err != nil {
			return err
		}
		writeJSON(w, 200, view)
		return nil
	}
	project, err := s.db.IntakeSubjectProject(r.Context(), p.operator.ID, kind, id)
	if err != nil {
		return err
	}
	if err = s.checkProject(p, project); err != nil {
		return err
	}
	if parts[3] != "artifacts" {
		return notFound()
	}
	if len(parts) == 4 {
		limit, err := s.historyLimit(r)
		if err != nil {
			return err
		}
		page, err := s.db.IntakeSubjectArtifacts(r.Context(), p.operator.ID, kind, id, r.URL.Query().Get("after"), limit)
		if err != nil {
			return err
		}
		writeJSON(w, 200, struct {
			Items        []model.ProgramArtifact            `json:"items"`
			Next         string                             `json:"next,omitempty"`
			OutputStates []store.ProgramArtifactOutputState `json:"output_states"`
			Snapshot     string                             `json:"snapshot"`
			RevisionHash string                             `json:"revision_hash"`
		}{page.Items, page.NextCursor, page.OutputStates, page.Snapshot, page.RevisionHash})
		return nil
	}
	if len(parts) != 5 && len(parts) != 6 {
		return notFound()
	}
	detail, err := s.db.IntakeSubjectArtifact(r.Context(), p.operator.ID, kind, id, parts[4])
	if err != nil {
		return err
	}
	if len(parts) == 6 {
		if parts[5] != "download" {
			return notFound()
		}
		download(w, []byte(detail.CanonicalSource), true)
		return nil
	}
	writeJSON(w, 200, struct {
		Artifact           model.ProgramArtifact            `json:"artifact"`
		CanonicalSource    string                           `json:"canonical_source"`
		Payload            json.RawMessage                  `json:"payload"`
		ExperimentSubjects []store.IntakeExperimentSubject  `json:"experiment_subjects,omitempty"`
		OutputState        store.ProgramArtifactOutputState `json:"output_state"`
		Snapshot           string                           `json:"snapshot"`
	}{detail.Artifact, detail.CanonicalSource, detail.Payload, detail.ExperimentSubjects, detail.OutputState, detail.Snapshot})
	return nil
}
func download(w http.ResponseWriter, source []byte, asJSON bool) {
	media, filename := "text/markdown; charset=utf-8", "openfactory-intake.md"
	if asJSON {
		media = "application/json"
		filename = "openfactory-intake.json"
	}
	w.Header().Set("Content-Type", media)
	w.Header().Set("Content-Disposition", "attachment; filename=\""+filename+"\"")
	w.Header().Set("Content-Length", strconv.Itoa(len(source)))
	w.WriteHeader(200)
	_, _ = w.Write(source)
}
