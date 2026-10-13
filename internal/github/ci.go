package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
)

// ActionsWorkflow 是 GitHub 返回的工作流注册身份；名称不用于信任判断。
type ActionsWorkflow struct {
	ID    int64  `json:"id"`
	Path  string `json:"path"`
	State string `json:"state"`
}

// ActionsPullRequest 记录该运行关联的 PR 快照，不能从显示标题或分支名推导身份。
type ActionsPullRequest struct {
	Number int `json:"number"`
	Head   struct {
		SHA  string     `json:"sha"`
		Repo Repository `json:"repo"`
	} `json:"head"`
	Base struct {
		SHA  string     `json:"sha"`
		Repo Repository `json:"repo"`
	} `json:"base"`
}

// ActionsRun 保留身份核验需要的 API 原始字段，不将 success 自动解释为门禁通过。
type ActionsRun struct {
	ID             int64                `json:"id"`
	WorkflowID     int64                `json:"workflow_id"`
	RunAttempt     int64                `json:"run_attempt"`
	Path           string               `json:"path"`
	Event          string               `json:"event"`
	HeadSHA        string               `json:"head_sha"`
	Status         string               `json:"status"`
	Conclusion     string               `json:"conclusion"`
	Repository     Repository           `json:"repository"`
	HeadRepository Repository           `json:"head_repository"`
	PullRequests   []ActionsPullRequest `json:"pull_requests"`
	CreatedAt      time.Time            `json:"created_at"`
	UpdatedAt      time.Time            `json:"updated_at"`
	HTMLURL        string               `json:"html_url"`
}

// RepositoryFileIdentity 只读取 Git blob 身份，不执行或解释仓库 YAML。
type RepositoryFileIdentity struct {
	Type string `json:"type"`
	Path string `json:"path"`
	SHA  string `json:"sha"`
}

func validCIRepo(owner, repo string) bool {
	return owner != "" && repo != "" && !strings.ContainsAny(owner+repo, "/\\\r\n")
}
func (i *InstallationClient) GetActionsWorkflow(ctx context.Context, owner, repo string, workflowID int64) (ActionsWorkflow, error) {
	var out ActionsWorkflow
	if !validCIRepo(owner, repo) || workflowID <= 0 {
		return out, errors.New("GitHub：工作流仓库或编号无效")
	}
	err := i.request(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/%s/actions/workflows/%d", q(owner), q(repo), workflowID), nil, &out)
	if err == nil && out.ID != workflowID {
		return out, errors.New("GitHub：工作流响应身份不匹配")
	}
	return out, err
}
func (i *InstallationClient) GetActionsRun(ctx context.Context, owner, repo string, runID int64) (ActionsRun, error) {
	var out ActionsRun
	if !validCIRepo(owner, repo) || runID <= 0 {
		return out, errors.New("GitHub：CI 运行仓库或编号无效")
	}
	err := i.request(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/%s/actions/runs/%d", q(owner), q(repo), runID), nil, &out)
	if err == nil && out.ID != runID {
		return out, errors.New("GitHub：CI 运行响应身份不匹配")
	}
	return out, err
}
func (i *InstallationClient) GetActionsRunAttempt(ctx context.Context, owner, repo string, runID, attempt int64) (ActionsRun, error) {
	var out ActionsRun
	if !validCIRepo(owner, repo) || runID <= 0 || attempt <= 0 {
		return out, errors.New("GitHub：CI 运行或重试编号无效")
	}
	err := i.request(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/%s/actions/runs/%d/attempts/%d", q(owner), q(repo), runID, attempt), nil, &out)
	if err == nil && (out.ID != runID || out.RunAttempt != attempt) {
		return out, errors.New("GitHub：CI 重试响应身份不匹配")
	}
	return out, err
}
func (i *InstallationClient) GetRepositoryFileIdentity(ctx context.Context, owner, repo, filePath, ref string) (RepositoryFileIdentity, error) {
	var out RepositoryFileIdentity
	if !validCIRepo(owner, repo) || ref == "" || filePath == "" || path.Clean(filePath) != filePath || strings.HasPrefix(filePath, "/") || strings.HasPrefix(filePath, "../") || strings.ContainsAny(filePath, "\\\r\n") {
		return out, errors.New("GitHub：文件身份查询参数无效")
	}
	segments := strings.Split(filePath, "/")
	for n := range segments {
		segments[n] = url.PathEscape(segments[n])
	}
	endpoint := "/repos/" + q(owner) + "/" + q(repo) + "/contents/" + strings.Join(segments, "/") + "?ref=" + url.QueryEscape(ref)
	err := i.request(ctx, http.MethodGet, endpoint, nil, &out)
	if err == nil && (out.Type != "file" || out.Path != filePath || out.SHA == "") {
		return out, errors.New("GitHub：文件响应缺少匹配的普通文件身份")
	}
	return out, err
}

// CIPullRequestSnapshot 同时读取 PR 身份及 GitHub 当前合成合并提交，避免将任意双亲提交冒充 test-merge。
type CIPullRequestSnapshot struct {
	PullRequest
	MergeCommitSHA string `json:"merge_commit_sha"`
	Mergeable      *bool  `json:"mergeable"`
}

func (i *InstallationClient) GetCIPullRequestSnapshot(ctx context.Context, owner, repo string, number int) (CIPullRequestSnapshot, error) {
	var out CIPullRequestSnapshot
	if !validCIRepo(owner, repo) || number <= 0 {
		return out, errors.New("GitHub：CI PR 快照查询参数无效")
	}
	err := i.request(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/%s/pulls/%d", q(owner), q(repo), number), nil, &out)
	if err == nil && out.Number != number {
		return out, errors.New("GitHub：CI PR 响应编号不匹配")
	}
	return out, err
}
