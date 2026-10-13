package orchestration

import (
	"io/fs"
	"testing"

	"github.com/ycychome/ai-workflow/internal/domain"
	gh "github.com/ycychome/ai-workflow/internal/github"
	"github.com/ycychome/ai-workflow/internal/sandbox"
)

func TestRemotePRIdentityRequiresExactOpenCandidate(t *testing.T) {
	w := domain.Workflow{RepositoryID: 17, Repository: "owner/repo", PR: 9, Branch: "ai-workflow/9", BaseBranch: "main", HeadSHA: "head", BaseSHA: "base"}
	valid := gh.PullRequest{Number: 9, State: "open"}
	valid.Head.Ref, valid.Head.SHA = w.Branch, w.HeadSHA
	valid.Base.Ref, valid.Base.SHA = w.BaseBranch, w.BaseSHA
	valid.Head.Repo = &gh.Repository{ID: w.RepositoryID, FullName: w.Repository}
	valid.Base.Repo = &gh.Repository{ID: w.RepositoryID, FullName: w.Repository}
	if reason := remotePRIdentityReason(w, valid); reason != "" {
		t.Fatalf("valid candidate rejected: %s", reason)
	}

	cases := []struct {
		name string
		edit func(*gh.PullRequest)
	}{
		{"closed", func(p *gh.PullRequest) { p.State = "closed" }},
		{"draft", func(p *gh.PullRequest) { p.Draft = true }},
		{"merged", func(p *gh.PullRequest) { p.Merged = true }},
		{"wrong PR", func(p *gh.PullRequest) { p.Number++ }},
		{"head SHA", func(p *gh.PullRequest) { p.Head.SHA = "other" }},
		{"base SHA", func(p *gh.PullRequest) { p.Base.SHA = "other" }},
		{"head ref", func(p *gh.PullRequest) { p.Head.Ref = "other" }},
		{"base ref", func(p *gh.PullRequest) { p.Base.Ref = "other" }},
		{"head repository", func(p *gh.PullRequest) { p.Head.Repo.ID++ }},
		{"base repository", func(p *gh.PullRequest) { p.Base.Repo.FullName = "fork/repo" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			candidate := valid
			headRepo, baseRepo := *valid.Head.Repo, *valid.Base.Repo
			candidate.Head.Repo, candidate.Base.Repo = &headRepo, &baseRepo
			tc.edit(&candidate)
			if reason := remotePRIdentityReason(w, candidate); reason == "" {
				t.Fatal("changed PR identity unexpectedly accepted")
			}
		})
	}
}

func TestPublishChangesExactlyMatch(t *testing.T) {
	base := map[string]sandbox.File{"keep.txt": {Data: []byte("same"), Mode: fs.FileMode(0644)}, "old.txt": {Data: []byte("old"), Mode: fs.FileMode(0644)}}
	head := map[string]sandbox.File{"keep.txt": {Data: []byte("same"), Mode: fs.FileMode(0644)}, "new.txt": {Data: []byte("new"), Mode: fs.FileMode(0644)}}
	want := []gh.FileChange{{Path: "old.txt", Delete: true}, {Path: "new.txt", Content: []byte("new"), Mode: "100644"}}
	if !publishChangesExactlyMatch(base, head, want) {
		t.Fatal("exact declared file changes were rejected")
	}

	cases := []struct {
		name    string
		changes []gh.FileChange
		files   map[string]sandbox.File
	}{
		{"undeclared file", want, map[string]sandbox.File{"keep.txt": base["keep.txt"], "new.txt": head["new.txt"], "extra.txt": {Data: []byte("extra"), Mode: fs.FileMode(0644)}}},
		{"missing deletion", []gh.FileChange{want[1]}, head},
		{"wrong content", []gh.FileChange{{Path: "old.txt", Delete: true}, {Path: "new.txt", Content: []byte("other"), Mode: "100644"}}, head},
		{"wrong mode", []gh.FileChange{{Path: "old.txt", Delete: true}, {Path: "new.txt", Content: []byte("new"), Mode: "100755"}}, head},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if publishChangesExactlyMatch(base, tc.files, tc.changes) {
				t.Fatal("unverified publish tree was accepted")
			}
		})
	}
}
