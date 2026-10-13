package orchestration

import (
	"archive/zip"
	"bytes"
	"io/fs"
	"testing"

	"github.com/ycychome/ai-workflow/internal/domain"
)

func TestDecodeArchiveNormalizesDOSZipPermissionsToGitModes(t *testing.T) {
	var archive bytes.Buffer
	writer := zip.NewWriter(&archive)
	for _, entry := range []struct {
		name string
		mode fs.FileMode
	}{
		{name: "dos-default/go.sum"},
		{name: "readonly.txt", mode: 0444},
		{name: "private.txt", mode: 0600},
		{name: "owner-execute.sh", mode: 0744},
		{name: "group-execute.txt", mode: 0654},
		{name: "other-execute.txt", mode: 0645},
	} {
		header := &zip.FileHeader{Name: "repo/" + entry.name}
		if entry.mode != 0 {
			header.SetMode(entry.mode)
		}
		file, err := writer.CreateHeader(header)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = file.Write([]byte(entry.name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	zr, err := zip.NewReader(bytes.NewReader(archive.Bytes()), int64(archive.Len()))
	if err != nil {
		t.Fatal(err)
	}
	if len(zr.File) == 0 || zr.File[0].ExternalAttrs != 0 || zr.File[0].Mode().Perm() != 0666 {
		t.Fatalf("fixture must use ZIP DOS defaults: external_attrs=%#x mode=%#o", zr.File[0].ExternalAttrs, zr.File[0].Mode().Perm())
	}
	files, err := decodeArchive(archive.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]fs.FileMode{
		"dos-default/go.sum": 0644,
		"readonly.txt":       0644,
		"private.txt":        0644,
		"owner-execute.sh":   0755,
		"group-execute.txt":  0644,
		"other-execute.txt":  0644,
	}
	for name, mode := range want {
		if got := files[name].Mode; got != mode {
			t.Errorf("%s mode=%#o, want Git mode %#o", name, got, mode)
		}
	}
}

func TestDecodeArchiveRejectsSymlinksAndSetuid(t *testing.T) {
	for _, mode := range []fs.FileMode{fs.ModeSymlink | 0777, fs.ModeSetuid | 0644} {
		var archive bytes.Buffer
		writer := zip.NewWriter(&archive)
		header := &zip.FileHeader{Name: "repo/unsafe"}
		header.SetMode(mode)
		file, err := writer.CreateHeader(header)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = file.Write([]byte("content"))
		if err = writer.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err = decodeArchive(archive.Bytes()); err == nil {
			t.Errorf("accepted unsupported ZIP mode %#o", mode)
		}
	}
}

func TestValidatePlanTaskKindScopes(t *testing.T) {
	tests := []struct {
		name    string
		task    domain.Task
		wantErr bool
	}{
		{
			name: "legacy empty kind remains code",
			task: domain.Task{ID: "legacy", Description: "legacy task", Paths: []string{"calculator.go"}},
		},
		{
			name:    "code empty scope rejected",
			task:    domain.Task{ID: "code-empty", Kind: "code", Description: "code task"},
			wantErr: true,
		},
		{
			name:    "code dot scope rejected",
			task:    domain.Task{ID: "code-dot", Kind: "code", Description: "code task", Paths: []string{"."}},
			wantErr: true,
		},
		{
			name:    "code parent traversal rejected",
			task:    domain.Task{ID: "code-parent", Kind: "code", Description: "code task", Paths: []string{"../outside"}},
			wantErr: true,
		},
		{
			name: "verification empty scope allowed",
			task: domain.Task{ID: "verify-empty", Kind: "verification", Description: "verify task"},
		},
		{
			name: "analysis dot scope allowed",
			task: domain.Task{ID: "analysis-dot", Kind: "analysis", Description: "analysis task", Paths: []string{"."}},
		},
		{
			name: "verification directory scope allowed",
			task: domain.Task{ID: "verify-directory", Kind: "verification", Description: "verify task", Paths: []string{"review_tests/"}},
		},
		{
			name:    "verification absolute directory rejected",
			task:    domain.Task{ID: "verify-absolute", Kind: "verification", Description: "verify task", Paths: []string{"/tmp/"}},
			wantErr: true,
		},
		{
			name:    "verification parent traversal rejected",
			task:    domain.Task{ID: "verify-parent", Kind: "verification", Description: "verify task", Paths: []string{"../outside"}},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validatePlan([]domain.Task{tt.task})
			if (err != nil) != tt.wantErr {
				t.Fatalf("validatePlan() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
