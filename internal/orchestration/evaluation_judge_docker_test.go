package orchestration

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ycychome/ai-workflow/internal/config"
	"github.com/ycychome/ai-workflow/internal/domain"
	"github.com/ycychome/ai-workflow/internal/sandbox"
)

func TestEvaluationJudgeDockerFrozenCandidateAndHiddenVerdict(t *testing.T) {
	alphaRequireDocker(t)
	for _, scenario := range []string{"正确候选及幂等复用", "分页缺陷真实失败", "执行后身份漂移"} {
		t.Run(scenario, func(t *testing.T) {
			e, ctx, w, manifestPath, prReads, archiveReads := evaluationJudgeDockerSetup(t, scenario, nil)
			result, err := e.JudgeEvaluation(ctx, w.ID, manifestPath)
			current, getErr := e.Store.Get(ctx, w.ID)
			if getErr != nil {
				t.Fatal(getErr)
			}
			if scenario == "执行后身份漂移" {
				if err == nil || len(current.Evidence) != 0 {
					t.Fatalf("漂移仍提交判定：%+v %v", result, err)
				}
				if prReads.Load() < 2 {
					t.Fatal("未执行后置身份核验")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			wantPass := scenario == "正确候选及幂等复用"
			if result.Passed != wantPass || !result.StoppedConfirmed || result.Evidence == "" || result.JudgeTreeSHA == result.TreeSHA {
				t.Fatalf("隐藏判定结果错误：%+v", result)
			}
			if (wantPass && result.Status != "PASS") || (!wantPass && (result.Status != "FAIL" || result.ExitCode == 0)) {
				t.Fatalf("失败被伪装成通过：%+v", result)
			}
			ev, ok := integrationEvidence(current, result.Evidence)
			if !ok {
				t.Fatal("真实隐藏证据没有加入工作流")
			}
			proof, err := readIntegrationEvidence(e.Config().DataDir, w.ID, ev)
			if err != nil {
				t.Fatal(err)
			}
			var recorded struct {
				Result   sandbox.RunResult `json:"result"`
				TestName string            `json:"test_name"`
			}
			if err = json.Unmarshal(proof, &recorded); err != nil {
				t.Fatal(err)
			}
			if recorded.TestName != "TestHiddenT01Pagination" || judgeTestPassed(recorded.Result, recorded.TestName) != wantPass {
				t.Fatal("证据未记录目标隐藏测试真实结果")
			}
			usedBefore, err := e.Store.AgentRuntimeSeconds(ctx, w.ID)
			if err != nil || usedBefore < 1 {
				t.Fatalf("判定未计预算：%d %v", usedBefore, err)
			}
			second, err := e.JudgeEvaluation(ctx, w.ID, manifestPath)
			if err != nil {
				t.Fatal(err)
			}
			usedAfter, err := e.Store.AgentRuntimeSeconds(ctx, w.ID)
			if err != nil || domain.Digest(result) != domain.Digest(second) || usedBefore != usedAfter || archiveReads.Load() != 1 {
				t.Fatal("可信判定重复请求重跑了测试或改变结果")
			}
			if err = e.sandboxManager().StopRun(ctx, result.RunID); err != nil {
				t.Fatalf("停止确认后仍存在资源：%v", err)
			}
		})
	}
}

// evaluationJudgeDockerFixture 为对抗回归提供相同的真实判定入口与冻结候选。
func evaluationJudgeDockerFixture(t *testing.T, mutate func(map[string]sandbox.File)) (*Engine, context.Context, domain.Workflow, string) {
	t.Helper()
	e, ctx, w, path, _, _ := evaluationJudgeDockerSetup(t, "", mutate)
	return e, ctx, w, path
}
func evaluationJudgeDockerSetup(t *testing.T, scenario string, mutate func(map[string]sandbox.File)) (*Engine, context.Context, domain.Workflow, string, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	e, ctx := safetyRegressionDB(t)
	root := filepath.Join("..", "..")
	manifestPath := filepath.Join(root, "eval", "manifest.json")
	manifestBody, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	var manifest evaluationJudgeManifest
	if err = json.Unmarshal(manifestBody, &manifest); err != nil {
		t.Fatal(err)
	}
	files := map[string]sandbox.File{}
	for name, digest := range manifest.BaselineFiles {
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(name)))
		if err != nil {
			t.Fatal(err)
		}
		if judgeSHA(data) != digest {
			t.Fatalf("候选基线文件与清单不符：%s", name)
		}
		files[name] = sandbox.File{Data: data, Mode: 0644}
	}
	// 恶意初始化代码尝试改写候选和已注入隐藏测试；必须被实际只读挂载拒绝。
	files["fixtures/issuedesk/readonly_probe.go"] = sandbox.File{Mode: 0644, Data: []byte(`package issuedesk
import "os"
func init() {
 for _, path := range []string{"store.go", "zz_trusted_judge_test.go"} {
  if err := os.WriteFile(path, []byte("已被候选改写"), 0644); err == nil { panic("候选能改写可信判定源码") }
 }
}
`)}
	if scenario == "分页缺陷真实失败" {
		file := files["fixtures/issuedesk/store.go"]
		before := "(created_at,id)<($9::timestamptz,$8)"
		if bytes.Count(file.Data, []byte(before)) != 1 {
			t.Fatal("冻结缺陷变体定位不唯一")
		}
		file.Data = bytes.Replace(file.Data, []byte(before), []byte("created_at<$9::timestamptz"), 1)
		files["fixtures/issuedesk/store.go"] = file
	}
	if mutate != nil {
		mutate(files)
	}
	tree, err := GitTreeSHA(files)
	if err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	zw := zip.NewWriter(&archive)
	for name, file := range files {
		header := &zip.FileHeader{Name: "frozen/" + name, Method: zip.Deflate}
		header.SetMode(0644)
		out, err := zw.CreateHeader(header)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = out.Write(file.Data); err != nil {
			t.Fatal(err)
		}
	}
	if err = zw.Close(); err != nil {
		t.Fatal(err)
	}
	w := domain.Workflow{ID: "judge-docker-" + domain.Digest([]any{t.Name(), time.Now().UnixNano()})[:16], Repository: "example/judge", RepositoryID: 881, InstallationID: 42, Issue: 1, PR: 2, Generation: 1, Control: "ACTIVE", Phase: "EVALUATION_COMPLETE", HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40), TestedSHA: strings.Repeat("c", 40), TestedTreeSHA: tree, Spec: domain.Spec{Approved: true, Digest: "frozen-spec"}}
	e.cfg.Repositories = []config.Repository{{Name: w.Repository, ID: w.RepositoryID, AllowedPaths: []string{"**"}}}
	executionConfig, err := e.ExecutionConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	w.Evaluation = &domain.EvaluationContext{TrialID: "judge-trial", TaskID: "T01", Kind: "review", Profile: "full", ConfigDigest: executionConfig.EvaluationConfigDigest(), ManifestDigest: judgeSHA(manifestBody), ManifestVersion: manifest.Version, BaseSHA: w.BaseSHA, BaseBranch: "main"}
	if err = e.Store.Create(ctx, w); err != nil {
		t.Fatal(err)
	}
	var prReads, archiveReads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		rw.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/app/installations/42/access_tokens":
			json.NewEncoder(rw).Encode(map[string]any{"token": "synthetic", "expires_at": time.Now().Add(time.Hour)})
		case "/repos/example/judge/zipball/" + w.TestedSHA:
			archiveReads.Add(1)
			rw.Header().Set("Content-Type", "application/zip")
			rw.Write(archive.Bytes())
		case "/repos/example/judge/pulls/2":
			reads := prReads.Add(1)
			head := w.HeadSHA
			if scenario == "执行后身份漂移" && reads > 1 {
				head = strings.Repeat("d", 40)
			}
			json.NewEncoder(rw).Encode(map[string]any{"number": w.PR, "state": "open", "merged": false, "mergeable": true, "merge_commit_sha": w.TestedSHA, "head": map[string]any{"sha": head, "repo": map[string]any{"id": w.RepositoryID}}, "base": map[string]any{"sha": w.BaseSHA, "repo": map[string]any{"id": w.RepositoryID}}})
		case "/repos/example/judge/git/commits/" + w.TestedSHA:
			json.NewEncoder(rw).Encode(map[string]any{"sha": w.TestedSHA, "message": "合成固定合并提交", "tree": map[string]string{"sha": tree}, "parents": []map[string]string{{"sha": w.BaseSHA}, {"sha": w.HeadSHA}}})
		default:
			t.Errorf("非预期外部访问：%s %s", r.Method, r.URL.Path)
			http.NotFound(rw, r)
		}
	}))
	t.Cleanup(server.Close)
	e.app = safetyGitHubClient(t, server.URL, server.Client())

	return e, ctx, w, manifestPath, &prReads, &archiveReads
}
