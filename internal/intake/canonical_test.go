package intake

import (
	"encoding/json"
	"strings"
	"testing"

	"openfactory/internal/model"
)

func intakeTestConfig() model.Config {
	return model.Config{Projects: map[string]model.Project{"cli": {Trusted: true, Checks: map[string]model.Command{"unit": {Argv: []string{"go", "test", "./..."}}}}}, Discovery: model.NormalizeDiscoveryPolicy(model.DiscoveryPolicy{})}
}

func TestLegacyFeatureRevisionAndExportRoundTrip(t *testing.T) {
	f, err := Parse([]byte(document))
	if err != nil {
		t.Fatal(err)
	}
	// Freeze the pre-intake source representation, including historical field names
	// and nil-slice encoding; a new empty association must not alter these bytes.
	legacy := struct {
		ID, Project, Title, Kind, Risk string
		Priority                       int
		DependsOn                      []string
		CompletionTarget               string
		Checks                         []string
		Acceptance                     []model.Acceptance
		Body                           string
		DiscoveryStepID                string                     `json:",omitempty"`
		ProgramEvidenceGate            *model.ProgramEvidenceGate `json:",omitempty"`
	}{f.ID, f.Project, f.Title, f.Kind, f.Risk, f.Priority, f.DependsOn, f.CompletionTarget, f.Checks, f.Acceptance, f.Body, f.DiscoveryStepID, f.ProgramEvidenceGate}
	old, _ := json.Marshal(legacy)
	if Revision(f) != SHA256(old) {
		t.Fatal("empty intake association changed historical revision")
	}
	f.ProgramEvidenceGate = &model.ProgramEvidenceGate{ProgramID: "research", Revision: 3, ManifestSHA: strings.Repeat("a", 64), EvaluationArtifactID: "evaluation"}
	f.Revision = Revision(f)
	f.State = "accepted"
	f.PolicyHash = "secret-policy"
	f.Reason = "private-ledger"
	exported, err := Export(f)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(exported), "private-ledger") || strings.Contains(string(exported), "secret-policy") {
		t.Fatal("export leaked mutable ledger metadata")
	}
	round, err := Parse(exported)
	if err != nil {
		t.Fatal(err)
	}
	if round.Revision != f.Revision || round.Body != f.Body || round.ProgramEvidenceGate.ManifestSHA != f.ProgramEvidenceGate.ManifestSHA {
		t.Fatal("public contract did not survive exact semantic round trip")
	}
	associated := f
	associated.IntakeStepID = "step-a"
	if Revision(associated) == f.Revision {
		t.Fatal("nonempty server association is missing from revision")
	}
	associated.IntakeStepID = "step-b"
	if Revision(associated) == Revision(func() model.Feature { g := f; g.IntakeStepID = "step-a"; return g }()) {
		t.Fatal("changed server association retained revision")
	}
}

func TestIntakeInternalAssociationCannotCrossImportBoundary(t *testing.T) {
	f, err := Parse([]byte(document))
	if err != nil {
		t.Fatal(err)
	}
	f.ID = model.IntakeFeaturePrefix + "job"
	f.Kind = model.IntakeFeatureKind
	f.CompletionTarget = "reviewed"
	f.IntakeStepID = "step-a"
	f.Checks = nil
	if err = Validate(f); err != nil {
		t.Fatal("legitimate engine work rejected:", err)
	}
	if err = ValidateExternal(f); err == nil {
		t.Fatal("external source forged server intake association")
	}
	if _, err = Export(f); err == nil {
		t.Fatal("internal work exported as importable product")
	}
	f.IntakeStepID = ""
	if err = Validate(f); err == nil {
		t.Fatal("reserved internal kind bypassed association gate")
	}
	for _, replacement := range []string{"risk: normal\nintake_step_id: forged", "risk: normal\nintake_step_id: ''"} {
		if _, err = Parse([]byte(strings.Replace(document, "risk: normal", replacement, 1))); err == nil {
			t.Fatal("external YAML association accepted")
		}
	}
	ordinary, err := Parse([]byte(document))
	if err != nil {
		t.Fatal(err)
	}
	ordinary.DependsOn = []string{model.IntakeFeaturePrefix + "reviewed"}
	if err = Validate(ordinary); err == nil {
		t.Fatal("internal draft used as product dependency")
	}
}

func TestCanonicalSourceExactSHAAndProjectAuthority(t *testing.T) {
	c := intakeTestConfig()
	a, err := ParseCanonical("feature", "feature_markdown", []byte(document), "cli", c)
	if err != nil {
		t.Fatal(err)
	}
	crlf := strings.ReplaceAll(document, "\n", "\r\n")
	b, err := ParseCanonical("feature", "feature_markdown", []byte(crlf), "cli", c)
	if err != nil {
		t.Fatal(err)
	}
	if a.SourceSHA == b.SourceSHA || a.DomainRevision != b.DomainRevision || a.CanonicalSHA != b.CanonicalSHA || b.CanonicalSource != crlf {
		t.Fatal("exact bytes and canonical domain identity were conflated")
	}
	c.Projects["other"] = c.Projects["cli"]
	if _, err = ParseCanonical("feature", "feature_markdown", []byte(document), "other", c); err == nil {
		t.Fatal("cross-project source accepted")
	}
	if _, err = ParseCanonical("plan", "feature_markdown", []byte(document), "cli", c); err == nil {
		t.Fatal("feature silently promoted to plan")
	}
	bad := strings.Replace(document, "checks: [unit]", "checks: [injected]", 1)
	if _, err = ParseCanonical("feature", "feature_markdown", []byte(bad), "cli", c); err == nil {
		t.Fatal("untrusted check accepted")
	}
	c.Intake.MaxCanonicalBytes = len(document) - 1
	if _, err = ParseCanonical("feature", "feature_markdown", []byte(document), "cli", c); err == nil {
		t.Fatal("canonical limit silently trimmed")
	}
}

func TestPlanConstructorRoundTripAndReadOnlyAuthority(t *testing.T) {
	f, err := NewPlan("plan-1", "cli", "A plan", "Inspect the parser, then propose bounded changes.")
	if err != nil {
		t.Fatal(err)
	}
	bytes, err := Export(f)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := Parse(bytes)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Revision != f.Revision || parsed.Kind != "plan" || parsed.CompletionTarget != "reviewed" || len(parsed.Acceptance) == 0 {
		t.Fatal("shared plan constructor lost read-only semantic contract")
	}
	f.CompletionTarget = "integrated"
	if _, err = Export(f); err == nil {
		t.Fatal("plan allowed execution target")
	}
}

func TestCanonicalProgramNormalizationAndHashScope(t *testing.T) {
	c := intakeTestConfig()
	spec := model.ProgramSpec{ID: "investigate", Project: "cli", Title: "Parser research", Objective: "Resolve parsing tradeoffs", Disclosure: model.ProgramDisclosure{ModelProviderIDs: []string{"provider"}}, Implementation: model.ProgramImplementationPolicy{PermittedFeatureKinds: []string{"feature"}, PermittedCompletionTargets: []string{"reviewed"}}}
	source, _ := json.Marshal(spec)
	result, err := ParseCanonical("program", "program_json", source, "cli", c)
	if err != nil {
		t.Fatal(err)
	}
	if result.Program.Limits.MaxModelCalls != c.Discovery.Limits.MaxModelCalls || result.SourceSHA != SHA256(source) || result.CanonicalSource != string(source) || result.DomainRevision != "" {
		t.Fatal("program draft identity or policy defaults are incorrect")
	}
	hash := ProgramRevisionHash(spec.ID, 1, *result.Program, "")
	if hash == ProgramRevisionHash(spec.ID, 2, *result.Program, "") || hash == ProgramRevisionHash(spec.ID, 1, *result.Program, strings.Repeat("a", 64)) {
		t.Fatal("program revision omitted number or exact seed bundle")
	}
	spec.Limits.MaxModelCalls = c.Discovery.Limits.MaxModelCalls + 1
	source, _ = json.Marshal(spec)
	if _, err = ParseCanonical("program", "program_json", source, "cli", c); err == nil {
		t.Fatal("program exceeded trusted policy")
	}
	spec.Limits.MaxModelCalls = 0
	spec.Project = "other"
	source, _ = json.Marshal(spec)
	if _, err = ParseCanonical("program", "program_json", source, "cli", c); err == nil {
		t.Fatal("cross-project program accepted")
	}
}

func TestExportRetainsNilVersusEmptySourceSlices(t *testing.T) {
	for _, depends := range [][]string{nil, {}} {
		for _, checks := range [][]string{nil, {}} {
			f, err := NewPlan("plan-slices", "cli", "Plan", "Inspect the goal.")
			if err != nil {
				t.Fatal(err)
			}
			f.DependsOn, f.Checks = depends, checks
			before := Revision(f)
			data, err := Export(f)
			if err != nil {
				t.Fatal(err)
			}
			round, err := Parse(data)
			if err != nil {
				t.Fatal(err)
			}
			if round.Revision != before {
				t.Fatal("export changed nil/empty historical source representation")
			}
		}
	}
}
