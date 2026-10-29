// Package model defines OpenFactory's shared configuration and execution contracts.
package model

import (
	"context"
	"time"
)

type Command struct {
	Argv           []string `json:"argv" yaml:"argv"`
	TimeoutSeconds int      `json:"timeout_seconds,omitempty" yaml:"timeout_seconds,omitempty"`
}

type GitAuth struct {
	Mode           string `json:"mode" yaml:"mode"`
	TokenFile      string `json:"token_file,omitempty" yaml:"token_file,omitempty"`
	SSHKeyFile     string `json:"ssh_key_file,omitempty" yaml:"ssh_key_file,omitempty"`
	KnownHostsFile string `json:"known_hosts_file,omitempty" yaml:"known_hosts_file,omitempty"`
}

type Release struct {
	Allowed             bool      `json:"allowed" yaml:"allowed"`
	AllowedRisks        []string  `json:"allowed_risks" yaml:"allowed_risks"`
	AllowedPaths        []string  `json:"allowed_paths" yaml:"allowed_paths"`
	ExpiresAt           time.Time `json:"expires_at" yaml:"expires_at"`
	Deploy              Command   `json:"deploy" yaml:"deploy"`
	Observe             Command   `json:"observe" yaml:"observe"`
	Rollback            Command   `json:"rollback" yaml:"rollback"`
	ObservationSeconds  int       `json:"observation_seconds" yaml:"observation_seconds"`
	MinimumObservations int       `json:"minimum_observations" yaml:"minimum_observations"`
	Qualified           bool      `json:"qualified" yaml:"qualified"`
}

type Project struct {
	Repository       string             `json:"repository" yaml:"repository"`
	Branch           string             `json:"branch" yaml:"branch"`
	Trusted          bool               `json:"trusted" yaml:"trusted"`
	Auth             GitAuth            `json:"auth" yaml:"auth"`
	Checks           map[string]Command `json:"checks" yaml:"checks"`
	ProtectedPaths   []string           `json:"protected_paths" yaml:"protected_paths"`
	Releases         map[string]Release `json:"releases,omitempty" yaml:"releases,omitempty"`
	GitHubRepository string             `json:"github_repository,omitempty" yaml:"github_repository,omitempty"`
	MaxConcurrent    int                `json:"max_concurrent" yaml:"max_concurrent"`
}

type Config struct {
	StateDir              string             `json:"state_dir" yaml:"state_dir"`
	WorkspaceDir          string             `json:"workspace_dir" yaml:"workspace_dir"`
	CheckSocket           string             `json:"check_socket,omitempty" yaml:"check_socket,omitempty"`
	ReleaseDir            string             `json:"release_dir" yaml:"release_dir"`
	BBURL                 string             `json:"bb_url" yaml:"bb_url"`
	BBHostAuthority       string             `json:"bb_host_authority,omitempty" yaml:"bb_host_authority,omitempty"`
	BBHostID              string             `json:"bb_host_id,omitempty" yaml:"bb_host_id,omitempty"`
	AstraModel            string             `json:"astra_model" yaml:"astra_model"`
	FastModel             string             `json:"fast_model,omitempty" yaml:"fast_model,omitempty"`
	PollSeconds           int                `json:"poll_seconds" yaml:"poll_seconds"`
	MaxConcurrent         int                `json:"max_concurrent" yaml:"max_concurrent"`
	MaxAttempts           int                `json:"max_attempts" yaml:"max_attempts"`
	MaxCallsPerDay        int                `json:"max_calls_per_day" yaml:"max_calls_per_day"`
	AttemptTimeoutSeconds int                `json:"attempt_timeout_seconds" yaml:"attempt_timeout_seconds"`
	MaxPromptBytes        int                `json:"max_prompt_bytes" yaml:"max_prompt_bytes"`
	WebhookURL            string             `json:"webhook_url,omitempty" yaml:"webhook_url,omitempty"`
	WebhookSecretFile     string             `json:"webhook_secret_file,omitempty" yaml:"webhook_secret_file,omitempty"`
	GitName               string             `json:"git_name" yaml:"git_name"`
	GitEmail              string             `json:"git_email" yaml:"git_email"`
	Projects              map[string]Project `json:"projects" yaml:"projects"`
	Discovery             DiscoveryPolicy    `json:"discovery" yaml:"discovery"`
	Intake                IntakePolicy       `json:"intake" yaml:"intake"`
	PolicyHash            string             `json:"-" yaml:"-"`
}

type Acceptance struct {
	ID       string `json:"id" yaml:"id"`
	Scenario string `json:"scenario" yaml:"scenario"`
	Expected string `json:"expected" yaml:"expected"`
}

type Feature struct {
	ID                  string               `json:"id" yaml:"id"`
	Project             string               `json:"project" yaml:"project"`
	Title               string               `json:"title" yaml:"title"`
	Kind                string               `json:"kind" yaml:"kind"`
	Risk                string               `json:"risk" yaml:"risk"`
	Priority            int                  `json:"priority" yaml:"priority"`
	DependsOn           []string             `json:"depends_on" yaml:"depends_on"`
	CompletionTarget    string               `json:"completion_target" yaml:"completion_target"`
	Checks              []string             `json:"checks" yaml:"checks"`
	Acceptance          []Acceptance         `json:"acceptance" yaml:"acceptance"`
	Body                string               `json:"body" yaml:"-"`
	Revision            string               `json:"revision" yaml:"-"`
	PolicyHash          string               `json:"policy_hash" yaml:"-"`
	State               string               `json:"state" yaml:"-"`
	Reason              string               `json:"reason,omitempty" yaml:"-"`
	Attempts            int                  `json:"attempts" yaml:"-"`
	Receipt             string               `json:"receipt,omitempty" yaml:"-"`
	CreatedAt           time.Time            `json:"created_at" yaml:"-"`
	UpdatedAt           time.Time            `json:"updated_at" yaml:"-"`
	DiscoveryStepID     string               `json:"discovery_step_id,omitempty" yaml:"-"`
	IntakeStepID        string               `json:"intake_step_id,omitempty" yaml:"-"`
	ProgramEvidenceGate *ProgramEvidenceGate `json:"program_evidence_gate,omitempty" yaml:"program_evidence_gate,omitempty"`
}

type Attempt struct {
	ID                   string      `json:"id"`
	FeatureID            string      `json:"feature_id"`
	Stage                string      `json:"stage"`
	ThreadID             string      `json:"thread_id,omitempty"`
	EnvironmentID        string      `json:"environment_id,omitempty"`
	Cursor               string      `json:"cursor,omitempty"`
	Workspace            string      `json:"workspace"`
	BaseSHA              string      `json:"base_sha"`
	CandidateSHA         string      `json:"candidate_sha,omitempty"`
	Fingerprint          string      `json:"fingerprint,omitempty"`
	StartedAt            time.Time   `json:"started_at"`
	Deadline             time.Time   `json:"deadline"`
	Summary              string      `json:"summary,omitempty"`
	State                string      `json:"state"`
	CandidateGeneration  int         `json:"candidate_generation"`
	SourceBaseSHA        string      `json:"source_base_sha,omitempty"`
	SourceCandidateSHA   string      `json:"source_candidate_sha,omitempty"`
	IntegrationToken     string      `json:"integration_token,omitempty"`
	DispatchKey          string      `json:"dispatch_key,omitempty"`
	Wait                 *WaitReason `json:"wait,omitempty"`
	StageStartedAt       time.Time   `json:"stage_started_at"`
	UpdatedAt            time.Time   `json:"updated_at"`
	ReconcileOperationID string      `json:"reconcile_operation_id,omitempty"`
	ReconcileGeneration  int         `json:"reconcile_generation"`
}

type Evidence struct {
	ID                  string    `json:"id"`
	FeatureID           string    `json:"feature_id"`
	AttemptID           string    `json:"attempt_id"`
	Kind                string    `json:"kind"`
	CandidateSHA        string    `json:"candidate_sha"`
	Passed              bool      `json:"passed"`
	Detail              string    `json:"detail"`
	CandidateGeneration int       `json:"candidate_generation"`
	CreatedAt           time.Time `json:"created_at"`
}

type Event struct {
	ID        string    `json:"id"`
	FeatureID string    `json:"feature_id"`
	Kind      string    `json:"kind"`
	Detail    string    `json:"detail"`
	CreatedAt time.Time `json:"created_at"`
}

type SpawnRequest struct {
	AttemptID     string
	ProjectID     string
	Workspace     string
	EnvironmentID string
	Model         string
	Prompt        string
}

type Session struct {
	ThreadID      string
	EnvironmentID string
}

type Observation struct {
	// State is running, completed, failed, interrupted, or blocked. Unknown is never success.
	State  string
	Cursor string
	Output string
	Reason string
}

type Backend interface {
	Doctor(context.Context) error
	EnsureProject(context.Context, string, string) (string, error)
	Spawn(context.Context, SpawnRequest) (Session, error)
	Poll(context.Context, string, string) (Observation, error)
	Find(context.Context, string, string) ([]Session, error)
	Stop(context.Context, string) error
}

type Workspace struct{ Path, BaseSHA string }

type Repositories interface {
	Prepare(context.Context, string, Project, string) (Workspace, error)
	Reconcile(context.Context, string, Project, ReconcileRequest) (ReconciledCandidate, error)
	Fingerprint(context.Context, string) (string, error)
	Commit(context.Context, string, string) (string, error)
	Snapshot(context.Context, string, string) (Workspace, error)
	Check(context.Context, string, Command) (string, error)
	ChangedPaths(context.Context, string, string, string) ([]string, error)
	Integrate(context.Context, string, Project, Attempt, string) (string, error)
	PullRequest(context.Context, string, Project, Attempt, Feature) (string, error)
	PreflightRelease(context.Context, string, Project, Attempt, Feature) error
	Release(context.Context, string, Project, Attempt, Feature) ([]Evidence, error)
}

type WaitReason struct {
	Code      string `json:"code"`
	Detail    string `json:"detail,omitempty"`
	FeatureID string `json:"feature_id,omitempty"`
	Resource  string `json:"resource,omitempty"`
	OwnerID   string `json:"owner_attempt_id,omitempty"`
}

type IntegrationLease struct {
	AttemptID string   `json:"attempt_id"`
	Token     string   `json:"token"`
	Resources []string `json:"resources"`
	Sequence  int64    `json:"sequence"`
}

type IntegrationStatus struct {
	Resources         []string `json:"resources"`
	State             string   `json:"state"`
	Position          int      `json:"position"`
	OwnerAttemptID    string   `json:"owner_attempt_id,omitempty"`
	ContendedResource string   `json:"contended_resource,omitempty"`
	Token             string   `json:"-"`
	Sequence          int64    `json:"sequence"`
}

type ReconcileRequest struct {
	OperationID       string
	Generation        int
	Attempt           Attempt
	Feature           Feature
	RequiredAncestors []string
}

type ReconciledCandidate struct {
	Workspace    Workspace
	CandidateSHA string
	Fingerprint  string
	Changed      bool
}

type DeliveryError struct {
	Kind        string
	Message     string
	ObservedSHA string
	Paths       []string
}

func (e *DeliveryError) Error() string   { return e.Message }
func (e *DeliveryError) Uncertain() bool { return e.Kind == "uncertain" }
