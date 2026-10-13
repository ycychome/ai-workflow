package orchestration

import (
	"archive/zip"
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ycychome/ai-workflow/internal/config"
	"github.com/ycychome/ai-workflow/internal/domain"
	"github.com/ycychome/ai-workflow/internal/sandbox"
)

func TestCIExecutorDockerSignsOnlyActualTrustedSuccess(t *testing.T) {
	alphaRequireDocker(t)
	for _, scenario := range []string{"实际通过签发及门禁消费", "实际命令失败不签发", "工作流来源漂移不签发"} {
		t.Run(scenario, func(t *testing.T) {
			e, ctx := safetyRegressionDB(t)
			policy, candidate, _, remote, _ := ciTestFixture()
			testSource := `package candidate
import ("os"; "testing")
func TestCandidate(t *testing.T) {
 if err := os.WriteFile("candidate_test.go", []byte("被候选修改"), 0644); err == nil { t.Fatal("候选源码可写") }
 if err := os.WriteFile(".github/workflows/verify.yml", []byte("被候选替换"), 0644); err == nil { t.Fatal("CI 来源可写") }
}
`
			if scenario == "实际命令失败不签发" {
				testSource = "package candidate\nimport \"testing\"\nfunc TestCandidate(t *testing.T) { t.Fatal(\"合成实际失败\") }\n"
			}
			files := map[string]sandbox.File{"go.mod": {Data: []byte("module candidate\ngo 1.24.0\n"), Mode: 0644}, "candidate_test.go": {Data: []byte(testSource), Mode: 0644}, policy.WorkflowPath: {Data: []byte("name: trusted\n"), Mode: 0644}}
			tree, err := GitTreeSHA(files)
			if err != nil {
				t.Fatal(err)
			}
			var archive bytes.Buffer
			zw := zip.NewWriter(&archive)
			for name, file := range files {
				header := &zip.FileHeader{Name: "candidate/" + name, Method: zip.Deflate}
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
			trusted := &config.TrustedCI{RepositoryID: policy.RepositoryID, Repository: policy.Repository, WorkflowID: policy.WorkflowID, WorkflowPath: policy.WorkflowPath, SourceCommitSHA: policy.SourceCommitSHA, SourceBlobSHA: policy.SourceBlobSHA, ReceiptPublicKey: policy.ReceiptPublicKey, MaxRecordAgeSeconds: policy.MaxRecordAgeSeconds}
			e.cfg.Repositories = []config.Repository{{Name: policy.Repository, ID: policy.RepositoryID, TrustedCI: trusted, Verification: [][]string{{"go", "test", "-count=1", "./..."}}}}
			w := domain.Workflow{ID: "ci-executor-docker", Repository: policy.Repository, RepositoryID: policy.RepositoryID, InstallationID: 42, Issue: 1, PR: candidate.PullRequest, Generation: 1, Control: "ACTIVE", Phase: "WAITING_CI", HeadSHA: candidate.HeadSHA, BaseSHA: candidate.BaseSHA, TestedSHA: candidate.TestMergeSHA, TestedTreeSHA: tree, Spec: domain.Spec{Approved: true, Digest: "frozen-ci-spec"}}
			if err = e.Store.Create(ctx, w); err != nil {
				t.Fatal(err)
			}
			w, err = e.Store.Get(ctx, w.ID)
			if err != nil {
				t.Fatal(err)
			}
			remote.run.CreatedAt = w.CreatedAt
			remote.run.UpdatedAt = time.Now().UTC()
			remote.latest = remote.run
			remote.merge.TreeSHA = tree
			if scenario == "工作流来源漂移不签发" {
				remote.file.SHA = strings.Repeat("9", 40)
			}
			server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
				rw.Header().Set("Content-Type", "application/json")
				write := func(v any) {
					if err := json.NewEncoder(rw).Encode(v); err != nil {
						t.Error(err)
					}
				}
				base := "/repos/" + policy.Repository
				switch r.URL.Path {
				case "/app/installations/42/access_tokens":
					write(map[string]any{"token": "synthetic", "expires_at": time.Now().Add(time.Hour)})
				case base:
					write(remote.repository)
				case base + "/zipball/" + w.TestedSHA:
					rw.Header().Set("Content-Type", "application/zip")
					rw.Write(archive.Bytes())
				case base + "/actions/runs/301", base + "/actions/runs/301/attempts/2":
					write(remote.run)
				case base + "/actions/workflows/202":
					write(remote.workflow)
				case base + "/pulls/3":
					write(remote.pr)
				case base + "/contents/" + policy.WorkflowPath:
					write(remote.file)
				case base + "/git/commits/" + w.TestedSHA:
					write(map[string]any{"sha": w.TestedSHA, "message": "合成可信合并候选", "tree": map[string]string{"sha": tree}, "parents": []map[string]string{{"sha": w.BaseSHA}, {"sha": w.HeadSHA}}})
				default:
					t.Errorf("非预期外部调用：%s %s", r.Method, r.URL.Path)
					http.NotFound(rw, r)
				}
			}))
			defer server.Close()
			e.app = safetyGitHubClient(t, server.URL, server.Client())
			private := ed25519.NewKeyFromSeed([]byte(strings.Repeat("s", ed25519.SeedSize)))
			keyPath := filepath.Join(t.TempDir(), "synthetic-signing-key")
			if err = os.WriteFile(keyPath, []byte(base64.StdEncoding.EncodeToString(private)), 0600); err != nil {
				t.Fatal(err)
			}
			result, err := e.ExecuteTrustedCIReceipt(ctx, CIExecutorOptions{WorkflowID: w.ID, RunID: 301, PrivateKeyFile: keyPath})
			receiptPath := filepath.Join(e.Config().DataDir, "ci-receipts", w.ID, e.key(ctx, w)+".json")
			current, getErr := e.Store.Get(ctx, w.ID)
			if getErr != nil {
				t.Fatal(getErr)
			}
			var auditCount int
			if getErr = e.Store.DB.QueryRow(ctx, `SELECT count(*) FROM audit WHERE workflow_id=$1 AND kind='trusted_ci_receipt_issued'`, w.ID).Scan(&auditCount); getErr != nil {
				t.Fatal(getErr)
			}
			if scenario != "实际通过签发及门禁消费" {
				if err == nil {
					t.Fatal("失败或来源漂移候选被签发")
				}
				if _, statErr := os.Stat(receiptPath); !os.IsNotExist(statErr) || auditCount != 0 {
					t.Fatalf("失败后仍存在签发回执或审计：%v %d", statErr, auditCount)
				}
				if ev, gateErr := e.trustedCIGate(ctx, current); gateErr == nil || ev != nil {
					t.Fatal("未签发候选仍被门禁接受")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				body, err := os.ReadFile(receiptPath)
				if err != nil {
					t.Fatal(err)
				}
				var receipt SignedCIReceipt
				if err = json.Unmarshal(body, &receipt); err != nil {
					t.Fatal(err)
				}
				if receipt.Signature == "" || result.ReceiptDigest != ciDigest(receipt) || auditCount != 1 {
					t.Fatal("宿主签名、落盘及审计未闭合")
				}
				proof, ok := integrationEvidence(current, result.EvidenceID)
				if !ok || proof.Kind != "TRUSTED_CI_EXECUTION" {
					t.Fatal("真实执行证据未持久化")
				}
				if _, err = readIntegrationEvidence(e.Config().DataDir, w.ID, proof); err != nil {
					t.Fatal(err)
				}
				ev, err := e.trustedCIGate(ctx, current)
				if err != nil || ev == nil || ev.Kind != "TRUSTED_CI" || ev.TestedTreeSHA != tree {
					t.Fatalf("门禁没有消费真实回执：%+v %v", ev, err)
				}
			}
			body, err := os.ReadFile(filepath.Join(e.Config().DataDir, "ci-executors", domain.Digest(w.ID)+".json"))
			if err != nil {
				t.Fatal(err)
			}
			var journal ciExecutorJournal
			if err = json.Unmarshal(body, &journal); err != nil || journal.Status != "STOPPED" || journal.StoppedAt.IsZero() {
				t.Fatalf("签发器未停止确认：%+v %v", journal, err)
			}
			if err = e.sandboxManager().StopRun(ctx, result.RunID); err != nil {
				t.Fatal(err)
			}
			used, err := e.Store.AgentRuntimeSeconds(ctx, w.ID)
			if err != nil || used < 1 {
				t.Fatalf("真实执行未计预算：%d %v", used, err)
			}
		})
	}
}
