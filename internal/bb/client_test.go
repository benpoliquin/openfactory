package bb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"openfactory/internal/model"
)

func testClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	s := httptest.NewServer(handler)
	t.Cleanup(s.Close)
	return New(model.Config{BBURL: s.URL, AstraModel: "astra", FastModel: "openrouter/fast"})
}
func respond(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}
func hosts(w http.ResponseWriter) {
	respond(w, []any{map[string]any{"id": "host", "status": "connected", "lifecycle": map[string]any{"phase": "active"}}})
}
func wireThread(id, status string) map[string]any {
	return map[string]any{"id": id, "projectId": "project", "environmentId": "env", "providerId": provider, "status": status}
}

// Required payload fields follow bb-app@0.47.1 dist/index.d.ts:
// turn/started and turn/input/accepted (639-667), agentMessage (1515-1517),
// client/turn/requested (2828-3121), and provider/warning (2765-2775).
const openfactoryRequestID = "creq_23456789ab"
const operatorRequestID = "creq_23456789ac"

func wireEvent(seq int, kind, turn string, data map[string]any) map[string]any {
	scope := map[string]any{"kind": "thread"}
	if turn != "" {
		scope = map[string]any{"kind": "turn", "turnId": turn}
	}
	switch kind {
	case "turn/started", "turn/completed", "turn/input/accepted", "item/completed", "provider/warning", "provider/error":
		data["providerThreadId"] = "provider-thread"
	}
	if kind == "item/completed" {
		data["item"].(map[string]any)["id"] = fmt.Sprintf("item-%d", seq)
	}
	return map[string]any{"id": fmt.Sprintf("event-%d", seq), "threadId": "thread", "seq": seq, "type": kind, "scope": scope, "data": data, "createdAt": 1}
}

func wireRequested(seq int, requestID, source, text string) map[string]any {
	method, target := "turn/start", "new-turn"
	if source == "spawn" {
		method, target = "thread/start", "thread-start"
	}
	return wireEvent(seq, "client/turn/requested", "", map[string]any{
		"direction": "outbound", "requestId": requestID, "source": source, "initiator": "user", "senderThreadId": nil,
		"input":  []any{map[string]any{"type": "text", "text": text, "mentions": []any{}}},
		"target": map[string]any{"kind": target}, "request": map[string]any{"method": method, "params": map[string]any{}},
		"execution": map[string]any{"model": "astra", "serviceTier": "", "reasoningLevel": "", "source": "client/turn/requested", "permissionMode": "full"},
	})
}

func wireOpenFactoryStart() []any {
	return []any{
		wireRequested(1, openfactoryRequestID, "spawn", "[openfactory-attempt:attempt]\n\nBuild it"),
		wireEvent(2, "turn/started", "root", map[string]any{}),
		wireEvent(3, "turn/input/accepted", "root", map[string]any{"clientRequestId": openfactoryRequestID}),
	}
}

func TestSpawnRejectsEmptyInputBeforeHTTP(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) { t.Fatal("unexpected request") })
	_, err := c.Spawn(context.Background(), model.SpawnRequest{AttemptID: "a", ProjectID: "project", Workspace: "/workspaces/repo", Model: "astra", Prompt: " "})
	var e *RequestError
	if !errors.As(err, &e) || e.Uncertain() {
		t.Fatalf("expected definitive local rejection: %v", err)
	}
}

func TestMutationErrorsDistinguishUncertainOutcomes(t *testing.T) {
	for _, test := range []struct {
		status    int
		malformed bool
		uncertain bool
	}{{400, false, false}, {403, false, false}, {409, false, false}, {408, false, true}, {500, false, true}, {502, false, true}, {200, true, true}} {
		t.Run(fmt.Sprintf("%d", test.status), func(t *testing.T) {
			calls := 0
			c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.WriteHeader(test.status)
				if test.malformed {
					_, _ = w.Write([]byte("{"))
				}
			})
			var out thread
			err := c.request(context.Background(), "POST", "/api/v1/threads", nil, map[string]any{}, &out)
			var e *RequestError
			if !errors.As(err, &e) || e.Uncertain() != test.uncertain || calls != 1 {
				t.Fatalf("error %v uncertain=%v calls=%d", err, e, calls)
			}
		})
	}
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		h := w.(http.Hijacker)
		conn, _, err := h.Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		_ = conn.Close()
	})
	err := c.Stop(context.Background(), "thread")
	var e *RequestError
	if !errors.As(err, &e) || !e.Uncertain() {
		t.Fatalf("transport outcome must be uncertain: %v", err)
	}
}

func TestPollRootCompletionSurvivesCursorRestart(t *testing.T) {
	phase := 0
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/threads/thread":
			respond(w, wireThread("thread", "idle"))
		case "/api/v1/threads/thread/interactions":
			respond(w, []any{})
		case "/api/v1/threads/thread/events":
			after, _ := strconv.Atoi(r.URL.Query().Get("afterSeq"))
			if phase == 0 {
				if after != 0 {
					t.Errorf("first cursor %d", after)
				}
				rows := wireOpenFactoryStart()
				rows = append(rows, wireEvent(4, "item/completed", "root", map[string]any{"item": map[string]any{"type": "agentMessage", "text": "root result"}}), wireEvent(5, "turn/started", "child", map[string]any{"parentToolCallId": "tool"}), wireEvent(6, "turn/completed", "child", map[string]any{"status": "completed"}))
				respond(w, rows)
			} else if phase == 1 {
				if after != 6 {
					t.Errorf("restart cursor %d", after)
				}
				respond(w, []any{wireEvent(7, "turn/completed", "root", map[string]any{"status": "completed"})})
			} else {
				if after != 7 {
					t.Errorf("terminal cursor %d", after)
				}
				respond(w, []any{})
			}
		default:
			t.Errorf("route %s", r.URL.Path)
			http.NotFound(w, r)
		}
	})
	first, err := c.Poll(context.Background(), "thread", "")
	if err != nil || first.State != "running" || first.Output != "" {
		t.Fatalf("nested completion or idle incorrectly succeeded: %#v %v", first, err)
	}
	phase = 1
	// A new Client has no memory of the previously observed root or text.
	restarted := New(c.cfg)
	second, err := restarted.Poll(context.Background(), "thread", first.Cursor)
	if err != nil || second.State != "completed" || second.Output != "root result" {
		t.Fatalf("root completion: %#v %v", second, err)
	}
	phase = 2
	third, err := restarted.Poll(context.Background(), "thread", second.Cursor)
	if err != nil || third.State != "completed" || third.Output != "root result" {
		t.Fatalf("durable completion: %#v %v", third, err)
	}
	if _, err = restarted.Poll(context.Background(), "different", second.Cursor); err == nil {
		t.Fatal("cross-thread cursor accepted")
	}
}

func TestPollDoesNotReuseOutputForNewTurn(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/events"):
			rows := wireOpenFactoryStart()
			rows = append(rows,
				wireEvent(4, "item/completed", "root", map[string]any{"item": map[string]any{"type": "agentMessage", "text": "unfinished openfactory output"}}),
				wireRequested(5, operatorRequestID, "tell", "Operator's unrelated request"),
				wireEvent(6, "turn/started", "operator", map[string]any{}),
				wireEvent(7, "turn/input/accepted", "operator", map[string]any{"clientRequestId": operatorRequestID}),
				wireEvent(8, "item/completed", "operator", map[string]any{"item": map[string]any{"type": "agentMessage", "text": "unrelated success"}}),
				wireEvent(9, "turn/completed", "operator", map[string]any{"status": "completed"}),
			)
			respond(w, rows)
		case strings.HasSuffix(r.URL.Path, "/interactions"):
			respond(w, []any{})
		default:
			respond(w, wireThread("thread", "idle"))
		}
	})
	o, err := c.Poll(context.Background(), "thread", "")
	if err != nil || o.State != "running" || o.Output != "" {
		t.Fatalf("stale completion: %#v %v", o, err)
	}
}

func TestPollTerminalAndBlockedBoundaries(t *testing.T) {
	for _, state := range []string{"failed", "interrupted", "blocked", "idle", "error"} {
		t.Run(state, func(t *testing.T) {
			c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasSuffix(r.URL.Path, "/events"):
					rows := wireOpenFactoryStart()
					if state == "failed" || state == "interrupted" {
						rows = append(rows, wireEvent(4, "turn/completed", "root", map[string]any{"status": state, "error": map[string]any{"message": "failure reason"}}))
					}
					respond(w, rows)
				case strings.HasSuffix(r.URL.Path, "/interactions"):
					if state == "blocked" {
						respond(w, []any{map[string]any{"status": "pending", "id": "question"}})
					} else {
						respond(w, []any{})
					}
				default:
					status := "idle"
					if state == "error" {
						status = "error"
					}
					respond(w, wireThread("thread", status))
				}
			})
			o, err := c.Poll(context.Background(), "thread", "")
			expected := state
			if state == "idle" {
				expected = "running"
			}
			if state == "error" {
				expected = "failed"
			}
			if err != nil || o.State != expected || o.Output != "" {
				t.Fatalf("%s: %#v %v", state, o, err)
			}
		})
	}
}

func TestPollOpenFactoryFailureCannotBeReplacedByOperatorSuccess(t *testing.T) {
	for _, failure := range []string{"failed", "interrupted", "thread-interrupted", "system-error", "provider-error", "rejected", "snapshot-error"} {
		for _, restart := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/restart=%t", failure, restart), func(t *testing.T) {
				rows := wireOpenFactoryStart()
				switch failure {
				case "failed", "interrupted":
					rows = append(rows, wireEvent(4, "turn/completed", "root", map[string]any{"status": failure, "error": map[string]any{"message": "original openfactory failure"}}))
				case "thread-interrupted":
					rows = append(rows, wireEvent(4, "system/thread/interrupted", "", map[string]any{"reason": "manual-stop"}))
				case "system-error":
					rows = append(rows, wireEvent(4, "system/error", "", map[string]any{"message": "original openfactory failure"}))
				case "provider-error":
					rows = append(rows, wireEvent(4, "provider/error", "root", map[string]any{"message": "original openfactory failure"}))
				case "rejected":
					rows = rows[:1]
					rows = append(rows, wireEvent(4, "client/turn/rejected", "", map[string]any{"requestId": openfactoryRequestID, "reason": "provider-failed", "message": "original openfactory failure"}))
				}
				initial := append([]any(nil), rows...)
				later := []any{
					wireRequested(5, operatorRequestID, "tell", "An unrelated operator request"),
					wireEvent(6, "turn/started", "operator", map[string]any{}),
					wireEvent(7, "turn/input/accepted", "operator", map[string]any{"clientRequestId": operatorRequestID}),
					wireEvent(8, "item/completed", "operator", map[string]any{"item": map[string]any{"type": "agentMessage", "text": "operator succeeded"}}),
					wireEvent(9, "turn/completed", "operator", map[string]any{"status": "completed"}),
				}
				if !restart {
					rows = append(rows, later...)
				}
				phase := 0
				c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
					switch {
					case strings.HasSuffix(r.URL.Path, "/events"):
						after, _ := strconv.Atoi(r.URL.Query().Get("afterSeq"))
						result := []any{}
						for _, value := range rows {
							if value.(map[string]any)["seq"].(int) > after {
								result = append(result, value)
							}
						}
						respond(w, result)
					case strings.HasSuffix(r.URL.Path, "/interactions"):
						respond(w, []any{})
					default:
						status := "idle"
						if failure == "snapshot-error" && phase == 0 {
							status = "error"
						}
						respond(w, wireThread("thread", status))
					}
				})
				first, err := c.Poll(context.Background(), "thread", "")
				want := "failed"
				if failure == "interrupted" || failure == "thread-interrupted" {
					want = "interrupted"
				}
				if err != nil || first.State != want || first.Output != "" || first.Reason == "" {
					t.Fatalf("openfactory failure replaced: %#v %v", first, err)
				}
				rows, phase = append(initial, later...), 1
				second, err := New(c.cfg).Poll(context.Background(), "thread", first.Cursor)
				if err != nil || second.State != want || second.Output != "" || second.Reason != first.Reason {
					t.Fatalf("openfactory failure lost after restart: %#v %v (was %#v)", second, err, first)
				}
			})
		}
	}
}

func TestPollCorrelatesOverlappingRootsAcrossCursorRecovery(t *testing.T) {
	rows := []any{
		wireRequested(1, openfactoryRequestID, "spawn", "[openfactory-attempt:attempt]\n\nBuild it"),
		wireEvent(2, "turn/started", "root", map[string]any{}),
		wireRequested(3, operatorRequestID, "tell", "Operator request"),
		wireEvent(4, "turn/started", "operator", map[string]any{}),
		wireEvent(5, "turn/input/accepted", "operator", map[string]any{"clientRequestId": operatorRequestID}),
	}
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/events"):
			after, _ := strconv.Atoi(r.URL.Query().Get("afterSeq"))
			result := []any{}
			for _, value := range rows {
				if value.(map[string]any)["seq"].(int) > after {
					result = append(result, value)
				}
			}
			respond(w, result)
		case strings.HasSuffix(r.URL.Path, "/interactions"):
			respond(w, []any{})
		default:
			respond(w, wireThread("thread", "active"))
		}
	})
	first, err := c.Poll(context.Background(), "thread", "")
	if err != nil || first.State != "running" || first.Output != "" {
		t.Fatalf("unaccepted request: %#v %v", first, err)
	}
	rows = append(rows,
		wireEvent(6, "turn/input/accepted", "root", map[string]any{"clientRequestId": openfactoryRequestID}),
		wireEvent(7, "item/completed", "root", map[string]any{"item": map[string]any{"type": "agentMessage", "text": "openfactory result"}}),
		wireEvent(8, "item/completed", "operator", map[string]any{"item": map[string]any{"type": "agentMessage", "text": "wrong result"}}),
		wireEvent(9, "turn/completed", "operator", map[string]any{"status": "completed"}),
	)
	second, err := New(c.cfg).Poll(context.Background(), "thread", first.Cursor)
	if err != nil || second.State != "running" || second.Output != "" {
		t.Fatalf("overlapping operator completion accepted: %#v %v", second, err)
	}
	rows = append(rows, wireEvent(10, "turn/completed", "root", map[string]any{"status": "completed"}))
	// Output and ownership must survive the active-to-idle snapshot race.
	third, err := New(c.cfg).Poll(context.Background(), "thread", second.Cursor)
	if err != nil || third.State != "running" {
		t.Fatalf("active snapshot accepted: %#v %v", third, err)
	}
	idle := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/events"), strings.HasSuffix(r.URL.Path, "/interactions"):
			respond(w, []any{})
		default:
			respond(w, wireThread("thread", "idle"))
		}
	})
	final, err := idle.Poll(context.Background(), "thread", third.Cursor)
	if err != nil || final.State != "completed" || final.Output != "openfactory result" {
		t.Fatalf("original correlated completion lost: %#v %v", final, err)
	}
}

func TestPollFailsClosedWithoutUniqueOpenFactoryOwnership(t *testing.T) {
	for _, scenario := range []string{"missing-dispatch", "missing-acceptance", "wrong-request", "nested-turn", "duplicate-dispatch", "multiple-accepted-turns", "shared-turn-before", "shared-turn-after", "ack-only"} {
		t.Run(scenario, func(t *testing.T) {
			rows := wireOpenFactoryStart()
			switch scenario {
			case "missing-dispatch":
				rows = rows[1:]
			case "missing-acceptance":
				rows = rows[:2]
			case "wrong-request":
				rows[2] = wireEvent(3, "turn/input/accepted", "root", map[string]any{"clientRequestId": operatorRequestID})
			case "nested-turn":
				rows[1] = wireEvent(2, "turn/started", "root", map[string]any{"parentToolCallId": "tool"})
			case "duplicate-dispatch":
				rows = append(rows, wireRequested(4, operatorRequestID, "spawn", "[openfactory-attempt:attempt]\n\nDuplicate"))
			case "multiple-accepted-turns":
				rows = append(rows, wireEvent(4, "turn/started", "other", map[string]any{}), wireEvent(5, "turn/input/accepted", "other", map[string]any{"clientRequestId": openfactoryRequestID}))
			case "shared-turn-before":
				rows[2] = wireEvent(3, "turn/input/accepted", "root", map[string]any{"clientRequestId": operatorRequestID})
				rows = append(rows, wireEvent(4, "turn/input/accepted", "root", map[string]any{"clientRequestId": openfactoryRequestID}))
			case "shared-turn-after":
				rows = append(rows, wireEvent(4, "turn/input/accepted", "root", map[string]any{"clientRequestId": operatorRequestID}))
			}
			rows = append(rows, wireEvent(6, "item/completed", "root", map[string]any{"item": map[string]any{"type": "agentMessage", "text": "ACK"}}))
			if scenario != "ack-only" {
				rows = append(rows, wireEvent(7, "turn/completed", "root", map[string]any{"status": "completed"}))
			}
			c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasSuffix(r.URL.Path, "/events"):
					after, _ := strconv.Atoi(r.URL.Query().Get("afterSeq"))
					if after == 0 {
						respond(w, rows)
					} else {
						respond(w, []any{})
					}
				case strings.HasSuffix(r.URL.Path, "/interactions"):
					respond(w, []any{})
				default:
					respond(w, wireThread("thread", "idle"))
				}
			})
			first, err := c.Poll(context.Background(), "thread", "")
			want := "blocked"
			if scenario == "ack-only" {
				want = "running"
			}
			if err != nil || first.State != want || first.Output != "" {
				t.Fatalf("unowned completion accepted: %#v %v", first, err)
			}
			second, err := New(c.cfg).Poll(context.Background(), "thread", first.Cursor)
			if err != nil || second.State != want || second.Output != "" {
				t.Fatalf("unowned completion accepted after restart: %#v %v", second, err)
			}
		})
	}
}

func TestFindPaginatesAndMatchesOnlyDurableExactMarker(t *testing.T) {
	pages := 0
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/threads" {
			pages++
			if r.URL.Query().Get("includeHidden") != "true" || r.URL.Query().Get("projectId") != "project" {
				t.Error("missing recovery filters")
			}
			if r.URL.Query().Get("archived") == "true" {
				respond(w, []any{})
				return
			}
			if r.URL.Query().Get("offset") == "0" {
				rows := []any{}
				for n := range pageSize {
					row := wireThread(fmt.Sprintf("other%d", n), "idle")
					row["title"] = "[openfactory-attempt:attempt]"
					rows = append(rows, row)
				}
				respond(w, rows)
			} else {
				respond(w, []any{wireThread("match", "idle"), wireThread("duplicate", "idle")})
			}
			return
		}
		if strings.HasSuffix(r.URL.Path, "/queued-messages") {
			respond(w, []any{})
			return
		}
		if strings.HasSuffix(r.URL.Path, "/events") {
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/threads/"), "/events")
			text := "[openfactory-attempt:attempt-other]\n\nnot ours"
			if id == "match" || id == "duplicate" {
				text = "[openfactory-attempt:attempt]\n\noriginal dispatch"
			}
			row := wireRequested(1, openfactoryRequestID, "spawn", text)
			row["threadId"] = id
			respond(w, []any{row})
			return
		}
		t.Errorf("unexpected recovery route %s", r.URL.Path)
		http.NotFound(w, r)
	})
	found, err := c.Find(context.Background(), "project", "attempt")
	if err != nil || len(found) != 2 || found[0].ThreadID != "match" || found[1].ThreadID != "duplicate" || pages != 3 {
		t.Fatalf("reconcile %#v %v pages=%d", found, err, pages)
	}
}

func TestFindRecoversPendingQueuedInputWithoutEnvironment(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/threads":
			if r.URL.Query().Get("archived") == "true" {
				respond(w, []any{})
				return
			}
			row := wireThread("pending", "pending")
			row["environmentId"] = nil
			row["title"] = "renamed"
			respond(w, []any{row})
		case "/api/v1/threads/pending/queued-messages":
			respond(w, []any{map[string]any{"threadId": "pending", "content": []any{map[string]any{"type": "text", "text": "[openfactory-attempt:attempt]\n\nBuild"}}}})
		case "/api/v1/threads/pending/events":
			respond(w, []any{})
		default:
			t.Errorf("route %s", r.URL.Path)
			http.NotFound(w, r)
		}
	})
	found, err := c.Find(context.Background(), "project", "attempt")
	if err != nil || len(found) != 1 || found[0].ThreadID != "pending" || found[0].EnvironmentID != "" {
		t.Fatalf("queued recovery %#v %v", found, err)
	}
}

func TestPollPaginatesEventsBeforeAcceptingCompletion(t *testing.T) {
	pages := 0
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/threads/thread":
			respond(w, wireThread("thread", "idle"))
		case "/api/v1/threads/thread/interactions":
			respond(w, []any{})
		case "/api/v1/threads/thread/events":
			pages++
			if r.URL.Query().Get("afterSeq") == "0" {
				rows := wireOpenFactoryStart()
				for n := range pageSize - 3 {
					rows = append(rows, wireEvent(n+4, "provider/warning", "root", map[string]any{"category": "general", "summary": "provider warning"}))
				}
				respond(w, rows)
			} else {
				if r.URL.Query().Get("afterSeq") != "100" {
					t.Error("wrong page cursor")
				}
				respond(w, []any{wireEvent(101, "item/completed", "root", map[string]any{"item": map[string]any{"type": "agentMessage", "text": "final"}}), wireEvent(102, "turn/completed", "root", map[string]any{"status": "completed"})})
			}
		default:
			t.Errorf("route %s", r.URL.Path)
		}
	})
	o, err := c.Poll(context.Background(), "thread", "")
	if err != nil || o.State != "completed" || o.Output != "final" || pages != 2 {
		t.Fatalf("pagination %#v %v pages=%d", o, err, pages)
	}
}

func TestEnsureProjectUsesHostPathAndRejectsNameCollision(t *testing.T) {
	for _, exists := range []bool{false, true} {
		t.Run(strconv.FormatBool(exists), func(t *testing.T) {
			created := 0
			c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v1/hosts":
					hosts(w)
				case "/api/v1/projects":
					if r.Method == "GET" {
						if exists {
							respond(w, []any{map[string]any{"id": "wrong", "name": "repo", "sources": []any{map[string]any{"type": "local_path", "hostId": "host", "path": "/other"}}}})
						} else {
							respond(w, []any{})
						}
						return
					}
					created++
					var body map[string]any
					_ = json.NewDecoder(r.Body).Decode(&body)
					source := body["source"].(map[string]any)
					if source["hostId"] != "host" || source["path"] != "/workspaces/repo" || source["type"] != "local_path" {
						t.Errorf("source %#v", source)
					}
					respond(w, map[string]any{"id": "project"})
				default:
					t.Errorf("route %s", r.URL.Path)
				}
			})
			id, err := c.EnsureProject(context.Background(), "repo", "/workspaces/repo")
			if exists {
				if err == nil || created != 0 {
					t.Fatal("collision created or reused")
				}
			} else if err != nil || id != "project" || created != 1 {
				t.Fatalf("create %q %v %d", id, err, created)
			}
		})
	}
}

func TestDoctorRequiresPinnedVersionAndExplicitCatalogModels(t *testing.T) {
	for _, failure := range []string{"", "version", "provider", "model", "catalog"} {
		t.Run(failure, func(t *testing.T) {
			c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/health":
					respond(w, map[string]any{"ok": true})
				case "/api/v1/system/version":
					v := Version
					if failure == "version" {
						v = "0.48.0"
					}
					respond(w, map[string]any{"currentVersion": v})
				case "/api/v1/hosts":
					hosts(w)
				case "/api/v1/system/execution-options":
					if r.URL.Query().Get("providerId") != provider || r.URL.Query().Get("hostId") != "host" {
						t.Error("unscoped model catalog")
					}
					models := []any{map[string]any{"model": "astra"}, map[string]any{"model": "openrouter/fast"}}
					if failure == "model" {
						models = models[:1]
					}
					var loadError any
					if failure == "catalog" {
						loadError = map[string]any{"code": "auth_required"}
					}
					respond(w, map[string]any{"providers": []any{map[string]any{"id": provider, "available": failure != "provider"}}, "models": models, "modelLoadError": loadError})
				default:
					t.Errorf("invented doctor route %s", r.URL.Path)
					http.NotFound(w, r)
				}
			})
			err := c.Doctor(context.Background())
			if (err != nil) != (failure != "") {
				t.Fatalf("doctor failure=%s: %v", failure, err)
			}
		})
	}
}
