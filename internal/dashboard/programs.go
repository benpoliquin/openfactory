package dashboard

import (
	"bytes"
	"encoding/json"
	"errors"
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"openfactory/internal/store"
)

const programResponseLimit = 16 << 20

// Render privately before committing headers or bytes so a size failure cannot
// expose a partial artifact or partially rendered page.
type programBuffer struct{ bytes.Buffer }

func (b *programBuffer) Write(p []byte) (int, error) {
	if len(p) > programResponseLimit-b.Len() {
		return 0, errors.New("program response exceeds bound")
	}
	return b.Buffer.Write(p)
}
func (b *programBuffer) WriteString(s string) (int, error) {
	if len(s) > programResponseLimit-b.Len() {
		return 0, errors.New("program response exceeds bound")
	}
	return b.Buffer.WriteString(s)
}
func writeProgramJSON(w http.ResponseWriter, v any) {
	var b programBuffer
	if err := json.NewEncoder(&b).Encode(v); err != nil {
		http.Error(w, "response unavailable", 503)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write(b.Bytes())
}

func programQuery(v url.Values) (store.ProgramQuery, error) {
	q := store.ProgramQuery{}
	if err := validateKeys(v, "project", "state", "after", "limit", "revision", "kind"); err != nil {
		return q, err
	}
	p, err := pageQuery(v, "after")
	if err != nil {
		return q, err
	}
	q.Project, q.State, q.Kind, q.After, q.Limit = v.Get("project"), v.Get("state"), v.Get("kind"), p.After, p.Limit
	if s := v.Get("revision"); s != "" {
		n, e := strconv.Atoi(s)
		if e != nil || n < 1 {
			return q, store.ErrInvalidQuery
		}
		q.Revision = n
	}
	return q, nil
}

type programSection struct {
	Title string
	Value any
}
type programLink struct{ Label, URL string }

// Output state is store-provided stage provenance, never inferred from kind or
// page position. In particular, evaluation stages have independent outputs.
func programArtifactOutputLabel(state store.ProgramArtifactOutputState) string {
	label := "Historical / non-current output"
	if state.CurrentStageOutput {
		label = "Current stage output (not approval authority)"
	}
	if state.HistoricalRevision {
		label += " · Historical revision"
	}
	return label + " · Producer stage: " + state.ProducerStepKind + " · Producer subject: " + state.ProducerSubjectID
}

type programView struct {
	Title, Next, JSON, Base string
	Sections                []programSection
	Links                   []programLink
	Project, State          string
	List                    bool
}

// programRoute serves GET /[api/]programs, /[api/]programs/{id},
// /[api/]programs/{id}/{questions,artifacts,evidence,experiments,features,events}
// and /[api/]programs/{id}/artifacts/{artifactID}. Histories are fetched only
// when selected, and opaque next cursors preserve all revision/filter arguments.
func (h *handler) programRoute(w http.ResponseWriter, r *http.Request, path string, api bool) {
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if len(parts) > 4 || (len(parts) > 1 && parts[1] == "") {
		http.NotFound(w, r)
		return
	}
	if len(parts) == 4 && (parts[2] != "artifacts" || parts[3] == "") {
		http.NotFound(w, r)
		return
	}
	if len(parts) == 3 {
		switch parts[2] {
		case "questions", "artifacts", "evidence", "experiments", "features", "events":
		default:
			http.NotFound(w, r)
			return
		}
	}
	q, err := programQuery(r.URL.Query())
	if err != nil {
		http.Error(w, "invalid query", 400)
		return
	}
	if len(parts) == 2 || len(parts) == 4 {
		if len(r.URL.Query()) != 0 {
			http.Error(w, "invalid query", 400)
			return
		}
	}
	v := programView{Title: "Discovery programs", JSON: "/api" + path + querySuffix(r.URL), Project: q.Project, State: q.State, List: len(parts) == 1}
	var result any
	var cursor string
	if len(parts) == 1 {
		p, e := h.reader.ProgramPage(r.Context(), q)
		result, err, cursor = p, e, p.NextCursor
		for _, item := range p.Items {
			v.Links = append(v.Links, programLink{item.ID + " · " + item.Title + " · " + item.State + " · " + item.Phase, "/programs/" + url.PathEscape(item.ID)})
		}
		v.Sections = append(v.Sections, programSection{"Programs", p.Items})
	} else {
		id := parts[1]
		v.Base = "/programs/" + url.PathEscape(id)
		v.Title = "Program " + id
		v.Links = append(v.Links, programLink{"Revision, limits, waits, approvals and invalidations", v.Base})
		for _, name := range []string{"questions", "artifacts", "experiments", "evidence", "features", "events"} {
			v.Links = append(v.Links, programLink{name, v.Base + "/" + name})
		}
		if len(parts) == 2 {
			p, e := h.reader.ProgramStatus(r.Context(), id)
			result, err = p, e
			v.Sections = []programSection{{"Program phase, iteration and waits", p.Program}, {"Revision, objective and lifetime limits", p.Revision}, {"Lifetime budget", p.Budget}, {"Recorded exact grant history (not current authority)", p.Grants}, {"Invalidations and stale gates", p.Invalidations}, {"Discovery steps", p.Steps}}
			v.Sections = append(v.Sections, programSection{"Status sublists truncated (use bounded drilldown pages)", p.Truncated})
			v.Sections = append(v.Sections, programSection{"Approval authority warning", "Superseded revisions or policies, invalidations and expired evidence can invalidate recorded approvals. Check current Program wait/phase and invalidations; grant presence alone is not current approval."})
		} else if len(parts) == 4 {
			p, e := h.reader.ProgramArtifactDetail(r.Context(), id, parts[3])
			result, err = p, e
			v.Sections = []programSection{{programArtifactOutputLabel(p.OutputState), p.OutputState}, {"Immutable artifact and supersession", p.Artifact}, {"Bounded/redacted presentation; content_sha identifies original immutable payload, not this displayed JSON. Quote verification proves occurrence in captured original text only, not truth or entailment.", p.Payload}}
		} else {
			switch parts[2] {
			case "artifacts":
				p, e := h.reader.ProgramArtifactPage(r.Context(), id, q)
				result, err, cursor = p, e, p.NextCursor
				outputStates := make(map[string]store.ProgramArtifactOutputState, len(p.OutputStates))
				for _, state := range p.OutputStates {
					outputStates[state.ArtifactID] = state
				}
				for _, a := range p.Items {
					state := outputStates[a.ID]
					v.Links = append(v.Links, programLink{a.Kind + " · " + a.ID + " · " + programArtifactOutputLabel(state), v.Base + "/artifacts/" + url.PathEscape(a.ID)})
					v.Sections = append(v.Sections, programSection{"Output state for " + a.ID + " · " + programArtifactOutputLabel(state), state})
				}
				v.Sections = append(v.Sections, programSection{"Artifacts: requirements, architecture alternatives, evaluations and supersession", p.Items})
			case "questions":
				p, e := h.reader.ProgramQuestionPage(r.Context(), id, q)
				result, err, cursor = p, e, p.NextCursor
				v.Sections = append(v.Sections, programSection{"Version-bound questions and operator answers", p.Items})
			case "experiments":
				p, e := h.reader.ProgramExperimentPage(r.Context(), id, q)
				result, err, cursor = p, e, p.NextCursor
				v.Sections = append(v.Sections, programSection{"Frozen experiment contracts, recorded grant history (not current authority), prerequisites and actual receipts", p.Items}, programSection{"Approval authority warning", "Superseded revisions or policies, invalidations and expired evidence can invalidate recorded approvals. Check current Program wait/phase and invalidations."})
			case "evidence":
				p, e := h.reader.ProgramSourcePage(r.Context(), id, q)
				result, err, cursor = p, e, p.NextCursor
				v.Sections = append(v.Sections, programSection{"Source provenance: URL, retrieval time and content hashes; valid reflects freshness TTL and invalidations", p.Items})
			case "features":
				p, e := h.reader.ProgramFeaturePage(r.Context(), id, q, h.cfg)
				result, err, cursor = p, e, p.NextCursor
				v.Sections = append(v.Sections, programSection{"Linked implementation proposals (not automatically approved)", p.Items})
				for _, f := range p.Items {
					v.Links = append(v.Links, programLink{f.Feature.Feature.ID, "/features/" + url.PathEscape(f.Feature.Feature.ID)})
				}
			case "events":
				p, e := h.reader.ProgramEventPage(r.Context(), id, q)
				result, err, cursor = p, e, p.NextCursor
				v.Sections = append(v.Sections, programSection{"Program event history", p.Items})
			}
		}
	}
	if err != nil {
		h.fail(w, err)
		return
	}
	if api {
		writeProgramJSON(w, result)
		return
	}
	v.Next = nextURL(r.URL, "after", cursor)
	var b programBuffer
	if err := programTemplates.Execute(&b, v); err != nil {
		http.Error(w, "view temporarily unavailable", 503)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(b.Bytes())
}
func querySuffix(u *url.URL) string {
	if u.RawQuery == "" {
		return ""
	}
	return "?" + u.RawQuery
}
func programPretty(v any) string {
	b, e := json.MarshalIndent(v, "", "  ")
	if e != nil {
		return "Unavailable"
	}
	return string(b)
}

var programTemplates = template.Must(template.New("programs").Funcs(template.FuncMap{"pretty": programPretty}).Parse(`<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>{{.Title}}</title><link rel="stylesheet" href="/assets/dashboard.css"></head><body><header><a href="/">OpenFactory lanes</a><a href="/programs">Discovery programs</a><span>Read-only discovery</span></header><main><h1>{{.Title}}</h1><p>Discovery proposals are not delivered software. Answers and exact approvals are managed through the CLI.</p>{{if .List}}<form method="get" action="/programs"><label>Project <input name="project" value="{{.Project}}"></label><label>State <input name="state" value="{{.State}}"></label><button type="submit">Filter view</button></form>{{end}}<nav><ul>{{range .Links}}<li><a href="{{.URL}}">{{.Label}}</a></li>{{end}}</ul></nav><p>Failed hypotheses remain visible. Missing prerequisites, infrastructure failures, inconclusive or truncated output are not falsification. Quote verification establishes occurrence in a captured source, not publisher truth or semantic entailment.</p>{{range .Sections}}<section><h2>{{.Title}}</h2><pre>{{pretty .Value}}</pre></section>{{end}}{{if .Next}}<a href="{{.Next}}">Next page →</a>{{end}}<p><a href="{{.JSON}}">Canonical JSON</a></p></main><footer>Bounded immutable revision history; source HTML is displayed as text, never executed.</footer></body></html>`))
