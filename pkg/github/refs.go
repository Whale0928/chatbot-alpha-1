package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// Ref는 git ref 응답 (브랜치/태그 공통).
type Ref struct {
	Ref    string `json:"ref"` // "refs/heads/main" 또는 "refs/tags/sandbox-product/v1.0.0"
	NodeID string `json:"node_id"`
	URL    string `json:"url"`
	Object struct {
		SHA  string `json:"sha"`
		Type string `json:"type"` // "commit" / "tag"
		URL  string `json:"url"`
	} `json:"object"`
}

// GetRef는 특정 ref 의 sha 를 조회한다.
// ref 인자는 "refs/" prefix 없이 "heads/main", "tags/sandbox-product/v1.0.0" 형식.
// 존재하지 않으면 404 → 호출자가 not-found 분기 가능하도록 IsNotFound 헬퍼 노출.
func (c *Client) GetRef(ctx context.Context, owner, repo, ref string) (*Ref, error) {
	if owner == "" || repo == "" || ref == "" {
		return nil, fmt.Errorf("github: owner/repo/ref is empty")
	}
	u := fmt.Sprintf("%s/repos/%s/%s/git/ref/%s", c.baseURL, owner, repo, escapeContentsPath(ref))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	c.setCommonHeaders(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("github: getref request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrNotFound
	}
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("github: %s %d: %s", u, resp.StatusCode, string(body))
	}
	var out Ref
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("github: decode ref: %w", err)
	}
	return &out, nil
}

// CreateRef는 새 ref 를 만든다 (브랜치/태그 공통, lightweight tag 만 지원).
// ref 인자는 "refs/heads/<branch>" 또는 "refs/tags/<tag>" 전체 형식.
// 이미 존재하면 GitHub 가 422 를 반환하므로 호출자가 GetRef 로 사전 확인하거나 ErrAlreadyExists 분기.
func (c *Client) CreateRef(ctx context.Context, owner, repo, ref, sha string) (*Ref, error) {
	if owner == "" || repo == "" || ref == "" || sha == "" {
		return nil, fmt.Errorf("github: createref: owner/repo/ref/sha is empty")
	}
	u := fmt.Sprintf("%s/repos/%s/%s/git/refs", c.baseURL, owner, repo)
	body, _ := json.Marshal(map[string]string{"ref": ref, "sha": sha})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	c.setCommonHeaders(req)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("github: createref request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnprocessableEntity {
		// "Reference already exists" 422 가 흔함. 호출자가 분기하게.
		return nil, ErrAlreadyExists
	}
	if resp.StatusCode/100 != 2 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("github: %s %d: %s", u, resp.StatusCode, string(respBody))
	}
	var out Ref
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("github: decode createref: %w", err)
	}
	return &out, nil
}
