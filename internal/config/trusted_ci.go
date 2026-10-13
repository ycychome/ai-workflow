package config

// TrustedCI is provisioned by an operator, never by a candidate checkout.
type TrustedCI struct {
	RepositoryID        int64  `yaml:"repository_id" json:"repository_id"`
	Repository          string `yaml:"repository" json:"repository"`
	WorkflowID          int64  `yaml:"workflow_id" json:"workflow_id"`
	WorkflowPath        string `yaml:"workflow_path" json:"workflow_path"`
	SourceCommitSHA     string `yaml:"source_commit_sha" json:"source_commit_sha"`
	SourceBlobSHA       string `yaml:"source_blob_sha" json:"source_blob_sha"`
	ReceiptPublicKey    string `yaml:"receipt_public_key" json:"receipt_public_key"`
	MaxRecordAgeSeconds int64  `yaml:"max_record_age_seconds" json:"max_record_age_seconds"`
}
