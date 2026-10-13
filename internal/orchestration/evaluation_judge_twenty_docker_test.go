package orchestration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"testing"

	"github.com/ycychome/ai-workflow/internal/domain"
	"github.com/ycychome/ai-workflow/internal/sandbox"
	"github.com/ycychome/ai-workflow/internal/store"
)

func TestEvaluationJudgeDockerTwentyFrozenCandidates(t *testing.T) {
	alphaRequireDocker(t)
	body, err := os.ReadFile("../../eval/variants.json")
	if err != nil {
		t.Fatal(err)
	}
	var variants map[string]struct {
		Edits []struct{ Path, Before, After string } `json:"edits"`
	}
	if err = json.Unmarshal(body, &variants); err != nil {
		t.Fatal(err)
	}
	for n := 1; n <= 10; n++ {
		taskID := fmt.Sprintf("T%02d", n)
		for _, kind := range []string{"defect", "fix"} {
			t.Run(taskID+"/"+kind, func(t *testing.T) {
				e, ctx, w, manifest := evaluationJudgeDockerFixture(t, func(files map[string]sandbox.File) {
					if kind != "defect" {
						return
					}
					for _, edit := range variants[taskID].Edits {
						file := files[edit.Path]
						if bytes.Count(file.Data, []byte(edit.Before)) != 1 {
							t.Fatalf("缺陷变体锚点不唯一：%s", edit.Path)
						}
						file.Data = bytes.Replace(file.Data, []byte(edit.Before), []byte(edit.After), 1)
						files[edit.Path] = file
					}
				})
				if err := e.Store.Update(ctx, w.ID, func(current *domain.Workflow) ([]store.Action, error) {
					current.Evaluation.TaskID = taskID
					return nil, nil
				}); err != nil {
					t.Fatal(err)
				}
				judgment, err := e.JudgeEvaluation(ctx, w.ID, manifest)
				if err != nil {
					t.Fatal(err)
				}
				wantPass := kind == "fix"
				if judgment.Passed != wantPass || !judgment.StoppedConfirmed || judgment.Evidence == "" {
					t.Fatalf("%s/%s 判定错误：%+v", taskID, kind, judgment)
				}
				current, err := e.Store.Get(ctx, w.ID)
				if err != nil {
					t.Fatal(err)
				}
				ev, ok := integrationEvidence(current, judgment.Evidence)
				if !ok {
					t.Fatal("判定证据未入库")
				}
				proof, err := readIntegrationEvidence(e.Config().DataDir, w.ID, ev)
				if err != nil {
					t.Fatal(err)
				}
				var recorded struct {
					Result sandbox.RunResult `json:"result"`
				}
				if err = json.Unmarshal(proof, &recorded); err != nil {
					t.Fatal(err)
				}
				if !wantPass && (judgment.ExitCode == 0 || bytes.Contains([]byte(recorded.Result.Stdout), []byte("[build failed]")) || bytes.Contains([]byte(recorded.Result.Stdout), []byte("候选服务未能提供真实健康接口"))) {
					t.Fatalf("缺陷必须由真实断言拒绝，不能用编译或服务不可用冒充：%s", recorded.Result.Stdout)
				}
			})
		}
	}
}
