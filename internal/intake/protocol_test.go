package intake

import (
	"encoding/json"
	"strings"
	"testing"

	"openfactory/internal/model"
)

const testUUID = "289f113a-4068-4a1b-9450-38130c2d0a23"

func TestCommandDigestCanonicalUnionAndExactText(t *testing.T) {
	version := 2
	raw := `{"command_id":"` + testUUID + `","action":"turn.append","request_id":"request-1","expected_content_version":2,"payload":{"client_message_id":"` + testUUID + `","text":"Keep exact words."}}`
	c, err := DecodeCommand([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	digest, err := CommandDigest(c)
	if err != nil {
		t.Fatal(err)
	}
	var payload model.TurnAppendPayload
	if err = json.Unmarshal(c.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	pretty, _ := json.MarshalIndent(payload, "", "  ")
	c.Payload = pretty
	if other, err := CommandDigest(c); err != nil || other != digest {
		t.Fatal("JSON whitespace changed command identity", err)
	}
	payload.Text += " "
	c.Payload, _ = json.Marshal(payload)
	if changed, _ := CommandDigest(c); changed == digest {
		t.Fatal("exact dictated text was trimmed out of digest")
	}
	c.ExpectedContentVersion = &version
	version++
	if changed, _ := CommandDigest(c); changed == digest {
		t.Fatal("content CAS binding omitted from command digest")
	}
}

func TestCommandRejectsAuthorityInjectionAndWrongUnion(t *testing.T) {
	payload := `{"kind":"feature","id":"product","revision":"` + strings.Repeat("a", 64) + `"}`
	base := `{"command_id":"` + testUUID + `","action":"feature.approve","payload":` + payload + `}`
	if _, err := DecodeCommand([]byte(base)); err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"actor":                     strings.Replace(base, `"action":`, `"actor":"admin","action":`, 1),
		"approved":                  strings.Replace(base, `"kind":"feature"`, `"approved":true,"kind":"feature"`, 1),
		"duplicate action":          strings.Replace(base, `"action":"feature.approve"`, `"action":"intake.cancel","action":"feature.approve"`, 1),
		"nested duplicate":          strings.Replace(base, `"id":"product"`, `"id":"hidden","id":"product"`, 1),
		"wrong scope":               strings.Replace(base, `"kind":"feature"`, `"kind":"program"`, 1),
		"new stale request binding": strings.Replace(base, `"payload":`, `"request_id":"request","expected_content_version":2,"payload":`, 1),
		"shell":                     strings.Replace(base, `"kind":"feature"`, `"shell":"run","kind":"feature"`, 1),
		"null payload":              strings.Replace(base, payload, `null`, 1),
		"trailing":                  base + ` {}`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeCommand([]byte(raw)); err == nil {
				t.Fatal("unsafe command accepted")
			}
		})
	}
}

func TestArtifactCommandBindsContainingArtifactAndSelectedContract(t *testing.T) {
	c := model.TypedCommand{CommandID: testUUID, Action: model.ActionProgramApproveExperiment}
	p := model.ProgramArtifactCommandPayload{ProgramID: "program", Revision: strings.Repeat("a", 64), ArtifactID: "contracts", ArtifactKind: model.ArtifactExperimentContracts, ArtifactSHA: strings.Repeat("b", 64), SubjectSHA: strings.Repeat("c", 64)}
	c.Payload, _ = json.Marshal(p)
	before, err := CommandDigest(c)
	if err != nil {
		t.Fatal(err)
	}
	p.SubjectSHA = strings.Repeat("d", 64)
	c.Payload, _ = json.Marshal(p)
	after, err := CommandDigest(c)
	if err != nil {
		t.Fatal(err)
	}
	if before == after {
		t.Fatal("experiment contract identity not bound")
	}
	c.Action = model.ActionProgramApproveArchitecture
	p.ArtifactKind = model.ArtifactArchitecture
	c.Payload, _ = json.Marshal(p)
	if _, err = CommandPayload(c); err == nil {
		t.Fatal("architecture approved a different subject hash")
	}
	p.SubjectSHA = p.ArtifactSHA
	c.Payload, _ = json.Marshal(p)
	if _, err = CommandPayload(c); err != nil {
		t.Fatal(err)
	}
}

func TestStrictModelOutputConsequencesAndDiscriminatedFields(t *testing.T) {
	good := `{"type":"questions","message":"Need a deployment target.","questions":[{"local_id":"target","text":"Which environment?","consequence":"Determines release authority and checks.","answer_type":"single_choice","choices":["reviewed","staging"]}]}`
	if _, err := ParseModelOutput([]byte(good), 256<<10); err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"missing consequence":  strings.Replace(good, `"consequence":"Determines release authority and checks.",`, ``, 1),
		"duplicate local id":   strings.Replace(good, `}]}`, `},{"local_id":"target","text":"Other?","consequence":"Changes scope.","answer_type":"text"}]}`, 1),
		"foreign branch":       strings.Replace(good, `"message":`, `"canonical_source":"forged","message":`, 1),
		"empty foreign branch": strings.Replace(good, `"message":`, `"route":"","message":`, 1),
		"authority":            strings.Replace(good, `"message":`, `"approved":true,"message":`, 1),
		"duplicate key":        strings.Replace(good, `"type":"questions"`, `"type":"draft","type":"questions"`, 1),
		"unresolved blocked":   `{"type":"blocked","message":"Cannot proceed.","missing_prerequisites":[]}`,
		"null list":            `{"type":"draft","message":"Draft ready.","route":"feature","format":"feature_markdown","canonical_source":"source","assumptions":null,"unresolved_risks":[]}`,
		"wrong format":         `{"type":"draft","message":"Draft ready.","route":"program","format":"feature_markdown","canonical_source":"source","assumptions":[],"unresolved_risks":[]}`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseModelOutput([]byte(raw), 256<<10); err == nil {
				t.Fatal("invalid model branch accepted")
			}
		})
	}
	if _, err := ParseModelOutput([]byte(good), len(good)-1); err == nil {
		t.Fatal("model output silently truncated")
	}
}

func TestSavedTextAndDraftConsentHaveSeparateCommands(t *testing.T) {
	version := 1
	create := model.TypedCommand{CommandID: testUUID, Action: model.ActionRequestCreate}
	create.Payload, _ = json.Marshal(model.RequestCreatePayload{Project: "cli", Text: "Save this exact intent.", ClientMessageID: testUUID})
	if _, err := CommandPayload(create); err != nil {
		t.Fatal("saving required model consent:", err)
	}
	appendTurn := model.TypedCommand{CommandID: testUUID, Action: model.ActionTurnAppend, RequestID: "request", ExpectedContentVersion: &version}
	appendTurn.Payload, _ = json.Marshal(model.TurnAppendPayload{ClientMessageID: testUUID, Text: "Save an amendment without dispatch."})
	if _, err := CommandPayload(appendTurn); err != nil {
		t.Fatal(err)
	}
	draft := model.TypedCommand{CommandID: testUUID, Action: model.ActionIntakeDraft, RequestID: "request", ExpectedContentVersion: &version, Payload: json.RawMessage(`{}`)}
	if _, err := CommandPayload(draft); err == nil {
		t.Fatal("drafting accepted without exact saved-context disclosure")
	}
	disclosure := model.IntakeDisclosure{ContextSHA: strings.Repeat("a", 64), PolicySHA: strings.Repeat("b", 64), ModelProviderIDs: []string{"provider/model"}}
	draft.Payload, _ = json.Marshal(model.IntakeDraftPayload{Disclosure: disclosure})
	if _, err := CommandPayload(draft); err != nil {
		t.Fatal(err)
	}
	draft.ExpectedContentVersion = nil
	if _, err := CommandPayload(draft); err == nil {
		t.Fatal("draft consent omitted saved-content binding")
	}
	var oldPayload map[string]any
	if err := json.Unmarshal(appendTurn.Payload, &oldPayload); err != nil {
		t.Fatal(err)
	}
	oldPayload["disclosure"] = disclosure
	appendTurn.Payload, _ = json.Marshal(oldPayload)
	if _, err := CommandPayload(appendTurn); err == nil {
		t.Fatal("obsolete save-and-dispatch payload accepted")
	}
}

func TestCanonicalProgramBundleSelectionIsNotModelConsent(t *testing.T) {
	version := 1
	c := model.TypedCommand{CommandID: testUUID, Action: model.ActionDraftReplace, RequestID: "request", ExpectedContentVersion: &version}
	p := model.DraftReplacePayload{Route: model.IntakeRouteProgram, Format: model.IntakeFormatProgram, CanonicalSource: `{"id":"program"}`, BundleManifestSHA: strings.Repeat("a", 64)}
	c.Payload, _ = json.Marshal(p)
	before, err := CommandDigest(c)
	if err != nil {
		t.Fatal("canonical bundle selection incorrectly required model disclosure:", err)
	}
	p.BundleManifestSHA = strings.Repeat("b", 64)
	c.Payload, _ = json.Marshal(p)
	after, err := CommandDigest(c)
	if err != nil {
		t.Fatal(err)
	}
	if before == after {
		t.Fatal("selected private seed bundle omitted from exact command identity")
	}
	for _, route := range []string{model.IntakeRouteFeature, model.IntakeRoutePlan} {
		p.Route, p.Format = route, model.IntakeFormatFeature
		c.Payload, _ = json.Marshal(p)
		if _, err := CommandPayload(c); err == nil {
			t.Fatal("non-program contract accepted a private Program seed bundle")
		}
	}
}
