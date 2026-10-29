package model

import "time"

type StatusQuery struct {
	Project string
	State   string
	After   string
	Limit   int
}

type PageQuery struct {
	After string
	Limit int
}

type ConcurrencyLimit struct {
	Active int `json:"active"`
	Limit  int `json:"limit"`
}

type ProjectLimit struct {
	Name   string `json:"name"`
	Active int    `json:"active"`
	Limit  int    `json:"limit"`
}

type StatusLimits struct {
	Global   ConcurrencyLimit `json:"global"`
	Projects []ProjectLimit   `json:"projects"`
}

type CallStatus struct {
	Day   string `json:"day"`
	Used  int    `json:"used"`
	Limit int    `json:"limit"`
}

type StatusSnapshot struct {
	ObservedAt time.Time    `json:"observed_at"`
	Paused     bool         `json:"paused"`
	Limits     StatusLimits `json:"limits"`
	Calls      CallStatus   `json:"calls"`
	// IntakeStepCount is the global active internal drafting-attempt count,
	// independent of product pagination or project/state filters. No intake
	// identity or content is exposed by this aggregate.
	IntakeStepCount int             `json:"intake_step_count"`
	Features        []FeatureStatus `json:"features"`
	NextCursor      string          `json:"next_cursor,omitempty"`
}

type FeatureStatus struct {
	Feature         Feature            `json:"feature"`
	Attempts        []AttemptStatus    `json:"attempts"`
	Sessions        []SessionStatus    `json:"sessions"`
	Checks          []CheckStatus      `json:"checks"`
	Integration     IntegrationStatus  `json:"integration"`
	Wait            *WaitReason        `json:"wait,omitempty"`
	CurrentEvidence []Evidence         `json:"current_evidence"`
	Receipt         string             `json:"receipt,omitempty"`
	Dependencies    []DependencyStatus `json:"dependencies"`
	Timeline        []StageInterval    `json:"timeline"`
}

// AttemptStatus deliberately enumerates public fields; adding private Attempt
// execution state cannot accidentally publish controller credentials or paths.
type AttemptStatus struct {
	ID                  string    `json:"id"`
	FeatureID           string    `json:"feature_id"`
	Stage               string    `json:"stage"`
	State               string    `json:"state"`
	Active              bool      `json:"active"`
	StartedAt           time.Time `json:"started_at"`
	StageStartedAt      time.Time `json:"stage_started_at"`
	UpdatedAt           time.Time `json:"updated_at"`
	Deadline            time.Time `json:"deadline"`
	BaseSHA             string    `json:"base_sha"`
	CandidateSHA        string    `json:"candidate_sha,omitempty"`
	CandidateGeneration int       `json:"candidate_generation"`
	SourceBaseSHA       string    `json:"source_base_sha,omitempty"`
	SourceCandidateSHA  string    `json:"source_candidate_sha,omitempty"`
}

type SessionStatus struct {
	AttemptID     string    `json:"attempt_id"`
	Stage         string    `json:"stage"`
	DispatchKey   string    `json:"dispatch_key"`
	ThreadID      string    `json:"thread_id"`
	EnvironmentID string    `json:"environment_id"`
	CreatedAt     time.Time `json:"created_at"`
}

type CheckStatus struct {
	AttemptID           string    `json:"attempt_id"`
	Name                string    `json:"name"`
	CandidateGeneration int       `json:"candidate_generation"`
	CandidateSHA        string    `json:"candidate_sha"`
	State               string    `json:"state"`
	Detail              string    `json:"detail"`
	StartedAt           time.Time `json:"started_at"`
	UpdatedAt           time.Time `json:"updated_at"`
}

type EventPage struct {
	Items      []Event `json:"items"`
	NextCursor string  `json:"next_cursor,omitempty"`
}

type EvidencePage struct {
	Items      []Evidence `json:"items"`
	NextCursor string     `json:"next_cursor,omitempty"`
}

type DependencyStatus struct {
	ID        string `json:"id"`
	State     string `json:"state"`
	Satisfied bool   `json:"satisfied"`
}

type StageInterval struct {
	Stage      string     `json:"stage"`
	State      string     `json:"state"`
	AttemptID  string     `json:"attempt_id,omitempty"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}
