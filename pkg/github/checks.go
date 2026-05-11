package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// CheckRun 은 /commits/{ref}/check-runs 응답 항목.
//
// Status:     queued / in_progress / completed
// Conclusion: success / failure / neutral / cancelled / skipped / timed_out / action_required (Status=completed 일 때만 의미)
type CheckRun struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	HTMLURL    string `json:"html_url"`
}

type apiCheckRuns struct {
	TotalCount int        `json:"total_count"`
	CheckRuns  []CheckRun `json:"check_runs"`
}

// ListCheckRuns 는 ref(보통 PR head sha) 의 check run 목록을 모두 가져온다.
// PR 의 CI 상태 표시에 사용.
func (c *Client) ListCheckRuns(ctx context.Context, owner, repo, ref string) ([]CheckRun, error) {
	if owner == "" || repo == "" || ref == "" {
		return nil, fmt.Errorf("github: listcheckruns: owner/repo/ref is empty")
	}
	u := fmt.Sprintf("%s/repos/%s/%s/commits/%s/check-runs?per_page=100", c.baseURL, owner, repo, ref)
	var all []CheckRun
	for u != "" {
		page, next, err := c.fetchCheckRunsPage(ctx, u)
		if err != nil {
			return nil, err
		}
		all = append(all, page...)
		u = next
	}
	return all, nil
}

func (c *Client) fetchCheckRunsPage(ctx context.Context, u string) ([]CheckRun, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, "", err
	}
	c.setCommonHeaders(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("github: check-runs request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, "", fmt.Errorf("github: %s %d: %s", u, resp.StatusCode, string(body))
	}
	var raw apiCheckRuns
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, "", fmt.Errorf("github: decode check-runs: %w", err)
	}
	return raw.CheckRuns, parseNextLink(resp.Header.Get("Link")), nil
}
