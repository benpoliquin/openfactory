package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"strconv"
	"strings"

	"openfactory/internal/artifacts"
	"openfactory/internal/executor"
	"openfactory/internal/intake"
	"openfactory/internal/model"
	"openfactory/internal/store"
)

type stringFlags []string

func (s *stringFlags) String() string { return strings.Join(*s, ",") }
func (s *stringFlags) Set(v string) error {
	if strings.TrimSpace(v) == "" || len(v) > 4096 || strings.ContainsAny(v, "\x00\r\n") {
		return errors.New("invalid prerequisite attestation")
	}
	*s = append(*s, v)
	return nil
}

type programCLIError struct{ cause error }

func (e *programCLIError) Error() string { return e.cause.Error() }
func (e *programCLIError) Unwrap() error { return e.cause }
func programInvalid(message string) error {
	return &model.ProgramError{Code: "invalid_contract", Message: message}
}
func renderProgramError(out io.Writer, err error) error {
	var p *model.ProgramError
	if !errors.As(err, &p) {
		code := "invalid_contract"
		var runner *executor.ExperimentError
		if errors.As(err, &runner) {
			code = runner.Code
		} else if errors.Is(err, sql.ErrNoRows) || errors.Is(err, store.ErrNotFound) {
			code = "evidence_invalid"
		}
		p = &model.ProgramError{Code: code, Message: err.Error()}
	}
	_ = json.NewEncoder(out).Encode(struct {
		Error *model.ProgramError `json:"error"`
	}{p})
	return &programCLIError{cause: err}
}

// Program inputs are canonical JSON, not alternate YAML representations. Paths
// are explicit operator inputs; models never ask this command to crawl a tree.
func programInput(file string, target any) error {
	data, err := readBounded(file, 1<<20)
	if err != nil {
		return err
	}
	if err = model.StrictDecode(data, target); err != nil {
		return programInvalid(err.Error())
	}
	return nil
}
func programOptions(args []string, allowed ...string) (map[string]string, error) {
	valid := map[string]bool{}
	for _, key := range allowed {
		valid[key] = true
	}
	opts := map[string]string{}
	for len(args) > 0 {
		key := args[0]
		if !valid[key] || len(args) < 2 || strings.HasPrefix(args[1], "--") {
			return nil, programInvalid("unexpected or missing program option: " + key)
		}
		if _, ok := opts[key]; ok {
			return nil, programInvalid("duplicate program option: " + key)
		}
		opts[key] = args[1]
		args = args[2:]
	}
	return opts, nil
}
func requireProgramOptions(opts map[string]string, names ...string) error {
	for _, name := range names {
		if strings.TrimSpace(opts[name]) == "" {
			return programInvalid("required option: " + name)
		}
	}
	return nil
}
func programQuery(opts map[string]string) (store.ProgramQuery, error) {
	q := store.ProgramQuery{After: opts["--after"], Project: opts["--project"], State: opts["--state"]}
	for key, dest := range map[string]*int{"--limit": &q.Limit, "--revision": &q.Revision} {
		if s := opts[key]; s != "" {
			n, err := strconv.Atoi(s)
			if err != nil || n < 1 || key == "--limit" && n > 500 {
				return q, programInvalid("invalid " + key)
			}
			*dest = n
		}
	}
	return q, nil
}
func normalizeProgramSpec(spec *model.ProgramSpec, c model.Config) error {
	if err := intake.NormalizeProgramSpec(spec, c); err != nil {
		return programInvalid(err.Error())
	}
	return nil
}
func runProgram(ctx context.Context, c model.Config, db *store.Store, args []string, out io.Writer) error {
	emit := func(v any) error { e := json.NewEncoder(out); e.SetIndent("", "  "); return e.Encode(v) }
	if len(args) == 0 {
		return programInvalid("usage: program import|status|approve|questions|answer|architecture|approve-architecture|experiments|approve-experiment|evidence|features|resume|cancel|invalidate|resolve-experiment-failed")
	}
	command := args[0]
	args = args[1:]
	if command == "status" && (len(args) == 0 || strings.HasPrefix(args[0], "--")) {
		opts, err := programOptions(args, "--project", "--state", "--limit", "--after")
		if err != nil {
			return err
		}
		q, err := programQuery(opts)
		if err != nil {
			return err
		}
		v, err := db.ProgramPage(ctx, q)
		if err != nil {
			return err
		}
		return emit(v)
	}
	if len(args) == 0 || strings.HasPrefix(args[0], "--") {
		return programInvalid("program " + command + " requires an ID (or FILE for import)")
	}
	id := args[0]
	args = args[1:]
	audit := model.TrustedLocalAudit()
	switch command {
	case "import":
		opts, err := programOptions(args, "--bundle")
		if err != nil {
			return err
		}
		var spec model.ProgramSpec
		if err = programInput(id, &spec); err != nil {
			return err
		}
		if err = normalizeProgramSpec(&spec, c); err != nil {
			return err
		}
		bundle := model.ProgramBundle{}
		if manifest := opts["--bundle"]; manifest != "" {
			var declared []string
			if err = programInput(manifest, &declared); err != nil {
				return err
			}
			if declared == nil {
				return programInvalid("bundle manifest must be an explicit JSON array of relative file paths")
			}
			cas := artifacts.New(filepath.Join(c.StateDir, "artifacts"))
			bundle, err = cas.ImportBundle(ctx, filepath.Dir(manifest), declared, model.BundleLimits{MaxFiles: c.Discovery.BundleMaxFiles, MaxFileBytes: c.Discovery.BundleMaxFileBytes, MaxTotalBytes: c.Discovery.BundleMaxTotalBytes})
			if err != nil {
				return programInvalid(err.Error())
			}
		} else {
			for _, seed := range spec.Seeds {
				if seed.Kind == "bundle" {
					return programInvalid("bundle seeds require --bundle MANIFEST")
				}
			}
		}
		revision, err := db.ImportProgram(ctx, spec, bundle)
		if err != nil {
			return err
		}
		return emit(revision)
	case "status":
		if len(args) != 0 {
			return programInvalid("usage: program status [ID]")
		}
		v, err := db.ProgramStatus(ctx, id)
		if err != nil {
			return err
		}
		return emit(v)
	case "approve", "resume", "cancel":
		opts, err := programOptions(args, "--revision")
		if err != nil {
			return err
		}
		if err = requireProgramOptions(opts, "--revision"); err != nil {
			return err
		}
		switch command {
		case "approve":
			err = db.ApproveProgram(ctx, id, opts["--revision"], c.PolicyHash, audit)
		case "resume":
			err = db.ResumeProgram(ctx, id, opts["--revision"], c, audit)
		case "cancel":
			err = db.CancelProgram(ctx, id, opts["--revision"], audit)
		}
		if err != nil {
			return err
		}
		v, err := db.ProgramStatus(ctx, id)
		if err != nil {
			return err
		}
		return emit(v)
	case "answer":
		opts, err := programOptions(args, "--revision", "--file")
		if err != nil {
			return err
		}
		if err = requireProgramOptions(opts, "--revision", "--file"); err != nil {
			return err
		}
		var answer model.ProgramAnswer
		if err = programInput(opts["--file"], &answer); err != nil {
			return err
		}
		if answer.ProgramID != "" && answer.ProgramID != id || answer.RevisionHash != "" && answer.RevisionHash != opts["--revision"] {
			return programInvalid("answer identity differs from command revision")
		}
		if answer.Actor != "" || answer.Audit != (model.AuditContext{}) || !answer.CreatedAt.IsZero() {
			return programInvalid("answer actor, audit and timestamp are controller-owned")
		}
		answer.ProgramID = id
		answer.RevisionHash = opts["--revision"]
		v, err := db.AnswerProgramQuestion(ctx, id, opts["--revision"], answer, audit)
		if err != nil {
			return err
		}
		return emit(v)
	case "approve-architecture", "approve-experiment":
		key := "--artifact"
		if command == "approve-experiment" {
			key = "--contract"
		}
		opts, err := programOptions(args, "--revision", key)
		if err != nil {
			return err
		}
		if err = requireProgramOptions(opts, "--revision", key); err != nil {
			return err
		}
		kind := model.ProgramArtifactKind(model.ArtifactArchitecture)
		if command == "approve-experiment" {
			kind = model.ArtifactExperimentContracts
		}
		if err = db.ApproveProgramArtifact(ctx, id, opts["--revision"], kind, opts[key], c.PolicyHash, audit); err != nil {
			return err
		}
		v, err := db.ProgramStatus(ctx, id)
		if err != nil {
			return err
		}
		return emit(v)
	case "questions", "architecture", "experiments", "features", "evidence":
		opts, err := programOptions(args, "--limit", "--after", "--revision", "--source", "--artifact", "--kind")
		if err != nil {
			return err
		}
		if command != "evidence" && (opts["--source"] != "" || opts["--artifact"] != "" || opts["--kind"] != "") {
			return programInvalid("source/artifact/kind selectors require evidence")
		}
		q, err := programQuery(opts)
		if err != nil {
			return err
		}
		switch command {
		case "questions":
			v, err := db.ProgramQuestionPage(ctx, id, q)
			if err != nil {
				return err
			}
			return emit(v)
		case "experiments":
			v, err := db.ProgramExperimentPage(ctx, id, q)
			if err != nil {
				return err
			}
			return emit(v)
		case "features":
			v, err := db.ProgramFeaturePage(ctx, id, q, c)
			if err != nil {
				return err
			}
			return emit(v)
		case "architecture":
			q.Kind = model.ArtifactArchitecture
			page, err := db.ProgramArtifactPage(ctx, id, q)
			if err != nil {
				return err
			}
			items := make([]store.ProgramArtifactDetail, 0, len(page.Items))
			for _, a := range page.Items {
				v, err := db.ProgramArtifactDetail(ctx, id, a.ID)
				if err != nil {
					return err
				}
				items = append(items, v)
			}
			return emit(struct {
				Items        []store.ProgramArtifactDetail `json:"items"`
				NextCursor   string                        `json:"next_cursor,omitempty"`
				Snapshot     string                        `json:"snapshot"`
				RevisionHash string                        `json:"revision_hash"`
			}{items, page.NextCursor, page.Snapshot, page.RevisionHash})
		case "evidence":
			return programEvidence(ctx, c, db, id, opts, q, emit)
		}
	case "invalidate":
		opts, err := programOptions(args, "--subject", "--reason")
		if err != nil {
			return err
		}
		if err = requireProgramOptions(opts, "--subject", "--reason"); err != nil {
			return err
		}
		if err = db.InvalidateProgramEvidence(ctx, id, opts["--subject"], opts["--reason"], audit); err != nil {
			return err
		}
		v, err := db.ProgramStatus(ctx, id)
		if err != nil {
			return err
		}
		return emit(v)
	case "resolve-experiment-failed":
		opts, err := programOptions(args, "--request", "--attestation")
		if err != nil {
			return err
		}
		if err = requireProgramOptions(opts, "--request", "--attestation"); err != nil {
			return err
		}
		if c.CheckSocket == "" {
			return &model.ProgramError{Code: "prerequisite_missing", Message: "experiment resolution requires the configured isolated executor"}
		}
		receipt, err := executor.NewClient(c.CheckSocket).ResolveFailed(ctx, id, opts["--request"], opts["--attestation"])
		if err != nil {
			return err
		}
		if err = db.RecordExperimentReceipt(ctx, id, receipt); err != nil {
			return err
		}
		if err = db.ResolveProgramExperimentFailed(ctx, id, opts["--request"], opts["--attestation"]); err != nil {
			return err
		}
		return emit(receipt)
	default:
		return programInvalid("unknown program command: " + command)
	}
	return programInvalid("unsupported program command")
}

func programEvidence(ctx context.Context, c model.Config, db *store.Store, id string, opts map[string]string, q store.ProgramQuery, emit func(any) error) error {
	if opts["--source"] != "" && opts["--artifact"] != "" {
		return programInvalid("select either --source or --artifact")
	}
	if artifact := opts["--artifact"]; artifact != "" {
		v, err := db.ProgramArtifactDetail(ctx, id, artifact)
		if err != nil {
			return err
		}
		return emit(v)
	}
	if source := opts["--source"]; source != "" {
		q.Limit = 500
		q.After = ""
		for {
			page, err := db.ProgramSourcePage(ctx, id, q)
			if err != nil {
				return err
			}
			for _, s := range page.Items {
				if s.ID == source {
					text, err := artifacts.New(filepath.Join(c.StateDir, "artifacts")).Read(ctx, s.TextSHA256, 1<<20)
					if err != nil {
						return err
					}
					return emit(struct {
						Source            model.SourceCapture `json:"source"`
						Text              string              `json:"text"`
						VerificationLimit string              `json:"verification_limit"`
					}{s, string(text), "Captured quote occurrence is verifiable; publisher truth and semantic entailment are not established by a citation."})
				}
			}
			if page.NextCursor == "" {
				break
			}
			q.After = page.NextCursor
		}
		return &model.ProgramError{Code: "evidence_invalid", Message: "source not found in selected program revision"}
	}
	switch opts["--kind"] {
	case "sources":
		sources, err := db.ProgramSourcePage(ctx, id, q)
		if err != nil {
			return err
		}
		return emit(sources)
	case "artifacts":
		page, err := db.ProgramArtifactPage(ctx, id, q)
		if err != nil {
			return err
		}
		return emit(page)
	case "":
		if q.After != "" {
			return programInvalid("evidence pagination requires --kind sources or --kind artifacts")
		}
		sources, err := db.ProgramSourcePage(ctx, id, q)
		if err != nil {
			return err
		}
		page, err := db.ProgramArtifactPage(ctx, id, q)
		if err != nil {
			return err
		}
		return emit(struct {
			Sources   store.ProgramSourcePage   `json:"sources"`
			Artifacts store.ProgramArtifactPage `json:"artifacts"`
		}{sources, page})
	default:
		return programInvalid("evidence --kind must be sources or artifacts")
	}
}
