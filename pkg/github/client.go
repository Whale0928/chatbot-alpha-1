// Package github은 GitHub REST API에 대한 가벼운 wrapper다.
// google/go-github 라이브러리를 들이지 않고 net/http만 사용.
// 봇이 실제로 쓰는 호출만 두고, 응답에서 필요한 필드만 파싱한다.
package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

const (
	apiBase = "https://api.github.com"
	// userAgent는 GitHub API 요청 식별용. GitHub은 UA 없는 요청을 거부한다.
	userAgent = "chatbot-alpha-1"
)

// Client는 GitHub API 호출자. 토큰 한 개 + 공유 *http.Client 만 보관.
type Client struct {
	token string
	http  *http.Client
}

// NewClient는 personal access token으로 클라이언트를 만든다.
// 빈 토큰을 넘기면 ErrMissingToken을 반환한다 (public-only API라도 rate limit이 너무 낮아 의미 없음).
func NewClient(token string) (*Client, error) {
	if token == "" {
		return nil, ErrMissingToken
	}
	return &Client{
		token: token,
		http:  &http.Client{Timeout: 15 * time.Second},
	}, nil
}

// ErrMissingToken은 NewClient에 빈 토큰이 들어왔을 때 반환된다.
var ErrMissingToken = fmt.Errorf("github: token is empty")

// Repo는 GitHub repository 응답에서 봇이 실제로 사용하는 필드만.
type Repo struct {
	Name            string    `json:"name"`
	FullName        string    `json:"full_name"`
	Archived        bool      `json:"archived"`
	HasIssues       bool      `json:"has_issues"`
	OpenIssuesCount int       `json:"open_issues_count"`
	UpdatedAt       time.Time `json:"updated_at"`
	HTMLURL         string    `json:"html_url"`
}

// ListOrgRepos는 organization의 모든 레포를 페이징하여 수집한다.
// per_page=100 + Link 헤더 추적. type=all (public + private 모두 — 토큰 권한에 따라 자동 필터됨).
//
// 호출자는 archived나 has_issues 같은 후처리를 직접 적용한다.
func (c *Client) ListOrgRepos(ctx context.Context, org string) ([]Repo, error) {
	if org == "" {
		return nil, fmt.Errorf("github: org is empty")
	}
	url := fmt.Sprintf("%s/orgs/%s/repos?per_page=100&type=all&sort=updated", apiBase, org)
	var all []Repo
	for url != "" {
		page, next, err := c.fetchRepoPage(ctx, url)
		if err != nil {
			return nil, err
		}
		all = append(all, page...)
		url = next
	}
	return all, nil
}

func (c *Client) fetchRepoPage(ctx context.Context, url string) (page []Repo, next string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
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
		return nil, "", fmt.Errorf("github: %s %d: %s", url, resp.StatusCode, string(body))
	}
	if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
		return nil, "", fmt.Errorf("github: decode repos: %w", err)
	}
	next = parseNextLink(resp.Header.Get("Link"))
	return page, next, nil
}

// parseNextLink는 GitHub Link 헤더에서 rel="next" URL만 추출한다.
//
// 예시 Link 헤더:
//
//	<https://api.github.com/orgs/foo/repos?page=2>; rel="next", <https://api.github.com/orgs/foo/repos?page=5>; rel="last"
//
// 다음 페이지가 없으면 빈 문자열.
func parseNextLink(linkHeader string) string {
	if linkHeader == "" {
		return ""
	}
	for _, part := range splitComma(linkHeader) {
		urlPart, relPart, ok := splitSemicolon(part)
		if !ok {
			continue
		}
		if relPart != `rel="next"` {
			continue
		}
		// urlPart 형식: "<https://...>"
		if len(urlPart) < 2 || urlPart[0] != '<' || urlPart[len(urlPart)-1] != '>' {
			continue
		}
		return urlPart[1 : len(urlPart)-1]
	}
	return ""
}

func splitComma(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == ',' {
			out = append(out, trimSpaces(s[start:i]))
			start = i + 1
		}
	}
	out = append(out, trimSpaces(s[start:]))
	return out
}

func splitSemicolon(s string) (left, right string, ok bool) {
	for i := 0; i < len(s); i++ {
		if s[i] == ';' {
			return trimSpaces(s[:i]), trimSpaces(s[i+1:]), true
		}
	}
	return "", "", false
}

func trimSpaces(s string) string {
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\t') {
		s = s[1:]
	}
	for len(s) > 0 && (s[len(s)-1] == ' ' || s[len(s)-1] == '\t') {
		s = s[:len(s)-1]
	}
	return s
}
