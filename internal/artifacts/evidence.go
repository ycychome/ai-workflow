package artifacts

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	c "github.com/ycychome/ai-workflow/internal/contracts"
)

type RunEvidence struct {
	AttemptID         string    `json:"attempt_id"`
	Generation        int64     `json:"generation"`
	Epoch             int64     `json:"lease_epoch"`
	Command           string    `json:"trusted_command"`
	ExitCode          int       `json:"exit_code"`
	Log               string    `json:"log"`
	RunnerImage       string    `json:"runner_image"`
	TestedCommit      string    `json:"tested_commit"`
	TestedFilesDigest string    `json:"tested_files_digest"`
	RecordedAt        time.Time `json:"recorded_at"`
}

// SaveEvidence writes an immutable, private report outside all Worker mounts.
func SaveEvidence(root string, e RunEvidence) (string, string, error) {
	if e.AttemptID == "" || strings.ContainsAny(e.AttemptID, "/\\\n") {
		return "", "", errors.New("invalid evidence ID")
	}
	digest := c.Digest(e)
	dir := filepath.Join(root, "evidence")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", "", err
	}
	path := filepath.Join(dir, digest+".json")
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return "", "", err
	}
	b, err := json.MarshalIndent(e, "", "  ")
	if err == nil {
		_, err = f.Write(b)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		os.Remove(path)
		return "", "", err
	}
	return digest, path, nil
}
