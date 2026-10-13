package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ycychome/ai-workflow/internal/domain"
)

func TestEvaluationTrialIDCannotEscapeRouteOrMarker(t *testing.T) {
	for _, id := range []string{"", "../T01", "T01/A", "T01 -->", "评估", "a?b"} {
		if evaluationID.MatchString(id) {
			t.Fatalf("不应接受试验ID %q", id)
		}
	}
	for _, id := range []string{"smoke-T01", "T01-A-1", "review-T10-defect"} {
		if !evaluationID.MatchString(id) {
			t.Fatalf("合法试验ID被拒绝 %q", id)
		}
	}
}
func TestEvaluationPRURLAbsentUntilRealPR(t *testing.T) {
	w := domain.Workflow{Repository: "ycychome/ai-workflow-pilot"}
	if evaluationPRURL(w) != "" {
		t.Fatal("没有真实PR时不能编造链接")
	}
	w.PR = 42
	if got := evaluationPRURL(w); got != "https://github.com/ycychome/ai-workflow-pilot/pull/42" {
		t.Fatal(got)
	}
}
func TestEvaluationSHAUsesRawFrozenBytes(t *testing.T) {
	if sha256Text([]byte("{}\n")) == sha256Text([]byte("{}")) {
		t.Fatal("冻结manifest必须绑定原始字节")
	}
}

func TestReadEvaluationEvidenceCompactsAndVerifiesExactJSONBytes(t *testing.T) {
	dataDir := t.TempDir()
	root := filepath.Join(dataDir, "evidence", "workflow-1")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	// Keep the large integer, key order, Chinese text, and JSON escapes byte-
	// exact. Decoding through float64 and re-marshaling would alter this receipt.
	pretty := []byte("{\n  \"z\": 900719925474099312345678901,\n  \"中文\": \"引号\\\" 反斜线\\\\ 换行\\n HTML \\u003c\\u0026\\u003e\",\n  \"a\": 1e+09\n}\n")
	var compact bytes.Buffer
	if err := json.Compact(&compact, pretty); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "pretty.json")
	if err := os.WriteFile(path, pretty, 0600); err != nil {
		t.Fatal(err)
	}
	ev := domain.Evidence{ID: "source-1", Path: path, Hash: sha256Text(compact.Bytes())}
	if ev.Hash == sha256Text(pretty) {
		t.Fatal("pretty evidence fixture must differ from its stored compact JSON digest")
	}
	got, err := readEvaluationEvidence(dataDir, "workflow-1", ev)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, compact.Bytes()) || strings.Contains(string(got), "\n") || !bytes.Contains(got, []byte("900719925474099312345678901")) || !bytes.Contains(got, []byte(`"z"`)) || !bytes.Contains(got, []byte(`"a"`)) {
		t.Fatalf("export did not preserve compact JSON bytes: %s", got)
	}
	if err := os.WriteFile(path, []byte(strings.Replace(string(pretty), "900719925474099312345678901", "900719925474099312345678902", 1)), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readEvaluationEvidence(dataDir, "workflow-1", ev); !errors.Is(err, errEvaluationEvidenceDigest) {
		t.Fatalf("tampered evidence was not rejected by compact digest: %v", err)
	}
}

func TestReadEvaluationEvidenceRejectsPathAndSymlinkEscape(t *testing.T) {
	dataDir := t.TempDir()
	root := filepath.Join(dataDir, "evidence", "workflow-1")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(dataDir, "outside.json")
	if err := os.WriteFile(outside, []byte(`{"evidence":"outside"}`), 0600); err != nil {
		t.Fatal(err)
	}
	for name, path := range map[string]string{
		"absolute outside path": outside,
		"lexical traversal":     filepath.Join(root, "..", "..", "outside.json"),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := readEvaluationEvidence(dataDir, "workflow-1", domain.Evidence{ID: "external", Path: path, Hash: sha256Text([]byte(`{"evidence":"outside"}`))}); !errors.Is(err, errEvaluationEvidencePath) {
				t.Fatalf("out-of-root evidence path was not rejected: %v", err)
			}
		})
	}
	symlink := filepath.Join(root, "linked.json")
	if err := os.Symlink(outside, symlink); err != nil {
		t.Fatal(err)
	}
	if _, err := readEvaluationEvidence(dataDir, "workflow-1", domain.Evidence{ID: "linked", Path: symlink, Hash: sha256Text([]byte(`{"evidence":"outside"}`))}); !errors.Is(err, errEvaluationEvidencePath) {
		t.Fatalf("symlink escape was not rejected: %v", err)
	}
}
