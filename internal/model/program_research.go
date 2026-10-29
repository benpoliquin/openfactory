package model

import (
	"context"
	"time"
)

type SourceCapture struct {
	ID                    string        `json:"id"`
	ProgramID             string        `json:"program_id"`
	Revision              int           `json:"revision"`
	RequestKey            string        `json:"request_key"`
	Kind                  string        `json:"kind"`
	RequestedURL          string        `json:"requested_url"`
	FinalURL              string        `json:"final_url"`
	PublisherCanonicalURL string        `json:"publisher_canonical_url,omitempty"`
	RetrievedAt           time.Time     `json:"retrieved_at"`
	Status                int           `json:"status"`
	MediaType             string        `json:"media_type"`
	ContentEncoding       string        `json:"content_encoding"`
	ByteLength            int64         `json:"byte_length"`
	ContentSHA256         string        `json:"content_sha256"`
	TextSHA256            string        `json:"text_sha256"`
	ExtractorVersion      string        `json:"extractor_version"`
	RedirectChain         []string      `json:"redirect_chain"`
	GitHub                *SourceGitHub `json:"github,omitempty"`
	Bundle                *SourceBundle `json:"bundle,omitempty"`
	FreshUntil            *time.Time    `json:"fresh_until,omitempty"`
	Valid                 bool          `json:"valid"`
}
type SourceGitHub struct {
	Repository             string `json:"repository"`
	Commit                 string `json:"commit"`
	Path                   string `json:"path"`
	BlobSHA                string `json:"blob_sha"`
	BlobIdentityScope      string `json:"blob_identity_scope"`
	CommitResolutionURL    string `json:"commit_resolution_url,omitempty"`
	CommitResolutionSHA256 string `json:"commit_resolution_sha256,omitempty"`
}
type SourceBundle struct {
	ManifestSHA string `json:"manifest_sha"`
	Member      string `json:"member"`
}
type SearchReceipt struct {
	ID          string       `json:"id"`
	ProgramID   string       `json:"program_id"`
	Revision    int          `json:"revision"`
	QueryID     string       `json:"query_id"`
	Query       string       `json:"query"`
	Provider    string       `json:"provider"`
	RetrievedAt time.Time    `json:"retrieved_at"`
	Leads       []SearchLead `json:"leads"`
	Status      string       `json:"status"`
	RequestID   string       `json:"request_id"`
}
type SearchLead struct {
	ID      string `json:"id"`
	URL     string `json:"url"`
	Title   string `json:"title"`
	Snippet string `json:"snippet"`
	Origin  string `json:"origin"`
}
type ResearchCapability struct {
	Token     string    `json:"-"`
	ProgramID string    `json:"program_id"`
	Revision  int       `json:"revision"`
	StepID    string    `json:"step_id"`
	AttemptID string    `json:"attempt_id"`
	Deadline  time.Time `json:"deadline"`
}
type ResearchOperationRequest struct {
	RequestID  string `json:"request_id"`
	Kind       string `json:"kind"`
	TopicID    string `json:"topic_id,omitempty"`
	SeedID     string `json:"seed_id,omitempty"`
	LeadID     string `json:"lead_id,omitempty"`
	URL        string `json:"url,omitempty"`
	Repository string `json:"repository,omitempty"`
	Ref        string `json:"ref,omitempty"`
	Path       string `json:"path,omitempty"`
}
type ResearchAuthorization struct {
	ID               string                   `json:"id"`
	ProgramID        string                   `json:"program_id"`
	Revision         int                      `json:"revision"`
	StepID           string                   `json:"step_id"`
	AttemptID        string                   `json:"attempt_id"`
	Request          ResearchOperationRequest `json:"request"`
	URL              string                   `json:"url,omitempty"`
	Query            string                   `json:"query,omitempty"`
	MaxCaptureBytes  int64                    `json:"max_capture_bytes"`
	MaxTextBytes     int64                    `json:"max_text_bytes"`
	MaxRedirects     int                      `json:"max_redirects"`
	TimeoutSeconds   int                      `json:"timeout_seconds"`
	FreshnessSeconds int                      `json:"freshness_seconds"`
	Replay           *ResearchOperationResult `json:"replay,omitempty"`
}
type ResearchOperationResult struct {
	Source        *SourceCapture `json:"source,omitempty"`
	Search        *SearchReceipt `json:"search,omitempty"`
	Leads         []SearchLead   `json:"leads,omitempty"`
	ObservedBytes int64          `json:"observed_bytes"`
	ErrorCode     string         `json:"error_code,omitempty"`
	ErrorMessage  string         `json:"error_message,omitempty"`
}
type ResearchAuthority interface {
	IssueResearchCapability(context.Context, Attempt, Config) (ResearchCapability, error)
	AuthorizeResearch(context.Context, string, ResearchOperationRequest) (ResearchAuthorization, error)
	CompleteResearchOperation(context.Context, ResearchAuthorization, ResearchOperationResult) (ResearchOperationResult, error)
	ResearchSource(context.Context, string, string) (SourceCapture, error)
}
