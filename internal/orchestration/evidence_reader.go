package orchestration

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/ycychome/ai-workflow/internal/domain"
)

// readRecordedEvidence accepts a supervisor-owned record, never a model path.
func readRecordedEvidence(dataDir, workflowID string, ev domain.Evidence) (json.RawMessage, error) {
	root, err := filepath.EvalSymlinks(filepath.Join(dataDir, "evidence", workflowID))
	if err != nil {
		return nil, err
	}
	path, err := filepath.EvalSymlinks(ev.Path)
	if err != nil {
		return nil, err
	}
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return nil, errors.New("evidence is outside its workflow directory")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("evidence is not a regular file")
	}
	b, err := io.ReadAll(io.LimitReader(f, (256<<10)+1))
	if err != nil {
		return nil, err
	}
	if len(b) > 256<<10 {
		return nil, errors.New("evidence exceeds the bounded reader limit; inspect candidate source or run a focused command")
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, b); err != nil {
		return nil, fmt.Errorf("invalid evidence JSON: %w", err)
	}
	// Compact preserves the field order used by Marshal/MarshalIndent, including structs.
	sum := sha256.Sum256(compact.Bytes())
	if hex.EncodeToString(sum[:]) != ev.Hash {
		return nil, errors.New("recorded evidence hash mismatch")
	}
	return append(json.RawMessage(nil), compact.Bytes()...), nil
}

// The skeptic needs the proofs actually cited by accepted reports, not every
// historical artifact from earlier attempts. Other source tools remain available.
func skepticEvidence(w domain.Workflow) []domain.Evidence {
	ids := map[string]bool{}
	for _, ob := range w.Review.Obligations {
		for _, id := range ob.Evidence {
			ids[id] = true
		}
	}
	for _, f := range w.Review.Findings {
		for _, id := range f.Evidence {
			ids[id] = true
		}
	}
	out := []domain.Evidence{}
	for _, ev := range w.Evidence {
		if ids[ev.ID] && ev.HeadSHA == w.HeadSHA && ev.BaseSHA == w.BaseSHA {
			out = append(out, ev)
		}
	}
	return out
}

func evidenceSummaries(evidence []domain.Evidence) []map[string]any {
	out := make([]map[string]any, 0, len(evidence))
	for _, ev := range evidence {
		out = append(out, map[string]any{"id": ev.ID, "kind": ev.Kind, "command": ev.Command, "exit_code": ev.ExitCode, "hash": ev.Hash, "created_at": ev.CreatedAt})
	}
	return out
}
