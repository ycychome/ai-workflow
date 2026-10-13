package orchestration

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"github.com/ycychome/ai-workflow/internal/sandbox"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type ciExecutorRunnerFake struct {
	Results []sandbox.RunResult
	Err     error
	Calls   int
}

func (f *ciExecutorRunnerFake) Run(context.Context, *sandbox.Workspace, []string, time.Duration) (sandbox.RunResult, error) {
	i := f.Calls
	f.Calls++
	if f.Err != nil {
		return sandbox.RunResult{}, f.Err
	}
	return f.Results[i], nil
}
func TestCIExecutorCommandsRequireActualSuccess(t *testing.T) {
	cases := []struct {
		name      string
		results   []sandbox.RunResult
		err       error
		wantCalls int
	}{
		{"成功", []sandbox.RunResult{{}, {}}, nil, 2},
		{"测试失败", []sandbox.RunResult{{ExitCode: 1}}, nil, 1},
		{"超时", []sandbox.RunResult{{TimedOut: true}}, nil, 1},
		{"沙箱错误", nil, errors.New("不可用"), 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &ciExecutorRunnerFake{Results: tc.results, Err: tc.err}
			results, err := runCIExecutorCommands(context.Background(), f, nil, [][]string{{"go", "test", "./..."}, {"go", "vet", "./..."}}, time.Minute)
			if (err == nil) != (tc.name == "成功") || f.Calls != tc.wantCalls || len(results) != tc.wantCalls {
				t.Fatalf("命令结果没有准确阻断：%v %+v", err, results)
			}
		})
	}
	for _, commands := range [][][]string{nil, {{}}, {{" "}}} {
		f := &ciExecutorRunnerFake{}
		if _, err := runCIExecutorCommands(context.Background(), f, nil, commands, time.Minute); err == nil || f.Calls != 0 {
			t.Fatal("空验证配置不能签发")
		}
	}
}
func TestCIExecutorPrivateKeyBoundary(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	filename := filepath.Join(t.TempDir(), "private")
	encoded := []byte(base64.StdEncoding.EncodeToString(private))
	if err = os.WriteFile(filename, encoded, 0600); err != nil {
		t.Fatal(err)
	}
	key, err := readCIExecutorKey(filename, base64.StdEncoding.EncodeToString(public))
	if err != nil || !key.Equal(private) {
		t.Fatal("受信密钥读取失败")
	}
	if err = os.Chmod(filename, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err = readCIExecutorKey(filename, base64.StdEncoding.EncodeToString(public)); err == nil || strings.Contains(err.Error(), filename) {
		t.Fatal("密钥权限或路径隐藏合同错误")
	}
	if err = os.Chmod(filename, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = readCIExecutorKey(filename, base64.StdEncoding.EncodeToString(make([]byte, 32))); err == nil {
		t.Fatal("错误公钥被接受")
	}
	link := filename + "-link"
	if err = os.Symlink(filename, link); err != nil {
		t.Fatal(err)
	}
	if _, err = readCIExecutorKey(link, base64.StdEncoding.EncodeToString(public)); err == nil {
		t.Fatal("私钥符号链接被接受")
	}
}
func TestCIExecutorAtomicReceipt(t *testing.T) {
	p := filepath.Join(t.TempDir(), "receipts", "receipt.json")
	if err := writeCIExecutorJSON(p, map[string]string{"state": "first"}); err != nil {
		t.Fatal(err)
	}
	if err := writeCIExecutorJSON(p, map[string]string{"state": "second"}); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(p)
	if err != nil || string(body) != "{\"state\":\"second\"}\n" {
		t.Fatalf("回执替换错误：%s %v", body, err)
	}
	info, err := os.Stat(p)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("回执权限错误")
	}
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(p), ".receipt-*"))
	if err != nil || len(matches) != 0 {
		t.Fatal("遗留暂存回执")
	}
}
func TestCIReceiptMustFollowRemoteCompletion(t *testing.T) {
	p, c, receipt, client, now := ciTestFixture()
	// 使用已签名有效回执，模拟 GitHub 最新完成时间晚于独立重验。
	client.run.UpdatedAt = now.Add(-time.Second)
	client.latest.UpdatedAt = client.run.UpdatedAt
	if record, err := FetchTrustedCIRecord(context.Background(), client, p, c, receipt, now); err == nil || record.Verdict == "PASS" {
		t.Fatal("独立重验不能早于远端完成")
	}
}
