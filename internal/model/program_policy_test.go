package model

import "testing"

func TestExperimentPolicySHAIsCanonicalAndScoped(t *testing.T) {
	limits := NormalizeDiscoveryPolicy(DiscoveryPolicy{}).ExperimentLimits
	if ExperimentPolicySHA(nil, ExperimentLimits{}) != ExperimentPolicySHA(map[string]ExperimentProfile{}, limits) {
		t.Fatal("omitted profiles and limits do not normalize canonically")
	}
	a := ExperimentProfile{Argv: []string{"/usr/bin/python3"}, AllowedArgs: []string{"main.py"}, ImageIdentity: "sha256:approved", Hardware: []string{"cpu"}}
	b := ExperimentProfile{Argv: []string{"/usr/bin/true"}}
	first := map[string]ExperimentProfile{"a": a, "b": b}
	second := map[string]ExperimentProfile{"b": b, "a": a}
	baseline := ExperimentPolicySHA(first, limits)
	if baseline != ExperimentPolicySHA(second, limits) || len(baseline) != 64 {
		t.Fatal("profile map order changes canonical digest")
	}
	for _, change := range []func(*ExperimentProfile){
		func(p *ExperimentProfile) { p.Argv = []string{"/bin/sh"} },
		func(p *ExperimentProfile) { p.AllowedArgs = []string{"other.py"} },
		func(p *ExperimentProfile) { p.ImageIdentity = "sha256:other" },
		func(p *ExperimentProfile) { p.Network = true },
		func(p *ExperimentProfile) { p.Hardware = []string{"gpu"} },
	} {
		changed := a
		change(&changed)
		if ExperimentPolicySHA(map[string]ExperimentProfile{"a": changed, "b": b}, limits) == baseline {
			t.Fatal("executor authority change did not change policy digest")
		}
	}
	changedLimits := limits
	changedLimits.WallSeconds--
	if ExperimentPolicySHA(first, changedLimits) == baseline {
		t.Fatal("execution ceiling change did not change policy digest")
	}
	if first["a"].Argv[0] != "/usr/bin/python3" {
		t.Fatal("digest mutated caller policy")
	}
}
