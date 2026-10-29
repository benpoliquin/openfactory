package model

import (
	"context"
	"encoding/json"
	"time"
)

const (
	IntakeCollecting      = "collecting"
	IntakeDraftQueued     = "draft_queued"
	IntakeDrafting        = "drafting"
	IntakeNeedsAnswers    = "needs_answers"
	IntakeReviewReady     = "review_ready"
	IntakeBlocked         = "blocked"
	IntakeSubmitted       = "submitted"
	IntakeCancelling      = "cancelling"
	IntakeCancelled       = "cancelled"
	IntakeFeatureKind     = "intake"
	IntakeFeaturePrefix   = "openfactory-intake-"
	IntakeRouteFeature    = "feature"
	IntakeRoutePlan       = "plan"
	IntakeRouteProgram    = "program"
	IntakeFormatFeature   = "feature_markdown"
	IntakeFormatProgram   = "program_json"
	AuditTrustedLocal     = "trusted_local"
	AuditVerifiedOperator = "verified_operator"
)

// IntakePolicy contains operator-owned limits only, never live credentials.
type IntakePolicy struct {
	Enabled                bool   `json:"enabled" yaml:"enabled"`
	ExternalOrigin         string `json:"external_origin" yaml:"external_origin"`
	RPID                   string `json:"rp_id" yaml:"rp_id"`
	RPName                 string `json:"rp_name" yaml:"rp_name"`
	PublicPath             string `json:"public_path" yaml:"public_path"`
	Bind                   string `json:"bind" yaml:"bind"`
	AllowNonLoopback       bool   `json:"allow_non_loopback" yaml:"allow_non_loopback"`
	MaxTurnBytes           int    `json:"max_turn_bytes" yaml:"max_turn_bytes"`
	MaxTurns               int    `json:"max_turns" yaml:"max_turns"`
	MaxTextBytes           int    `json:"max_text_bytes" yaml:"max_text_bytes"`
	MaxCanonicalBytes      int    `json:"max_canonical_bytes" yaml:"max_canonical_bytes"`
	MaxOutputBytes         int    `json:"max_output_bytes" yaml:"max_output_bytes"`
	MaxDraftCalls          int    `json:"max_draft_calls" yaml:"max_draft_calls"`
	HistoryDefault         int    `json:"history_default" yaml:"history_default"`
	HistoryMax             int    `json:"history_max" yaml:"history_max"`
	PairingTTLSeconds      int    `json:"pairing_ttl_seconds" yaml:"pairing_ttl_seconds"`
	ChallengeTTLSeconds    int    `json:"challenge_ttl_seconds" yaml:"challenge_ttl_seconds"`
	SessionIdleSeconds     int    `json:"session_idle_seconds" yaml:"session_idle_seconds"`
	SessionAbsoluteSeconds int    `json:"session_absolute_seconds" yaml:"session_absolute_seconds"`
	CredentialTTLSeconds   int    `json:"credential_ttl_seconds" yaml:"credential_ttl_seconds"`
	MaxCredentials         int    `json:"max_credentials" yaml:"max_credentials"`
	AuthRateLimit          int    `json:"auth_rate_limit" yaml:"auth_rate_limit"`
}

// AuditContext is constructed by trusted controller code, never decoded from a command.
type AuditContext struct {
	Kind         string `json:"kind"`
	OperatorID   string `json:"operator_id,omitempty"`
	CredentialID string `json:"credential_id,omitempty"`
	CommandID    string `json:"command_id,omitempty"`
	ApprovalID   string `json:"approval_id,omitempty"`
}

func TrustedLocalAudit() AuditContext { return AuditContext{Kind: AuditTrustedLocal} }

type IntakeRequest struct {
	ID                  string         `json:"id"`
	OperatorID          string         `json:"operator_id"`
	Project             string         `json:"project"`
	State               string         `json:"state"`
	ContentVersion      int            `json:"content_version"`
	LatestDraftRevision int            `json:"latest_draft_revision"`
	ActiveStepID        string         `json:"active_step_id,omitempty"`
	CreationPolicySHA   string         `json:"creation_policy_sha"`
	LifetimeDispatches  int            `json:"lifetime_dispatches"`
	MaxDraftDispatches  int            `json:"max_draft_dispatches"`
	ContextManifest     IntakeManifest `json:"context_manifest"`
	EventSequence       int64          `json:"event_sequence"`
	ParentRequestID     string         `json:"parent_request_id,omitempty"`
	Wait                *WaitReason    `json:"wait,omitempty"`
	CreatedAt           time.Time      `json:"created_at"`
	UpdatedAt           time.Time      `json:"updated_at"`
}
type IntakeDisclosure struct {
	ContextSHA              string   `json:"context_sha"`
	PolicySHA               string   `json:"policy_sha"`
	ModelProviderIDs        []string `json:"model_provider_ids"`
	BundleManifestSHAs      []string `json:"bundle_manifest_shas,omitempty"`
	PrivateMaterialToModels bool     `json:"private_material_to_models"`
}
type IntakeQuestionRef struct {
	QuestionID       string `json:"question_id"`
	ContentVersion   int    `json:"content_version"`
	InputManifestSHA string `json:"input_manifest_sha"`
	OutputSHA        string `json:"output_sha"`
}
type IntakeTurn struct {
	RequestID       string             `json:"request_id"`
	Sequence        int64              `json:"sequence"`
	ClientMessageID string             `json:"client_message_id,omitempty"`
	Role            string             `json:"role"`
	Text            string             `json:"text"`
	ProducerStepID  string             `json:"producer_step_id,omitempty"`
	Audit           AuditContext       `json:"audit"`
	Question        *IntakeQuestionRef `json:"question,omitempty"`
	Disclosure      *IntakeDisclosure  `json:"disclosure,omitempty"`
	CreatedAt       time.Time          `json:"created_at"`
}
type IntakeQuestion struct {
	ID                 string   `json:"id"`
	RequestID          string   `json:"request_id"`
	ContentVersion     int      `json:"content_version"`
	InputManifestSHA   string   `json:"input_manifest_sha"`
	OutputSHA          string   `json:"output_sha"`
	LocalID            string   `json:"local_id"`
	Text               string   `json:"text"`
	Consequence        string   `json:"consequence"`
	AnswerType         string   `json:"answer_type"`
	Choices            []string `json:"choices,omitempty"`
	AnsweredBySequence int64    `json:"answered_by_sequence,omitempty"`
}
type IntakeFinding struct {
	Code     string `json:"code"`
	Severity string `json:"severity"`
	Detail   string `json:"detail"`
}
type IntakeDraft struct {
	RequestID         string          `json:"request_id"`
	Revision          int             `json:"revision"`
	ContentVersion    int             `json:"content_version"`
	Route             string          `json:"route"`
	Format            string          `json:"format"`
	CanonicalSource   string          `json:"canonical_source"`
	SourceSHA         string          `json:"source_sha"`
	CanonicalSHA      string          `json:"canonical_sha"`
	DomainRevision    string          `json:"domain_revision"`
	InputManifestSHA  string          `json:"input_manifest_sha"`
	BundleManifestSHA string          `json:"bundle_manifest_sha,omitempty"`
	OutputSHA         string          `json:"output_sha"`
	PolicySHA         string          `json:"policy_sha"`
	StepID            string          `json:"step_id,omitempty"`
	Assumptions       []string        `json:"assumptions"`
	UnresolvedRisks   []string        `json:"unresolved_risks"`
	Findings          []IntakeFinding `json:"findings"`
	CreatedAt         time.Time       `json:"created_at"`
}
type IntakeInputRef struct {
	Kind  string `json:"kind"`
	ID    string `json:"id"`
	SHA   string `json:"sha"`
	Bytes int64  `json:"bytes,omitempty"`
}
type IntakeManifest struct {
	SHA            string           `json:"sha"`
	RequestID      string           `json:"request_id"`
	ContentVersion int              `json:"content_version"`
	Project        string           `json:"project"`
	PolicySHA      string           `json:"policy_sha"`
	Inputs         []IntakeInputRef `json:"inputs"`
	Disclosure     IntakeDisclosure `json:"disclosure"`
}
type IntakeStep struct {
	ID               string         `json:"id"`
	RequestID        string         `json:"request_id"`
	ContentVersion   int            `json:"content_version"`
	FeatureID        string         `json:"feature_id"`
	InputManifestSHA string         `json:"input_manifest_sha"`
	PolicySHA        string         `json:"policy_sha"`
	Superseded       bool           `json:"superseded"`
	Manifest         IntakeManifest `json:"manifest"`
	Model            string         `json:"model"`
	State            string         `json:"state"`
	AttemptID        string         `json:"attempt_id,omitempty"`
	ThreadID         string         `json:"thread_id,omitempty"`
	OutputSHA        string         `json:"output_sha,omitempty"`
	CreatedAt        time.Time      `json:"created_at"`
	UpdatedAt        time.Time      `json:"updated_at"`
}
type IntakeLink struct {
	RequestID       string    `json:"request_id"`
	DraftRevision   int       `json:"draft_revision"`
	SubjectKind     string    `json:"subject_kind"`
	SubjectID       string    `json:"subject_id"`
	SubjectRevision string    `json:"subject_revision"`
	CommandID       string    `json:"command_id"`
	CreatedAt       time.Time `json:"created_at"`
}
type IntakeEvent struct {
	RequestID string    `json:"request_id"`
	Sequence  int64     `json:"sequence"`
	Kind      string    `json:"kind"`
	Detail    string    `json:"detail"`
	CreatedAt time.Time `json:"created_at"`
}

// Auth credential/session payloads are opaque library serializations. Model has no crypto dependency.
type Operator struct {
	ID           string     `json:"id"`
	Label        string     `json:"label"`
	Enabled      bool       `json:"enabled"`
	ProjectScope []string   `json:"project_scope"`
	UserHandle   []byte     `json:"user_handle"`
	RevokedAt    *time.Time `json:"revoked_at,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
}
type OperatorCredential struct {
	ID             string          `json:"id"`
	OperatorID     string          `json:"operator_id"`
	CredentialData json.RawMessage `json:"credential_data"`
	PublicKey      []byte          `json:"public_key,omitempty"`
	UserHandle     []byte          `json:"user_handle,omitempty"`
	SignCount      uint32          `json:"sign_count"`
	BackupEligible bool            `json:"backup_eligible"`
	BackupState    bool            `json:"backup_state"`
	Fingerprint    string          `json:"fingerprint"`
	CreatedAt      time.Time       `json:"created_at"`
	ExpiresAt      time.Time       `json:"expires_at"`
	RevokedAt      *time.Time      `json:"revoked_at,omitempty"`
	LastUsedAt     time.Time       `json:"last_used_at"`
}
type OperatorPairing struct {
	ID                string              `json:"id"`
	SecretHash        string              `json:"secret_hash"`
	OperatorID        string              `json:"operator_id"`
	ProjectScope      []string            `json:"project_scope"`
	MaxCredentials    int                 `json:"max_credentials"`
	State             string              `json:"state"`
	PendingCredential *OperatorCredential `json:"pending_credential,omitempty"`
	AuthGeneration    string              `json:"auth_generation"`
	CreatedAt         time.Time           `json:"created_at"`
	ExpiresAt         time.Time           `json:"expires_at"`
}
type BrowserSession struct {
	ID                string     `json:"id"`
	TokenHash         string     `json:"token_hash"`
	OperatorID        string     `json:"operator_id"`
	CredentialID      string     `json:"credential_id"`
	CSRFHash          string     `json:"csrf_hash"`
	AuthGeneration    string     `json:"auth_generation"`
	CreatedAt         time.Time  `json:"created_at"`
	LastUsedAt        time.Time  `json:"last_used_at"`
	ExpiresAt         time.Time  `json:"expires_at"`
	AbsoluteExpiresAt time.Time  `json:"absolute_expires_at"`
	RevokedAt         *time.Time `json:"revoked_at,omitempty"`
}
type AuthChallenge struct {
	ID             string          `json:"id"`
	Purpose        string          `json:"purpose"`
	OperatorID     string          `json:"operator_id,omitempty"`
	CredentialID   string          `json:"credential_id,omitempty"`
	SessionID      string          `json:"session_id,omitempty"`
	PairingID      string          `json:"pairing_id,omitempty"`
	CommandID      string          `json:"command_id,omitempty"`
	CommandDigest  string          `json:"command_digest,omitempty"`
	ClaimDigest    string          `json:"claim_digest,omitempty"`
	ReviewID       string          `json:"review_id,omitempty"`
	SessionData    json.RawMessage `json:"session_data"`
	OptionsData    json.RawMessage `json:"options_data,omitempty"`
	AuthGeneration string          `json:"auth_generation"`
	CreatedAt      time.Time       `json:"created_at"`
	ExpiresAt      time.Time       `json:"expires_at"`
	ConsumedAt     *time.Time      `json:"consumed_at,omitempty"`
}
type ReviewClaims struct {
	Version             int               `json:"version"`
	InstallationID      string            `json:"installation_id"`
	OperatorID          string            `json:"operator_id"`
	CredentialID        string            `json:"credential_id"`
	Action              string            `json:"action"`
	RequestID           string            `json:"request_id,omitempty"`
	ContentVersion      int               `json:"content_version,omitempty"`
	DraftRevision       int               `json:"draft_revision,omitempty"`
	SubjectKind         string            `json:"subject_kind"`
	SubjectID           string            `json:"subject_id"`
	SubjectRevision     string            `json:"subject_revision"`
	CanonicalPayloadSHA string            `json:"canonical_payload_sha"`
	ReviewManifestSHA   string            `json:"review_manifest_sha"`
	PolicySHA           string            `json:"policy_sha"`
	ExecutionPolicySHA  string            `json:"execution_policy_sha,omitempty"`
	InputManifestSHA    string            `json:"input_manifest_sha,omitempty"`
	Evidence            []ProgramInputRef `json:"evidence"`
	BundleManifestSHAs  []string          `json:"bundle_manifest_shas"`
	CommandID           string            `json:"command_id"`
	ChallengeID         string            `json:"challenge_id"`
	IssuedAt            time.Time         `json:"issued_at"`
	ExpiresAt           time.Time         `json:"expires_at"`
}
type IntakeReview struct {
	ID        string       `json:"id"`
	Claims    ReviewClaims `json:"claims"`
	ClaimsSHA string       `json:"claims_sha"`
	// Canonical material contains the exact UTF-8 bytes used for the bound
	// hashes; browser review must not reconstruct Go JSON from decoded objects.
	ClaimsCanonical           string             `json:"claims_canonical"`
	ManifestCanonical         string             `json:"manifest_canonical"`
	CanonicalPayload          string             `json:"canonical_payload"`
	CommandCanonical          string             `json:"command_canonical"`
	SelectedArtifactCanonical string             `json:"selected_artifact_canonical,omitempty"`
	Command                   TypedCommand       `json:"command"`
	CanonicalSource           string             `json:"canonical_source"`
	Feature                   *Feature           `json:"feature,omitempty"`
	ProgramRevision           *ProgramRevision   `json:"program_revision,omitempty"`
	Answers                   []QuestionAnswer   `json:"answers,omitempty"`
	SeedManifest              *ProgramBundle     `json:"seed_manifest,omitempty"`
	Artifacts                 []ProgramArtifact  `json:"artifacts,omitempty"`
	ResumeWork                *ProgramResumeWork `json:"resume_work,omitempty"`
	Findings                  []IntakeFinding    `json:"findings"`
	CreatedAt                 time.Time          `json:"created_at"`
}

// ProgramResumeWork binds a resume review to one current, settled failed step.
// It exposes no raw worker output or workspace paths. AttemptActive is explicit:
// retry authority must never be inferred from an omitted ownership marker.
type ProgramResumeWork struct {
	StepID                  string    `json:"step_id"`
	StepRevision            int       `json:"step_revision"`
	StepKind                string    `json:"step_kind"`
	StepState               string    `json:"step_state"`
	Iteration               int       `json:"iteration"`
	InputManifestSHA        string    `json:"input_manifest_sha"`
	SubjectID               string    `json:"subject_id,omitempty"`
	ExecutionPolicySHA      string    `json:"execution_policy_sha,omitempty"`
	BundleManifestSHA       string    `json:"bundle_manifest_sha,omitempty"`
	PriorLaunchAbsent       bool      `json:"prior_launch_absent,omitempty"`
	PriorRunID              string    `json:"prior_run_id,omitempty"`
	PriorRequestSHA         string    `json:"prior_request_sha,omitempty"`
	PriorReceiptSHA         string    `json:"prior_receipt_sha,omitempty"`
	PriorReceiptState       string    `json:"prior_receipt_state,omitempty"`
	PriorReceiptConclusive  bool      `json:"prior_receipt_conclusive,omitempty"`
	PriorExecutionOutcome   string    `json:"prior_execution_outcome,omitempty"`
	PriorObservationOutcome string    `json:"prior_observation_outcome,omitempty"`
	PriorHypothesisOutcome  string    `json:"prior_hypothesis_outcome,omitempty"`
	FeatureID               string    `json:"feature_id"`
	FeatureRevision         string    `json:"feature_revision"`
	FeatureState            string    `json:"feature_state"`
	FeatureAttempts         int       `json:"feature_attempts"`
	FeatureMaxAttempts      int       `json:"feature_max_attempts"`
	AttemptID               string    `json:"attempt_id"`
	AttemptState            string    `json:"attempt_state"`
	AttemptStage            string    `json:"attempt_stage"`
	AttemptActive           bool      `json:"attempt_active"`
	AttemptOutputSHA        string    `json:"attempt_output_sha"`
	AttemptUpdatedAt        time.Time `json:"attempt_updated_at"`
}
type ApprovalReceipt struct {
	ID        string           `json:"id"`
	ReviewID  string           `json:"review_id"`
	Claims    ReviewClaims     `json:"claims"`
	ClaimsSHA string           `json:"claims_sha"`
	Audit     AuditContext     `json:"audit"`
	Evidence  WebAuthnEvidence `json:"evidence"`
	CreatedAt time.Time        `json:"created_at"`
}
type WebAuthnEvidence struct {
	ChallengeID  string    `json:"challenge_id"`
	CredentialID string    `json:"credential_id"`
	UserPresent  bool      `json:"user_present"`
	UserVerified bool      `json:"user_verified"`
	AssertionSHA string    `json:"assertion_sha"`
	SignCount    uint32    `json:"sign_count"`
	VerifiedAt   time.Time `json:"verified_at"`
}
type CommandReceipt struct {
	OperatorID string          `json:"operator_id"`
	CommandID  string          `json:"command_id"`
	Digest     string          `json:"digest"`
	Action     string          `json:"action"`
	State      string          `json:"state"`
	Result     json.RawMessage `json:"result"`
	Audit      AuditContext    `json:"audit"`
	ApprovalID string          `json:"approval_id,omitempty"`
	CreatedAt  time.Time       `json:"created_at"`
}

// IntakeModelOutput is decoded as a strict discriminated union by intake.ParseModelOutput.
type IntakeModelOutput struct {
	Type                 string                `json:"type"`
	Message              string                `json:"message"`
	Questions            []IntakeModelQuestion `json:"questions,omitempty"`
	Route                string                `json:"route,omitempty"`
	Format               string                `json:"format,omitempty"`
	CanonicalSource      string                `json:"canonical_source,omitempty"`
	Assumptions          []string              `json:"assumptions,omitempty"`
	UnresolvedRisks      []string              `json:"unresolved_risks,omitempty"`
	MissingPrerequisites []string              `json:"missing_prerequisites,omitempty"`
}
type IntakeModelQuestion struct {
	LocalID     string   `json:"local_id"`
	Text        string   `json:"text"`
	Consequence string   `json:"consequence"`
	AnswerType  string   `json:"answer_type"`
	Choices     []string `json:"choices,omitempty"`
}
type PlanningQuestion struct {
	ID              string    `json:"id"`
	FeatureID       string    `json:"feature_id"`
	FeatureRevision string    `json:"feature_revision"`
	AttemptID       string    `json:"attempt_id"`
	OutputSHA       string    `json:"output_sha"`
	Question        string    `json:"question"`
	CreatedAt       time.Time `json:"created_at"`
}
type PlanningAnswer struct {
	Question  PlanningQuestion `json:"question"`
	Text      string           `json:"text"`
	Audit     AuditContext     `json:"audit"`
	CreatedAt time.Time        `json:"created_at"`
}

type IntakeWorkspace struct{ Path, ProjectRoot, BaseSHA string }
type IntakeSession struct {
	Project    string            `json:"project"`
	RequestID  string            `json:"request_id"`
	StepID     string            `json:"step_id"`
	AttemptID  string            `json:"attempt_id"`
	Deadline   time.Time         `json:"deadline"`
	InputFiles map[string][]byte `json:"-"`
}
type IntakeRepositories interface {
	PrepareIntake(context.Context, string, Project, string, string, string) (IntakeWorkspace, error)
	// ReopenIntake verifies persisted private ownership for reconciliation without
	// applying changed enrollment policy as permission to abandon live work.
	ReopenIntake(context.Context, string, string, string, string, string) (IntakeWorkspace, error)
	ConfigureIntakeSession(context.Context, IntakeWorkspace, IntakeSession) error
	FingerprintIntake(context.Context, IntakeWorkspace) (string, error)
}
