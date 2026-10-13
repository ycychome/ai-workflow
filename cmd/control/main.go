package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/ycychome/ai-workflow/internal/agent"
	"github.com/ycychome/ai-workflow/internal/config"
	"github.com/ycychome/ai-workflow/internal/orchestration"
	"github.com/ycychome/ai-workflow/internal/server"
	"github.com/ycychome/ai-workflow/internal/store"
)

func main() {
	configDefault := os.Getenv("CONFIG_FILE")
	if configDefault == "" {
		configDefault = "secrets/config.yaml"
	}
	configPath := flag.String("config", configDefault, "path to the control configuration")
	doctor := flag.Bool("doctor", false, "check database and configured integrations, then exit")
	flag.Parse()
	if err := run(*configPath, *doctor); err != nil {
		log.Printf("control: %v", err)
		os.Exit(1)
	}
}

func run(configPath string, doctor bool) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("load configuration: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	st, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer st.Close()

	e, err := orchestration.New(st, cfg)
	if err != nil {
		return fmt.Errorf("initialize workflow engine: %w", err)
	}
	tokenPath := dashboardTokenPath(cfg)
	if err := ensureDashboardToken(tokenPath); err != nil {
		return fmt.Errorf("prepare dashboard token: %w", err)
	}
	srv, err := server.New(e, configPath, tokenPath)
	if err != nil {
		return fmt.Errorf("initialize server: %w", err)
	}
	if doctor {
		return runDoctor(context.Background(), cfg, e, st)
	}
	recoveryCtx, recoveryCancel := context.WithTimeout(context.Background(), 15*time.Second)
	if err := e.Sandbox.Recover(recoveryCtx); err != nil {
		log.Printf("sandboxunavailable: startup recovery incomplete: %v", err)
	}
	recoveryCancel()
	return serve(cfg, e, srv)
}

func dashboardTokenPath(cfg config.Config) string {
	keyPath := strings.TrimSpace(cfg.GitHub.PrivateKeyFile)
	if keyPath == "" {
		return filepath.Join("secrets", "dashboard-token")
	}
	return filepath.Join(filepath.Dir(keyPath), "dashboard-token")
}

func ensureDashboardToken(path string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	if b, err := os.ReadFile(path); err == nil {
		if !validToken(b) {
			return errors.New("existing dashboard token is not a 32-byte hex token")
		}
		return os.Chmod(path, 0600)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return err
	}
	token := []byte(hex.EncodeToString(raw))
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if errors.Is(err, os.ErrExist) {
		b, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		if !validToken(b) {
			return errors.New("existing dashboard token is not a 32-byte hex token")
		}
		return os.Chmod(path, 0600)
	}
	if err != nil {
		return err
	}
	if _, err = file.Write(token); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return err
	}
	if err = file.Sync(); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return err
	}
	return file.Close()
}

func validToken(b []byte) bool {
	b = []byte(strings.TrimSpace(string(b)))
	if len(b) != 64 {
		return false
	}
	decoded := make([]byte, 32)
	_, err := hex.Decode(decoded, b)
	return err == nil
}

func runDoctor(ctx context.Context, cfg config.Config, e *orchestration.Engine, st *store.Store) error {
	log.Print("doctor: database connection and schema are ready")
	var workflowCount int64
	if err := st.DB.QueryRow(ctx, "SELECT count(*) FROM workflows").Scan(&workflowCount); err != nil {
		return fmt.Errorf("doctor: count workflows: %w", err)
	}
	log.Printf("doctor: workflow records=%d", workflowCount)
	log.Printf("doctor: request budget per issue=%d daily=%d; issue runtime=%d minutes", cfg.Budget.IssueCalls, cfg.Budget.DailyCalls, cfg.Budget.IssueMinutes)

	if cfg.GitHub.AppID == 0 || cfg.GitHub.PrivateKeyFile == "" {
		log.Print("doctor: GitHub App check skipped (App ID or private key is not configured)")
	} else if _, err := e.App(); err != nil {
		return fmt.Errorf("doctor: GitHub App configuration: %w", err)
	} else {
		log.Print("doctor: GitHub App credentials are readable")
	}

	if !cfg.ModelReady() {
		log.Print("doctor: Responses API check skipped (endpoint, token file, or model is not configured)")
		return nil
	}
	token, err := os.ReadFile(cfg.Model.TokenFile)
	if err != nil {
		return fmt.Errorf("doctor: read model token: %w", err)
	}
	runner, err := agent.NewRunner(agent.Config{BaseURL: cfg.Model.BaseURL, Token: strings.TrimSpace(string(token)), Model: cfg.Model.Dev, Stream: false, MaxOutputTokens: cfg.Model.MaxOutputTokens})
	if err != nil {
		return fmt.Errorf("doctor: initialize Responses API client: %w", err)
	}
	probeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := runner.Doctor(probeCtx); err != nil {
		return fmt.Errorf("doctor: Responses API probe: %w", err)
	}
	log.Print("doctor: Responses API endpoint and development model are ready")
	return nil
}

func serve(cfg config.Config, e *orchestration.Engine, srv *server.Handlers) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	engineDone := make(chan struct{})
	go func() {
		defer close(engineDone)
		e.Run(ctx)
	}()

	public := newHTTPServer(cfg.Listen, srv.PublicHandler())
	dashboard := newHTTPServer(cfg.SetupListen, srv.DashboardHandler())
	serverErrors := make(chan error, 2)
	go listen(public, serverErrors)
	go listen(dashboard, serverErrors)

	fmt.Printf("AI workflow dashboard: %s/setup\nDashboard token file: %s\n", localURL(cfg.SetupListen), dashboardTokenPath(cfg))
	var serveErr error
	select {
	case <-ctx.Done():
	case err := <-serverErrors:
		if !errors.Is(err, http.ErrServerClosed) {
			serveErr = err
		}
	}
	stop()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	shutdownErrors := make(chan error, 2)
	go func() { shutdownErrors <- public.Shutdown(shutdownCtx) }()
	go func() { shutdownErrors <- dashboard.Shutdown(shutdownCtx) }()
	for range 2 {
		if err := <-shutdownErrors; err != nil && serveErr == nil {
			serveErr = err
		}
	}
	select {
	case <-engineDone:
	case <-shutdownCtx.Done():
		if serveErr == nil {
			serveErr = shutdownCtx.Err()
		}
	}
	if serveErr != nil {
		return fmt.Errorf("HTTP server: %w", serveErr)
	}
	return nil
}

func newHTTPServer(addr string, handler http.Handler) *http.Server {
	return &http.Server{Addr: addr, Handler: handler, ReadHeaderTimeout: 5 * time.Second, MaxHeaderBytes: 1 << 20}
}

func listen(s *http.Server, errs chan<- error) {
	err := s.ListenAndServe()
	if !errors.Is(err, http.ErrServerClosed) {
		errs <- fmt.Errorf("listen on %s: %w", s.Addr, err)
		return
	}
	errs <- http.ErrServerClosed
}

func localURL(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "http://127.0.0.1:8081"
	}
	if host == "" || host == "0.0.0.0" || host == "::" || host == "[::]" {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port)
}
