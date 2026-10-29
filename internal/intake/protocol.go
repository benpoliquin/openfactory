package intake

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"openfactory/internal/model"
)

var commandUUID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// DecodeStrict rejects duplicate names recursively as well as unknown fields,
// trailing JSON, null roots and invalid UTF-8. encoding/json alone accepts
// duplicate keys and replacement characters, unsuitable for review digests.
func DecodeStrict(data []byte, dst any) error {
	if !utf8.Valid(data) {
		return errors.New("JSON must be UTF-8")
	}
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return errors.New("JSON root must not be null")
	}
	return model.StrictDecode(data, dst)
}

func validSHA(s string) bool {
	b, err := hex.DecodeString(s)
	return err == nil && len(b) == 32 && strings.ToLower(s) == s
}
func nonempty(s string) bool { return strings.TrimSpace(s) != "" }
func validateDisclosure(d model.IntakeDisclosure) error {
	if !validSHA(d.ContextSHA) || !validSHA(d.PolicySHA) || len(d.ModelProviderIDs) == 0 {
		return errors.New("exact context, policy and provider disclosure are required")
	}
	seen := map[string]bool{}
	for _, id := range d.ModelProviderIDs {
		if !nonempty(id) || len(id) > 1024 || strings.ContainsAny(id, "\r\n\x00") || seen[id] {
			return errors.New("invalid provider disclosure")
		}
		seen[id] = true
	}
	seen = map[string]bool{}
	for _, sha := range d.BundleManifestSHAs {
		if !validSHA(sha) || seen[sha] || !d.PrivateMaterialToModels {
			return errors.New("invalid private bundle disclosure")
		}
		seen[sha] = true
	}
	return nil
}

func DecodeCommand(data []byte) (model.TypedCommand, error) {
	var c model.TypedCommand
	if len(data) > 3<<20 {
		return c, errors.New("command exceeds input limit")
	}
	if err := DecodeStrict(data, &c); err != nil {
		return c, err
	}
	if _, err := CommandPayload(c); err != nil {
		return c, err
	}
	return c, nil
}

// CommandPayload validates the complete action union without accepting arbitrary
// maps. All authorities and policy are resolved later from controller state.
func CommandPayload(c model.TypedCommand) (any, error) {
	if !commandUUID.MatchString(c.CommandID) {
		return nil, errors.New("command_id must be a UUID")
	}
	if c.ExpectedContentVersion != nil && *c.ExpectedContentVersion < 0 {
		return nil, errors.New("invalid content version")
	}
	requestAction := strings.HasPrefix(c.Action, "intake.") || c.Action == model.ActionTurnAppend || c.Action == model.ActionDraftReplace
	if requestAction && (!identifier.MatchString(c.RequestID) || c.ExpectedContentVersion == nil) {
		return nil, errors.New("request ID and expected content version are required")
	}
	if !requestAction && (c.RequestID != "" || c.ExpectedContentVersion != nil) {
		return nil, errors.New("request binding is not valid for this action")
	}
	var payload any
	switch c.Action {
	case model.ActionRequestCreate:
		payload = &model.RequestCreatePayload{}
	case model.ActionTurnAppend:
		payload = &model.TurnAppendPayload{}
	case model.ActionIntakeDraft:
		payload = &model.IntakeDraftPayload{}
	case model.ActionDraftReplace:
		payload = &model.DraftReplacePayload{}
	case model.ActionIntakeCancel, model.ActionIntakeRetry:
		payload = &model.EmptyCommandPayload{}
	case model.ActionIntakeApproveQueue:
		payload = &model.IntakeApprovePayload{}
	case model.ActionProgramAnswer:
		payload = &model.ProgramAnswerPayload{}
	case model.ActionPlanningAnswer:
		payload = &model.PlanningAnswerPayload{}
	case model.ActionSubjectCancel, model.ActionSubjectRetry, model.ActionFeatureApprove, model.ActionProgramApproveRevision, model.ActionProgramResume:
		payload = &model.SubjectCommandPayload{}
	case model.ActionProgramApproveArchitecture, model.ActionProgramApproveExperiment:
		payload = &model.ProgramArtifactCommandPayload{}
	default:
		return nil, errors.New("unsupported action")
	}
	if len(c.Payload) == 0 {
		return nil, errors.New("payload is required")
	}
	if err := DecodeStrict(c.Payload, payload); err != nil {
		return nil, err
	}
	switch p := payload.(type) {
	case *model.RequestCreatePayload:
		if !identifier.MatchString(p.Project) || !nonempty(p.Text) || len(p.Text) > 16<<10 || !commandUUID.MatchString(p.ClientMessageID) || p.ParentRequestID != "" && !identifier.MatchString(p.ParentRequestID) {
			return nil, errors.New("invalid request creation payload")
		}
	case *model.TurnAppendPayload:
		if !nonempty(p.Text) || len(p.Text) > 16<<10 || !commandUUID.MatchString(p.ClientMessageID) {
			return nil, errors.New("invalid turn payload")
		}
		if p.Question != nil && (!identifier.MatchString(p.Question.QuestionID) || p.Question.ContentVersion < 1 || !validSHA(p.Question.InputManifestSHA) || !validSHA(p.Question.OutputSHA)) {
			return nil, errors.New("invalid question binding")
		}
	case *model.IntakeDraftPayload:
		if err := validateDisclosure(p.Disclosure); err != nil {
			return nil, err
		}
	case *model.DraftReplacePayload:
		if !validRouteFormat(p.Route, p.Format) || !nonempty(p.CanonicalSource) || len(p.CanonicalSource) > 256<<10 {
			return nil, errors.New("invalid draft replacement")
		}
		if p.BundleManifestSHA != "" && (p.Route != model.IntakeRouteProgram || !validSHA(p.BundleManifestSHA)) {
			return nil, errors.New("only program replacement may select an exact seed bundle")
		}
	case *model.IntakeApprovePayload:
		if p.DraftRevision < 1 || !validSHA(p.SourceSHA) || !validSHA(p.DomainRevision) {
			return nil, errors.New("invalid draft approval binding")
		}
	case *model.SubjectCommandPayload:
		if (p.Kind != "feature" && p.Kind != "plan" && p.Kind != "program") || !identifier.MatchString(p.ID) || !validSHA(p.Revision) {
			return nil, errors.New("invalid subject binding")
		}
		if c.Action == model.ActionFeatureApprove && p.Kind == "program" || (c.Action == model.ActionProgramApproveRevision || c.Action == model.ActionProgramResume) && p.Kind != "program" {
			return nil, errors.New("action does not match subject kind")
		}
	case *model.ProgramArtifactCommandPayload:
		expected := model.ArtifactArchitecture
		if c.Action == model.ActionProgramApproveExperiment {
			expected = model.ArtifactExperimentContracts
		}
		if !identifier.MatchString(p.ProgramID) || !validSHA(p.Revision) || !identifier.MatchString(p.ArtifactID) || p.ArtifactKind != expected || !validSHA(p.ArtifactSHA) || !validSHA(p.SubjectSHA) || c.Action == model.ActionProgramApproveArchitecture && p.SubjectSHA != p.ArtifactSHA {
			return nil, errors.New("invalid artifact approval binding")
		}
	case *model.ProgramAnswerPayload:
		if !identifier.MatchString(p.ProgramID) || !validSHA(p.RevisionHash) || !identifier.MatchString(p.QuestionArtifactID) || !validSHA(p.QuestionArtifactSHA) || !identifier.MatchString(p.QuestionID) || !nonempty(p.Text) || len(p.Text) > 16<<10 {
			return nil, errors.New("invalid program answer binding")
		}
		for _, r := range p.SupportingRefs {
			if !nonempty(r.Kind) || !identifier.MatchString(r.ID) || !validSHA(r.SHA) || r.Revision < 0 {
				return nil, errors.New("invalid answer evidence reference")
			}
		}
	case *model.PlanningAnswerPayload:
		if !identifier.MatchString(p.FeatureID) || !validSHA(p.FeatureRevision) || !identifier.MatchString(p.AttemptID) || !validSHA(p.OutputSHA) || !identifier.MatchString(p.QuestionID) || !nonempty(p.Text) || len(p.Text) > 16<<10 {
			return nil, errors.New("invalid planning answer binding")
		}
	}
	return payload, nil
}

func CommandDigest(c model.TypedCommand) (string, error) {
	data, err := CommandCanonical(c)
	if err != nil {
		return "", err
	}
	return SHA256(data), nil
}

// CommandCanonical returns the exact bytes bound by CommandDigest. Review
// consumers hash these bytes rather than attempting cross-language Go encoding.
func CommandCanonical(c model.TypedCommand) ([]byte, error) {
	payload, err := CommandPayload(c)
	if err != nil {
		return nil, err
	}
	normalized, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	c.Payload = normalized
	return json.Marshal(c)
}

func RequiresHumanVerification(action string) bool {
	switch action {
	case model.ActionIntakeApproveQueue, model.ActionFeatureApprove, model.ActionProgramApproveRevision, model.ActionProgramApproveArchitecture, model.ActionProgramApproveExperiment, model.ActionSubjectRetry, model.ActionProgramResume, model.ActionIntakeRetry:
		return true
	}
	return false
}
func validRouteFormat(route, format string) bool {
	return (route == model.IntakeRouteFeature || route == model.IntakeRoutePlan) && format == model.IntakeFormatFeature || route == model.IntakeRouteProgram && format == model.IntakeFormatProgram
}

func ParseModelOutput(data []byte, maxBytes int) (model.IntakeModelOutput, error) {
	var out model.IntakeModelOutput
	if maxBytes <= 0 || maxBytes > 256<<10 {
		maxBytes = 256 << 10
	}
	if len(data) > maxBytes {
		return out, errors.New("intake output exceeds limit")
	}
	if err := DecodeStrict(data, &out); err != nil {
		return out, err
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(data, &keys); err != nil {
		return out, err
	}
	allowed := map[string]bool{"type": true, "message": true}
	switch out.Type {
	case "questions":
		allowed["questions"] = true
		if len(out.Questions) < 1 || len(out.Questions) > 3 {
			return out, errors.New("questions output requires one to three questions")
		}
		seen := map[string]bool{}
		for _, q := range out.Questions {
			if !identifier.MatchString(q.LocalID) || seen[q.LocalID] || !nonempty(q.Text) || !nonempty(q.Consequence) {
				return out, errors.New("invalid consequential question")
			}
			seen[q.LocalID] = true
			switch q.AnswerType {
			case "text":
				if len(q.Choices) > 0 {
					return out, errors.New("text questions cannot provide choices")
				}
			case "single_choice", "multiple_choice":
				if len(q.Choices) < 2 {
					return out, errors.New("choice questions require choices")
				}
				choices := map[string]bool{}
				for _, choice := range q.Choices {
					if !nonempty(choice) || choices[choice] {
						return out, errors.New("invalid question choices")
					}
					choices[choice] = true
				}
			default:
				return out, errors.New("unsupported question answer type")
			}
		}
	case "draft":
		for _, key := range []string{"route", "format", "canonical_source", "assumptions", "unresolved_risks"} {
			allowed[key] = true
			if _, ok := keys[key]; !ok {
				return out, fmt.Errorf("draft output requires %s", key)
			}
		}
		if !validRouteFormat(out.Route, out.Format) || !nonempty(out.CanonicalSource) || len(out.CanonicalSource) > 256<<10 {
			return out, errors.New("invalid draft route or source")
		}
	case "blocked":
		allowed["missing_prerequisites"] = true
		if len(out.MissingPrerequisites) == 0 {
			return out, errors.New("blocked output requires missing prerequisites")
		}
	default:
		return out, errors.New("unsupported intake output type")
	}
	if !nonempty(out.Message) {
		return out, errors.New("output message is required")
	}
	for key := range keys {
		if !allowed[key] {
			return out, fmt.Errorf("field %q does not belong to output type", key)
		}
	}
	for key, raw := range keys {
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return out, fmt.Errorf("output field %q must not be null", key)
		}
	}
	for _, list := range [][]string{out.Assumptions, out.UnresolvedRisks, out.MissingPrerequisites} {
		for _, s := range list {
			if !nonempty(s) {
				return out, errors.New("output list items must be nonempty")
			}
		}
	}
	return out, nil
}
