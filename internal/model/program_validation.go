package model

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"path"
	"strings"
)

// StrictDecode rejects ambiguous JSON before decoding into its declared schema.
func StrictDecode(data []byte, target any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	if err := scanProgramJSON(d, 0); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return fmt.Errorf("trailing JSON tokens")
	}
	d = json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	d.DisallowUnknownFields()
	return d.Decode(target)
}
func scanProgramJSON(d *json.Decoder, depth int) error {
	if depth > 128 {
		return fmt.Errorf("JSON nesting exceeds 128")
	}
	t, err := d.Token()
	if err != nil {
		return err
	}
	delim, ok := t.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for d.More() {
			k, err := d.Token()
			if err != nil {
				return err
			}
			key, ok := k.(string)
			if !ok || seen[key] {
				return fmt.Errorf("duplicate or invalid JSON key %v", k)
			}
			seen[key] = true
			if err := scanProgramJSON(d, depth+1); err != nil {
				return err
			}
		}
	case '[':
		for d.More() {
			if err := scanProgramJSON(d, depth+1); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("unexpected JSON delimiter")
	}
	_, err = d.Token()
	return err
}
func programRequired(v ...string) error {
	for _, s := range v {
		if strings.TrimSpace(s) == "" {
			return fmt.Errorf("required field is empty")
		}
	}
	return nil
}

// programIdentifier matches intake's public identifier grammar without an import cycle.
func programIdentifier(s string) bool {
	if len(s) == 0 || len(s) > 128 {
		return false
	}
	for i := range len(s) {
		c := s[i]
		alphanumeric := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
		if !alphanumeric && (i == 0 || c != '.' && c != '_' && c != '-') {
			return false
		}
	}
	return true
}
func programEnum(s string, values ...string) bool {
	for _, v := range values {
		if s == v {
			return true
		}
	}
	return false
}
func programIDs(ids []string) error {
	seen := map[string]bool{}
	for _, id := range ids {
		if strings.TrimSpace(id) == "" || seen[id] {
			return fmt.Errorf("empty or duplicate identifier %q", id)
		}
		seen[id] = true
	}
	return nil
}
func programSHA(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
func programPath(s string) bool {
	return s != "" && s != "." && !strings.ContainsAny(s, "\\\x00") && !path.IsAbs(s) && path.Clean(s) == s && s != ".." && !strings.HasPrefix(s, "../")
}
func programRefs(refs []ProgramInputRef) error {
	seen := map[string]bool{}
	for _, r := range refs {
		if programRequired(r.Kind, r.ID) != nil || !programSHA(r.SHA) || r.Revision < 0 {
			return fmt.Errorf("invalid input reference")
		}
		key := r.Kind + "/" + r.ID
		if seen[key] {
			return fmt.Errorf("duplicate input reference")
		}
		seen[key] = true
	}
	return nil
}

// CanonicalProgramPayload returns server-typed Go JSON, not RFC 8785 JSON.
// Referential existence, provenance, and authorization are deliberately store concerns.
func CanonicalProgramPayload(kind string, payload json.RawMessage) ([]byte, error) {
	var target any
	switch kind {
	case ArtifactRequirements:
		target = &RequirementsPayload{}
	case ArtifactQuestions:
		target = &QuestionsPayload{}
	case ArtifactClaims:
		target = &ClaimsPayload{}
	case ArtifactArchitecture:
		target = &ArchitecturePayload{}
	case ArtifactExperimentContracts:
		target = &ExperimentContractsPayload{}
	case ArtifactEvaluation:
		target = &EvaluationPayload{}
	case ArtifactImplementationContracts:
		target = &ImplementationContractsPayload{}
	case ArtifactRevision:
		target = &RevisionPayload{}
	default:
		return nil, fmt.Errorf("unknown artifact kind %q", kind)
	}
	if bytes.Equal(bytes.TrimSpace(payload), []byte("null")) {
		return nil, fmt.Errorf("payload must be an object")
	}
	if err := StrictDecode(payload, target); err != nil {
		return nil, err
	}
	if err := validateProgramPayload(target); err != nil {
		return nil, fmt.Errorf("%s: %w", kind, err)
	}
	return json.Marshal(target)
}
func validateProgramPayload(payload any) error {
	var ids []string
	switch p := payload.(type) {
	case *RequirementsPayload:
		if len(p.Requirements) == 0 {
			return fmt.Errorf("requirements are empty")
		}
		for _, r := range p.Requirements {
			ids = append(ids, r.ID)
			if programRequired(r.Behavior, r.Rationale, r.Priority) != nil || len(r.Acceptance) == 0 {
				return fmt.Errorf("incomplete requirement")
			}
			if !programEnum(r.Priority, "must", "should", "could", "wont", "high", "medium", "low") {
				return fmt.Errorf("invalid requirement priority")
			}
			var aIDs []string
			for _, a := range r.Acceptance {
				aIDs = append(aIDs, a.ID)
				if programRequired(a.Scenario, a.Expected) != nil {
					return fmt.Errorf("incomplete acceptance")
				}
			}
			if err := programIDs(aIDs); err != nil {
				return err
			}
			if err := programIDs(r.ClaimRefs); err != nil {
				return err
			}
			if err := programIDs(r.QuestionRefs); err != nil {
				return err
			}
		}
	case *QuestionsPayload:
		for _, q := range p.Questions {
			ids = append(ids, q.ID)
			if programRequired(q.Text) != nil || !programEnum(q.Route, "source", "experiment", "operator") {
				return fmt.Errorf("invalid question")
			}
			if err := programIDs(q.AffectedIDs); err != nil {
				return err
			}
		}
	case *ClaimsPayload:
		for _, c := range p.Claims {
			ids = append(ids, c.ID)
			if programRequired(c.Statement) != nil || !programEnum(c.Classification, "public_statement", "inference", "unknown", "contradicted") {
				return fmt.Errorf("invalid claim")
			}
			if c.Classification == "public_statement" && len(c.Citations) == 0 {
				return fmt.Errorf("public statement requires citation")
			}
			if c.Classification == "inference" && len(c.InferencePremises) == 0 {
				return fmt.Errorf("inference requires premises")
			}
			if err := programIDs(c.InferencePremises); err != nil {
				return err
			}
			for _, premise := range c.InferencePremises {
				if premise == c.ID {
					return fmt.Errorf("self-referential inference")
				}
			}
			for _, cite := range c.Citations {
				if programRequired(cite.SourceID, cite.Quote) != nil || !programSHA(cite.TextSHA) || cite.StartLine < 0 || cite.EndLine < 0 || (cite.StartLine == 0) != (cite.EndLine == 0) || cite.EndLine < cite.StartLine || (cite.JSONPointer != "" && !strings.HasPrefix(cite.JSONPointer, "/")) {
					return fmt.Errorf("invalid citation")
				}
			}
		}
	case *ArchitecturePayload:
		if len(p.Decisions) == 0 {
			return fmt.Errorf("decisions are empty")
		}
		for _, d := range p.Decisions {
			ids = append(ids, d.ID)
			if programRequired(d.Title, d.Selected) != nil || len(d.Alternatives) < 2 || len(d.Criteria) == 0 || len(d.Tradeoffs) == 0 || len(d.Consequences) == 0 {
				return fmt.Errorf("incomplete architecture decision")
			}
			alternatives := map[string]bool{}
			var altIDs []string
			for _, a := range d.Alternatives {
				altIDs = append(altIDs, a.ID)
				alternatives[a.ID] = true
				if programRequired(a.Description) != nil {
					return fmt.Errorf("empty alternative")
				}
			}
			if err := programIDs(altIDs); err != nil {
				return err
			}
			if !alternatives[d.Selected] || len(d.Rejected) != len(d.Alternatives)-1 {
				return fmt.Errorf("selected/rejected alternatives do not cover decision")
			}
			if err := programIDs(d.Rejected); err != nil {
				return err
			}
			for _, r := range d.Rejected {
				if r == d.Selected || !alternatives[r] {
					return fmt.Errorf("invalid rejected alternative")
				}
			}
			for _, refs := range [][]string{d.RequirementRefs, d.QuestionRefs, d.ExperimentRefs} {
				if err := programIDs(refs); err != nil {
					return err
				}
			}
		}
	case *ExperimentContractsPayload:
		if len(p.Experiments) == 0 {
			return fmt.Errorf("experiments are empty")
		}
		for _, e := range p.Experiments {
			ids = append(ids, e.ID)
			if err := validateProgramExperiment(e); err != nil {
				return err
			}
		}
	case *EvaluationPayload:
		if !programSHA(p.EvaluatedManifestSHA) || !programEnum(p.Verdict, "accept", "revise", "needs_operator", "insufficient_evidence") {
			return fmt.Errorf("invalid evaluation")
		}
		if p.Verdict == "revise" && len(p.RequiredChanges) == 0 {
			return fmt.Errorf("revision requires changes")
		}
		for _, f := range p.Findings {
			ids = append(ids, f.ID)
			if programRequired(f.Detail) != nil || !programEnum(f.Severity, "info", "warning", "error", "critical", "blocker") {
				return fmt.Errorf("invalid finding")
			}
			if err := programRefs(f.Refs); err != nil {
				return err
			}
		}
		var subjects []string
		for _, c := range p.Coverage {
			subjects = append(subjects, c.SubjectID)
			if !programEnum(c.Status, "covered", "partial", "missing", "contradicted", "unknown", "satisfied", "unsatisfied") {
				return fmt.Errorf("invalid coverage")
			}
			if err := programRefs(c.EvidenceRefs); err != nil {
				return err
			}
		}
		if err := programIDs(subjects); err != nil {
			return err
		}
	case *ImplementationContractsPayload:
		if len(p.Contracts) == 0 {
			return fmt.Errorf("contracts are empty")
		}
		for _, c := range p.Contracts {
			f := c.Feature
			ids = append(ids, f.ID)
			if programRequired(f.ID, f.Project, f.Title, f.Body) != nil {
				return fmt.Errorf("incomplete feature")
			}
			if !programIdentifier(f.ID) || !programIdentifier(f.Project) {
				return fmt.Errorf("feature id and project must be safe identifiers")
			}
			if strings.HasPrefix(f.ID, "discovery-") || strings.HasPrefix(f.ID, "discovery/") || strings.HasPrefix(f.ID, "openfactory-discovery-") || strings.HasPrefix(f.ID, IntakeFeaturePrefix) || f.IntakeStepID != "" || f.DiscoveryStepID != "" || f.ProgramEvidenceGate != nil || f.Revision != "" || f.PolicyHash != "" || f.State != "" || f.Reason != "" || f.Attempts != 0 || f.Receipt != "" || !f.CreatedAt.IsZero() || !f.UpdatedAt.IsZero() {
				return fmt.Errorf("server-owned feature metadata is forbidden")
			}
			if !programEnum(f.Kind, "feature", "fix", "maintenance", "research", "plan") || !programEnum(f.CompletionTarget, "reviewed", "pull_request", "integrated", "staging", "production") || !programEnum(f.Risk, "low", "normal", "high", "critical") {
				return fmt.Errorf("invalid feature kind, risk or completion target")
			}
			if f.Kind == "plan" && f.CompletionTarget != "reviewed" {
				return fmt.Errorf("planning may only target reviewed")
			}
			if (f.Kind != "plan" && len(f.Checks) == 0) || len(f.Acceptance) == 0 {
				return fmt.Errorf("feature checks and acceptance required")
			}
			if err := programIDs(f.Checks); err != nil {
				return err
			}
			for _, check := range f.Checks {
				if !programIdentifier(check) {
					return fmt.Errorf("invalid check identifier")
				}
			}
			if err := programIDs(f.DependsOn); err != nil {
				return err
			}
			for _, dependency := range f.DependsOn {
				if strings.HasPrefix(dependency, IntakeFeaturePrefix) || strings.HasPrefix(dependency, "openfactory-discovery-") {
					return fmt.Errorf("internal work cannot be a code dependency")
				}
				if !programIdentifier(dependency) || dependency == f.ID {
					return fmt.Errorf("self-dependent feature")
				}
			}
			var acceptanceIDs []string
			for _, a := range f.Acceptance {
				acceptanceIDs = append(acceptanceIDs, a.ID)
				if !programIdentifier(a.ID) {
					return fmt.Errorf("invalid acceptance identifier")
				}
				if programRequired(a.Scenario, a.Expected) != nil {
					return fmt.Errorf("incomplete acceptance")
				}
			}
			if err := programIDs(acceptanceIDs); err != nil {
				return err
			}
			if len(c.RequirementRefs) == 0 || len(c.DecisionRefs) == 0 {
				return fmt.Errorf("implementation trace references required")
			}
			if err := programIDs(c.RequirementRefs); err != nil {
				return err
			}
			if err := programIDs(c.DecisionRefs); err != nil {
				return err
			}
			if err := programRefs(c.EvidenceRefs); err != nil {
				return err
			}
		}
		if err := validateProgramContractDependencies(p.Contracts); err != nil {
			return err
		}
	case *RevisionPayload:
		if programRequired(p.Trigger) != nil || !programSHA(p.PredecessorManifestSHA) || !programEnum(p.Materiality, "material", "non_material") {
			return fmt.Errorf("invalid revision")
		}
		if len(p.ChangedDecisions)+len(p.ChangedAssumptions) == 0 {
			return fmt.Errorf("revision has no changes")
		}
		if err := programRefs(p.FalsifiedEvidence); err != nil {
			return err
		}
		if err := programIDs(p.ChangedDecisions); err != nil {
			return err
		}
		if err := programIDs(p.ChangedAssumptions); err != nil {
			return err
		}
	}
	return programIDs(ids)
}
func validateProgramExperiment(e ExperimentContract) error {
	if programRequired(e.ID, e.Hypothesis, e.Falsification, e.Environment.Profile) != nil || !programSHA(e.BundleManifestSHA) || len(e.Commands) == 0 || len(e.Commands) > 32 || len(e.Observations) == 0 || len(e.Observations) > 128 || len(e.Acceptance) == 0 || len(e.Acceptance) > 128 {
		return fmt.Errorf("incomplete or oversized experiment")
	}
	if len(e.BundleFiles) == 0 || len(e.BundleFiles) > 128 {
		return fmt.Errorf("experiment requires 1 to 128 declared bundle files")
	}
	if err := programIDs(e.BundleFiles); err != nil {
		return err
	}
	if !programIdentifier(e.ID) {
		return fmt.Errorf("invalid experiment identifier")
	}
	for _, file := range e.BundleFiles {
		if !programPath(file) || !strings.HasPrefix(file, "experiments/"+e.ID+"/") {
			return fmt.Errorf("bundle file must be strictly under experiment directory")
		}
	}
	if err := programIDs(e.QuestionRefs); err != nil {
		return err
	}
	if err := programIDs(e.DecisionRefs); err != nil {
		return err
	}
	if err := programRefs(e.Environment.DatasetRefs); err != nil {
		return err
	}
	for _, c := range e.Commands {
		if programRequired(c.Profile) != nil || len(c.Args) > 128 {
			return fmt.Errorf("invalid command profile/arguments")
		}
		for _, a := range c.Args {
			if len(a) > 4096 || strings.ContainsRune(a, 0) {
				return fmt.Errorf("invalid command argument")
			}
		}
	}
	observations := map[string]ExperimentObservationSpec{}
	var ids []string
	for _, o := range e.Observations {
		ids = append(ids, o.Name)
		observations[o.Name] = o
		if !programEnum(o.Type, "number", "integer", "string", "boolean") || !programPath(o.OutputPath) || (o.JSONPointer != "" && !strings.HasPrefix(o.JSONPointer, "/")) {
			return fmt.Errorf("invalid observation")
		}
		for _, input := range e.BundleFiles {
			if o.OutputPath == input {
				return fmt.Errorf("observation output must not be a frozen bundle input")
			}
		}
	}
	if err := programIDs(ids); err != nil {
		return err
	}
	for _, p := range e.Acceptance {
		o, ok := observations[p.Observation]
		if !ok || !programEnum(p.Operator, "eq", "ne", "lt", "le", "gt", "ge", "in") || p.Unit != o.Unit || len(p.Value) > 16384 {
			return fmt.Errorf("invalid predicate")
		}
		var value any
		if err := StrictDecode(p.Value, &value); err != nil {
			return err
		}
		if p.Operator == "in" {
			values, ok := value.([]any)
			if !ok || len(values) == 0 || len(values) > 128 {
				return fmt.Errorf("membership requires bounded array")
			}
			for _, v := range values {
				if !programScalar(o.Type, v) {
					return fmt.Errorf("predicate type mismatch")
				}
			}
		} else {
			if !programScalar(o.Type, value) {
				return fmt.Errorf("predicate type mismatch")
			}
			if programEnum(p.Operator, "lt", "le", "gt", "ge") && !programEnum(o.Type, "number", "integer") {
				return fmt.Errorf("ordering requires numeric observation")
			}
		}
	}
	l := e.Limits
	if l.WallSeconds <= 0 || l.WallSeconds > 300 || l.MaxOutputBytes <= 0 || l.MaxOutputBytes > 1<<20 || l.MaxArtifactBytes <= 0 || l.MaxArtifactBytes > 16<<20 || l.MaxArtifactFiles <= 0 || l.MaxArtifactFiles > 128 {
		return fmt.Errorf("experiment limits must be positive and bounded")
	}
	if e.ExternalAuthority.Spend < 0 {
		return fmt.Errorf("negative spend")
	}
	return nil
}
func programScalar(kind string, v any) bool {
	switch kind {
	case "number":
		_, ok := v.(json.Number)
		return ok
	case "integer":
		n, ok := v.(json.Number)
		if !ok {
			return false
		}
		_, err := n.Int64()
		return err == nil
	case "string":
		_, ok := v.(string)
		return ok
	case "boolean":
		_, ok := v.(bool)
		return ok
	}
	return false
}

func ValidateProgramLimits(l ProgramLimits) error {
	if l.MaxIterations < 0 || l.MaxModelCalls < 0 || l.MaxSteps < 0 || l.MaxExperiments < 0 || l.MaxSearches < 0 || l.MaxSources < 0 || l.MaxTotalCaptureBytes < 0 {
		return fmt.Errorf("negative program limit")
	}
	return nil
}
func ValidateProgramSpec(s ProgramSpec) error {
	return validateProgramSpec(s, true)
}

// ValidateProgramDraftSpec checks source shape without granting or requiring
// model/search disclosure consent. Importing an unapproved proposal is not
// execution authority; approval and dispatch must use ValidateProgramSpec.
func ValidateProgramDraftSpec(s ProgramSpec) error {
	return validateProgramSpec(s, false)
}

func validateProgramSpec(s ProgramSpec, requireDisclosure bool) error {
	if !programIdentifier(s.ID) || !programIdentifier(s.Project) {
		return fmt.Errorf("program id and project must be safe identifiers")
	}
	if programRequired(s.ID, s.Project, s.Title, s.Objective) != nil {
		return fmt.Errorf("program identity and objective required")
	}
	if err := ValidateProgramLimits(s.Limits); err != nil {
		return err
	}
	if requireDisclosure && len(s.Disclosure.ModelProviderIDs) == 0 {
		return fmt.Errorf("model provider disclosure required")
	}
	if err := programIDs(s.Disclosure.ModelProviderIDs); err != nil {
		return err
	}
	var ids []string
	if err := programIDs(s.Implementation.PermittedFeatureKinds); err != nil {
		return err
	}
	for _, kind := range s.Implementation.PermittedFeatureKinds {
		if !programEnum(kind, "feature", "fix", "maintenance", "research", "plan") {
			return fmt.Errorf("invalid permitted feature kind")
		}
	}
	if err := programIDs(s.Implementation.PermittedCompletionTargets); err != nil {
		return err
	}
	for _, target := range s.Implementation.PermittedCompletionTargets {
		if !programEnum(target, "reviewed", "pull_request", "integrated", "staging", "production") {
			return fmt.Errorf("invalid permitted completion target")
		}
	}
	for _, seed := range s.Seeds {
		ids = append(ids, seed.ID)
		switch seed.Kind {
		case "public_url":
			u, err := url.Parse(seed.URL)
			if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" || (u.Port() != "" && u.Port() != "443") || seed.Path != "" {
				return fmt.Errorf("invalid public seed URL")
			}
		case "bundle":
			if !programPath(seed.Path) || seed.URL != "" {
				return fmt.Errorf("invalid bundle seed")
			}
			if requireDisclosure && !s.Disclosure.PrivateMaterialToModels {
				return fmt.Errorf("bundle seeds require explicit private_material_to_models disclosure")
			}
		default:
			return fmt.Errorf("invalid seed kind")
		}
	}
	if err := programIDs(ids); err != nil {
		return err
	}
	ids = nil
	if requireDisclosure && !s.Disclosure.PublicSearchAllowed && len(s.Disclosure.PublicSearchTopics) > 0 {
		return fmt.Errorf("search topics require public search disclosure")
	}
	for _, t := range s.Disclosure.PublicSearchTopics {
		ids = append(ids, t.ID)
		if programRequired(t.Query) != nil {
			return fmt.Errorf("empty search query")
		}
	}
	return programIDs(ids)
}

// NormalizeDiscoveryPolicy fills omitted limits with the discovery contract defaults.
func NormalizeDiscoveryPolicy(p DiscoveryPolicy) DiscoveryPolicy {
	def := func(v *int, n int) {
		if *v <= 0 {
			*v = n
		}
	}
	def64 := func(v *int64, n int64) {
		if *v <= 0 {
			*v = n
		}
	}
	def(&p.Limits.MaxIterations, 2)
	def(&p.Limits.MaxModelCalls, 20)
	def(&p.Limits.MaxSteps, 32)
	def(&p.Limits.MaxExperiments, 8)
	def(&p.Limits.MaxSearches, 20)
	def(&p.Limits.MaxSources, 100)
	def64(&p.Limits.MaxTotalCaptureBytes, 128<<20)
	def64(&p.MaxCaptureBytes, 8<<20)
	def64(&p.MaxTextBytes, 1<<20)
	def(&p.BundleMaxFiles, 128)
	def64(&p.BundleMaxFileBytes, 8<<20)
	def64(&p.BundleMaxTotalBytes, 64<<20)
	def(&p.ResearchTimeoutSeconds, 30)
	def(&p.MaxRedirects, 3)
	def(&p.SourceFreshnessSeconds, 30*24*60*60)
	def(&p.ExperimentLimits.WallSeconds, 300)
	def64(&p.ExperimentLimits.MaxOutputBytes, 1<<20)
	def64(&p.ExperimentLimits.MaxArtifactBytes, 16<<20)
	def(&p.ExperimentLimits.MaxArtifactFiles, 128)
	if p.BrokerURL == "" {
		p.BrokerURL = "http://127.0.0.1:38888"
	}
	if p.BrokerBind == "" {
		p.BrokerBind = "127.0.0.1:38888"
	}
	return p
}

// Only dependencies within this batch can be resolved without store authority.
func validateProgramContractDependencies(contracts []ImplementationContract) error {
	dependencies := make(map[string][]string, len(contracts))
	for _, contract := range contracts {
		dependencies[contract.Feature.ID] = contract.Feature.DependsOn
	}
	state := make(map[string]uint8, len(contracts))
	var visit func(string) error
	visit = func(id string) error {
		if state[id] == 1 {
			return fmt.Errorf("implementation contracts contain a dependency cycle")
		}
		if state[id] == 2 {
			return nil
		}
		state[id] = 1
		for _, dependency := range dependencies[id] {
			if _, internal := dependencies[dependency]; internal {
				if err := visit(dependency); err != nil {
					return err
				}
			}
		}
		state[id] = 2
		return nil
	}
	for id := range dependencies {
		if err := visit(id); err != nil {
			return err
		}
	}
	return nil
}
