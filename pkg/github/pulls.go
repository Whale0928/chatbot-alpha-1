package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// PullRequest 는 /pulls 응답에서 봇이 사용하는 필드만.
type PullRequest struct {
	Number  int    `json:"number"`
	State   string `json:"state"` // "open" / "closed"
	Title   string `json:"title"`
	HTMLURL string `json:"html_url"`
	Body    string `json:"body"`

	Head struct {
		SHA string `json:"sha"`
		Ref string `json:"ref"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`

	Mergeable      *bool  `json:"mergeable"`       // nil = GitHub 이 아직 계산 중
	MergeableState string `json:"mergeable_state"` // "clean" / "blocked" / "dirty" / "unknown" / "unstable"
	Merged         bool   `json:"merged"`
}

// CreatePullRequestInput 는 PR 생성 입력.
type CreatePullRequestInput struct {
	Title string
	Body  string
	Head  string // 예: "main"
	Base  string // 예: "release/sandbox-product"
	Draft bool
}

// CreatePullRequest 는 새 PR 을 생성한다 (Pulls API).
// 동일 head/base 조합으로 이미 열린 PR 이 있으면 422.
func (c *Client) CreatePullRequest(ctx context.Context, owner, repo string, in CreatePullRequestInput) (*PullRequest, error) {
	if owner == "" || repo == "" {
		return nil, fmt.Errorf("github: createpr: owner/repo is empty")
	}
	if in.Title == "" || in.Head == "" || in.Base == "" {
		return nil, fmt.Errorf("github: createpr: title/head/base is empty")
	}

	body := map[string]any{
		"title": in.Title,
		"head":  in.Head,
		"base":  in.Base,
		"body":  in.Body,
		"draft": in.Draft,
	}
	bs, _ := json.Marshal(body)

	u := fmt.Sprintf("%s/repos/%s/%s/pulls", c.baseURL, owner, repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(bs))
	if err != nil {
		return nil, err
	}
	c.setCommonHeaders(req)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("github: createpr request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("github: %s %d: %s", u, resp.StatusCode, string(respBody))
	}
	var out PullRequest
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("github: decode createpr: %w", err)
	}
	return &out, nil
}

// ListPullRequestsByHead 는 owner:head 와 base 가 모두 일치하는 PR 만 필터해 반환한다.
// state 는 "open"/"closed"/"all" 중 하나. head 는 "branch" 또는 "owner:branch" 형식.
//
// 봇이 새 PR 만들기 전 같은 조합의 open PR 존재 여부 확인용 (멱등성 보장).
func (c *Client) ListPullRequestsByHead(ctx context.Context, owner, repo, head, base, state string) ([]PullRequest, error) {
	if owner == "" || repo == "" {
		return nil, fmt.Errorf("github: listpr: owner/repo is empty")
	}
	// head/base/state 는 query param 으로 — 빈 값은 GitHub 가 기본값 적용.
	q := []string{"per_page=100"}
	if head != "" {
		q = append(q, "head="+head)
	}
	if base != "" {
		q = append(q, "base="+base)
	}
	if state != "" {
		q = append(q, "state="+state)
	}
	u := fmt.Sprintf("%s/repos/%s/%s/pulls?%s", c.baseURL, owner, repo, joinQuery(q))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	c.setCommonHeaders(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("github: listpr request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("github: %s %d: %s", u, resp.StatusCode, string(body))
	}
	var out []PullRequest
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("github: decode listpr: %w", err)
	}
	return out, nil
}

// joinQuery 는 query param slice 를 "&" 로 join. url.Values 사용보다 단순.
func joinQuery(parts []string) string {
	if len(parts) == 0 {
		return ""
	}
	out := parts[0]
	for _, p := range parts[1:] {
		out += "&" + p
	}
	return out
}

// UpdatePullRequestInput 은 PATCH PR 입력. 빈 값은 갱신 X (GitHub default 동작).
type UpdatePullRequestInput struct {
	Title string
	Body  string
}

// UpdatePullRequest 는 PR title/body 를 PATCH 로 갱신한다 (멱등 갱신).
// title/body 가 둘 다 비어있으면 에러 — 의도 모호한 호출 거부.
func (c *Client) UpdatePullRequest(ctx context.Context, owner, repo string, number int, in UpdatePullRequestInput) (*PullRequest, error) {
	if owner == "" || repo == "" {
		return nil, fmt.Errorf("github: updatepr: owner/repo is empty")
	}
	if number <= 0 {
		return nil, fmt.Errorf("github: updatepr: invalid number %d", number)
	}
	body := map[string]any{}
	if in.Title != "" {
		body["title"] = in.Title
	}
	if in.Body != "" {
		body["body"] = in.Body
	}
	if len(body) == 0 {
		return nil, fmt.Errorf("github: updatepr: title/body 둘 다 비어있음")
	}
	bs, _ := json.Marshal(body)

	u := fmt.Sprintf("%s/repos/%s/%s/pulls/%d", c.baseURL, owner, repo, number)
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, u, bytes.NewReader(bs))
	if err != nil {
		return nil, err
	}
	c.setCommonHeaders(req)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("github: updatepr request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("github: %s %d: %s", u, resp.StatusCode, string(respBody))
	}
	var out PullRequest
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("github: decode updatepr: %w", err)
	}
	return &out, nil
}

// GetPullRequest 는 PR 단건을 조회한다 (Slice 7 폴링에서 사용).
func (c *Client) GetPullRequest(ctx context.Context, owner, repo string, number int) (*PullRequest, error) {
	if owner == "" || repo == "" {
		return nil, fmt.Errorf("github: getpr: owner/repo is empty")
	}
	if number <= 0 {
		return nil, fmt.Errorf("github: getpr: invalid number %d", number)
	}
	u := fmt.Sprintf("%s/repos/%s/%s/pulls/%d", c.baseURL, owner, repo, number)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	c.setCommonHeaders(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("github: getpr request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrNotFound
	}
	if resp.StatusCode/100 != 2 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("github: %s %d: %s", u, resp.StatusCode, string(respBody))
	}
	var out PullRequest
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("github: decode getpr: %w", err)
	}
	return &out, nil
}
