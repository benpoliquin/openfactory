package model

import "encoding/json"

const (
	ActionRequestCreate              = "request.create"
	ActionTurnAppend                 = "turn.append"
	ActionIntakeDraft                = "intake.draft"
	ActionDraftReplace               = "draft.replace"
	ActionIntakeCancel               = "intake.cancel"
	ActionProgramAnswer              = "program.answer"
	ActionPlanningAnswer             = "planning.answer"
	ActionSubjectCancel              = "subject.cancel"
	ActionIntakeApproveQueue         = "intake.approve_queue"
	ActionFeatureApprove             = "feature.approve"
	ActionProgramApproveRevision     = "program.approve_revision"
	ActionProgramApproveArchitecture = "program.approve_architecture"
	ActionProgramApproveExperiment   = "program.approve_experiment"
	ActionSubjectRetry               = "subject.retry"
	ActionProgramResume              = "program.resume"
	ActionIntakeRetry                = "intake.retry"
)

// TypedCommand has no actor or authority fields. Payload is decoded by action
// into exactly one of the following structs by intake.DecodeCommand.
type TypedCommand struct {
	CommandID              string          `json:"command_id"`
	Action                 string          `json:"action"`
	RequestID              string          `json:"request_id,omitempty"`
	ExpectedContentVersion *int            `json:"expected_content_version,omitempty"`
	Payload                json.RawMessage `json:"payload"`
}
type RequestCreatePayload struct {
	Project         string `json:"project"`
	Text            string `json:"text"`
	ClientMessageID string `json:"client_message_id"`
	ParentRequestID string `json:"parent_request_id,omitempty"`
}
type TurnAppendPayload struct {
	ClientMessageID string             `json:"client_message_id"`
	Text            string             `json:"text"`
	Question        *IntakeQuestionRef `json:"question,omitempty"`
}

// IntakeDraftPayload authorizes one bounded draft of the exact CURRENT saved
// context. Saving request text or turns never authorizes model dispatch.
type IntakeDraftPayload struct {
	Disclosure IntakeDisclosure `json:"disclosure"`
}
type DraftReplacePayload struct {
	Route             string `json:"route"`
	Format            string `json:"format"`
	CanonicalSource   string `json:"canonical_source"`
	BundleManifestSHA string `json:"bundle_manifest_sha,omitempty"`
}
type EmptyCommandPayload struct{}
type IntakeApprovePayload struct {
	DraftRevision  int    `json:"draft_revision"`
	SourceSHA      string `json:"source_sha"`
	DomainRevision string `json:"domain_revision"`
}
type SubjectCommandPayload struct {
	Kind     string `json:"kind"`
	ID       string `json:"id"`
	Revision string `json:"revision"`
}
type ProgramArtifactCommandPayload struct {
	ProgramID    string `json:"program_id"`
	Revision     string `json:"revision"`
	ArtifactID   string `json:"artifact_id"`
	ArtifactKind string `json:"artifact_kind"`
	ArtifactSHA  string `json:"artifact_sha"`
	SubjectSHA   string `json:"subject_sha"`
}
type ProgramAnswerPayload struct {
	ProgramID           string            `json:"program_id"`
	RevisionHash        string            `json:"revision_hash"`
	QuestionArtifactID  string            `json:"question_artifact_id"`
	QuestionArtifactSHA string            `json:"question_artifact_sha"`
	QuestionID          string            `json:"question_id"`
	Text                string            `json:"text"`
	SupportingRefs      []ProgramInputRef `json:"supporting_refs"`
}
type PlanningAnswerPayload struct {
	FeatureID       string `json:"feature_id"`
	FeatureRevision string `json:"feature_revision"`
	AttemptID       string `json:"attempt_id"`
	OutputSHA       string `json:"output_sha"`
	QuestionID      string `json:"question_id"`
	Text            string `json:"text"`
}
type ReviewRequest struct {
	Command TypedCommand `json:"command"`
}
type AuthorizationOptionsRequest struct {
	ReviewID string `json:"review_id"`
}
type AuthorizationCommitRequest struct {
	CommandID   string          `json:"command_id"`
	ChallengeID string          `json:"challenge_id"`
	Assertion   json.RawMessage `json:"assertion"`
}
type IntakeErrorBody struct {
	Code      string          `json:"code"`
	Message   string          `json:"message"`
	Retryable bool            `json:"retryable"`
	Current   json.RawMessage `json:"current,omitempty"`
}
type IntakeErrorResponse struct {
	Error IntakeErrorBody `json:"error"`
}
type IntakeBootstrap struct {
	Enabled        bool   `json:"enabled"`
	ExternalOrigin string `json:"external_origin"`
	PublicPath     string `json:"public_path"`
	RPName         string `json:"rp_name"`
}
type IntakeSessionView struct {
	InstallationID string   `json:"installation_id"`
	AuthGeneration string   `json:"auth_generation"`
	OperatorID     string   `json:"operator_id"`
	Label          string   `json:"label"`
	CredentialID   string   `json:"credential_id"`
	ProjectScope   []string `json:"project_scope"`
	CSRFToken      string   `json:"csrf_token"`
}
type IntakeProjectView struct {
	ID                string       `json:"id"`
	Checks            []string     `json:"checks"`
	CompletionTargets []string     `json:"completion_targets"`
	ModelProviderIDs  []string     `json:"model_provider_ids"`
	Limits            IntakeLimits `json:"limits"`
}
type IntakeRequestView struct {
	Request   IntakeRequest    `json:"request"`
	Questions []IntakeQuestion `json:"questions"`
	Draft     *IntakeDraft     `json:"draft,omitempty"`
	Steps     []IntakeStep     `json:"steps"`
	Links     []IntakeLink     `json:"links"`
}
type IntakeRequestPage struct {
	Items []IntakeRequest `json:"items"`
	Next  string          `json:"next,omitempty"`
}
type IntakeEventPage struct {
	Items []IntakeEvent `json:"items"`
	Next  int64         `json:"next,omitempty"`
}
type IntakeTurnPage struct {
	Items []IntakeTurn `json:"items"`
	Next  int64        `json:"next,omitempty"`
}
type IntakeSubjectView struct {
	Kind              string             `json:"kind"`
	ID                string             `json:"id"`
	Project           string             `json:"project"`
	Revision          string             `json:"revision"`
	Feature           *Feature           `json:"feature,omitempty"`
	Program           *Program           `json:"program,omitempty"`
	ProgramRevision   *ProgramRevision   `json:"program_revision,omitempty"`
	Answers           []QuestionAnswer   `json:"answers,omitempty"`
	PlanningQuestions []PlanningQuestion `json:"planning_questions,omitempty"`
	PlanningAnswers   []PlanningAnswer   `json:"planning_answers,omitempty"`
	SeedManifest      *ProgramBundle     `json:"seed_manifest,omitempty"`
	Artifacts         []ProgramArtifact  `json:"artifacts,omitempty"`
}
type BundleUploadFile struct {
	ID         string `json:"id"`
	Path       string `json:"path"`
	Bytes      int64  `json:"bytes"`
	ContentSHA string `json:"content_sha"`
}
type BundleUploadManifest struct {
	Project string             `json:"project"`
	Files   []BundleUploadFile `json:"files"`
}
type BundleUpload struct {
	ID         string               `json:"id"`
	OperatorID string               `json:"operator_id"`
	Manifest   BundleUploadManifest `json:"manifest"`
	State      string               `json:"state"`
	Bundle     *ProgramBundle       `json:"bundle,omitempty"`
}

// IntakeLimits is public metadata, intentionally excluding listener/auth config.
type IntakeLimits struct {
	MaxTurnBytes      int `json:"max_turn_bytes"`
	MaxTurns          int `json:"max_turns"`
	MaxTextBytes      int `json:"max_text_bytes"`
	MaxCanonicalBytes int `json:"max_canonical_bytes"`
	MaxOutputBytes    int `json:"max_output_bytes"`
	MaxDraftCalls     int `json:"max_draft_calls"`
	HistoryDefault    int `json:"history_default"`
	HistoryMax        int `json:"history_max"`
}
