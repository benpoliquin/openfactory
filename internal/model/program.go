package model

import (
	"context"
	"encoding/json"
	"time"
)

type ProgramArtifactKind string

const (
	ProgramDraft                    = "draft"
	ProgramActive                   = "active"
	ProgramWaiting                  = "waiting"
	ProgramBlocked                  = "blocked"
	ProgramProposed                 = "proposed"
	ProgramCancelled                = "cancelled"
	StepResearch                    = "research"
	StepArchitecture                = "architecture"
	StepPlanEvaluation              = "plan_evaluation"
	StepExperimentRun               = "experiment_run"
	StepResultEvaluation            = "result_evaluation"
	StepContracts                   = "contracts"
	StepContractEvaluation          = "contract_evaluation"
	ArtifactRequirements            = "requirements"
	ArtifactQuestions               = "questions"
	ArtifactClaims                  = "claims"
	ArtifactArchitecture            = "architecture"
	ArtifactExperimentContracts     = "experiment_contracts"
	ArtifactEvaluation              = "evaluation"
	ArtifactImplementationContracts = "implementation_contracts"
	ArtifactRevision                = "revision"
)

type Program struct {
	ID              string      `json:"id"`
	Project         string      `json:"project"`
	Title           string      `json:"title"`
	CurrentRevision int         `json:"current_revision"`
	State           string      `json:"state"`
	Phase           string      `json:"phase"`
	Iteration       int         `json:"iteration"`
	Wait            *WaitReason `json:"wait,omitempty"`
	CreatedAt       time.Time   `json:"created_at"`
	UpdatedAt       time.Time   `json:"updated_at"`
}
type ProgramRevision struct {
	ProgramID       string      `json:"program_id"`
	Number          int         `json:"number"`
	RevisionHash    string      `json:"revision_hash"`
	Spec            ProgramSpec `json:"spec"`
	SeedManifestSHA string      `json:"seed_manifest_sha"`
	CreatedAt       time.Time   `json:"created_at"`
	Supersedes      int         `json:"supersedes"`
	Reason          string      `json:"reason"`
}
type ProgramSpec struct {
	ID             string                      `json:"id" yaml:"id"`
	Project        string                      `json:"project" yaml:"project"`
	Title          string                      `json:"title" yaml:"title"`
	Objective      string                      `json:"objective" yaml:"objective"`
	Scope          []string                    `json:"scope" yaml:"scope"`
	NonGoals       []string                    `json:"non_goals" yaml:"non_goals"`
	Constraints    []string                    `json:"constraints" yaml:"constraints"`
	Seeds          []ProgramSeed               `json:"seeds" yaml:"seeds"`
	Disclosure     ProgramDisclosure           `json:"disclosure" yaml:"disclosure"`
	Limits         ProgramLimits               `json:"limits" yaml:"limits"`
	Implementation ProgramImplementationPolicy `json:"implementation" yaml:"implementation"`
}
type ProgramSeed struct {
	ID   string `json:"id" yaml:"id"`
	Kind string `json:"kind" yaml:"kind"`
	URL  string `json:"url,omitempty" yaml:"url,omitempty"`
	Path string `json:"path,omitempty" yaml:"path,omitempty"`
}
type ProgramDisclosure struct {
	ModelProviderIDs        []string             `json:"model_provider_ids" yaml:"model_provider_ids"`
	PublicSearchAllowed     bool                 `json:"public_search_allowed" yaml:"public_search_allowed"`
	PublicSearchTopics      []ProgramSearchTopic `json:"public_search_topics" yaml:"public_search_topics"`
	PrivateMaterialToModels bool                 `json:"private_material_to_models" yaml:"private_material_to_models"`
}
type ProgramSearchTopic struct {
	ID    string `json:"id" yaml:"id"`
	Query string `json:"query" yaml:"query"`
}
type ProgramLimits struct {
	MaxIterations        int   `json:"max_iterations" yaml:"max_iterations"`
	MaxModelCalls        int   `json:"max_model_calls" yaml:"max_model_calls"`
	MaxSteps             int   `json:"max_steps" yaml:"max_steps"`
	MaxExperiments       int   `json:"max_experiments" yaml:"max_experiments"`
	MaxSearches          int   `json:"max_searches" yaml:"max_searches"`
	MaxSources           int   `json:"max_sources" yaml:"max_sources"`
	MaxTotalCaptureBytes int64 `json:"max_total_capture_bytes" yaml:"max_total_capture_bytes"`
}
type ProgramImplementationPolicy struct {
	PermittedFeatureKinds      []string `json:"permitted_feature_kinds" yaml:"permitted_feature_kinds"`
	PermittedCompletionTargets []string `json:"permitted_completion_targets" yaml:"permitted_completion_targets"`
}
type ProgramEvidenceGate struct {
	ProgramID            string `json:"program_id" yaml:"program_id"`
	Revision             int    `json:"revision" yaml:"revision"`
	ManifestSHA          string `json:"manifest_sha" yaml:"manifest_sha"`
	EvaluationArtifactID string `json:"evaluation_artifact_id" yaml:"evaluation_artifact_id"`
}
type ProgramStep struct {
	ID                string    `json:"id"`
	ProgramID         string    `json:"program_id"`
	Revision          int       `json:"revision"`
	Iteration         int       `json:"iteration"`
	Kind              string    `json:"kind"`
	SubjectID         string    `json:"subject_id"`
	InputManifestSHA  string    `json:"input_manifest_sha"`
	FeatureID         string    `json:"feature_id"`
	State             string    `json:"state"`
	OutputManifestSHA string    `json:"output_manifest_sha"`
	Model             string    `json:"model"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
}
type ProgramStepSpec struct {
	ProgramID        string `json:"program_id"`
	Revision         int    `json:"revision"`
	Iteration        int    `json:"iteration"`
	Kind             string `json:"kind"`
	SubjectID        string `json:"subject_id"`
	InputManifestSHA string `json:"input_manifest_sha"`
	Model            string `json:"model,omitempty"`
}
type ProgramArtifact struct {
	ID                   string    `json:"id"`
	ProgramID            string    `json:"program_id"`
	Revision             int       `json:"revision"`
	Iteration            int       `json:"iteration"`
	Kind                 string    `json:"kind"`
	SchemaVersion        int       `json:"schema_version"`
	ContentSHA           string    `json:"content_sha"`
	InputManifestSHA     string    `json:"input_manifest_sha"`
	ProducerStepID       string    `json:"producer_step_id"`
	SupersedesID         string    `json:"supersedes_id,omitempty"`
	CreatedAt            time.Time `json:"created_at"`
	ProducerAttemptID    string    `json:"producer_attempt_id"`
	ProducerThreadID     string    `json:"producer_thread_id"`
	EvaluationVerdict    string    `json:"evaluation_verdict,omitempty"`
	EvaluatedManifestSHA string    `json:"evaluated_manifest_sha,omitempty"`
}
type ArtifactRef struct {
	ID  string `json:"id"`
	SHA string `json:"sha"`
}
type ProgramInputRef struct {
	Kind     string `json:"kind"`
	ID       string `json:"id"`
	SHA      string `json:"sha"`
	Revision int    `json:"revision,omitempty"`
}
type ProgramManifest struct {
	SHA    string            `json:"sha"`
	Inputs []ProgramInputRef `json:"inputs"`
}
type ProgramArtifactDraft struct {
	Kind         string          `json:"kind"`
	Payload      json.RawMessage `json:"payload"`
	SupersedesID string          `json:"supersedes_id,omitempty"`
}
type ProgramStepResult struct {
	Artifacts []ProgramArtifactDraft `json:"artifacts"`
	Outcome   string                 `json:"outcome"`
	Message   string                 `json:"message"`
}
type QuestionAnswer struct {
	ProgramID           string            `json:"program_id"`
	RevisionHash        string            `json:"revision_hash"`
	QuestionArtifactID  string            `json:"question_artifact_id"`
	QuestionArtifactSHA string            `json:"question_artifact_sha"`
	QuestionID          string            `json:"question_id"`
	Text                string            `json:"text"`
	Actor               string            `json:"actor"`
	Audit               AuditContext      `json:"audit,omitempty"`
	SupportingRefs      []ProgramInputRef `json:"supporting_refs"`
	CreatedAt           time.Time         `json:"created_at"`
}
type ProgramAnswer = QuestionAnswer

type RequirementsPayload struct {
	Requirements []ProgramRequirement `json:"requirements"`
}
type ProgramRequirement struct {
	ID           string       `json:"id"`
	Behavior     string       `json:"behavior"`
	Rationale    string       `json:"rationale"`
	Constraints  []string     `json:"constraints"`
	Acceptance   []Acceptance `json:"acceptance"`
	Priority     string       `json:"priority"`
	ClaimRefs    []string     `json:"claim_refs"`
	QuestionRefs []string     `json:"question_refs"`
}
type QuestionsPayload struct {
	Questions []ProgramQuestion `json:"questions"`
}
type ProgramQuestion struct {
	ID          string   `json:"id"`
	Text        string   `json:"text"`
	Decisive    bool     `json:"decisive"`
	AffectedIDs []string `json:"affected_ids"`
	Route       string   `json:"route"`
}
type ClaimsPayload struct {
	Claims []ProgramClaim `json:"claims"`
}
type ProgramClaim struct {
	ID                string           `json:"id"`
	Statement         string           `json:"statement"`
	Classification    string           `json:"classification"`
	Citations         []SourceCitation `json:"citations"`
	InferencePremises []string         `json:"inference_premises"`
	Scope             []string         `json:"scope"`
	Limitations       []string         `json:"limitations"`
}
type SourceCitation struct {
	SourceID        string `json:"source_id"`
	TextSHA         string `json:"text_sha"`
	Quote           string `json:"quote"`
	StartLine       int    `json:"start_line,omitempty"`
	EndLine         int    `json:"end_line,omitempty"`
	JSONPointer     string `json:"json_pointer,omitempty"`
	ExtractionBlock string `json:"extraction_block,omitempty"`
}
type ArchitecturePayload struct {
	Decisions []ArchitectureDecision `json:"decisions"`
}
type ArchitectureDecision struct {
	ID              string                    `json:"id"`
	Title           string                    `json:"title"`
	Alternatives    []ArchitectureAlternative `json:"alternatives"`
	Criteria        []string                  `json:"criteria"`
	Selected        string                    `json:"selected"`
	Rejected        []string                  `json:"rejected"`
	Tradeoffs       []string                  `json:"tradeoffs"`
	Consequences    []string                  `json:"consequences"`
	Assumptions     []string                  `json:"assumptions"`
	RequirementRefs []string                  `json:"requirement_refs"`
	QuestionRefs    []string                  `json:"question_refs"`
	ExperimentRefs  []string                  `json:"experiment_refs"`
}
type ArchitectureAlternative struct {
	ID          string `json:"id"`
	Description string `json:"description"`
}
type ExperimentContractsPayload struct {
	Experiments []ExperimentContract `json:"experiments"`
}
type EvaluationPayload struct {
	EvaluatedManifestSHA string               `json:"evaluated_manifest_sha"`
	Findings             []EvaluationFinding  `json:"findings"`
	Coverage             []EvaluationCoverage `json:"coverage"`
	Verdict              string               `json:"verdict"`
	RequiredChanges      []string             `json:"required_changes"`
}
type EvaluationFinding struct {
	ID       string            `json:"id"`
	Severity string            `json:"severity"`
	Detail   string            `json:"detail"`
	Refs     []ProgramInputRef `json:"refs"`
}
type EvaluationCoverage struct {
	SubjectID    string            `json:"subject_id"`
	Status       string            `json:"status"`
	EvidenceRefs []ProgramInputRef `json:"evidence_refs"`
}
type ImplementationContractsPayload struct {
	Contracts []ImplementationContract `json:"contracts"`
}
type ImplementationContract struct {
	Feature         Feature           `json:"feature"`
	RequirementRefs []string          `json:"requirement_refs"`
	DecisionRefs    []string          `json:"decision_refs"`
	EvidenceRefs    []ProgramInputRef `json:"evidence_refs"`
}
type RevisionPayload struct {
	Trigger                string            `json:"trigger"`
	FalsifiedEvidence      []ProgramInputRef `json:"falsified_evidence"`
	ChangedDecisions       []string          `json:"changed_decisions"`
	ChangedAssumptions     []string          `json:"changed_assumptions"`
	Materiality            string            `json:"materiality"`
	PredecessorManifestSHA string            `json:"predecessor_manifest_sha"`
}
type BlobRef struct {
	SHA   string `json:"sha"`
	Bytes int64  `json:"bytes"`
}
type ProgramBundle struct {
	ManifestSHA string              `json:"manifest_sha"`
	Files       []ProgramBundleFile `json:"files"`
}
type ProgramBundleFile struct {
	Path       string `json:"path"`
	ContentSHA string `json:"content_sha"`
	Bytes      int64  `json:"bytes"`
}
type BundleLimits struct {
	MaxFiles      int
	MaxFileBytes  int64
	MaxTotalBytes int64
}

type DiscoveryPolicy struct {
	Limits                 ProgramLimits                `json:"limits" yaml:"limits"`
	MaxCaptureBytes        int64                        `json:"max_capture_bytes" yaml:"max_capture_bytes"`
	MaxTextBytes           int64                        `json:"max_text_bytes" yaml:"max_text_bytes"`
	BundleMaxFiles         int                          `json:"bundle_max_files" yaml:"bundle_max_files"`
	BundleMaxFileBytes     int64                        `json:"bundle_max_file_bytes" yaml:"bundle_max_file_bytes"`
	BundleMaxTotalBytes    int64                        `json:"bundle_max_total_bytes" yaml:"bundle_max_total_bytes"`
	ResearchTimeoutSeconds int                          `json:"research_timeout_seconds" yaml:"research_timeout_seconds"`
	MaxRedirects           int                          `json:"max_redirects" yaml:"max_redirects"`
	SourceFreshnessSeconds int                          `json:"source_freshness_seconds" yaml:"source_freshness_seconds"`
	BrokerURL              string                       `json:"broker_url" yaml:"broker_url"`
	BrokerBind             string                       `json:"broker_bind" yaml:"broker_bind"`
	AllowNonLoopback       bool                         `json:"allow_non_loopback" yaml:"allow_non_loopback"`
	ExperimentLimits       ExperimentLimits             `json:"experiment_limits" yaml:"experiment_limits"`
	ExperimentProfiles     map[string]ExperimentProfile `json:"experiment_profiles" yaml:"experiment_profiles"`
}
type ProgramBudget struct {
	ModelCalls           int   `json:"model_calls"`
	Steps                int   `json:"steps"`
	Experiments          int   `json:"experiments"`
	Searches             int   `json:"searches"`
	Sources              int   `json:"sources"`
	CaptureBytes         int64 `json:"capture_bytes"`
	ReservedCaptureBytes int64 `json:"reserved_capture_bytes"`
}
type ProgramGrant struct {
	ProgramID    string       `json:"program_id"`
	RevisionHash string       `json:"revision_hash"`
	SubjectHash  string       `json:"subject_hash,omitempty"`
	PolicyHash   string       `json:"policy_hash"`
	Actor        string       `json:"actor"`
	Audit        AuditContext `json:"audit,omitempty"`
	CreatedAt    time.Time    `json:"created_at"`
}
type ProgramInvalidation struct {
	ID        string    `json:"id"`
	ProgramID string    `json:"program_id"`
	Subject   string    `json:"subject"`
	Reason    string    `json:"reason"`
	Actor     string    `json:"actor"`
	CreatedAt time.Time `json:"created_at"`
}
type ProgramWorkItem struct {
	Program  Program          `json:"program"`
	Revision ProgramRevision  `json:"revision"`
	Step     *ProgramStepSpec `json:"step,omitempty"`
	Manifest ProgramManifest  `json:"manifest"`
	Wait     *WaitReason      `json:"wait,omitempty"`
	Publish  bool             `json:"publish"`
}
type ProgramFeatureLink struct {
	ProgramID          string `json:"program_id"`
	Revision           int    `json:"revision"`
	FeatureID          string `json:"feature_id"`
	ContractArtifactID string `json:"contract_artifact_id"`
	ManifestSHA        string `json:"manifest_sha"`
}
type ProgramEvent struct {
	ID        string    `json:"id"`
	ProgramID string    `json:"program_id"`
	Kind      string    `json:"kind"`
	Detail    string    `json:"detail"`
	CreatedAt time.Time `json:"created_at"`
}
type ProgramError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *ProgramError) Error() string { return e.Code + ": " + e.Message }

type DiscoveryWorkspace struct {
	Path        string `json:"path"`
	ProjectRoot string `json:"project_root"`
	BaseSHA     string `json:"base_sha"`
}

// DiscoveryInputFile transports verified controller inputs into a private
// discovery workspace. It is never part of the model-visible control JSON.
type DiscoveryInputFile struct {
	Path       string
	ContentSHA string
	Data       []byte
}
type DiscoverySession struct {
	ProgramID         string               `json:"program_id"`
	Revision          int                  `json:"revision"`
	StepID            string               `json:"step_id"`
	AttemptID         string               `json:"attempt_id"`
	Role              string               `json:"role"`
	BrokerURL         string               `json:"broker_url"`
	CapabilityToken   string               `json:"capability_token"`
	Deadline          time.Time            `json:"deadline"`
	ReadOnly          bool                 `json:"read_only"`
	AllowedWritePaths []string             `json:"allowed_write_paths"`
	InputFiles        []DiscoveryInputFile `json:"-"`
}
type DiscoveryRepositories interface {
	PrepareDiscovery(context.Context, string, Project, string, string, string) (DiscoveryWorkspace, error)
	ConfigureDiscoverySession(context.Context, DiscoveryWorkspace, DiscoverySession) error
	FreezeDiscoveryBundle(context.Context, DiscoveryWorkspace, []string) (ProgramBundle, error)
	FingerprintDiscovery(context.Context, DiscoveryWorkspace) (string, error)
	MaterializeExperiment(context.Context, Attempt, ProgramBundle) (Workspace, error)
}
