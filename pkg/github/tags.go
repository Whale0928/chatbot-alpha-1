package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// Tag는 /tags 응답에서 봇이 사용하는 필드만 발췌.
// Commit.SHA는 태그가 가리키는 커밋. 태그 자체의 sha와 다를 수 있다 (annotated tag).
type Tag struct {
	Name   string `json:"name"`
	Commit struct {
		SHA string `json:"sha"`
		URL string `json:"url"`
	} `json:"commit"`
}

// ListTags는 owner/repo의 모든 태그를 페이징하여 수집한다.
// 응답 순서는 GitHub default(보통 default branch 기준 최근순)이지만 호출자가
// 정렬을 가정하지 말 것 — 호출자가 모듈별 prefix 필터 + semver 정렬을 직접 수행.
func (c *Client) ListTags(ctx context.Context, owner, repo string) ([]Tag, error) {
	if owner == "" || repo == "" {
		return nil, fmt.Errorf("github: owner/repo is empty")
	}
	u := fmt.Sprintf("%s/repos/%s/%s/tags?per_page=100", c.baseURL, owner, repo)
	var all []Tag
	for u != "" {
		page, next, err := c.fetchTagPage(ctx, u)
		if err != nil {
			return nil, err
		}
		all = append(all, page...)
		u = next
	}
	return all, nil
}

func (c *Client) fetchTagPage(ctx context.Context, u string) ([]Tag, string, error) {
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
		return nil, "", fmt.Errorf("github: tags request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, "", fmt.Errorf("github: %s %d: %s", u, resp.StatusCode, string(body))
	}
	var page []Tag
	if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
		return nil, "", fmt.Errorf("github: decode tags: %w", err)
	}
	return page, parseNextLink(resp.Header.Get("Link")), nil
}
