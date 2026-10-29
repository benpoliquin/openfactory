// Package intake parses immutable, operator-reviewed feature documents.
package intake

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
	"openfactory/internal/model"
)

var identifier = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$`)

// Validate checks the source contract independently of project policy.
func Validate(f model.Feature) error {
	if !identifier.MatchString(f.ID) || !identifier.MatchString(f.Project) {
		return errors.New("id and project must be safe identifiers")
	}
	if strings.TrimSpace(f.Title) == "" || strings.TrimSpace(f.Body) == "" {
		return errors.New("title and markdown body are required")
	}
	if f.Kind != "feature" && f.Kind != "fix" && f.Kind != "maintenance" && f.Kind != "research" && f.Kind != "plan" && f.Kind != model.IntakeFeatureKind {
		return fmt.Errorf("unsupported kind %q", f.Kind)
	}
	if (f.Kind == model.IntakeFeatureKind || f.IntakeStepID != "" || strings.HasPrefix(f.ID, model.IntakeFeaturePrefix)) &&
		(f.Kind != model.IntakeFeatureKind || !identifier.MatchString(f.IntakeStepID) || !strings.HasPrefix(f.ID, model.IntakeFeaturePrefix) || f.DiscoveryStepID != "" || f.CompletionTarget != "reviewed") {
		return errors.New("intake features require a server association, reserved identifier, and reviewed target")
	}
	if f.Risk != "low" && f.Risk != "normal" && f.Risk != "high" && f.Risk != "critical" {
		return fmt.Errorf("unsupported risk %q", f.Risk)
	}
	switch f.CompletionTarget {
	case "reviewed", "pull_request", "integrated", "staging", "production":
	default:
		return fmt.Errorf("unsupported completion target %q", f.CompletionTarget)
	}
	if f.Kind == "plan" && f.CompletionTarget != "reviewed" {
		return errors.New("planning may only target reviewed")
	}
	if (len(f.Checks) == 0 && f.Kind != "plan" && f.DiscoveryStepID == "" && f.IntakeStepID == "") || len(f.Acceptance) == 0 {
		return errors.New("nonempty checks and acceptance are required")
	}
	seen := map[string]bool{}
	for _, c := range f.Checks {
		if !identifier.MatchString(c) || seen[c] {
			return fmt.Errorf("invalid or duplicate check %q", c)
		}
		seen[c] = true
	}
	seen = map[string]bool{}
	for _, a := range f.Acceptance {
		if !identifier.MatchString(a.ID) || seen[a.ID] || strings.TrimSpace(a.Scenario) == "" || strings.TrimSpace(a.Expected) == "" {
			return fmt.Errorf("invalid or duplicate acceptance %q", a.ID)
		}
		seen[a.ID] = true
	}
	seen = map[string]bool{}
	for _, d := range f.DependsOn {
		if strings.HasPrefix(d, "openfactory-discovery-") {
			return fmt.Errorf("discovery feature %q cannot be a code dependency; use a program evidence gate", d)
		}
		if strings.HasPrefix(d, model.IntakeFeaturePrefix) {
			return fmt.Errorf("intake feature %q cannot be a code dependency", d)
		}
		if !identifier.MatchString(d) || d == f.ID || seen[d] {
			return fmt.Errorf("invalid or duplicate dependency %q", d)
		}
		seen[d] = true
	}
	if gate := f.ProgramEvidenceGate; gate != nil {
		if !identifier.MatchString(gate.ProgramID) || gate.Revision < 1 || !identifier.MatchString(gate.EvaluationArtifactID) {
			return errors.New("program evidence gate requires safe program and evaluation identifiers and a positive revision")
		}
		hash, err := hex.DecodeString(gate.ManifestSHA)
		if err != nil || len(hash) != sha256.Size || gate.ManifestSHA != strings.ToLower(gate.ManifestSHA) {
			return errors.New("program evidence gate requires a lowercase SHA-256 manifest hash")
		}
	}
	return nil
}

// Revision hashes only immutable source fields, never mutable ledger state.
func Revision(f model.Feature) string {
	source := struct {
		ID, Project, Title, Kind, Risk string
		Priority                       int
		DependsOn                      []string
		CompletionTarget               string
		Checks                         []string
		Acceptance                     []model.Acceptance
		Body                           string
		DiscoveryStepID                string                     `json:",omitempty"`
		ProgramEvidenceGate            *model.ProgramEvidenceGate `json:",omitempty"`
		IntakeStepID                   string                     `json:",omitempty"`
	}{f.ID, f.Project, f.Title, f.Kind, f.Risk, f.Priority, f.DependsOn, f.CompletionTarget, f.Checks, f.Acceptance, f.Body, f.DiscoveryStepID, f.ProgramEvidenceGate, f.IntakeStepID}
	b, _ := json.Marshal(source)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func Parse(data []byte) (model.Feature, error) {
	var f model.Feature
	data = bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n"))
	if !bytes.HasPrefix(data, []byte("---\n")) {
		return f, errors.New("document must begin with YAML frontmatter")
	}
	end := bytes.Index(data[4:], []byte("\n---\n"))
	if end < 0 {
		return f, errors.New("unterminated YAML frontmatter")
	}
	end += 4
	dec := yaml.NewDecoder(bytes.NewReader(data[4:end]))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil {
		return f, fmt.Errorf("frontmatter: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return f, errors.New("frontmatter must contain exactly one YAML document")
	}
	// Discovery associations are server-owned. KnownFields also rejects any
	// discovery_step_id key, including an empty value, because its YAML tag is "-".
	if err := ValidateExternal(f); err != nil {
		return f, err
	}
	f.Body = strings.TrimSpace(string(data[end+5:]))
	if f.Kind == "" {
		f.Kind = "feature"
	}
	if f.Risk == "" {
		f.Risk = "normal"
	}
	if f.CompletionTarget == "" {
		f.CompletionTarget = "integrated"
	}
	if err := Validate(f); err != nil {
		return f, err
	}
	f.Revision = Revision(f)
	return f, nil
}

// ValidateExternal rejects reserved server-owned work at every import boundary.
// Validate deliberately accepts legitimate internal work for engine admission;
// the store additionally checks the persisted step-to-feature relation.
func ValidateExternal(f model.Feature) error {
	if strings.HasPrefix(f.ID, "openfactory-discovery-") || f.DiscoveryStepID != "" {
		return errors.New("discovery features may only be created by the server")
	}
	if strings.HasPrefix(f.ID, model.IntakeFeaturePrefix) || f.IntakeStepID != "" || f.Kind == model.IntakeFeatureKind {
		return errors.New("intake features may only be created by the server")
	}
	return nil
}
