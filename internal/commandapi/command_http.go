package commandapi

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"openfactory/internal/intake"
	"openfactory/internal/model"
)

func isMissing(err error) bool                           { return errors.Is(err, sql.ErrNoRows) }
func commandDigest(c model.TypedCommand) (string, error) { return intake.CommandDigest(c) }
func canonicalCommand(c model.TypedCommand) (model.TypedCommand, error) {
	if !validUUID(c.CommandID) {
		return c, invalidJSON()
	}
	raw, err := json.Marshal(c)
	if err != nil {
		return c, err
	}
	canonical, err := intake.DecodeCommand(raw)
	if err != nil {
		return c, apiError{Status: 400, Code: "invalid_request", Message: "Request does not match the supported command contract."}
	}
	return canonical, nil
}
func (s *Server) command(w http.ResponseWriter, r *http.Request, p principal) error {
	var c model.TypedCommand
	if err := decodeJSON(w, r, &c, int64(s.cfg.Intake.MaxCanonicalBytes+s.cfg.Intake.MaxTurnBytes+(64<<10))); err != nil {
		return err
	}
	c, err := canonicalCommand(c)
	if err != nil {
		return err
	}
	receipt, err := s.db.ExecuteCommand(r.Context(), p.tokenSHA, p.csrfSHA, c, s.cfg, nil)
	if err != nil {
		return err
	}
	writeJSON(w, 200, receipt)
	return nil
}
func (s *Server) review(w http.ResponseWriter, r *http.Request, p principal) error {
	var req model.ReviewRequest
	if err := decodeJSON(w, r, &req, int64(s.cfg.Intake.MaxCanonicalBytes+s.cfg.Intake.MaxTurnBytes+(64<<10))); err != nil {
		return err
	}
	c, err := canonicalCommand(req.Command)
	if err != nil {
		return err
	}
	if !intake.RequiresHumanVerification(c.Action) {
		return apiError{Status: 400, Code: "unsupported_action", Message: "This command does not require an authority grant."}
	}
	review, err := s.db.CreateIntakeReview(r.Context(), p.tokenSHA, c, s.cfg)
	if err != nil {
		return err
	}
	writeJSON(w, 200, review)
	return nil
}

type disclosureRequest struct {
	Project            string   `json:"project"`
	RequestID          string   `json:"request_id"`
	BundleManifestSHAs []string `json:"bundle_manifest_shas,omitempty"`
}

func (s *Server) disclosure(w http.ResponseWriter, r *http.Request, p principal) error {
	var req disclosureRequest
	if err := decodeJSON(w, r, &req, 64<<10); err != nil {
		return err
	}
	_, disclosure, err := s.currentDisclosure(r, p, req)
	if err != nil {
		return err
	}
	writeJSON(w, 200, disclosure)
	return nil
}
func (s *Server) currentDisclosure(r *http.Request, p principal, req disclosureRequest) (int, model.IntakeDisclosure, error) {
	if req.RequestID == "" {
		return 0, model.IntakeDisclosure{}, invalidJSON()
	}
	request, err := s.scopedRequest(r, p, req.RequestID)
	if err != nil {
		return 0, model.IntakeDisclosure{}, err
	}
	if request.Project != req.Project {
		return 0, model.IntakeDisclosure{}, notFound()
	}
	disclosure, err := s.db.IntakeDisclosure(r.Context(), s.cfg, p.operator.ID, req.Project, req.RequestID, req.BundleManifestSHAs)
	if err != nil {
		return 0, disclosure, err
	}
	current, err := s.scopedRequest(r, p, req.RequestID)
	if err != nil {
		return 0, disclosure, err
	}
	if current.ContentVersion != request.ContentVersion {
		return 0, disclosure, apiError{Status: 409, Code: "stale_revision", Message: "The saved context changed. Review it again before drafting."}
	}
	return current.ContentVersion, disclosure, nil
}
func (s *Server) bundle(w http.ResponseWriter, r *http.Request, p principal) error {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/v1/"), "/")
	if len(parts) == 1 && parts[0] == "bundles" && r.Method == "POST" {
		var manifest model.BundleUploadManifest
		if err := decodeJSON(w, r, &manifest, 256<<10); err != nil {
			return err
		}
		if err := s.checkProject(p, manifest.Project); err != nil {
			return err
		}
		upload, err := s.db.CreateBundleUpload(r.Context(), p.operator.ID, manifest, s.cfg)
		if err != nil {
			return err
		}
		writeJSON(w, 201, upload)
		return nil
	}
	if len(parts) < 2 || parts[0] != "bundles" {
		return notFound()
	}
	upload, err := s.db.BundleUpload(r.Context(), p.operator.ID, parts[1])
	if err != nil {
		return err
	}
	if err = s.checkProject(p, upload.Manifest.Project); err != nil {
		return err
	}
	if len(parts) == 3 && parts[2] == "finalize" && r.Method == "POST" {
		var req struct{}
		if err := decodeJSON(w, r, &req, 1024); err != nil {
			return err
		}
		finalized, err := s.db.FinalizeBundleUpload(r.Context(), p.operator.ID, upload.ID, s.cfg)
		if err != nil {
			return err
		}
		writeJSON(w, 200, finalized)
		return nil
	}
	if len(parts) == 4 && parts[2] == "files" && r.Method == "PUT" {
		types := r.Header.Values("Content-Type")
		if len(types) != 1 || types[0] != "application/octet-stream" || len(r.Header.Values("Content-Encoding")) != 0 {
			return apiError{Status: 400, Code: "invalid_contract", Message: "Bundle files must be raw application/octet-stream bytes."}
		}
		var declared *model.BundleUploadFile
		for i := range upload.Manifest.Files {
			if upload.Manifest.Files[i].ID == parts[3] {
				declared = &upload.Manifest.Files[i]
				break
			}
		}
		if declared == nil {
			return notFound()
		}
		if r.ContentLength >= 0 && r.ContentLength != declared.Bytes {
			return apiError{Status: 400, Code: "invalid_contract", Message: "File size does not match the declared manifest."}
		}
		r.Body = http.MaxBytesReader(w, r.Body, declared.Bytes+1)
		saved, err := s.db.PutBundleUploadFile(r.Context(), p.operator.ID, upload.ID, declared.ID, r.Body)
		if err != nil {
			return err
		}
		writeJSON(w, 200, saved)
		return nil
	}
	return notFound()
}
