package dashboard

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"openfactory/internal/model"
	"openfactory/internal/store"
)

type programReaderFake struct {
	query       store.ProgramQuery
	page        store.ProgramPage
	status      store.ProgramStatus
	artifacts   store.ProgramArtifactPage
	artifact    store.ProgramArtifactDetail
	experiments store.ProgramExperimentPage
	err         error
	calls       int
}

func (f *programReaderFake) ProgramPage(_ context.Context, q store.ProgramQuery) (store.ProgramPage, error) {
	f.query = q
	f.calls++
	return f.page, f.err
}
func (f *programReaderFake) ProgramStatus(context.Context, string) (store.ProgramStatus, error) {
	f.calls++
	return f.status, f.err
}
func (f *programReaderFake) ProgramArtifactPage(_ context.Context, _ string, q store.ProgramQuery) (store.ProgramArtifactPage, error) {
	f.query = q
	f.calls++
	return f.artifacts, f.err
}
func (f *programReaderFake) ProgramArtifactDetail(context.Context, string, string) (store.ProgramArtifactDetail, error) {
	f.calls++
	return f.artifact, f.err
}
func (f *programReaderFake) ProgramQuestionPage(_ context.Context, _ string, q store.ProgramQuery) (store.ProgramQuestionPage, error) {
	f.query = q
	f.calls++
	return store.ProgramQuestionPage{}, f.err
}
func (f *programReaderFake) ProgramExperimentPage(_ context.Context, _ string, q store.ProgramQuery) (store.ProgramExperimentPage, error) {
	f.query = q
	f.calls++
	return f.experiments, f.err
}
func (f *programReaderFake) ProgramSourcePage(_ context.Context, _ string, q store.ProgramQuery) (store.ProgramSourcePage, error) {
	f.query = q
	f.calls++
	return store.ProgramSourcePage{}, f.err
}
func (f *programReaderFake) ProgramFeaturePage(_ context.Context, _ string, q store.ProgramQuery, _ model.Config) (store.ProgramFeaturePage, error) {
	f.query = q
	f.calls++
	return store.ProgramFeaturePage{}, f.err
}
func (f *programReaderFake) ProgramEventPage(_ context.Context, _ string, q store.ProgramQuery) (store.ProgramEventPage, error) {
	f.query = q
	f.calls++
	return store.ProgramEventPage{}, f.err
}

func TestProgramListValuesAndNavigation(t *testing.T) {
	f := &fakeReader{}
	f.page = store.ProgramPage{Items: []model.Program{{ID: "p", Title: "Hostile <script>alert(1)</script>", State: "waiting", Phase: "experiments", CurrentRevision: 3}}, NextCursor: "opaque+/="}
	h := New(model.Config{}, f)
	w := request(h, "GET", "/api/programs?project=demo&state=waiting&limit=7&after=old", "localhost")
	var got store.ProgramPage
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &got) != nil || len(got.Items) != 1 || got.Items[0].CurrentRevision != 3 || got.NextCursor != f.page.NextCursor {
		t.Fatalf("list response: %d %s", w.Code, w.Body.String())
	}
	if f.programReaderFake.query.Project != "demo" || f.programReaderFake.query.Limit != 7 || f.programReaderFake.query.After != "old" {
		t.Fatalf("query: %+v", f.programReaderFake.query)
	}
	w = request(h, "GET", "/programs?project=demo&limit=7", "localhost")
	for _, s := range []string{"/programs/p", "&lt;script&gt;", "after=opaque%2B%2F%3D", "project=demo"} {
		if !strings.Contains(w.Body.String(), s) {
			t.Fatalf("missing %q: %s", s, w.Body.String())
		}
	}
	if strings.Contains(w.Body.String(), "<script>") {
		t.Fatal("executed title")
	}
}
func TestProgramRoutesAndBoundaries(t *testing.T) {
	f := &fakeReader{}
	h := New(model.Config{}, f)
	for _, path := range []string{"/api/programs?limit=501", "/api/programs?revision=0", "/api/programs?revision=no", "/api/programs?limit=", "/api/programs?state=a&state=b", "/api/programs?after=" + strings.Repeat("a", 2049), "/api/programs/p?after=x"} {
		if strings.HasSuffix(path, "limit=") {
			continue
		}
		if w := request(h, "GET", path, "localhost"); w.Code != 400 {
			t.Fatalf("%s: %d", path, w.Code)
		}
	}
	for _, path := range []string{"/api/programs/p/unknown", "/programs/", "/api/programs/p/artifacts/", "/api/programs/p/artifacts/a/extra"} {
		if w := request(h, "GET", path, "localhost"); w.Code != 404 {
			t.Fatalf("%s: %d", path, w.Code)
		}
	}
	for _, path := range []string{"/api/programs", "/api/programs/p", "/api/programs/p/experiments", "/programs/p"} {
		if w := request(h, "POST", path, "localhost"); w.Code != 405 {
			t.Fatalf("%s: %d", path, w.Code)
		}
	}
	for _, name := range []string{"questions", "artifacts", "evidence", "experiments", "features", "events"} {
		for _, prefix := range []string{"/api/programs/p/", "/programs/p/"} {
			w := request(h, "GET", prefix+name+"?revision=2&limit=1", "localhost")
			if w.Code != 200 || f.programReaderFake.query.Revision != 2 || f.programReaderFake.query.Limit != 1 {
				t.Fatalf("%s: %d %s", name, w.Code, w.Body.String())
			}
		}
	}
	for _, tc := range []struct {
		err  error
		code int
	}{{store.ErrNotFound, 404}, {store.ErrInvalidQuery, 400}} {
		f.programReaderFake.err = tc.err
		if w := request(h, "GET", "/api/programs/p", "localhost"); w.Code != tc.code {
			t.Fatalf("error: %d", w.Code)
		}
	}
}
func TestProgramArtifactEscapingAndLazyDetail(t *testing.T) {
	f := &fakeReader{}
	f.status.Program = model.Program{ID: "p", CurrentRevision: 2, Iteration: 1, Phase: "experiments"}
	f.artifact = store.ProgramArtifactDetail{Artifact: model.ProgramArtifact{ID: "a", Kind: "evaluation", SupersedesID: "old"}, Payload: json.RawMessage(`{"verdict":"revise","findings":["<script>evil</script>"],"required_changes":["failed hypothesis"]}`)}
	h := New(model.Config{}, f)
	w := request(h, "GET", "/programs/p", "localhost")
	if w.Code != 200 || f.programReaderFake.calls != 1 || !strings.Contains(w.Body.String(), "Recorded exact grant history (not current authority)") || !strings.Contains(w.Body.String(), "grant presence alone is not current approval") || !strings.Contains(w.Body.String(), "/programs/p/artifacts") {
		t.Fatalf("detail: %d %s", w.Code, w.Body.String())
	}
	w = request(h, "GET", "/programs/p/artifacts/a", "localhost")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "failed hypothesis") || !strings.Contains(w.Body.String(), "old") || strings.Contains(w.Body.String(), "<script>") {
		t.Fatalf("artifact: %d %s", w.Code, w.Body.String())
	}
	w = request(h, "GET", "/api/programs/p/artifacts/a", "localhost")
	var got store.ProgramArtifactDetail
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &got) != nil || got.Artifact.SupersedesID != "old" || !strings.Contains(string(got.Payload), "revise") {
		t.Fatalf("canonical artifact: %s", w.Body.String())
	}
}

func TestProgramArtifactStageHistoryValues(t *testing.T) {
	f := &fakeReader{}
	// Store order is newest first, not artifact-ID or kind order. The metadata
	// deliberately has a different order so joining by array index is wrong.
	f.artifacts = store.ProgramArtifactPage{
		Items: []model.ProgramArtifact{
			{ID: "z-revised", Kind: "evaluation", SupersedesID: "a-accepted"},
			{ID: "m-result", Kind: "evaluation"},
			{ID: "b-contract", Kind: "evaluation"},
			{ID: "a-accepted", Kind: "evaluation"},
		},
		OutputStates: []store.ProgramArtifactOutputState{
			{ArtifactID: "a-accepted", ProducerStepKind: "plan_evaluation", ProducerSubjectID: "plan", SupersededBy: []string{"z-revised"}, HistoricalRevision: true},
			{ArtifactID: "b-contract", ProducerStepKind: "contract_evaluation", ProducerSubjectID: "contract-1", CurrentStageOutput: true},
			{ArtifactID: "m-result", ProducerStepKind: "result_evaluation", ProducerSubjectID: "result-1", CurrentStageOutput: true},
			{ArtifactID: "z-revised", ProducerStepKind: "plan_evaluation", ProducerSubjectID: "plan", CurrentStageOutput: true},
		},
		NextCursor: "older-history",
	}
	h := New(model.Config{}, f)
	w := request(h, "GET", "/programs/p/artifacts?revision=2&kind=evaluation", "localhost")
	body := w.Body.String()
	if w.Code != 200 {
		t.Fatalf("artifact history: %d %s", w.Code, body)
	}
	previous := -1
	for _, id := range []string{"z-revised", "m-result", "b-contract", "a-accepted"} {
		position := strings.Index(body, `href="/programs/p/artifacts/`+id+`"`)
		if position <= previous {
			t.Fatalf("store history order or artifact link lost for %s: %s", id, body)
		}
		previous = position
	}
	for _, stage := range []string{"plan_evaluation", "result_evaluation", "contract_evaluation"} {
		if !strings.Contains(body, "Current stage output (not approval authority) · Producer stage: "+stage) {
			t.Fatalf("independent current stage %s missing: %s", stage, body)
		}
	}
	for _, value := range []string{"Historical / non-current output · Historical revision · Producer stage: plan_evaluation", "superseded_by", "older-history", "revision=2", "kind=evaluation", "contract-1", "result-1"} {
		if !strings.Contains(body, value) {
			t.Fatalf("missing history value %q: %s", value, body)
		}
	}
	for _, tc := range []struct {
		index              int
		id, verdict, label string
	}{
		{0, "a-accepted", "accept", "Historical / non-current output · Historical revision"},
		{3, "z-revised", "revise", "Current stage output (not approval authority)"},
	} {
		f.artifact = store.ProgramArtifactDetail{
			Artifact:    model.ProgramArtifact{ID: tc.id, Kind: "evaluation"},
			OutputState: f.artifacts.OutputStates[tc.index],
			Payload:     json.RawMessage(`{"verdict":"` + tc.verdict + `"}`),
		}
		w = request(h, "GET", "/programs/p/artifacts/"+tc.id, "localhost")
		for _, value := range []string{tc.id, tc.verdict, tc.label, "Producer stage: plan_evaluation", "superseded_by"} {
			if w.Code != 200 || !strings.Contains(w.Body.String(), value) {
				t.Fatalf("detail missing %q: %d %s", value, w.Code, w.Body.String())
			}
		}
		if tc.id == "a-accepted" && (!strings.Contains(w.Body.String(), "z-revised") || strings.Contains(w.Body.String(), "Current stage output (not approval authority)")) {
			t.Fatalf("old accept presented as current or superseder lost: %s", w.Body.String())
		}
	}
}

func TestProgramFailedExperimentRemainsVisible(t *testing.T) {
	f := &fakeReader{}
	f.experiments = store.ProgramExperimentPage{Items: []store.ProgramExperimentView{{Contract: model.ExperimentContract{ID: "duplicate-order", Hypothesis: "arrival order is safe", Falsification: "duplicate reordered output"}, ContractSHA: "frozen-contract", Receipts: []model.ExperimentReceipt{{RunID: "run-1", State: "completed", Accepted: false, Conclusive: true, Detail: "duplicate reordered output"}}}}, NextCursor: "next-experiment"}
	h := New(model.Config{}, f)
	w := request(h, "GET", "/api/programs/p/experiments?revision=2", "localhost")
	var got store.ProgramExperimentPage
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &got) != nil || len(got.Items) != 1 || len(got.Items[0].Receipts) != 1 || got.Items[0].Receipts[0].Accepted || !got.Items[0].Receipts[0].Conclusive {
		t.Fatalf("receipt lost: %s", w.Body.String())
	}
	w = request(h, "GET", "/programs/p/experiments?revision=2", "localhost")
	for _, s := range []string{"duplicate reordered output", "frozen-contract", "next-experiment", "revision=2", "not falsification"} {
		if !strings.Contains(w.Body.String(), s) {
			t.Fatalf("missing %q: %s", s, w.Body.String())
		}
	}
}

func TestProgramOversizedResponsesFailBeforeCommitting(t *testing.T) {
	f := &fakeReader{}
	// A canonical artifact below 8 MiB can exceed the presentation ceiling once
	// JSON escapes hostile characters. Neither API nor HTML may stream a prefix.
	payload := json.RawMessage(`{"finding":"private-source-marker` + strings.Repeat("<", 3<<20) + `"}`)
	f.artifact = store.ProgramArtifactDetail{Artifact: model.ProgramArtifact{ID: "a"}, Payload: payload}
	h := New(model.Config{}, f)
	for _, path := range []string{"/api/programs/p/artifacts/a", "/programs/p/artifacts/a"} {
		w := request(h, "GET", path, "localhost")
		if w.Code != 503 || strings.Contains(w.Body.String(), "private-source-marker") || strings.Contains(w.Body.String(), "<!doctype") || w.Body.Len() > 256 {
			t.Fatalf("oversized response was partially exposed: status=%d bytes=%d", w.Code, w.Body.Len())
		}
	}
}

func TestProgramBufferBoundaries(t *testing.T) {
	var b programBuffer
	if n, e := b.WriteString(strings.Repeat("x", programResponseLimit)); e != nil || n != programResponseLimit {
		t.Fatalf("exact limit: %d %v", n, e)
	}
	if n, e := b.Write([]byte("y")); e == nil || n != 0 || b.Len() != programResponseLimit {
		t.Fatal("byte write exceeded bound")
	}
	if n, e := b.WriteString("z"); e == nil || n != 0 || b.Len() != programResponseLimit {
		t.Fatal("string write exceeded bound")
	}
}
