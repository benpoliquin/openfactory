package intake

import (
	"strings"
	"testing"

	"openfactory/internal/model"
)

const document = `---
id: csv-parser
project: cli
title: Preserve quoted fields
kind: feature
risk: normal
completion_target: integrated
checks: [unit]
acceptance:
  - id: quoted
    scenario: Parse a quoted comma.
    expected: Return one field.
---
Implement the existing CSV contract.
`

func TestParseImmutableRevision(t *testing.T) {
	f, err := Parse([]byte(document))
	if err != nil {
		t.Fatal(err)
	}
	g, err := Parse([]byte(strings.ReplaceAll(document, "\n", "\r\n")))
	if err != nil {
		t.Fatal(err)
	}
	if f.Revision == "" || f.Revision != g.Revision {
		t.Fatal("normalized source revision unstable")
	}
	g.State = "accepted"
	g.PolicyHash = "other"
	if Revision(g) != f.Revision {
		t.Fatal("mutable ledger state entered source revision")
	}
	g.Body += " Different behavior."
	if Revision(g) == f.Revision {
		t.Fatal("changed contract retained revision")
	}
}
func TestParseRejectsUnreviewableSource(t *testing.T) {
	cases := map[string]string{
		"unknown key":            strings.Replace(document, "risk: normal", "risk: normal\ncommand: rm -rf /", 1),
		"duplicate key":          strings.Replace(document, "risk: normal", "risk: normal\nrisk: high", 1),
		"state injection":        strings.Replace(document, "risk: normal", "risk: normal\nstate: accepted", 1),
		"unsafe id":              strings.Replace(document, "id: csv-parser", "id: ../escape", 1),
		"no checks":              strings.Replace(document, "checks: [unit]", "checks: []", 1),
		"bad target":             strings.Replace(document, "completion_target: integrated", "completion_target: deploy-anywhere", 1),
		"duplicate acceptance":   strings.Replace(document, "---\nImplement", "  - id: quoted\n    scenario: Other\n    expected: Other\n---\nImplement", 1),
		"self dependency":        strings.Replace(document, "checks: [unit]", "checks: [unit]\ndepends_on: [csv-parser]", 1),
		"discovery marker":       strings.Replace(document, "risk: normal", "risk: normal\ndiscovery_step_id: forged-step", 1),
		"empty discovery marker": strings.Replace(document, "risk: normal", "risk: normal\ndiscovery_step_id: ''", 1),
		"reserved discovery id":  strings.Replace(document, "id: csv-parser", "id: openfactory-discovery-forged", 1),
		"discovery dependency":   strings.Replace(document, "checks: [unit]", "checks: [unit]\ndepends_on: [openfactory-discovery-reviewed]", 1),
	}
	for name, source := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(source)); err == nil {
				t.Fatal("unsafe source accepted")
			}
		})
	}
}
func TestPlanningIsReadOnlyOnly(t *testing.T) {
	source := strings.Replace(document, "kind: feature", "kind: plan", 1)
	source = strings.Replace(source, "checks: [unit]", "checks: []", 1)
	if _, err := Parse([]byte(source)); err == nil {
		t.Fatal("planner allowed integration target")
	}
	source = strings.Replace(source, "completion_target: integrated", "completion_target: reviewed", 1)
	if _, err := Parse([]byte(source)); err != nil {
		t.Fatal(err)
	}
}

func TestDiscoveryChecksRequireAssociation(t *testing.T) {
	f, err := Parse([]byte(document))
	if err != nil {
		t.Fatal(err)
	}
	f.Checks = nil
	if err := Validate(f); err == nil {
		t.Fatal("ordinary feature accepted without checks")
	}
	f.DiscoveryStepID = strings.Repeat("a", 64)
	if err := Validate(f); err != nil {
		t.Fatalf("server-associated discovery rejected: %v", err)
	}
	f.Acceptance = nil
	if err := Validate(f); err == nil {
		t.Fatal("discovery association bypassed acceptance validation")
	}
}

func TestDiscoveryEvidenceCannotBeCodeDependency(t *testing.T) {
	f, err := Parse([]byte(document))
	if err != nil {
		t.Fatal(err)
	}
	f.DependsOn = []string{"ordinary-reviewed", "openfactory-discovery-"}
	if err := Validate(f); err == nil {
		t.Fatal("reserved discovery prefix accepted as code dependency")
	}
	f.DependsOn = []string{"openfactory-discovery-reviewed"}
	if err := Validate(f); err == nil {
		t.Fatal("reviewed discovery feature accepted as code dependency")
	}
	f.DiscoveryStepID = strings.Repeat("a", 64)
	if err := Validate(f); err == nil {
		t.Fatal("server discovery association bypassed code dependency validation")
	}
	f.DependsOn = []string{"ordinary-reviewed", "openfactory-discovery"}
	if err := Validate(f); err != nil {
		t.Fatalf("ordinary dependencies rejected: %v", err)
	}
}

func TestRevisionBindsDiscoveryAssociationAndEvidenceGate(t *testing.T) {
	f, err := Parse([]byte(document))
	if err != nil {
		t.Fatal(err)
	}
	ordinary := Revision(f)
	f.DiscoveryStepID = strings.Repeat("a", 64)
	associated := Revision(f)
	if associated == ordinary {
		t.Fatal("discovery association missing from revision")
	}
	f.DiscoveryStepID = strings.Repeat("b", 64)
	if Revision(f) == associated {
		t.Fatal("changed discovery association retained revision")
	}
	f.ProgramEvidenceGate = &model.ProgramEvidenceGate{
		ProgramID: "discovery", Revision: 1, ManifestSHA: strings.Repeat("a", 64), EvaluationArtifactID: "evaluation",
	}
	gated := Revision(f)
	withoutGate := f
	withoutGate.ProgramEvidenceGate = nil
	if gated == Revision(withoutGate) {
		t.Fatal("evidence gate missing from revision")
	}
	for name, change := range map[string]func(*model.ProgramEvidenceGate){
		"program":    func(g *model.ProgramEvidenceGate) { g.ProgramID = "other" },
		"revision":   func(g *model.ProgramEvidenceGate) { g.Revision++ },
		"manifest":   func(g *model.ProgramEvidenceGate) { g.ManifestSHA = strings.Repeat("b", 64) },
		"evaluation": func(g *model.ProgramEvidenceGate) { g.EvaluationArtifactID = "other" },
	} {
		t.Run(name, func(t *testing.T) {
			g := f
			gate := *f.ProgramEvidenceGate
			g.ProgramEvidenceGate = &gate
			change(&gate)
			if Revision(g) == gated {
				t.Fatal("changed evidence gate retained revision")
			}
		})
	}
}

func TestEvidenceGateValidation(t *testing.T) {
	gate := "\nprogram_evidence_gate:\n  program_id: discovery\n  revision: 1\n  manifest_sha: " + strings.Repeat("a", 64) + "\n  evaluation_artifact_id: evaluation"
	source := strings.Replace(document, "risk: normal", "risk: normal"+gate, 1)
	if _, err := Parse([]byte(source)); err != nil {
		t.Fatalf("valid gate rejected: %v", err)
	}
	for name, change := range map[string][2]string{
		"missing program":    {"program_id: discovery", "program_id: ''"},
		"unsafe program":     {"program_id: discovery", "program_id: ../discovery"},
		"missing revision":   {"revision: 1", "revision: 0"},
		"negative revision":  {"revision: 1", "revision: -1"},
		"missing manifest":   {strings.Repeat("a", 64), "''"},
		"short manifest":     {strings.Repeat("a", 64), "abcd"},
		"nonhex manifest":    {strings.Repeat("a", 64), strings.Repeat("z", 64)},
		"uppercase manifest": {strings.Repeat("a", 64), strings.Repeat("A", 64)},
		"missing evaluation": {"evaluation_artifact_id: evaluation", "evaluation_artifact_id: ''"},
		"unsafe evaluation":  {"evaluation_artifact_id: evaluation", "evaluation_artifact_id: ../evaluation"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(strings.Replace(source, change[0], change[1], 1))); err == nil {
				t.Fatal("invalid evidence gate accepted")
			}
		})
	}
}
