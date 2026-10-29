package intake

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
	"openfactory/internal/model"
)

// Export serializes only immutable public source. Internal features cannot be
// exported as importable product contracts.
func Export(f model.Feature) ([]byte, error) {
	if err := ValidateExternal(f); err != nil {
		return nil, err
	}
	if err := Validate(f); err != nil {
		return nil, err
	}
	// Parse normalizes whitespace at the body boundary. Refuse lossy export.
	if f.Body != strings.TrimSpace(f.Body) || strings.Contains(f.Body, "\r\n") {
		return nil, errors.New("feature body must have canonical boundary whitespace and line endings")
	}
	// Nil and empty slices have different historical domain hashes. Optional
	// pointer fields retain both states through YAML instead of turning nil into [].
	var depends, checks *[]string
	if f.DependsOn != nil {
		depends = &f.DependsOn
	}
	if f.Checks != nil {
		checks = &f.Checks
	}
	source := struct {
		ID                  string                     `yaml:"id"`
		Project             string                     `yaml:"project"`
		Title               string                     `yaml:"title"`
		Kind                string                     `yaml:"kind"`
		Risk                string                     `yaml:"risk"`
		Priority            int                        `yaml:"priority"`
		DependsOn           *[]string                  `yaml:"depends_on,omitempty"`
		CompletionTarget    string                     `yaml:"completion_target"`
		Checks              *[]string                  `yaml:"checks,omitempty"`
		Acceptance          []model.Acceptance         `yaml:"acceptance"`
		ProgramEvidenceGate *model.ProgramEvidenceGate `yaml:"program_evidence_gate,omitempty"`
	}{f.ID, f.Project, f.Title, f.Kind, f.Risk, f.Priority, depends, f.CompletionTarget, checks, f.Acceptance, f.ProgramEvidenceGate}
	front, err := yaml.Marshal(source)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	out.WriteString("---\n")
	out.Write(front)
	out.WriteString("---\n\n")
	out.WriteString(f.Body)
	out.WriteByte('\n')
	return out.Bytes(), nil
}

// NewPlan is the shared CLI and authenticated intake planning contract.
func NewPlan(id, project, title, goal string) (model.Feature, error) {
	f := model.Feature{ID: id, Project: project, Title: title, Kind: "plan", Risk: "normal", Priority: 10, CompletionTarget: "reviewed", Body: strings.TrimSpace(strings.ReplaceAll(goal, "\r\n", "\n")), Acceptance: []model.Acceptance{{ID: "read-only-plan", Scenario: "Inspect current source and goal", Expected: "Return a bounded dependency graph of feature proposals without modifying the repository or approving implementation"}}}
	if err := ValidateExternal(f); err != nil {
		return f, err
	}
	if err := Validate(f); err != nil {
		return f, err
	}
	f.Revision = Revision(f)
	return f, nil
}

// NormalizeProgramSpec is shared by CLI and intake; no caller may invent more
// permissive defaults than the operator's discovery caps.
func NormalizeProgramSpec(spec *model.ProgramSpec, c model.Config) error {
	if spec == nil {
		return errors.New("program spec is required")
	}
	p, ok := c.Projects[spec.Project]
	if !ok || !p.Trusted {
		return errors.New("program references an unenrolled project")
	}
	if err := model.ValidateProgramDraftSpec(*spec); err != nil {
		return err
	}
	limits := c.Discovery.Limits
	pairs := []struct {
		value *int
		cap   int
	}{{&spec.Limits.MaxIterations, limits.MaxIterations}, {&spec.Limits.MaxModelCalls, limits.MaxModelCalls}, {&spec.Limits.MaxSteps, limits.MaxSteps}, {&spec.Limits.MaxExperiments, limits.MaxExperiments}, {&spec.Limits.MaxSearches, limits.MaxSearches}, {&spec.Limits.MaxSources, limits.MaxSources}}
	for _, pair := range pairs {
		if *pair.value == 0 {
			*pair.value = pair.cap
		}
		if *pair.value < 0 || *pair.value > pair.cap {
			return errors.New("program limit exceeds operator discovery policy")
		}
	}
	if spec.Limits.MaxTotalCaptureBytes == 0 {
		spec.Limits.MaxTotalCaptureBytes = limits.MaxTotalCaptureBytes
	}
	if spec.Limits.MaxTotalCaptureBytes < 0 || spec.Limits.MaxTotalCaptureBytes > limits.MaxTotalCaptureBytes {
		return errors.New("program capture budget exceeds operator policy")
	}
	return nil
}

type CanonicalDraft struct {
	Route            string
	Format           string
	CanonicalSource  string
	CanonicalPayload string
	SourceSHA        string
	CanonicalSHA     string
	DomainRevision   string
	Feature          *model.Feature
	Program          *model.ProgramSpec
}

// SHA256 returns the lowercase digest of exact bytes, without newline rewriting.
func SHA256(data []byte) string { h := sha256.Sum256(data); return hex.EncodeToString(h[:]) }

// ProgramRevisionHash preserves the store's historical imported-revision shape.
// Answer amendments have their own lineage hash and must not use this helper.
func ProgramRevisionHash(programID string, number int, spec model.ProgramSpec, seedManifestSHA string) string {
	b, _ := json.Marshal(struct {
		ProgramID       string
		Number          int
		Spec            model.ProgramSpec
		SeedManifestSHA string
	}{programID, number, spec, seedManifestSHA})
	return SHA256(b)
}

// ParseCanonical retains exact input bytes, validates the selected project and
// trusted execution policy, and never repairs or silently trims a model draft.
func ParseCanonical(route, format string, source []byte, project string, c model.Config) (CanonicalDraft, error) {
	out := CanonicalDraft{Route: route, Format: format, CanonicalSource: string(source), SourceSHA: SHA256(source)}
	limit := c.Intake.MaxCanonicalBytes
	if limit <= 0 {
		limit = 256 << 10
	}
	if len(source) == 0 || len(source) > limit || !utf8.Valid(source) {
		return out, errors.New("canonical source exceeds bounds or is not UTF-8")
	}
	p, ok := c.Projects[project]
	if !ok || !p.Trusted {
		return out, errors.New("canonical source references an unenrolled project")
	}
	switch route {
	case model.IntakeRouteFeature, model.IntakeRoutePlan:
		if format != model.IntakeFormatFeature {
			return out, errors.New("feature and plan routes require feature_markdown")
		}
		f, err := Parse(source)
		if err != nil {
			return out, err
		}
		if f.Project != project {
			return out, errors.New("canonical project does not match request project")
		}
		if route == model.IntakeRoutePlan && f.Kind != "plan" || route == model.IntakeRouteFeature && f.Kind == "plan" {
			return out, errors.New("canonical kind does not match route")
		}
		for _, check := range f.Checks {
			if _, ok := p.Checks[check]; !ok {
				return out, fmt.Errorf("untrusted check %q", check)
			}
		}
		if f.CompletionTarget == "staging" || f.CompletionTarget == "production" {
			if release, ok := p.Releases[f.CompletionTarget]; !ok || !release.Allowed {
				return out, errors.New("completion target is not enabled by operator policy")
			}
		}
		out.Feature = &f
		out.DomainRevision = f.Revision
		canonical, err := Export(f)
		if err != nil {
			return out, err
		}
		out.CanonicalSHA = SHA256(canonical)
		out.CanonicalPayload = string(canonical)
	case model.IntakeRouteProgram:
		if format != model.IntakeFormatProgram {
			return out, errors.New("program route requires program_json")
		}
		var spec model.ProgramSpec
		if err := DecodeStrict(source, &spec); err != nil {
			return out, err
		}
		if spec.Project != project {
			return out, errors.New("canonical project does not match request project")
		}
		if err := NormalizeProgramSpec(&spec, c); err != nil {
			return out, err
		}
		canonical, err := json.Marshal(spec)
		if err != nil {
			return out, err
		}
		out.Program = &spec
		out.CanonicalSHA = SHA256(canonical)
		out.CanonicalPayload = string(canonical)
		// Actual program revision also binds number and bundle and is assigned by
		// the store under its current-revision transaction, not guessed here.
	default:
		return out, errors.New("unsupported intake route")
	}
	return out, nil
}
