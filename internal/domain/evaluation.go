package domain

import "time"

// EvaluationContext is immutable admission metadata written by the authenticated
// evaluation API. Issues, labels and agent outputs cannot select an ablation.
type EvaluationContext struct {
	TrialID         string    `json:"trial_id"`
	Profile         string    `json:"profile"`
	TaskID          string    `json:"task_id"`
	Kind            string    `json:"kind"`
	CandidateID     string    `json:"candidate_id"`
	CandidateSHA    string    `json:"candidate_sha"`
	BaseSHA         string    `json:"base_sha"`
	BaseBranch      string    `json:"base_branch"`
	ConfigDigest    string    `json:"config_digest"`
	ManifestDigest  string    `json:"manifest_digest"`
	ManifestVersion string    `json:"manifest_version"`
	Repeat          int       `json:"repeat"`
	Seed            int64     `json:"seed"`
	CreatedAt       time.Time `json:"created_at"`
}

const EvaluationProtocolVersion = "alpha-evaluation/v1"

func ValidEvaluationProfile(profile string) bool {
	return profile == "builder_ci" || profile == "single_review" || profile == "full"
}
