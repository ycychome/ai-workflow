// Package github provides a small GitHub App client built only on net/http.
package github

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const defaultBaseURL = "https://api.github.com"

// Config contains GitHub App credentials and transport settings.
type Config struct {
	AppID         string
	PrivateKeyPEM []byte
	BaseURL       string
	HTTPClient    *http.Client
	Now           func() time.Time
}

// Client authenticates as a GitHub App and caches short-lived installation tokens.
type Client struct {
	appID   string
	key     *rsa.PrivateKey
	baseURL string
	http    *http.Client
	now     func() time.Time
	mu      sync.Mutex
	tokens  map[int64]tokenEntry
}

type tokenEntry struct {
	value   string
	expires time.Time
}

func New(cfg Config) (*Client, error) {
	if strings.TrimSpace(cfg.AppID) == "" {
		return nil, errors.New("github: AppID is required")
	}
	key, err := parsePrivateKey(cfg.PrivateKeyPEM)
	if err != nil {
		return nil, fmt.Errorf("github: parse app private key: %w", err)
	}
	base := strings.TrimRight(cfg.BaseURL, "/")
	if base == "" {
		base = defaultBaseURL
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = http.DefaultClient
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Client{appID: cfg.AppID, key: key, baseURL: base, http: cfg.HTTPClient, now: cfg.Now, tokens: map[int64]tokenEntry{}}, nil
}

func parsePrivateKey(data []byte) (*rsa.PrivateKey, error) {
	b, _ := pem.Decode(data)
	if b == nil {
		return nil, errors.New("invalid PEM")
	}
	if k, e := x509.ParsePKCS1PrivateKey(b.Bytes); e == nil {
		return k, nil
	}
	k, e := x509.ParsePKCS8PrivateKey(b.Bytes)
	if e != nil {
		return nil, e
	}
	r, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("private key is not RSA")
	}
	return r, nil
}

// InstallationClient is a client scoped to one GitHub App installation.
type InstallationClient struct {
	app            *Client
	installationID int64
	mu             sync.Mutex
	login          string
}

func (c *Client) ForInstallation(ctx context.Context, id int64) *InstallationClient {
	return &InstallationClient{app: c, installationID: id}
}

func (c *Client) jwt() (string, error) {
	now := c.now().Unix()
	enc := base64.RawURLEncoding.EncodeToString
	header := enc([]byte(`{"alg":"RS256","typ":"JWT"}`))
	payload, _ := json.Marshal(map[string]any{"iat": now - 30, "exp": now + 540, "iss": c.appID})
	message := header + "." + enc(payload)
	digest := sha256.Sum256([]byte(message))
	sig, e := rsa.SignPKCS1v15(rand.Reader, c.key, crypto.SHA256, digest[:])
	if e != nil {
		return "", e
	}
	return message + "." + enc(sig), nil
}

func (c *Client) appRequest(ctx context.Context, method, path string, body any, out any) error {
	token, e := c.jwt()
	if e != nil {
		return e
	}
	return c.request(ctx, method, path, body, out, "Bearer "+token)
}

func (i *InstallationClient) accessToken(ctx context.Context) (string, error) {
	if i.installationID <= 0 {
		return "", errors.New("github: installation id must be positive")
	}
	c := i.app
	c.mu.Lock()
	t, ok := c.tokens[i.installationID]
	c.mu.Unlock()
	if ok && c.now().Before(t.expires.Add(-time.Minute)) {
		return t.value, nil
	}
	var got struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	err := c.appRequest(ctx, http.MethodPost, fmt.Sprintf("/app/installations/%d/access_tokens", i.installationID), struct{}{}, &got)
	if err != nil {
		return "", err
	}
	if got.Token == "" {
		return "", errors.New("github: empty installation token")
	}
	if got.ExpiresAt.IsZero() {
		got.ExpiresAt = c.now().Add(50 * time.Minute)
	}
	c.mu.Lock()
	c.tokens[i.installationID] = tokenEntry{got.Token, got.ExpiresAt}
	c.mu.Unlock()
	return got.Token, nil
}

func (i *InstallationClient) request(ctx context.Context, method, path string, body, out any) error {
	token, e := i.accessToken(ctx)
	if e != nil {
		return e
	}
	return i.app.request(ctx, method, path, body, out, "token "+token)
}

// APIError describes an HTTP rejection. Response bodies are intentionally omitted to avoid leaking credentials.
type APIError struct {
	StatusCode   int
	Method, Path string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("github: %s %s returned HTTP %d", e.Method, e.Path, e.StatusCode)
}

// OutcomeUnknownError means a request may have reached GitHub but its response was lost.
type OutcomeUnknownError struct {
	Method, Path string
	Err          error
}

func (e *OutcomeUnknownError) Error() string {
	return fmt.Sprintf("github: outcome unknown for %s %s", e.Method, e.Path)
}
func (e *OutcomeUnknownError) Unwrap() error { return e.Err }

func (c *Client) request(ctx context.Context, method, path string, body, out any, authorization string) error {
	var rd io.Reader
	if body != nil {
		b, e := json.Marshal(body)
		if e != nil {
			return e
		}
		rd = bytes.NewReader(b)
	}
	u := c.baseURL + path
	req, e := http.NewRequestWithContext(ctx, method, u, rd)
	if e != nil {
		return e
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", authorization)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, e := c.http.Do(req)
	if e != nil {
		if method != http.MethodGet && method != http.MethodHead && method != http.MethodOptions {
			return &OutcomeUnknownError{method, path, e}
		}
		return fmt.Errorf("github: request failed: %w", e)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		if resp.StatusCode >= 500 && method != http.MethodGet && method != http.MethodHead && method != http.MethodOptions {
			return &OutcomeUnknownError{Method: method, Path: path}
		}
		return &APIError{resp.StatusCode, method, path}
	}
	if out == nil {
		io.Copy(io.Discard, resp.Body)
		return nil
	}
	if resp.StatusCode == http.StatusNoContent {
		return nil
	}
	if e = json.NewDecoder(resp.Body).Decode(out); e != nil {
		if method != http.MethodGet && method != http.MethodHead && method != http.MethodOptions {
			return &OutcomeUnknownError{Method: method, Path: path, Err: e}
		}
		return fmt.Errorf("github: decode response: %w", e)
	}
	return nil
}

// Do performs an arbitrary GitHub REST request. path must begin with '/'.
func (i *InstallationClient) Do(ctx context.Context, method, path string, body any, out any) error {
	if !strings.HasPrefix(path, "/") {
		return errors.New("github: REST path must start with /")
	}
	return i.request(ctx, method, path, body, out)
}

// DoBytes performs an authenticated REST request with a raw response body.
// Use it for binary endpoints such as archive downloads.
func (i *InstallationClient) DoBytes(ctx context.Context, method, path string, body []byte) ([]byte, error) {
	if !strings.HasPrefix(path, "/") {
		return nil, errors.New("github: REST path must start with /")
	}
	token, err := i.accessToken(ctx)
	if err != nil {
		return nil, err
	}
	return i.app.requestBytes(ctx, method, path, body, "token "+token)
}

func (c *Client) requestBytes(ctx context.Context, method, path string, body []byte, authorization string) ([]byte, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", authorization)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if body != nil {
		req.Header.Set("Content-Type", "application/octet-stream")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if method != http.MethodGet && method != http.MethodHead && method != http.MethodOptions {
			return nil, &OutcomeUnknownError{Method: method, Path: path, Err: err}
		}
		return nil, fmt.Errorf("github: request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		if resp.StatusCode >= 500 && method != http.MethodGet && method != http.MethodHead && method != http.MethodOptions {
			return nil, &OutcomeUnknownError{Method: method, Path: path}
		}
		return nil, &APIError{StatusCode: resp.StatusCode, Method: method, Path: path}
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("github: read response body: %w", err)
	}
	return data, nil
}

func q(s string) string { return url.PathEscape(s) }

// Manifest describes the fields returned by GitHub App manifest conversion.
type Manifest struct {
	ID            int64  `json:"id"`
	Slug          string `json:"slug"`
	Name          string `json:"name"`
	ClientID      string `json:"client_id"`
	ClientSecret  string `json:"client_secret"`
	WebhookSecret string `json:"webhook_secret"`
	Pem           string `json:"pem"`
	HTMLURL       string `json:"html_url"`
	AppID         int64  `json:"app_id"`
}

func (c *Client) ConvertManifest(ctx context.Context, code string) (Manifest, error) {
	var m Manifest
	e := c.appRequest(ctx, http.MethodPost, "/app-manifests/"+q(code)+"/conversions", nil, &m)
	return m, e
}

type Installation struct {
	ID                  int64             `json:"id"`
	AppID               int64             `json:"app_id"`
	Account             Account           `json:"account"`
	RepositorySelection string            `json:"repository_selection"`
	Permissions         map[string]string `json:"permissions"`
	HTMLURL             string            `json:"html_url"`
}
type Account struct {
	Login string `json:"login"`
	ID    int64  `json:"id"`
	Type  string `json:"type"`
}

func (c *Client) ListInstallations(ctx context.Context) ([]Installation, error) {
	var all []Installation
	for n := 1; n <= 100; n++ {
		var page []Installation
		path := fmt.Sprintf("/app/installations?per_page=100&page=%d", n)
		e := c.appRequest(ctx, http.MethodGet, path, nil, &page)
		if e != nil {
			return nil, e
		}
		all = append(all, page...)
		if len(page) < 100 {
			return all, nil
		}
	}
	return nil, errors.New("github: installation pagination limit exceeded")
}
func (c *Client) GetInstallation(ctx context.Context, id int64) (Installation, error) {
	var x Installation
	e := c.appRequest(ctx, http.MethodGet, fmt.Sprintf("/app/installations/%d", id), nil, &x)
	return x, e
}

// SelectInstallation returns the first installation matching login, or the sole installation when login is empty.
func (c *Client) SelectInstallation(ctx context.Context, login string) (Installation, error) {
	xs, e := c.ListInstallations(ctx)
	if e != nil {
		return Installation{}, e
	}
	var found []Installation
	for _, x := range xs {
		if login == "" || strings.EqualFold(x.Account.Login, login) {
			found = append(found, x)
		}
	}
	if len(found) != 1 {
		return Installation{}, fmt.Errorf("github: expected one matching installation, found %d", len(found))
	}
	return found[0], nil
}
