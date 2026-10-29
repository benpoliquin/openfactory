package model

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"
)

type ExperimentContract struct {
	ID                string                      `json:"id"`
	QuestionRefs      []string                    `json:"question_refs"`
	DecisionRefs      []string                    `json:"decision_refs"`
	Hypothesis        string                      `json:"hypothesis"`
	Falsification     string                      `json:"falsification"`
	Environment       ExperimentEnvironment       `json:"environment"`
	BundleManifestSHA string                      `json:"bundle_manifest_sha"`
	Commands          []ExperimentCommand         `json:"commands"`
	Observations      []ExperimentObservationSpec `json:"observations"`
	Acceptance        []ExperimentPredicate       `json:"acceptance"`
	Limits            ExperimentLimits            `json:"limits"`
	ExternalAuthority ExperimentExternalAuthority `json:"external_authority"`
	Limitations       []string                    `json:"limitations"`
	BundleFiles       []string                    `json:"bundle_files"`
}
type ExperimentEnvironment struct {
	Profile       string            `json:"profile"`
	ImageIdentity string            `json:"image_identity"`
	Tools         []string          `json:"tools"`
	Prerequisites []string          `json:"prerequisites"`
	DatasetRefs   []ProgramInputRef `json:"dataset_refs"`
}
type ExperimentCommand struct {
	Profile string   `json:"profile"`
	Args    []string `json:"args"`
}
type ExperimentObservationSpec struct {
	Name        string `json:"name"`
	Type        string `json:"type"`
	Unit        string `json:"unit"`
	OutputPath  string `json:"output_path"`
	JSONPointer string `json:"json_pointer,omitempty"`
}
type ExperimentPredicate struct {
	Observation string          `json:"observation"`
	Operator    string          `json:"operator"`
	Value       json.RawMessage `json:"value"`
	Unit        string          `json:"unit,omitempty"`
}
type ExperimentLimits struct {
	WallSeconds         int    `json:"wall_seconds" yaml:"wall_seconds"`
	MaxOutputBytes      int64  `json:"max_output_bytes" yaml:"max_output_bytes"`
	MaxArtifactBytes    int64  `json:"max_artifact_bytes" yaml:"max_artifact_bytes"`
	MaxArtifactFiles    int    `json:"max_artifact_files" yaml:"max_artifact_files"`
	ResourceDescription string `json:"resource_description" yaml:"resource_description"`
}
type ExperimentExternalAuthority struct {
	Network     bool     `json:"network"`
	Accounts    []string `json:"accounts"`
	Credentials []string `json:"credentials"`
	Hardware    []string `json:"hardware"`
	Spend       float64  `json:"spend"`
}
type ExperimentProfile struct {
	Argv          []string `json:"argv" yaml:"argv"`
	AllowedArgs   []string `json:"allowed_args" yaml:"allowed_args"`
	ImageIdentity string   `json:"image_identity" yaml:"image_identity"`
	Network       bool     `json:"network" yaml:"network"`
	Hardware      []string `json:"hardware" yaml:"hardware"`
}
type ExperimentRunRequest struct {
	RunID              string             `json:"run_id"`
	RequestSHA         string             `json:"request_sha"`
	ExecutionPolicySHA string             `json:"execution_policy_sha"`
	ProgramID          string             `json:"program_id"`
	Revision           int                `json:"revision"`
	StepID             string             `json:"step_id"`
	AttemptID          string             `json:"attempt_id"`
	ContractSHA        string             `json:"contract_sha"`
	Contract           ExperimentContract `json:"contract"`
	Bundle             ProgramBundle      `json:"bundle"`
	Ordinal            int                `json:"ordinal"`
}
type ExperimentReceipt struct {
	RunID                 string                     `json:"run_id"`
	RequestSHA            string                     `json:"request_sha"`
	ProgramID             string                     `json:"program_id"`
	Revision              int                        `json:"revision"`
	StepID                string                     `json:"step_id"`
	AttemptID             string                     `json:"attempt_id"`
	ContractSHA           string                     `json:"contract_sha"`
	BundleManifestSHA     string                     `json:"bundle_manifest_sha"`
	State                 string                     `json:"state"`
	StartedAt             time.Time                  `json:"started_at"`
	FinishedAt            *time.Time                 `json:"finished_at,omitempty"`
	Commands              []ExperimentCommandReceipt `json:"commands"`
	Artifacts             ProgramBundle              `json:"artifacts"`
	Observations          []ExperimentObservation    `json:"observations"`
	Accepted              bool                       `json:"accepted"`
	Conclusive            bool                       `json:"conclusive"`
	FailureKind           string                     `json:"failure_kind,omitempty"`
	Detail                string                     `json:"detail,omitempty"`
	DeclaredImageIdentity string                     `json:"declared_image_identity"`
	AttestationScope      string                     `json:"attestation_scope"`
	ResolutionAttestation string                     `json:"resolution_attestation,omitempty"`
	ExecutionOutcome      string                     `json:"execution_outcome"`
	ObservationOutcome    string                     `json:"observation_outcome"`
	HypothesisOutcome     string                     `json:"hypothesis_outcome"`
}
type ExperimentCommandReceipt struct {
	Argv                []string `json:"argv"`
	BinarySHA           string   `json:"binary_sha"`
	ExitCode            *int     `json:"exit_code,omitempty"`
	Signal              string   `json:"signal,omitempty"`
	Termination         string   `json:"termination"`
	StdoutSHA           string   `json:"stdout_sha"`
	StderrSHA           string   `json:"stderr_sha"`
	StdoutBytes         int64    `json:"stdout_bytes"`
	StderrBytes         int64    `json:"stderr_bytes"`
	StdoutObservedBytes int64    `json:"stdout_observed_bytes"`
	StderrObservedBytes int64    `json:"stderr_observed_bytes"`
	StdoutTruncated     bool     `json:"stdout_truncated"`
	StderrTruncated     bool     `json:"stderr_truncated"`
}
type ExperimentObservation struct {
	Name       string          `json:"name"`
	Type       string          `json:"type"`
	Unit       string          `json:"unit"`
	Value      json.RawMessage `json:"value"`
	SourcePath string          `json:"source_path"`
	SourceSHA  string          `json:"source_sha"`
}
type ExperimentRunner interface {
	Start(context.Context, ExperimentRunRequest) (ExperimentReceipt, error)
	Status(context.Context, string) (ExperimentReceipt, error)
	Cancel(context.Context, string) (ExperimentReceipt, error)
	CancelIntent(context.Context, ExperimentRunRequest) (ExperimentReceipt, error)
	ResolveFailed(context.Context, string, string, string) (ExperimentReceipt, error)
}

// ExperimentPolicySHA binds a launch to the executor's enrolled profiles and
// execution ceilings. It excludes unrelated controller policy and secrets.
func ExperimentPolicySHA(profiles map[string]ExperimentProfile, limits ExperimentLimits) string {
	data := experimentPolicyCanonical(profiles, limits)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// experimentPolicyCanonical keeps the historical hash material private: enrolled
// profiles may contain host paths and argv unsuitable for browser disclosure.
func experimentPolicyCanonical(profiles map[string]ExperimentProfile, limits ExperimentLimits) []byte {
	if profiles == nil {
		profiles = map[string]ExperimentProfile{}
	}
	limits = NormalizeDiscoveryPolicy(DiscoveryPolicy{ExperimentLimits: limits}).ExperimentLimits
	data, _ := json.Marshal(struct {
		Profiles map[string]ExperimentProfile `json:"profiles"`
		Limits   ExperimentLimits             `json:"limits"`
	}{profiles, limits})
	return data
}

// ExperimentContractCanonical returns the existing frozen contract hash preimage
// without reordering fields or changing the encoding of embedded JSON values.
func ExperimentContractCanonical(contract ExperimentContract) ([]byte, error) {
	return json.Marshal(contract)
}
