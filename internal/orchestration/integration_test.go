package orchestration

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ycychome/ai-workflow/internal/domain"
	"github.com/ycychome/ai-workflow/internal/sandbox"
)

func TestGitTreeSHAEmpty(t *testing.T) {
	got, err := GitTreeSHA(nil)
	if err != nil || got != "4b825dc642cb6eb9a060e54bf8d69288fbee4904" {
		t.Fatalf("空树身份错误：%s %v", got, err)
	}
}

func TestGitTreeSHANormalizesRegularPermissionBits(t *testing.T) {
	base := map[string]sandbox.File{"go.sum": {Data: []byte("module checksums\n"), Mode: 0644}}
	want, err := GitTreeSHA(base)
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []fs.FileMode{0444, 0600, 0640, 0654, 0645} {
		files := map[string]sandbox.File{"go.sum": {Data: base["go.sum"].Data, Mode: mode}}
		got, err := GitTreeSHA(files)
		if err != nil || got != want {
			t.Errorf("mode %#o changed a Git tree: got=%s want=%s err=%v", mode, got, want, err)
		}
	}
	executable, err := GitTreeSHA(map[string]sandbox.File{"go.sum": {Data: base["go.sum"].Data, Mode: 0744}})
	if err != nil || executable == want {
		t.Fatalf("owner execute bit must change Git tree identity: got=%s base=%s err=%v", executable, want, err)
	}
}

func TestGitPermissionOnlyChangesDoNotBecomeIntegrationScope(t *testing.T) {
	base := map[string]sandbox.File{"go.sum": {Data: []byte("module checksums\n"), Mode: 0644}}
	permissions := map[string]sandbox.File{"go.sum": {Data: base["go.sum"].Data, Mode: 0600}}
	if !sameIntegrationFiles(base, permissions) {
		t.Fatal("read/write permission differences must not change a Git source scope")
	}
	baseChange := sandbox.Change{Path: "go.sum", Status: "M", Data: base["go.sum"].Data, Mode: 0644}
	permissionChange := baseChange
	permissionChange.Mode = 0640
	if !sameIntegrationChanges([]sandbox.Change{baseChange}, []sandbox.Change{permissionChange}) {
		t.Fatal("permission-only snapshots must have the same integration changes")
	}
	if err := applyIntegrationChange(cloneSnapshot(base), base, permissionChange); err == nil {
		t.Fatal("permission-only update must not masquerade as a source change")
	}
}

// 对照真正的 Git 对象实现，覆盖目录排序、二进制内容与执行位。
func TestGitTreeSHAMatchesGit(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("未安装 Git")
	}
	root := t.TempDir()
	files := map[string]sandbox.File{
		"a.c":               {Data: []byte("普通文件\n"), Mode: 0644},
		"a/z":               {Data: []byte{0, 1, 255, 10}, Mode: 0644},
		"a0":                {Data: []byte("排序边界"), Mode: 0644},
		"run.sh":            {Data: []byte("#!/bin/sh\nexit 0\n"), Mode: 0755},
		"中文/工单.txt":         {Data: []byte("工单内容"), Mode: 0644},
		"readonly.txt":      {Data: []byte("只读文件"), Mode: 0444},
		"private.txt":       {Data: []byte("仅所有者访问"), Mode: 0600},
		"owner-execute.txt": {Data: []byte("所有者可执行"), Mode: 0744},
		"group-execute.txt": {Data: []byte("仅组执行位"), Mode: 0654},
		"other-execute.txt": {Data: []byte("仅其他执行位"), Mode: 0645},
	}
	run := func(args ...string) string {
		t.Helper()
		c := exec.Command(git, args...)
		c.Dir = root
		c.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "HOME="+root)
		out, err := c.CombinedOutput()
		if err != nil {
			t.Fatalf("Git 对照失败：%v %s", err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("init", "--quiet")
	run("config", "core.filemode", "true")
	for name, file := range files {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, file.Data, file.Mode); err != nil {
			t.Fatal(err)
		}
	}
	run("add", "-f", "--", ".")
	want := run("write-tree")
	got, err := GitTreeSHA(files)
	if err != nil || got != want {
		t.Fatalf("实际 Git 树不一致：%s != %s，%v", got, want, err)
	}
}

func TestGitTreeRejectsUnsafeSnapshots(t *testing.T) {
	cases := []map[string]sandbox.File{
		{"../escape": {Mode: 0644}}, {".git/config": {Mode: 0644}}, {"dir/.git/config": {Mode: 0644}},
		{"link": {Mode: fs.ModeSymlink | 0755}}, {"directory": {Mode: fs.ModeDir | 0755}},
		{"setuid": {Mode: fs.ModeSetuid | 0644}}, {"setgid": {Mode: fs.ModeSetgid | 0644}}, {"sticky": {Mode: fs.ModeSticky | 0644}},
		{"a": {Mode: 0644}, "a/b": {Mode: 0644}}, {"nul\x00name": {Mode: 0644}},
	}
	for i, files := range cases {
		if _, err := GitTreeSHA(files); err == nil {
			t.Fatalf("错误接纳非法快照 %d", i)
		}
	}
}

func integrationTestInput(id, path, content string) IntegrationInput {
	return IntegrationInput{Task: domain.Task{ID: id}, Artifact: domain.SourceArtifact{ID: "artifact-" + id, TaskID: id}, Changes: []sandbox.Change{{Path: path, Status: "A", Mode: 0644, Data: []byte(content)}}}
}
func TestPrepareIntegrationTreeIndependentArtifacts(t *testing.T) {
	base := map[string]sandbox.File{"base.txt": {Data: []byte("基线"), Mode: 0644}}
	before := cloneSnapshot(base)
	first, second := integrationTestInput("one", "one.txt", "一"), integrationTestInput("two", "two.txt", "二")
	merged, artifacts, changes, err := prepareIntegrationTree(base, []IntegrationInput{second, first})
	if err != nil {
		t.Fatal(err)
	}
	if !sameIntegrationFiles(base, before) || len(merged) != 3 || len(artifacts) != 2 || len(changes) != 2 {
		t.Fatal("集成修改了基线或遗漏来源")
	}
	tree, err := GitTreeSHA(merged)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range artifacts {
		if a.TreeSHA == "" || a.TreeSHA == tree {
			t.Fatal("来源树应当绑定独立基线加单个产物")
		}
	}
	first.Artifact.TreeSHA = artifacts[0].TreeSHA
	if _, _, _, err := prepareIntegrationTree(base, []IntegrationInput{first}); err != nil {
		t.Fatal(err)
	}
	first.Artifact.TreeSHA = strings.Repeat("0", 40)
	if _, _, _, err := prepareIntegrationTree(base, []IntegrationInput{first}); err == nil {
		t.Fatal("错误接纳伪造来源树")
	}
}
func TestPrepareIntegrationConflicts(t *testing.T) {
	for _, paths := range [][2]string{{"same", "same"}, {"dir", "dir/file"}} {
		_, _, _, err := prepareIntegrationTree(nil, []IntegrationInput{integrationTestInput("one", paths[0], "一"), integrationTestInput("two", paths[1], "二")})
		var conflict *IntegrationConflict
		if !errors.As(err, &conflict) || len(conflict.TaskIDs) != 2 || len(conflict.Paths) == 0 {
			t.Fatalf("冲突缺少任务与路径：%v", err)
		}
	}
	in := integrationTestInput("one", "same", "一")
	in.Changes = append(in.Changes, in.Changes[0])
	_, _, _, err := prepareIntegrationTree(nil, []IntegrationInput{in})
	var conflict *IntegrationConflict
	if !errors.As(err, &conflict) || conflict.Kind != "duplicate_path" {
		t.Fatalf("未拒绝重复路径：%v", err)
	}
}
func TestIntegrationRejectsNoopAndFalseStatus(t *testing.T) {
	base := map[string]sandbox.File{"a": {Data: []byte("原文"), Mode: 0644}}
	for _, c := range []sandbox.Change{
		{Path: "a", Status: "A", Data: []byte("新文"), Mode: 0644},
		{Path: "missing", Status: "M", Data: []byte("新文"), Mode: 0644},
		{Path: "a", Status: "M", Data: []byte("原文"), Mode: 0644},
		{Path: "missing", Status: "D", Deleted: true},
		{Path: "a", Status: "D", Deleted: true, Data: []byte("非空")},
	} {
		if err := applyIntegrationChange(cloneSnapshot(base), base, c); err == nil {
			t.Fatalf("错误接纳无效变更：%+v", c)
		}
	}
}
func TestPendingIntegrationBindsGenerationAndSourceSet(t *testing.T) {
	w := domain.Workflow{ID: "workflow", Generation: 1, HeadSHA: "head", Spec: domain.Spec{Digest: "spec"}}
	a, b := integrationTestInput("a", "a", "一"), integrationTestInput("b", "b", "二")
	record := PendingIntegration(w, []IntegrationInput{a, b})
	if record.ID != PendingIntegration(w, []IntegrationInput{b, a}).ID || record.Status != "PREPARING" || IntegrationRecordRunID(record) != record.ID {
		t.Fatal("集成身份必须稳定且可恢复")
	}
	w.Generation++
	if record.ID == PendingIntegration(w, []IntegrationInput{a, b}).ID {
		t.Fatal("代际变更必须撤销旧集成身份")
	}
}
func TestIntegrationChangesRequireExactContent(t *testing.T) {
	a, b := integrationTestInput("a", "a", "一").Changes[0], integrationTestInput("b", "b", "二").Changes[0]
	if !sameIntegrationChanges([]sandbox.Change{a, b}, []sandbox.Change{b, a}) {
		t.Fatal("顺序不应影响变更身份")
	}
	tampered := a
	tampered.Data = []byte("篡改")
	if sameIntegrationChanges([]sandbox.Change{a}, []sandbox.Change{tampered}) {
		t.Fatal("变更内容不匹配不能通过")
	}
}

func TestIntegrationEvidenceRejectsTamperingAndEscape(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "evidence", "workflow")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	payload := map[string]any{"generation": 1, "base_sha": "head", "changes": []sandbox.Change{}}
	body, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(root, "proof.json")
	if err := os.WriteFile(p, body, 0600); err != nil {
		t.Fatal(err)
	}
	ev := domain.Evidence{Path: p, Hash: domain.Digest(payload)}
	if _, err := readIntegrationEvidence(dir, "workflow", ev); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(`{"generation":2}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readIntegrationEvidence(dir, "workflow", ev); err == nil {
		t.Fatal("篡改证据被接纳")
	}
	outside := filepath.Join(dir, "outside.json")
	if err := os.WriteFile(outside, body, 0600); err != nil {
		t.Fatal(err)
	}
	ev.Path = outside
	if _, err := readIntegrationEvidence(dir, "workflow", ev); err == nil {
		t.Fatal("工作流目录外证据被接纳")
	}
	link := filepath.Join(root, "link.json")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	ev.Path = link
	if _, err := readIntegrationEvidence(dir, "workflow", ev); err == nil {
		t.Fatal("软链接越界证据被接纳")
	}
}
