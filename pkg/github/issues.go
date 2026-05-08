package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// bytesReader는 io.Reader를 만드는 단일 진입점. (http.NewRequest body용)
func bytesReader(b []byte) io.Reader { return bytes.NewReader(b) }

// Issue는 GitHub issues API 응답에서 봇이 사용하는 필드만.
// 풀 리퀘스트도 issues API에 섞여 나오므로 PullRequest 필드로 구분한다.
type Issue struct {
	Number      int        `json:"number"`
	Title       string     `json:"title"`
	State       string     `json:"state"` // "open" | "closed"
	Body        string     `json:"body"`
	HTMLURL     string     `json:"html_url"`
	User        User       `json:"user"`
	Labels      []Label    `json:"labels"`
	Assignees   []User     `json:"assignees"`
	Comments    int        `json:"comments"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	ClosedAt    *time.Time `json:"closed_at"`
	PullRequest *struct{}  `json:"pull_request,omitempty"` // 존재하면 PR — 필터링용
}

// IsPullRequest는 issues 응답에 섞여 들어오는 PR을 분리한다.
func (i Issue) IsPullRequest() bool { return i.PullRequest != nil }

// User는 작성자/담당자 정보. login만 사용.
type User struct {
	Login string `json:"login"`
}

// Label은 GitHub label. name만 사용.
type Label struct {
	Name string `json:"name"`
}

// ListIssuesOptions는 ListIssues의 필터.
//
// State: "open" | "closed" | "all" (기본 "all" 권장 — 주간 분석은 그 사이 닫힌 것도 봐야 함)
// Since: 이 시점 이후 업데이트된 이슈만 반환. zero이면 미적용.
// IncludePullRequests: false면 IsPullRequest() 인 항목을 결과에서 제외 (기본 false).
type ListIssuesOptions struct {
	State               string
	Since               time.Time
	IncludePullRequests bool
}

// ListIssues는 owner/repo의 이슈를 페이징하여 모두 수집한다.
// per_page=100 + Link 헤더 따라가기.
//
// 주의: GitHub /issues 엔드포인트는 PR도 포함한다. opts.IncludePullRequests=false면 결과에서 제외.
func (c *Client) ListIssues(ctx context.Context, owner, repo string, opts ListIssuesOptions) ([]Issue, error) {
	if owner == "" || repo == "" {
		return nil, fmt.Errorf("github: owner/repo is empty")
	}
	q := url.Values{}
	q.Set("per_page", "100")
	state := opts.State
	if state == "" {
		state = "all"
	}
	q.Set("state", state)
	if !opts.Since.IsZero() {
		q.Set("since", opts.Since.UTC().Format(time.RFC3339))
	}
	q.Set("sort", "updated")
	q.Set("direction", "desc")

	u := fmt.Sprintf("%s/repos/%s/%s/issues?%s", apiBase, owner, repo, q.Encode())
	var all []Issue
	for u != "" {
		page, next, err := c.fetchIssuePage(ctx, u)
		if err != nil {
			return nil, err
		}
		all = append(all, page...)
		u = next
	}

	if !opts.IncludePullRequests {
		filtered := all[:0]
		for _, it := range all {
			if it.IsPullRequest() {
				continue
			}
			filtered = append(filtered, it)
		}
		all = filtered
	}
	return all, nil
}

// CloseIssue는 PATCH /repos/{owner}/{repo}/issues/{number} 로 이슈를 닫는다.
// state_reason은 "completed" (완료로 마무리). "not_planned"가 필요하면 별도 옵션화.
//
// 응답 body는 무시하고 200 여부만 검사. 실패 시 4xx/5xx body 일부를 에러에 포함.
func (c *Client) CloseIssue(ctx context.Context, owner, repo string, number int) error {
	if owner == "" || repo == "" {
		return fmt.Errorf("github: owner/repo is empty")
	}
	if number <= 0 {
		return fmt.Errorf("github: invalid issue number: %d", number)
	}
	u := fmt.Sprintf("%s/repos/%s/%s/issues/%d", apiBase, owner, repo, number)
	body := []byte(`{"state":"closed","state_reason":"completed"}`)

	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, u, bytesReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("github: close request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("github: close #%d %d: %s", number, resp.StatusCode, string(respBody))
	}
	return nil
}

func (c *Client) fetchIssuePage(ctx context.Context, u string) (page []Issue, next string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", userAgent)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("github: request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, "", fmt.Errorf("github: %s %d: %s", u, resp.StatusCode, string(body))
	}
	if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
		return nil, "", fmt.Errorf("github: decode issues: %w", err)
	}
	return page, parseNextLink(resp.Header.Get("Link")), nil
}
