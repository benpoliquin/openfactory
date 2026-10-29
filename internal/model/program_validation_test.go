package model

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestStrictDecodeBoundaries(t *testing.T) {
	type envelope struct {
		Nested struct {
			Value json.Number `json:"value"`
		} `json:"nested"`
	}
	for _, input := range []string{`{"nested":{"value":1,"value":2}}`, `{"nested":{"value":1,"unknown":2}}`, `{"nested":{"value":1}} {}`, `{"nested":{"value":1},"nested":{"value":2}}`, `{"nested":{"value":1,"\u0076alue":2}}`} {
		var got envelope
		if err := StrictDecode([]byte(input), &got); err == nil {
			t.Errorf("accepted ambiguous input %s", input)
		}
	}
	var value any
	if err := StrictDecode([]byte(`9007199254740993`), &value); err != nil {
		t.Fatal(err)
	}
	if value != json.Number("9007199254740993") {
		t.Fatalf("lost number precision: %#v", value)
	}
}
func canonicalTest(t *testing.T, kind string, v any) error {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	_, err = CanonicalProgramPayload(kind, data)
	return err
}
func validArchitectureForValidation() ArchitecturePayload {
	return ArchitecturePayload{Decisions: []ArchitectureDecision{{ID: "decision", Title: "Delivery", Alternatives: []ArchitectureAlternative{{ID: "a", Description: "First"}, {ID: "b", Description: "Second"}}, Criteria: []string{"correctness"}, Selected: "a", Rejected: []string{"b"}, Tradeoffs: []string{"latency"}, Consequences: []string{"serialize"}, RequirementRefs: []string{"requirement"}}}}
}
func TestArchitectureStructuralValidation(t *testing.T) {
	if err := canonicalTest(t, ArtifactArchitecture, validArchitectureForValidation()); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*ArchitectureDecision){func(d *ArchitectureDecision) { d.Alternatives = d.Alternatives[:1] }, func(d *ArchitectureDecision) { d.Alternatives[1].ID = "a" }, func(d *ArchitectureDecision) { d.Selected = "absent" }, func(d *ArchitectureDecision) { d.Rejected = []string{"a"} }, func(d *ArchitectureDecision) { d.Rejected = nil }, func(d *ArchitectureDecision) { d.RequirementRefs = []string{"r", "r"} }} {
		p := validArchitectureForValidation()
		mutate(&p.Decisions[0])
		if err := canonicalTest(t, ArtifactArchitecture, p); err == nil {
			t.Error("accepted invalid architecture")
		}
	}
}
func TestPayloadRejectsForgedMetadataAndEnums(t *testing.T) {
	for _, tc := range []struct{ kind, input string }{
		{ArtifactEvaluation, `{"evaluated_manifest_sha":"` + strings.Repeat("a", 64) + `","verdict":"accept","producer_thread_id":"forged"}`},
		{ArtifactEvaluation, `{"evaluated_manifest_sha":"` + strings.Repeat("a", 64) + `","verdict":"approved"}`},
		{ArtifactQuestions, `{"questions":[{"id":"q","text":"Why?","route":"shell"}]}`},
		{ArtifactClaims, `{"claims":[{"id":"c","statement":"True","classification":"fact"}]}`},
		{ArtifactClaims, `{"claims":[{"id":"c","statement":"True","classification":"public_statement","citations":[]}]}`},
		{ArtifactClaims, `{"claims":[{"id":"c","statement":"True","classification":"inference","inference_premises":["c"]}]}`},
		{ArtifactClaims, `{"claims":[{"id":"c","statement":"True","classification":"public_statement","citations":[{"source_id":"s","text_sha":"` + strings.Repeat("a", 64) + `","quote":"text","start_line":2,"end_line":1}]}]}`},
		{ArtifactRevision, `{"trigger":"change","materiality":"material","predecessor_manifest_sha":"bad","changed_decisions":["d"]}`},
		{ArtifactQuestions, `null`},
	} {
		if _, err := CanonicalProgramPayload(tc.kind, json.RawMessage(tc.input)); err == nil {
			t.Errorf("accepted %s", tc.input)
		}
	}
}
func TestImplementationRejectsServerAuthority(t *testing.T) {
	feature := Feature{ID: "feature", Project: "project", Title: "Implement", Kind: "feature", Risk: "normal", CompletionTarget: "integrated", Body: "Description", Checks: []string{"unit"}, Acceptance: []Acceptance{{ID: "a", Scenario: "input", Expected: "output"}}}
	makePayload := func(f Feature) ImplementationContractsPayload {
		return ImplementationContractsPayload{Contracts: []ImplementationContract{{Feature: f, RequirementRefs: []string{"r"}, DecisionRefs: []string{"d"}}}}
	}
	if err := canonicalTest(t, ArtifactImplementationContracts, makePayload(feature)); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*Feature){func(f *Feature) { f.DiscoveryStepID = "step" }, func(f *Feature) { f.ID = "discovery-forged" }, func(f *Feature) { f.ProgramEvidenceGate = &ProgramEvidenceGate{} }, func(f *Feature) { f.State = "approved" }, func(f *Feature) { f.PolicyHash = "forged" }, func(f *Feature) { f.Attempts = 1 }} {
		f := feature
		mutate(&f)
		if err := canonicalTest(t, ArtifactImplementationContracts, makePayload(f)); err == nil {
			t.Error("accepted forged server metadata")
		}
	}
}

func TestImplementationRejectsInternalIntakeAuthorityAndDependencies(t *testing.T) {
	base := Feature{ID: "product", Project: "project", Title: "Implement", Kind: "feature", Risk: "normal", CompletionTarget: "reviewed", Body: "Implement exactly the contract.", Checks: []string{"unit"}, Acceptance: []Acceptance{{ID: "a", Scenario: "input", Expected: "output"}}}
	for name, mutate := range map[string]func(*Feature){
		"association":          func(f *Feature) { f.IntakeStepID = "forged" },
		"reserved identifier":  func(f *Feature) { f.ID = IntakeFeaturePrefix + "forged" },
		"reserved kind":        func(f *Feature) { f.Kind = IntakeFeatureKind },
		"intake dependency":    func(f *Feature) { f.DependsOn = []string{IntakeFeaturePrefix + "reviewed"} },
		"discovery dependency": func(f *Feature) { f.DependsOn = []string{"openfactory-discovery-reviewed"} },
	} {
		t.Run(name, func(t *testing.T) {
			f := base
			mutate(&f)
			payload := ImplementationContractsPayload{Contracts: []ImplementationContract{{Feature: f, RequirementRefs: []string{"requirement"}, DecisionRefs: []string{"decision"}}}}
			if err := canonicalTest(t, ArtifactImplementationContracts, payload); err == nil {
				t.Fatal("untrusted implementation artifact obtained internal server authority")
			}
		})
	}
}
func validValidationExperiment() ExperimentContract {
	return ExperimentContract{ID: "experiment", Hypothesis: "ordered", Falsification: "reordered", Environment: ExperimentEnvironment{Profile: "fixture"}, BundleFiles: []string{"experiments/experiment/main.py"}, BundleManifestSHA: strings.Repeat("a", 64), Commands: []ExperimentCommand{{Profile: "fixture"}}, Observations: []ExperimentObservationSpec{{Name: "count", Type: "integer", Unit: "items", OutputPath: "result.json", JSONPointer: "/count"}}, Acceptance: []ExperimentPredicate{{Observation: "count", Operator: "eq", Value: json.RawMessage(`2`), Unit: "items"}}, Limits: ExperimentLimits{WallSeconds: 10, MaxOutputBytes: 1024, MaxArtifactBytes: 1024, MaxArtifactFiles: 1}}
}

func TestExperimentObservationCannotUseFrozenInput(t *testing.T) {
	e := validValidationExperiment()
	e.BundleFiles = append(e.BundleFiles, "experiments/experiment/result.json")
	e.Observations[0].OutputPath = "experiments/experiment/result.json"
	if err := canonicalTest(t, ArtifactExperimentContracts, ExperimentContractsPayload{Experiments: []ExperimentContract{e}}); err == nil {
		t.Fatal("accepted frozen input JSON as an experiment observation")
	}
	e.Observations[0].OutputPath = "experiments/experiment/produced.json"
	if err := canonicalTest(t, ArtifactExperimentContracts, ExperimentContractsPayload{Experiments: []ExperimentContract{e}}); err != nil {
		t.Fatalf("rejected distinct produced output path: %v", err)
	}
}
func TestExperimentPredicateAndPathBoundaries(t *testing.T) {
	wrap := func(e ExperimentContract) ExperimentContractsPayload {
		return ExperimentContractsPayload{Experiments: []ExperimentContract{e}}
	}
	if err := canonicalTest(t, ArtifactExperimentContracts, wrap(validValidationExperiment())); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*ExperimentContract){func(e *ExperimentContract) { e.Observations[0].OutputPath = "../secret" }, func(e *ExperimentContract) { e.Acceptance[0].Observation = "missing" }, func(e *ExperimentContract) { e.Acceptance[0].Operator = "eval" }, func(e *ExperimentContract) { e.Acceptance[0].Value = json.RawMessage(`"2"`) }, func(e *ExperimentContract) {
		e.Acceptance[0].Operator = "in"
		e.Acceptance[0].Value = json.RawMessage(`[]`)
	}, func(e *ExperimentContract) { e.Limits.WallSeconds = 301 }, func(e *ExperimentContract) { e.Acceptance[0].Unit = "seconds" }} {
		e := validValidationExperiment()
		mutate(&e)
		if err := canonicalTest(t, ArtifactExperimentContracts, wrap(e)); err == nil {
			t.Error("accepted unsafe experiment")
		}
	}
}
func TestPolicyDefaultsAndProgramSpec(t *testing.T) {
	p := NormalizeDiscoveryPolicy(DiscoveryPolicy{})
	if p.Limits.MaxIterations != 2 || p.Limits.MaxModelCalls != 20 || p.MaxCaptureBytes != 8<<20 || p.ExperimentLimits.WallSeconds != 300 {
		t.Fatalf("incorrect defaults: %+v", p)
	}
	custom := NormalizeDiscoveryPolicy(DiscoveryPolicy{MaxCaptureBytes: 1024})
	if custom.MaxCaptureBytes != 1024 {
		t.Fatal("overwrote explicit cap")
	}
	s := ProgramSpec{ID: "p", Project: "project", Title: "title", Objective: "goal", Disclosure: ProgramDisclosure{ModelProviderIDs: []string{"provider"}}}
	if err := ValidateProgramSpec(s); err != nil {
		t.Fatal(err)
	}
	s.Seeds = []ProgramSeed{{ID: "seed", Kind: "bundle", Path: "source.txt"}}
	if err := ValidateProgramSpec(s); err == nil {
		t.Fatal("accepted undisclosed private bundle")
	}
	s.Disclosure.PrivateMaterialToModels = true
	if err := ValidateProgramSpec(s); err != nil {
		t.Fatal(err)
	}
	s.Seeds[0].Path = "../secret"
	if err := ValidateProgramSpec(s); err == nil {
		t.Fatal("accepted escaping bundle path")
	}
	s.Seeds = nil
	s.Limits.MaxSources = -1
	if err := ValidateProgramSpec(s); err == nil {
		t.Fatal("accepted negative budget")
	}
}

func TestDraftProgramShapeDoesNotGrantDisclosureConsent(t *testing.T) {
	spec := ProgramSpec{ID: "manual", Project: "project", Title: "Private proposal", Objective: "Review before sharing", Seeds: []ProgramSeed{{ID: "private", Kind: "bundle", Path: "inputs/private.txt"}}, Disclosure: ProgramDisclosure{PublicSearchTopics: []ProgramSearchTopic{{ID: "planned", Query: "A planned question"}}}}
	if err := ValidateProgramDraftSpec(spec); err != nil {
		t.Fatal("unapproved source shape required execution consent:", err)
	}
	if spec.Disclosure.PrivateMaterialToModels || spec.Disclosure.PublicSearchAllowed || len(spec.Disclosure.ModelProviderIDs) != 0 {
		t.Fatal("shape validation synthesized disclosure authority")
	}
	if err := ValidateProgramSpec(spec); err == nil {
		t.Fatal("draft shape acquired execution authority")
	}
	spec.Seeds[0].Path = "../escape"
	if err := ValidateProgramDraftSpec(spec); err == nil {
		t.Fatal("draft-only validation bypassed structural seed confinement")
	}
}

func TestExperimentDeclaredBundleFiles(t *testing.T) {
	cases := map[string][]string{
		"missing":        nil,
		"duplicate":      {"experiments/experiment/main.py", "experiments/experiment/main.py"},
		"traversal":      {"experiments/experiment/../secret"},
		"outside":        {"experiments/other/main.py"},
		"prefix sibling": {"experiments/experiment-other/main.py"},
		"directory":      {"experiments/experiment"},
		"absolute":       {"/experiments/experiment/main.py"},
	}
	for name, files := range cases {
		t.Run(name, func(t *testing.T) {
			e := validValidationExperiment()
			e.BundleFiles = files
			if err := canonicalTest(t, ArtifactExperimentContracts, ExperimentContractsPayload{Experiments: []ExperimentContract{e}}); err == nil {
				t.Fatal("accepted invalid declared files")
			}
		})
	}
	e := validValidationExperiment()
	e.BundleFiles = make([]string, 129)
	for i := range e.BundleFiles {
		e.BundleFiles[i] = "experiments/experiment/" + strings.Repeat("x", i+1)
	}
	if err := canonicalTest(t, ArtifactExperimentContracts, ExperimentContractsPayload{Experiments: []ExperimentContract{e}}); err == nil {
		t.Fatal("accepted excessive bundle files")
	}
}

func TestProgramAndExperimentSafeIdentifiers(t *testing.T) {
	for _, id := range []string{"nested/id", "/absolute", "with space", strings.Repeat("x", 129), "_leading", "é"} {
		t.Run(id, func(t *testing.T) {
			spec := ProgramSpec{ID: "program", Project: "project", Title: "title", Objective: "goal", Disclosure: ProgramDisclosure{ModelProviderIDs: []string{"provider"}}}
			spec.ID = id
			if err := ValidateProgramSpec(spec); err == nil {
				t.Fatal("accepted unsafe program identifier")
			}
			spec.ID = "program"
			spec.Project = id
			if err := ValidateProgramSpec(spec); err == nil {
				t.Fatal("accepted unsafe project identifier")
			}
			experiment := validValidationExperiment()
			experiment.ID = id
			experiment.BundleFiles = []string{"experiments/" + id + "/main.py"}
			if err := canonicalTest(t, ArtifactExperimentContracts, ExperimentContractsPayload{Experiments: []ExperimentContract{experiment}}); err == nil {
				t.Fatal("accepted unsafe experiment identifier")
			}
		})
	}
	for _, id := range []string{"a", "A0._-z", strings.Repeat("x", 128), "openfactory-discovery-program"} {
		spec := ProgramSpec{ID: id, Project: id, Title: "title", Objective: "goal", Disclosure: ProgramDisclosure{ModelProviderIDs: []string{"provider"}}}
		if err := ValidateProgramSpec(spec); err != nil {
			t.Fatalf("rejected safe identifier %q: %v", id, err)
		}
		experiment := validValidationExperiment()
		experiment.ID = id
		experiment.BundleFiles = []string{"experiments/" + id + "/main.py"}
		if err := canonicalTest(t, ArtifactExperimentContracts, ExperimentContractsPayload{Experiments: []ExperimentContract{experiment}}); err != nil {
			t.Fatalf("rejected safe experiment identifier %q: %v", id, err)
		}
	}
}

func implementationValidationFixture(id string) ImplementationContract {
	return ImplementationContract{
		Feature:         Feature{ID: id, Project: "project", Title: "Implement", Kind: "feature", Risk: "normal", CompletionTarget: "integrated", Body: "Description", Checks: []string{"unit"}, Acceptance: []Acceptance{{ID: "acceptance", Scenario: "input", Expected: "output"}}},
		RequirementRefs: []string{"requirement"}, DecisionRefs: []string{"decision"},
	}
}

func TestImplementationConsumerIdentifiers(t *testing.T) {
	for name, mutate := range map[string]func(*Feature){
		"feature slash":        func(f *Feature) { f.ID = "nested/feature" },
		"project space":        func(f *Feature) { f.Project = "bad project" },
		"acceptance slash":     func(f *Feature) { f.Acceptance[0].ID = "nested/acceptance" },
		"dependency slash":     func(f *Feature) { f.DependsOn = []string{"nested/dependency"} },
		"check slash":          func(f *Feature) { f.Checks = []string{"nested/check"} },
		"self dependency":      func(f *Feature) { f.DependsOn = []string{f.ID} },
		"duplicate dependency": func(f *Feature) { f.DependsOn = []string{"external", "external"} },
		"overlength feature":   func(f *Feature) { f.ID = strings.Repeat("x", 129) },
	} {
		t.Run(name, func(t *testing.T) {
			contract := implementationValidationFixture("feature")
			mutate(&contract.Feature)
			if err := canonicalTest(t, ArtifactImplementationContracts, ImplementationContractsPayload{Contracts: []ImplementationContract{contract}}); err == nil {
				t.Fatal("accepted contract rejected by feature consumer")
			}
		})
	}
}

func TestImplementationBatchDependencyCycles(t *testing.T) {
	first, second, third := implementationValidationFixture("first"), implementationValidationFixture("second"), implementationValidationFixture("third")
	first.Feature.DependsOn = []string{"second"}
	second.Feature.DependsOn = []string{"third"}
	third.Feature.DependsOn = []string{"first"}
	payload := ImplementationContractsPayload{Contracts: []ImplementationContract{first, second, third}}
	if err := canonicalTest(t, ArtifactImplementationContracts, payload); err == nil {
		t.Fatal("accepted same-batch dependency cycle")
	}
	payload.Contracts[2].Feature.DependsOn = []string{"external"}
	if err := canonicalTest(t, ArtifactImplementationContracts, payload); err != nil {
		t.Fatalf("rejected acyclic batch with store-resolved external dependency: %v", err)
	}
}
