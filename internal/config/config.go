package config

import (
	"errors"
	"gopkg.in/yaml.v3"
	"os"
	"strings"
	"time"
)

const DefaultWorkspaceBytes int64 = 512 << 20
const MaxWorkspaceBytes int64 = 8 << 30

type Config struct {
	Listen      string `yaml:"listen"`
	SetupListen string `yaml:"setup_listen"`
	DatabaseURL string `yaml:"database_url"`
	DataDir     string `yaml:"data_dir"`
	GitHub      struct {
		AppID             int64  `yaml:"app_id"`
		PrivateKeyFile    string `yaml:"private_key_file"`
		WebhookSecretFile string `yaml:"webhook_secret_file"`
		InstallationID    int64  `yaml:"installation_id"`
		APIURL            string `yaml:"api_url"`
		PublicURL         string `yaml:"public_url"`
	} `yaml:"github"`
	Model struct {
		BaseURL         string `yaml:"base_url"`
		TokenFile       string `yaml:"token_file"`
		Dev             string `yaml:"dev"`
		Review          string `yaml:"review"`
		Stream          bool   `yaml:"stream"`
		MaxOutputTokens int    `yaml:"max_output_tokens"`
	} `yaml:"model"`
	Runner struct {
		Mode                        string `yaml:"mode"`
		Image                       string `yaml:"image"`
		WorkspaceBytes              int64  `yaml:"workspace_bytes"`
		Builders                    int    `yaml:"builders"`  // Deprecated: fallback for agent_concurrency.
		Reviewers                   int    `yaml:"reviewers"` // Deprecated: fallback for agent_concurrency.
		AgentConcurrency            int    `yaml:"agent_concurrency"`
		WorkflowAgentConcurrency    int    `yaml:"workflow_agent_concurrency"`
		MaxTasksPerWorkflow         int    `yaml:"max_tasks_per_workflow"`
		MaxEvidenceRequests         int    `yaml:"max_evidence_requests"`
		WorkflowConcurrency         int    `yaml:"workflow_concurrency"`
		RequireAcceptForAnalysis    bool   `yaml:"require_accept_for_analysis"`
		RequireAcceptForDevelopment bool   `yaml:"require_accept_for_development"`
		AttemptMinutes              int    `yaml:"attempt_minutes"`
	} `yaml:"runner"`
	Budget struct {
		IssueCalls   int     `yaml:"issue_calls"`
		DailyCalls   int     `yaml:"daily_calls"`
		IssueMinutes int     `yaml:"issue_minutes"`
		MaxAttempts  int     `yaml:"max_attempts"`
		MaxFixRounds int     `yaml:"max_fix_rounds"`
		InputPrice   float64 `yaml:"input_price_per_million"`
		OutputPrice  float64 `yaml:"output_price_per_million"`
	} `yaml:"budget"`
	Repositories []Repository `yaml:"repositories"`
}
type Repository struct {
	Epoch                string     `yaml:"epoch"`
	TrustedCI            *TrustedCI `yaml:"trusted_ci,omitempty"`
	Name                 string     `yaml:"name"`
	ID                   int64      `yaml:"id"`
	BaseBranch           string     `yaml:"base_branch"`
	Maintainers          []string   `yaml:"maintainers"`
	AllowedPaths         []string   `yaml:"allowed_paths"`
	ProtectedPaths       []string   `yaml:"protected_paths"`
	Verification         [][]string `yaml:"verification"`
	VerificationServices []string   `yaml:"verification_services"`
	PolicyVersion        string     `yaml:"policy_version"`
}

func Load(path string) (Config, error) {
	c := Config{Listen: "127.0.0.1:8080", SetupListen: "127.0.0.1:8081", DataDir: "./data"}
	c.Runner.Mode = "responses"
	c.Runner.Image = "ai-workflow-runner:local"
	c.Runner.WorkspaceBytes = DefaultWorkspaceBytes
	c.Runner.Builders = 2
	c.Runner.Reviewers = 2
	c.Runner.WorkflowConcurrency = 2
	c.Runner.MaxTasksPerWorkflow = 64
	c.Runner.MaxEvidenceRequests = 64
	c.Runner.AttemptMinutes = 30
	c.Model.Stream = true
	c.Model.MaxOutputTokens = 8192
	c.Budget.IssueCalls = 80
	c.Budget.DailyCalls = 500
	c.Budget.IssueMinutes = 120
	c.Budget.MaxAttempts = 2
	c.Budget.MaxFixRounds = 2
	b, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	// yaml.v3 otherwise silently truncates floating-point scalars into int64.
	// A filesystem capacity must be an exact integral number of bytes.
	var rawQuota struct {
		Runner struct {
			WorkspaceBytes           any `yaml:"workspace_bytes"`
			AgentConcurrency         any `yaml:"agent_concurrency"`
			WorkflowAgentConcurrency any `yaml:"workflow_agent_concurrency"`
			MaxTasksPerWorkflow      any `yaml:"max_tasks_per_workflow"`
			MaxEvidenceRequests      any `yaml:"max_evidence_requests"`
		} `yaml:"runner"`
	}
	if err = yaml.Unmarshal(b, &rawQuota); err != nil {
		return c, err
	}
	if rawQuota.Runner.WorkspaceBytes != nil {
		switch rawQuota.Runner.WorkspaceBytes.(type) {
		case int, int64, uint64:
		default:
			return c, errors.New("runner.workspace_bytes must be an integer number of bytes")
		}
	}
	for name, value := range map[string]any{"agent_concurrency": rawQuota.Runner.AgentConcurrency, "workflow_agent_concurrency": rawQuota.Runner.WorkflowAgentConcurrency, "max_tasks_per_workflow": rawQuota.Runner.MaxTasksPerWorkflow, "max_evidence_requests": rawQuota.Runner.MaxEvidenceRequests} {
		if value == nil {
			continue
		}
		switch value.(type) {
		case int, int64, uint64:
		default:
			return c, errors.New("runner." + name + " must be a positive integer")
		}
	}
	if err = yaml.Unmarshal(b, &c); err != nil {
		return c, err
	}
	if rawQuota.Runner.AgentConcurrency == nil {
		c.Runner.AgentConcurrency = c.Runner.Builders + c.Runner.Reviewers
	}
	if rawQuota.Runner.WorkflowAgentConcurrency == nil {
		c.Runner.WorkflowAgentConcurrency = max(c.Runner.Builders, c.Runner.Reviewers)
	}
	if v := os.Getenv("DATABASE_URL"); v != "" {
		c.DatabaseURL = v
	}
	if v := os.Getenv("DATA_DIR"); v != "" {
		c.DataDir = v
	}
	if v := os.Getenv("HTTP_ADDR"); v != "" {
		c.Listen = v
	}
	if c.DatabaseURL == "" {
		return c, errors.New("database_url or DATABASE_URL is required")
	}
	if c.Runner.Builders < 1 || c.Runner.Reviewers < 1 {
		return c, errors.New("deprecated builders/reviewers must be positive")
	}
	if c.Runner.AgentConcurrency < 1 || c.Runner.WorkflowAgentConcurrency < 1 || c.Runner.WorkflowAgentConcurrency > c.Runner.AgentConcurrency {
		return c, errors.New("agent concurrency must be positive and workflow_agent_concurrency must not exceed agent_concurrency")
	}
	if c.Runner.MaxTasksPerWorkflow < 1 || c.Runner.MaxEvidenceRequests < 1 {
		return c, errors.New("task and evidence limits must be positive")
	}
	if !ValidWorkspaceBytes(c.Runner.WorkspaceBytes) {
		return c, errors.New("runner.workspace_bytes must be between 1 and 8589934592 bytes (8 GiB)")
	}
	if !ValidWorkflowConcurrency(c.Runner.WorkflowConcurrency) {
		return c, errors.New("runner.workflow_concurrency must be between 1 and 4")
	}
	if c.Runner.Mode != "fake" && c.Runner.Mode != "responses" {
		return c, errors.New("runner.mode must be responses or fake")
	}
	if c.Budget.IssueCalls < 1 || c.Budget.DailyCalls < 1 || c.Budget.MaxAttempts < 1 || c.Budget.MaxFixRounds < 0 || c.Runner.AttemptMinutes < 1 || c.Budget.IssueMinutes < 1 || c.Model.MaxOutputTokens < 1 {
		return c, errors.New("invalid budget or runner limit")
	}
	for i := range c.Repositories {
		r := &c.Repositories[i]
		for _, service := range r.VerificationServices {
			if service != "postgres" {
				return c, errors.New("unsupported verification service: " + service)
			}
		}
		if r.BaseBranch == "" {
			r.BaseBranch = "main"
		}
		if r.PolicyVersion == "" {
			r.PolicyVersion = "v1"
		}
		if len(r.AllowedPaths) == 0 {
			r.AllowedPaths = []string{"**"}
		}
		if len(r.Verification) == 0 {
			r.Verification = [][]string{{"go", "test", "./..."}, {"go", "vet", "./..."}}
		}
		for _, p := range []string{".git/**", ".github/workflows/**"} {
			exists := false
			for _, existing := range r.ProtectedPaths {
				if existing == p {
					exists = true
				}
			}
			if !exists {
				r.ProtectedPaths = append(r.ProtectedPaths, p)
			}
		}
	}
	return c, nil
}

func ValidWorkflowConcurrency(n int) bool { return n >= 1 && n <= 4 }
func (c Config) Repository(name string) (Repository, bool) {
	for _, r := range c.Repositories {
		if r.Name == name {
			return r, true
		}
	}
	return Repository{}, false
}
func (c Config) ModelReady() bool {
	if c.Model.BaseURL == "" || c.Model.TokenFile == "" || c.Model.Dev == "" || c.Model.Review == "" {
		return false
	}
	b, err := os.ReadFile(c.Model.TokenFile)
	return err == nil && strings.TrimSpace(string(b)) != ""
}
func (c Config) AttemptTimeout() time.Duration {
	return time.Duration(c.Runner.AttemptMinutes) * time.Minute
}

// Programmatic zero-value configs retain the same bounded default as omitted YAML.
// Load separately rejects an explicitly configured zero or negative capacity.
func (c Config) EffectiveWorkspaceBytes() int64 {
	if c.Runner.WorkspaceBytes == 0 {
		return DefaultWorkspaceBytes
	}
	return c.Runner.WorkspaceBytes
}

func ValidWorkspaceBytes(n int64) bool { return n > 0 && n <= MaxWorkspaceBytes }

// Effective methods also support programmatic configs that omit new fields.
func (c Config) EffectiveAgentConcurrency() int {
	if c.Runner.AgentConcurrency != 0 {
		return c.Runner.AgentConcurrency
	}
	if c.Runner.Builders > 0 || c.Runner.Reviewers > 0 {
		return max(1, c.Runner.Builders) + max(1, c.Runner.Reviewers)
	}
	return 4
}
func (c Config) EffectiveWorkflowAgentConcurrency() int {
	if c.Runner.WorkflowAgentConcurrency != 0 {
		return c.Runner.WorkflowAgentConcurrency
	}
	if c.Runner.Builders > 0 || c.Runner.Reviewers > 0 {
		return max(c.Runner.Builders, c.Runner.Reviewers)
	}
	return 2
}
func (c Config) EffectiveMaxTasksPerWorkflow() int {
	if c.Runner.MaxTasksPerWorkflow != 0 {
		return c.Runner.MaxTasksPerWorkflow
	}
	return 64
}
func (c Config) EffectiveMaxEvidenceRequests() int {
	if c.Runner.MaxEvidenceRequests != 0 {
		return c.Runner.MaxEvidenceRequests
	}
	return 64
}

// EffectiveEpoch preserves existing installations until an explicit reset.
func (r Repository) EffectiveEpoch() string {
	if r.Epoch == "" {
		return "legacy"
	}
	return r.Epoch
}
