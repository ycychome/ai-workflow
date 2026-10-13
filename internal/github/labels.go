package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
)

// Label is a repository label managed by the workflow service.
type Label struct {
	Name        string `json:"name"`
	Color       string `json:"color"`
	Description string `json:"description"`
}

const labelPageSize = 100

var labelColorPattern = regexp.MustCompile(`^[0-9a-fA-F]{6}$`)

type issueLabel struct {
	Name string `json:"name"`
}

// SyncManagedLabels ensures desired repository labels exist and synchronizes only
// issue/PR labels under prefix. Labels outside the prefix are always preserved.
// Existing repository labels are left untouched; only missing labels are created.
func (i *InstallationClient) SyncManagedLabels(ctx context.Context, owner, repo string, number int, prefix string, desired []Label) error {
	if strings.TrimSpace(owner) == "" || strings.TrimSpace(repo) == "" || number <= 0 {
		return errors.New("github: owner, repo, and positive issue number are required")
	}
	if strings.TrimSpace(prefix) == "" {
		return errors.New("github: managed label prefix is required")
	}
	wanted := make(map[string]Label, len(desired))
	for _, label := range desired {
		if strings.TrimSpace(label.Name) == "" || !strings.HasPrefix(label.Name, prefix) {
			return fmt.Errorf("github: desired label %q must have the managed prefix", label.Name)
		}
		if !labelColorPattern.MatchString(label.Color) {
			return fmt.Errorf("github: desired label %q must have a six-digit hex color", label.Name)
		}
		key := strings.ToLower(label.Name)
		if _, duplicate := wanted[key]; duplicate {
			return fmt.Errorf("github: duplicate desired label %q", label.Name)
		}
		wanted[key] = label
	}

	base := fmt.Sprintf("/repos/%s/%s", q(owner), q(repo))
	issuePath := base + fmt.Sprintf("/issues/%d", number)
	current, err := i.listIssueLabels(ctx, issuePath)
	if err != nil {
		return err
	}

	// Creating labels before attaching them makes the operation repeatable after
	// partial success or a lost response.
	for _, label := range desired {
		if err := i.ensureRepositoryLabel(ctx, base, label); err != nil {
			return err
		}
	}

	var add []string
	for _, label := range desired {
		key := strings.ToLower(label.Name)
		if _, exists := current[key]; !exists {
			add = append(add, label.Name)
		}
	}
	if len(add) > 0 {
		if err := i.request(ctx, http.MethodPost, issuePath+"/labels", map[string]any{"labels": add}, nil); err != nil {
			return err
		}
	}

	remove := make([]string, 0)
	for key, label := range current {
		if strings.HasPrefix(label.Name, prefix) {
			if _, keep := wanted[key]; !keep {
				remove = append(remove, label.Name)
			}
		}
	}
	sort.Strings(remove)
	for _, name := range remove {
		path := issuePath + "/labels/" + q(name)
		if err := i.request(ctx, http.MethodDelete, path, nil, nil); err != nil {
			// A concurrent writer may have already removed the label after our
			// paginated read; the desired state is still satisfied.
			if isAPIStatus(err, http.StatusNotFound) {
				continue
			}
			return err
		}
	}
	return nil
}

func (i *InstallationClient) listIssueLabels(ctx context.Context, issuePath string) (map[string]issueLabel, error) {
	labels := make(map[string]issueLabel)
	for page := 1; page <= 100; page++ {
		var got []issueLabel
		path := fmt.Sprintf("%s/labels?per_page=%d&page=%d", issuePath, labelPageSize, page)
		if err := i.request(ctx, http.MethodGet, path, nil, &got); err != nil {
			return nil, err
		}
		for _, label := range got {
			labels[strings.ToLower(label.Name)] = label
		}
		if len(got) < labelPageSize {
			return labels, nil
		}
	}
	return nil, errors.New("github: issue label pagination limit exceeded")
}

func (i *InstallationClient) ensureRepositoryLabel(ctx context.Context, base string, label Label) error {
	path := base + "/labels/" + q(label.Name)
	var existing Label
	err := i.request(ctx, http.MethodGet, path, nil, &existing)
	if err == nil {
		return nil
	}
	if !isAPIStatus(err, http.StatusNotFound) {
		return err
	}
	err = i.request(ctx, http.MethodPost, base+"/labels", label, nil)
	if err == nil {
		return nil
	}
	if isAPIStatus(err, http.StatusUnprocessableEntity) {
		// Another writer may have created the same label after our GET. Confirm
		// that the desired resource now exists before treating the race as success.
		var confirmed Label
		if getErr := i.request(ctx, http.MethodGet, path, nil, &confirmed); getErr == nil {
			return nil
		} else {
			return fmt.Errorf("github: create label %q failed and confirming it also failed: %w", label.Name, getErr)
		}
	}
	return err
}

func isAPIStatus(err error, status int) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == status
}
