package orchestration

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ycychome/ai-workflow/internal/domain"
	gh "github.com/ycychome/ai-workflow/internal/github"
	"github.com/ycychome/ai-workflow/internal/sandbox"
	"github.com/ycychome/ai-workflow/internal/store"
)

func TestReviewAndDiffUseVerifiedMergeSnapshot(t *testing.T) {
	// Remote snapshot access also checks the persistent repository epoch fence.
	e, _ := safetyRegressionDB(t)
	var archive bytes.Buffer
	var headArchive bytes.Buffer
	zw := zip.NewWriter(&archive)
	for name, content := range map[string]string{"config": "secure base setting", "handler": "new handler"} {
		header := &zip.FileHeader{Name: "repo/" + name}
		header.SetMode(0644)
		f, err := zw.CreateHeader(header)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = f.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	hw := zip.NewWriter(&headArchive)
	for name, content := range map[string]string{"config": "old insecure setting", "handler": "new handler"} {
		header := &zip.FileHeader{Name: "repo/" + name}
		header.SetMode(0644)
		f, err := hw.CreateHeader(header)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := hw.Close(); err != nil {
		t.Fatal(err)
	}
	var archiveRequests []string
	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/app/installations/42/access_tokens":
			fmt.Fprint(rw, `{"token":"test","expires_at":"2099-01-01T00:00:00Z"}`)
		case "/repos/acme/pilot/pulls/1":
			fmt.Fprint(rw, `{"state":"open","mergeable":true,"head":{"sha":"head"},"base":{"sha":"base"},"merge_commit_sha":"merge"}`)
		case "/repos/acme/pilot/git/commits/merge":
			fmt.Fprint(rw, `{"sha":"merge","message":"Merge head into base","tree":{"sha":"merge-tree"},"parents":[{"sha":"base"},{"sha":"head"}]}`)
		case "/repos/acme/pilot/zipball/merge":
			archiveRequests = append(archiveRequests, "merge")
			rw.Write(archive.Bytes())
		case "/repos/acme/pilot/zipball/head":
			archiveRequests = append(archiveRequests, "head")
			rw.Write(headArchive.Bytes())
		default:
			t.Errorf("unexpected request: %s", r.URL.Path)
			rw.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	e.app, err = gh.New(gh.Config{AppID: "1", PrivateKeyPEM: pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}), BaseURL: server.URL, HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	w := domain.Workflow{ID: "review-snapshot", Repository: "acme/pilot", InstallationID: 42, PR: 1, HeadSHA: "head", BaseSHA: "base"}
	files, pending, err := e.verificationSnapshot(context.Background(), &w)
	if err != nil || pending {
		t.Fatalf("verification snapshot: pending=%v err=%v", pending, err)
	}
	if w.TestedSHA != "merge" || w.TestedTreeSHA != "merge-tree" || string(files["config"].Data) != "secure base setting" {
		t.Fatal("wrong verified tree")
	}
	w.Review.Key = e.key(context.Background(), w)
	w.Review.VerificationPassed = true
	reviewFiles, err := e.reviewSnapshot(context.Background(), w)
	if err != nil || string(reviewFiles["config"].Data) != "secure base setting" {
		t.Fatalf("review source is not the verified merge snapshot: %v", err)
	}
	base := map[string]sandbox.File{"config": {Data: []byte("secure base setting"), Mode: 0644}, "handler": {Data: []byte("old handler"), Mode: 0644}}
	diff, err := e.candidateDiff(context.Background(), w, base, w.TestedSHA)
	if err != nil {
		t.Fatal(err)
	}
	if len(diff) != 1 || diff[0]["path"] != "handler" || diff[0]["candidate"] != "new handler" {
		t.Fatalf("diff did not use merged base setting: %#v", diff)
	}
	if fmt.Sprint(archiveRequests) != "[merge]" {
		t.Fatalf("review/diff downloaded another snapshot: %v", archiveRequests)
	}
	// Development analysis/verification tasks have no synthetic merge yet;
	// their same-named tool must still inspect the current development head.
	development := w
	development.TestedSHA, development.TestedTreeSHA = "", ""
	devDiff, err := e.candidateDiff(context.Background(), development, base, "")
	if err != nil || len(devDiff) != 2 {
		t.Fatalf("pre-verification development diff failed: changes=%v err=%v", devDiff, err)
	}
	if fmt.Sprint(archiveRequests) != "[merge head]" {
		t.Fatalf("development diff did not use current head: %v", archiveRequests)
	}
	for _, mutate := range []func(*domain.Workflow){func(w *domain.Workflow) { w.BaseSHA = "changed-base" }, func(w *domain.Workflow) { w.HeadSHA = "changed-head" }, func(w *domain.Workflow) { w.TestedTreeSHA = "wrong-tree" }} {
		stale := w
		mutate(&stale)
		if _, err := e.reviewSnapshot(context.Background(), stale); !errors.Is(err, store.ErrStale) {
			t.Fatalf("stale merge identity accepted: %v", err)
		}
	}
	empty := w
	empty.TestedSHA = ""
	if _, err := e.reviewSnapshot(context.Background(), empty); err == nil {
		t.Fatal("missing verified commit accepted")
	}
}
