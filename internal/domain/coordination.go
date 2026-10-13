package domain

import "time"

// PlanDelta is an append-only planner decision bound to an immutable spec and
// an expected revision. Completed work and cumulative budgets survive replans.
type PlanDelta struct {
	ID               string    `json:"id"`
	Team             string    `json:"team"`
	Generation       int64     `json:"generation"`
	SpecDigest       string    `json:"spec_digest"`
	ExpectedRevision int       `json:"expected_revision"`
	Revision         int       `json:"revision"`
	Reason           string    `json:"reason"`
	Add              []Task    `json:"add"`
	Supersede        []string  `json:"supersede"`
	CreatedAt        time.Time `json:"created_at"`
}

type Preflight struct {
	AgentRunID  string    `json:"agent_run_id"`
	ContextHash string    `json:"context_hash"`
	Evidence    []string  `json:"evidence"`
	CheckedAt   time.Time `json:"checked_at"`
}

type SourceArtifact struct {
	ID            string    `json:"id"`
	TaskID        string    `json:"task_id"`
	AgentRunID    string    `json:"agent_run_id"`
	Generation    int64     `json:"generation"`
	PlanRevision  int       `json:"plan_revision"`
	SpecDigest    string    `json:"spec_digest"`
	ParentSHA     string    `json:"parent_sha"`
	TreeSHA       string    `json:"tree_sha"`
	ChangesDigest string    `json:"changes_digest"`
	EvidenceID    string    `json:"evidence_id"`
	CreatedAt     time.Time `json:"created_at"`
}

type IntegrationRecord struct {
	ID          string    `json:"id"`
	ArtifactIDs []string  `json:"artifact_ids"`
	ParentSHA   string    `json:"parent_sha"`
	TreeSHA     string    `json:"tree_sha"`
	CommitSHA   string    `json:"commit_sha"`
	Generation  int64     `json:"generation"`
	SpecDigest  string    `json:"spec_digest"`
	Status      string    `json:"status"`
	Reason      string    `json:"reason"`
	CreatedAt   time.Time `json:"created_at"`
}

type EvidenceRequest struct {
	TargetRole      string    `json:"target_role"`
	AgentRunID      string    `json:"agent_run_id"`
	UpdatedAt       time.Time `json:"updated_at"`
	Attempt         int       `json:"attempt"`
	ID              string    `json:"id"`
	RequestedBy     string    `json:"requested_by"`
	TaskID          string    `json:"task_id"`
	FindingID       string    `json:"finding_id"`
	Question        string    `json:"question"`
	VerificationKey string    `json:"verification_key"`
	Status          string    `json:"status"`
	Evidence        []string  `json:"evidence"`
	CreatedAt       time.Time `json:"created_at"`
}

type FindingResolution struct {
	SourceVerificationKey string   `json:"source_verification_key"`
	CandidateSHA          string   `json:"candidate_sha"`
	ID                    string   `json:"id"`
	FindingID             string   `json:"finding_id"`
	TaskID                string   `json:"task_id"`
	Kind                  string   `json:"kind"`
	Argument              string   `json:"argument"`
	Evidence              []string `json:"evidence"`
	VerificationKey       string   `json:"verification_key"`
	VerifiedBy            string   `json:"verified_by"`
	State                 string   `json:"state"`
}

type Waiver struct {
	FindingID       string    `json:"finding_id"`
	Actor           string    `json:"actor"`
	Reason          string    `json:"reason"`
	VerificationKey string    `json:"verification_key"`
	ExpiresAt       time.Time `json:"expires_at"`
	CreatedAt       time.Time `json:"created_at"`
}
