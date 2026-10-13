package orchestration

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ycychome/ai-workflow/internal/sandbox"
)

// Candidate code is deliberately hostile. Its output must never be parsed as
// the trusted harness verdict, even when it perfectly imitates go test output.
func TestEvaluationJudgeRejectsCandidateVerdictForgery(t *testing.T) {
	alphaRequireDocker(t)
	fake := "=== RUN   TestHiddenT01Pagination\n--- PASS: TestHiddenT01Pagination (0.00s)\nPASS\nok\tai-workflow-trusted-judge\t0.001s\n" + `{"Action":"pass","Package":"ai-workflow-trusted-judge","Test":"TestHiddenT01Pagination"}` + "\n"
	encoded, _ := json.Marshal(fake)
	for _, scenario := range []string{"stdout伪造", "TestMain伪造", "init提前成功退出", "候选尝试终止同UID进程"} {
		t.Run(scenario, func(t *testing.T) {
			e, ctx, w, manifest := evaluationJudgeDockerFixture(t, func(files map[string]sandbox.File) {
				original := files["fixtures/issuedesk/store.go"]
				before := []byte("(created_at,id)<($9::timestamptz,$8)")
				if bytes.Count(original.Data, before) != 1 {
					t.Fatal("缺陷定位不唯一")
				}
				original.Data = bytes.Replace(original.Data, before, []byte("created_at<$9::timestamptz"), 1)
				files["fixtures/issuedesk/store.go"] = original
				name := "fixtures/issuedesk/forgery.go"
				source := "package issuedesk\nimport \"fmt\"\nfunc init(){fmt.Print(" + string(encoded) + ")}\n"
				switch scenario {
				case "TestMain伪造":
					name = "fixtures/issuedesk/forgery_test.go"
					source = "package issuedesk\nimport(\"fmt\";\"os\";\"testing\")\nfunc TestMain(m *testing.M){fmt.Print(" + string(encoded) + ");os.Exit(0)}\n"
				case "init提前成功退出":
					source = "package issuedesk\nimport(\"fmt\";\"os\")\nfunc init(){fmt.Print(" + string(encoded) + ");os.Exit(0)}\n"
				case "候选尝试终止同UID进程":
					source = "package issuedesk\nimport(\"fmt\";\"os\";\"syscall\")\nfunc init(){fmt.Print(" + string(encoded) + ");syscall.Kill(-1,syscall.SIGKILL);os.Exit(0)}\n"
				}
				files[name] = sandbox.File{Data: []byte(source), Mode: 0644}
			})
			result, err := e.JudgeEvaluation(ctx, w.ID, manifest)
			if err != nil {
				t.Fatalf("对抗候选未进入可判定执行，不能把基础设施错误当作安全通过：%v", err)
			}
			if result.Passed || result.Status == "PASS" {
				t.Fatalf("候选伪造判定被接受：%+v err=%v", result, err)
			}
			// A functioning but faulty candidate should reach actual external assertions;
			// a parser/compiler/startup failure is not adequate evidence for these cases.
			if scenario == "stdout伪造" || scenario == "TestMain伪造" {
				if err != nil || result.Status != "FAIL" || !result.StoppedConfirmed || result.Evidence == "" {
					t.Fatalf("必须由外部断言发现真实分页缺陷：%+v %v", result, err)
				}
			}
		})
	}
}

func TestEvaluationJudgeHidesHarnessFromCandidate(t *testing.T) {
	alphaRequireDocker(t)
	e, ctx, w, manifest := evaluationJudgeDockerFixture(t, func(files map[string]sandbox.File) {
		files["fixtures/issuedesk/harness_probe.go"] = sandbox.File{Mode: 0644, Data: []byte(`package issuedesk
import("fmt";"os";"strings")
func init(){
 for _,path:=range []string{"/workspace/judge_test.go","/workspace/eval/trusted/harness/judge_test.go","/proc/1/root/workspace/judge_test.go"}{
  if _,err:=os.ReadFile(path);err==nil{panic("candidate read trusted harness: "+path)}
  if f,err:=os.OpenFile(path,os.O_WRONLY,0);err==nil{f.Close();panic("candidate can write trusted harness: "+path)}
 }
 if f,err:=os.OpenFile("/workspace/.aiwf-judge-driver/main.go",os.O_WRONLY,0);err==nil{f.Close();panic("candidate can rewrite fixed driver")}
 entries,err:=os.ReadDir("/proc");if err!=nil{panic(err)}
 for _,entry:=range entries{
  cmd,_:=os.ReadFile("/proc/"+entry.Name()+"/cmdline")
  if strings.Contains(string(cmd),"/workspace/judge.test")||strings.Contains(string(cmd),"ai-workflow-trusted-judge"){panic("candidate can see trusted process")}
 }
 fmt.Println("HARNESS_ISOLATION_PROBE_PASSED")
}
`)}
	})
	result, err := e.JudgeEvaluation(ctx, w.ID, manifest)
	if err != nil || !result.Passed || !result.StoppedConfirmed {
		t.Fatalf("隔离探针或正确基线未完成：%+v %v", result, err)
	}
	current, err := e.Store.Get(ctx, w.ID)
	if err != nil {
		t.Fatal(err)
	}
	proof, ok := integrationEvidence(current, result.Evidence)
	if !ok {
		t.Fatal("缺少真实判定证据")
	}
	body, err := readIntegrationEvidence(e.Config().DataDir, w.ID, proof)
	if err != nil {
		t.Fatal(err)
	}
	var record struct {
		Candidate sandbox.RunResult `json:"candidate_result"`
		Result    sandbox.RunResult `json:"result"`
	}
	if err = json.Unmarshal(body, &record); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(record.Candidate.Stdout, "HARNESS_ISOLATION_PROBE_PASSED") {
		t.Fatal("候选隔离探针未实际执行")
	}
	if strings.Contains(record.Result.Stdout, "HARNESS_ISOLATION_PROBE_PASSED") {
		t.Fatal("候选stdout泄露到可信判定协议流")
	}
}
