package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/ycychome/ai-workflow/internal/execution"
	gh "github.com/ycychome/ai-workflow/internal/github"
	"github.com/ycychome/ai-workflow/internal/orchestration"
	"github.com/ycychome/ai-workflow/internal/persistence"
	"github.com/ycychome/ai-workflow/internal/policies"
)

func value(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}
func number(name string) int64 { n, _ := strconv.ParseInt(os.Getenv(name), 10, 64); return n }
func run() error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	secret := os.Getenv("WEBHOOK_SECRET")
	if len(secret) < 32 {
		return errors.New("WEBHOOK_SECRET must contain at least 32 bytes")
	}
	app, installation, repoID := number("GITHUB_APP_ID"), number("GITHUB_INSTALLATION_ID"), number("GITHUB_REPOSITORY_ID")
	if app <= 0 || installation <= 0 || repoID <= 0 {
		return errors.New("App, installation and repository IDs are required")
	}
	keyBytes, e := os.ReadFile(os.Getenv("GITHUB_APP_KEY_FILE"))
	if e != nil {
		return errors.New("cannot read configured GitHub App key file")
	}
	key, e := gh.ParseKey(keyBytes)
	if e != nil {
		return e
	}
	store, e := persistence.Open(ctx, os.Getenv("DATABASE_URL"))
	if e != nil {
		return e
	}
	defer store.Close()
	leader, e := store.Pool.Acquire(ctx)
	if e != nil {
		return e
	}
	defer leader.Release()
	var locked bool
	if e = leader.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, repoID).Scan(&locked); e != nil {
		return e
	}
	if !locked {
		return errors.New("another Runner Supervisor owns this repository")
	}
	defer leader.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, repoID)
	profile, e := policies.Get(value("VERIFICATION_PROFILE", "greeting-file"))
	if e != nil {
		return e
	}
	runner, e := execution.NewDocker(value("DOCKER_BINARY", "docker"), value("RUNNER_IMAGE", profile.DefaultImage))
	if e != nil {
		return e
	}
	defer runner.Close()
	runner.Namespace = fmt.Sprintf("repo-%d", repoID)
	if e = runner.Recover(ctx); e != nil {
		return fmt.Errorf("cannot stop orphan containers: %w", e)
	}
	if e = store.RecoverExecutions(ctx); e != nil {
		return e
	}
	root := value("WORKSPACE_ROOT", ".runtime")
	if e = os.MkdirAll(root, 0700); e != nil {
		return e
	}
	client := &gh.Client{BaseURL: "https://api.github.com", HTTP: &http.Client{Timeout: 15 * time.Second}, AppID: app, InstallationID: installation, Bot: os.Getenv("GITHUB_BOT_LOGIN"), Key: key}
	if client.Bot == "" {
		return errors.New("GITHUB_BOT_LOGIN required for receipt ownership checks")
	}
	service := &orchestration.Service{Store: store, GitHub: client, Runner: runner, Repository: os.Getenv("GITHUB_REPOSITORY"), RepositoryID: repoID, BaseBranch: value("GITHUB_BASE_BRANCH", "main"), Label: value("AUTHORIZED_LABEL", "ai:m0"), WorkspaceRoot: root, MaxAttempts: 4}
	service.VerificationProfile = profile.Name
	if e = service.Validate(); e != nil {
		return e
	}
	wait := service.Run(ctx)
	defer func() { cancel(); wait() }()
	mux := http.NewServeMux()
	mux.Handle("/webhooks/github", gh.Gateway{Store: store, Secret: []byte(secret), Repository: service.Repository, RepositoryID: repoID, InstallationID: installation})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if store.Pool.Ping(r.Context()) != nil {
			http.Error(w, "database unavailable", 503)
			return
		}
		w.Write([]byte("ok\n"))
	})
	server := &http.Server{Addr: value("LISTEN_ADDR", ":8080"), Handler: mux, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second, MaxHeaderBytes: 16384}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		server.Shutdown(shutdown)
	}()
	slog.Info("M0 control started", "repository", service.Repository, "runner", runner.Image)
	e = server.ListenAndServe()
	if errors.Is(e, http.ErrServerClosed) {
		return nil
	}
	return e
}
func main() {
	if e := run(); e != nil {
		slog.Error("control stopped", "error", e)
		os.Exit(1)
	}
}
