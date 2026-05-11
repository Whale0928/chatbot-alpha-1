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
