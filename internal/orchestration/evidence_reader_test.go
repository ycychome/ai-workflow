package orchestration

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/ycychome/ai-workflow/internal/domain"
)

func TestRecordedEvidenceReaderChecksHashAndWorkflowBoundary(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(root, "evidence", "workflow")
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	// Struct order differs from map order; the artifact reader must preserve it.
	proof := struct {
		Z int    `json:"z"`
		A string `json:"a"`
	}{0, "actual command result"}
	b, err := json.MarshalIndent(proof, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "proof.json")
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	ev := domain.Evidence{ID: "proof", Path: path, Hash: domain.Digest(proof)}
	if _, err := readRecordedEvidence(root, "workflow", ev); err != nil {
		t.Fatal(err)
	}
	if _, err := readRecordedEvidence(root, "other", ev); err == nil {
		t.Fatal("cross-workflow read accepted")
	}
	if err := os.WriteFile(path, []byte(`{"z":0,"a":"tampered"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readRecordedEvidence(root, "workflow", ev); err == nil {
		t.Fatal("tampered evidence accepted")
	}
}
