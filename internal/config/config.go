package config

import (
	"errors"
	"gopkg.in/yaml.v3"
	"os"
	"strings"
	"time"
)

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
		Builders                    int    `yaml:"builders"`
		Reviewers                   int    `yaml:"reviewers"`
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
	Name           string     `yaml:"name"`
	ID             int64      `yaml:"id"`
	BaseBranch     string     `yaml:"base_branch"`
	Maintainers    []string   `yaml:"maintainers"`
	AllowedPaths   []string   `yaml:"allowed_paths"`
	ProtectedPaths []string   `yaml:"protected_paths"`
	Verification   [][]string `yaml:"verification"`
	PolicyVersion  string     `yaml:"policy_version"`
}

func Load(path string) (Config, error) {
	c := Config{Listen: "127.0.0.1:8080", SetupListen: "127.0.0.1:8081", DataDir: "./data"}
	c.Runner.Mode = "responses"
	c.Runner.Image = "ai-workflow-runner:local"
	c.Runner.Builders = 2
	c.Runner.Reviewers = 2
	c.Runner.WorkflowConcurrency = 2
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
	if err = yaml.Unmarshal(b, &c); err != nil {
		return c, err
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
	if c.Runner.Builders < 1 || c.Runner.Builders > 2 || c.Runner.Reviewers < 1 || c.Runner.Reviewers > 2 {
		return c, errors.New("MVP allows 1 or 2 builders/reviewers")
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
