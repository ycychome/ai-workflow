package orchestration

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	gh "github.com/ycychome/ai-workflow/internal/github"
)

type ciIdentityFake struct {
	repository  gh.Repository
	workflow    gh.ActionsWorkflow
	run, latest gh.ActionsRun
	file        gh.RepositoryFileIdentity
	merge       gh.CommitInfo
	pr          gh.CIPullRequestSnapshot
	fail        string
	calls       int
}

func (f *ciIdentityFake) call(name string) error {
	f.calls++
	if f.fail == name {
		return errors.New("合成读取故障")
	}
	return nil
}
func (f *ciIdentityFake) Repository(context.Context, string, string) (gh.Repository, error) {
	return f.repository, f.call("repository")
}
func (f *ciIdentityFake) GetActionsWorkflow(context.Context, string, string, int64) (gh.ActionsWorkflow, error) {
	return f.workflow, f.call("workflow")
}
func (f *ciIdentityFake) GetActionsRun(context.Context, string, string, int64) (gh.ActionsRun, error) {
	return f.latest, f.call("latest")
}
func (f *ciIdentityFake) GetActionsRunAttempt(context.Context, string, string, int64, int64) (gh.ActionsRun, error) {
	return f.run, f.call("attempt")
}
func (f *ciIdentityFake) GetRepositoryFileIdentity(context.Context, string, string, string, string) (gh.RepositoryFileIdentity, error) {
	return f.file, f.call("file")
}
func (f *ciIdentityFake) GetCommitInfo(context.Context, string, string, string) (gh.CommitInfo, error) {
	return f.merge, f.call("merge")
}
func (f *ciIdentityFake) GetCIPullRequestSnapshot(context.Context, string, string, int) (gh.CIPullRequestSnapshot, error) {
	return f.pr, f.call("pr")
}
func ciTestFixture() (TrustedCIPolicy, CICandidateIdentity, SignedCIReceipt, *ciIdentityFake, time.Time) {
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	private := ed25519.NewKeyFromSeed([]byte(strings.Repeat("s", ed25519.SeedSize)))
	policy := TrustedCIPolicy{RepositoryID: 101, Repository: "example/issuedesk", WorkflowID: 202, WorkflowPath: ".github/workflows/verify.yml", SourceCommitSHA: strings.Repeat("a", 40), SourceBlobSHA: strings.Repeat("b", 40), ReceiptPublicKey: base64.StdEncoding.EncodeToString(private.Public().(ed25519.PublicKey)), MaxRecordAgeSeconds: 120}
	candidate := CICandidateIdentity{RepositoryID: 101, PullRequest: 3, Generation: 4, VerificationKey: "冻结验证键", HeadSHA: strings.Repeat("c", 40), BaseSHA: strings.Repeat("d", 40), TestMergeSHA: strings.Repeat("e", 40), TreeSHA: strings.Repeat("f", 40), RunID: 301, RunAttempt: 2, NotBefore: now.Add(-10 * time.Minute)}
	receipt := SignedCIReceipt{Payload: CIReceiptPayload{Schema: "ai-workflow-ci-v1", Candidate: candidate, WorkflowID: policy.WorkflowID, WorkflowPath: policy.WorkflowPath, WorkflowSourceSHA: candidate.TestMergeSHA, WorkflowBlobSHA: policy.SourceBlobSHA, Conclusion: "success", CompletedAt: now.Add(-10 * time.Second)}}
	body, _ := CIReceiptSigningBytes(receipt.Payload)
	receipt.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(private, body))
	repository := gh.Repository{ID: policy.RepositoryID, FullName: policy.Repository}
	pull := gh.ActionsPullRequest{Number: candidate.PullRequest}
	pull.Head.SHA = candidate.HeadSHA
	pull.Head.Repo = repository
	pull.Base.SHA = candidate.BaseSHA
	pull.Base.Repo = repository
	run := gh.ActionsRun{ID: candidate.RunID, WorkflowID: policy.WorkflowID, RunAttempt: candidate.RunAttempt, Path: policy.WorkflowPath, Event: "pull_request", HeadSHA: candidate.HeadSHA, Status: "completed", Conclusion: "success", Repository: repository, HeadRepository: repository, PullRequests: []gh.ActionsPullRequest{pull}, CreatedAt: now.Add(-9 * time.Minute), UpdatedAt: now.Add(-30 * time.Second), HTMLURL: "https://github.com/example/issuedesk/actions/runs/301"}
	pr := gh.CIPullRequestSnapshot{MergeCommitSHA: candidate.TestMergeSHA}
	yes := true
	pr.Mergeable = &yes
	pr.Number = candidate.PullRequest
	pr.State = "open"
	pr.Head.SHA = candidate.HeadSHA
	pr.Head.Repo = &repository
	pr.Base.SHA = candidate.BaseSHA
	pr.Base.Repo = &repository
	fake := &ciIdentityFake{repository: repository, workflow: gh.ActionsWorkflow{ID: policy.WorkflowID, Path: policy.WorkflowPath, State: "active"}, run: run, latest: run, file: gh.RepositoryFileIdentity{Type: "file", Path: policy.WorkflowPath, SHA: policy.SourceBlobSHA}, merge: gh.CommitInfo{SHA: candidate.TestMergeSHA, TreeSHA: candidate.TreeSHA, Parents: []string{candidate.BaseSHA, candidate.HeadSHA}}, pr: pr}
	return policy, candidate, receipt, fake, now
}
func TestTrustedCIExactIdentityPassesAndExpires(t *testing.T) {
	p, c, receipt, client, now := ciTestFixture()
	record, err := FetchTrustedCIRecord(context.Background(), client, p, c, receipt, now)
	if err != nil || record.Verdict != "PASS" || !record.Current(p, c, now) {
		t.Fatalf("完整身份应通过：%+v %v", record, err)
	}
	if record.Current(p, c, now.Add(121*time.Second)) {
		t.Fatal("过期记录仍通过")
	}
	c.Generation++
	if record.Current(p, c, now) {
		t.Fatal("旧验证轮次仍通过")
	}
}
func TestTrustedCIRejectsIdentityDrift(t *testing.T) {
	cases := []struct {
		name   string
		change func(*TrustedCIPolicy, *CICandidateIdentity, *SignedCIReceipt, *ciIdentityFake)
	}{
		{"fork来源", func(_ *TrustedCIPolicy, _ *CICandidateIdentity, _ *SignedCIReceipt, f *ciIdentityFake) {
			f.run.HeadRepository.ID = 999
		}},
		{"伪造仓库名", func(_ *TrustedCIPolicy, _ *CICandidateIdentity, _ *SignedCIReceipt, f *ciIdentityFake) {
			f.repository.ID = 999
		}},
		{"旧重试成功", func(_ *TrustedCIPolicy, _ *CICandidateIdentity, _ *SignedCIReceipt, f *ciIdentityFake) {
			f.latest.RunAttempt++
		}},
		{"未知工作流", func(_ *TrustedCIPolicy, _ *CICandidateIdentity, _ *SignedCIReceipt, f *ciIdentityFake) {
			f.run.WorkflowID++
		}},
		{"同名不同路径", func(_ *TrustedCIPolicy, _ *CICandidateIdentity, _ *SignedCIReceipt, f *ciIdentityFake) {
			f.workflow.Path = ".github/workflows/untrusted.yml"
		}},
		{"来源文件被PR替换", func(_ *TrustedCIPolicy, _ *CICandidateIdentity, _ *SignedCIReceipt, f *ciIdentityFake) {
			f.file.SHA = strings.Repeat("1", 40)
		}},
		{"错误事件", func(_ *TrustedCIPolicy, _ *CICandidateIdentity, _ *SignedCIReceipt, f *ciIdentityFake) {
			f.run.Event = "pull_request_target"
		}},
		{"旧head", func(_ *TrustedCIPolicy, _ *CICandidateIdentity, _ *SignedCIReceipt, f *ciIdentityFake) {
			f.run.HeadSHA = strings.Repeat("2", 40)
		}},
		{"PR基础版本漂移", func(_ *TrustedCIPolicy, _ *CICandidateIdentity, _ *SignedCIReceipt, f *ciIdentityFake) {
			f.pr.Base.SHA = strings.Repeat("2", 40)
		}},
		{"当前合并提交漂移", func(_ *TrustedCIPolicy, _ *CICandidateIdentity, _ *SignedCIReceipt, f *ciIdentityFake) {
			f.pr.MergeCommitSHA = strings.Repeat("2", 40)
		}},
		{"合并状态未知", func(_ *TrustedCIPolicy, _ *CICandidateIdentity, _ *SignedCIReceipt, f *ciIdentityFake) {
			f.pr.Mergeable = nil
		}},
		{"父提交不匹配", func(_ *TrustedCIPolicy, _ *CICandidateIdentity, _ *SignedCIReceipt, f *ciIdentityFake) {
			f.merge.Parents = []string{strings.Repeat("1", 40), strings.Repeat("2", 40)}
		}},
		{"树不匹配", func(_ *TrustedCIPolicy, _ *CICandidateIdentity, _ *SignedCIReceipt, f *ciIdentityFake) {
			f.merge.TreeSHA = strings.Repeat("3", 40)
		}},
		{"关联PR缺失", func(_ *TrustedCIPolicy, _ *CICandidateIdentity, _ *SignedCIReceipt, f *ciIdentityFake) {
			f.run.PullRequests = nil
		}},
		{"创建早于轮次", func(_ *TrustedCIPolicy, c *CICandidateIdentity, _ *SignedCIReceipt, f *ciIdentityFake) {
			f.run.CreatedAt = c.NotBefore.Add(-time.Second)
		}},
		{"无可信签名", func(_ *TrustedCIPolicy, _ *CICandidateIdentity, r *SignedCIReceipt, _ *ciIdentityFake) {
			r.Signature = ""
		}},
		{"回执被篡改", func(_ *TrustedCIPolicy, _ *CICandidateIdentity, r *SignedCIReceipt, _ *ciIdentityFake) {
			r.Payload.Candidate.TreeSHA = strings.Repeat("3", 40)
		}},
		{"工作流禁用", func(_ *TrustedCIPolicy, _ *CICandidateIdentity, _ *SignedCIReceipt, f *ciIdentityFake) {
			f.workflow.State = "disabled_manually"
		}},
		{"缺少配置公钥", func(p *TrustedCIPolicy, _ *CICandidateIdentity, _ *SignedCIReceipt, _ *ciIdentityFake) {
			p.ReceiptPublicKey = ""
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, c, r, f, now := ciTestFixture()
			tc.change(&p, &c, &r, f)
			record, err := FetchTrustedCIRecord(context.Background(), f, p, c, r, now)
			if err == nil || record.Verdict == "PASS" || record.Current(p, c, now) {
				t.Fatalf("不应通过：%+v %v", record, err)
			}
		})
	}
}
func TestTrustedCINonSuccessAndIncompleteNeverPass(t *testing.T) {
	for _, state := range []struct{ status, conclusion, want string }{{"queued", "", "PENDING"}, {"in_progress", "", "PENDING"}, {"completed", "failure", "FAIL"}, {"completed", "skipped", "FAIL"}, {"completed", "neutral", "FAIL"}, {"completed", "", "FAIL"}, {"completed", "cancelled", "FAIL"}} {
		t.Run(state.status+state.conclusion, func(t *testing.T) {
			p, c, r, f, now := ciTestFixture()
			f.run.Status = state.status
			f.run.Conclusion = state.conclusion
			f.latest = f.run
			record, err := FetchTrustedCIRecord(context.Background(), f, p, c, r, now)
			if err != nil || record.Verdict != state.want || record.Current(p, c, now) {
				t.Fatalf("状态判断不符：%+v %v", record, err)
			}
		})
	}
}
func TestTrustedCIReadErrorsNeverPass(t *testing.T) {
	for _, operation := range []string{"repository", "pr", "latest", "attempt", "workflow", "file", "merge"} {
		t.Run(operation, func(t *testing.T) {
			p, c, r, f, now := ciTestFixture()
			f.fail = operation
			record, err := FetchTrustedCIRecord(context.Background(), f, p, c, r, now)
			if err == nil || record.Verdict == "PASS" {
				t.Fatal("读取错误被放行")
			}
		})
	}
}
func TestTrustedCIRejectsUnsignedBeforeRemoteRead(t *testing.T) {
	p, c, r, f, now := ciTestFixture()
	r.Signature = ""
	_, err := FetchTrustedCIRecord(context.Background(), f, p, c, r, now)
	if err == nil || f.calls != 0 {
		t.Fatal("未验证回执不应触发远端身份读取")
	}
}
func TestHighRiskPathsClassifyAndRejectAmbiguity(t *testing.T) {
	risks, err := ClassifyHighRiskPaths([]string{".github/workflows/ci.yml", "db/migrations/001.sql", "internal/auth/session.go", "internal/policy.go", "go.sum", "web/button.js", "go.sum"})
	if err != nil || len(risks) != 5 {
		t.Fatalf("风险分类不符：%+v %v", risks, err)
	}
	for _, name := range []string{"../policy.go", "x/../policy.go", "/absolute", "x\\policy.go", ""} {
		if _, err = ClassifyHighRiskPaths([]string{name}); err == nil {
			t.Fatalf("接受了不规范路径：%q", name)
		}
	}
}

func TestTrustedCINewerAttemptRequiresFreshReceiptAndTrustedSource(t *testing.T) {
	p, c, receipt, client, now := ciTestFixture()
	client.latest.RunAttempt++
	client.latest.Status = "in_progress"
	client.latest.Conclusion = ""
	if _, err := FetchTrustedCIRecord(context.Background(), client, p, c, receipt, now); !errors.Is(err, ErrCIReceiptSuperseded) {
		t.Fatalf("合法新重试必须撤销旧通过并等待：%v", err)
	}
	if err := validateWaitingCISource(context.Background(), client, p, c); err != nil {
		t.Fatal(err)
	}
	client.file.SHA = strings.Repeat("9", 40)
	if err := validateWaitingCISource(context.Background(), client, p, c); err == nil {
		t.Fatal("新重试不能掩盖来源替换")
	}
	client.latest.Status = "completed"
	client.latest.Conclusion = "failure"
	if _, err := FetchTrustedCIRecord(context.Background(), client, p, c, receipt, now); err == nil || errors.Is(err, ErrCIReceiptSuperseded) {
		t.Fatalf("新失败必须阻断而非无限等待：%v", err)
	}
}
