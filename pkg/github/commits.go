package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// Commit은 /commits 응답에서 봇이 사용하는 필드만 발췌한 형태.
//
// GitHub commit JSON은 nested 구조다:
//
//	{ "sha": "...", "author": { "login": "..." }, "commit": { "author": { "name": "...", "date": "..." }, "message": "..." } }
//
// 우리는 nested unmarshal 대신 ApiCommit 중간 타입을 두고 정규화해 평탄한 Commit으로 변환.
type Commit struct {
	SHA         string
	AuthorLogin string    // GitHub user login (없으면 "")
	AuthorName  string    // commit.author.name (실제 git user.name)
	Date        time.Time // commit.author.date (작성 시점)
	Message     string    // commit.message 전체 (호출자가 첫 줄만 자름)
	HTMLURL     string
}

// apiCommit은 GitHub /commits 응답 한 항목의 raw JSON 구조.
type apiCommit struct {
	SHA     string `json:"sha"`
	HTMLURL string `json:"html_url"`
	Author  *User  `json:"author"` // null일 수 있음 (외부 contributor 등)
	Commit  struct {
		Message string `json:"message"`
		Author  struct {
			Name string    `json:"name"`
			Date time.Time `json:"date"`
		} `json:"author"`
	} `json:"commit"`
}

func (a apiCommit) toCommit() Commit {
	out := Commit{
		SHA:        a.SHA,
		AuthorName: a.Commit.Author.Name,
		Date:       a.Commit.Author.Date,
		Message:    a.Commit.Message,
		HTMLURL:    a.HTMLURL,
	}
	if a.Author != nil {
		out.AuthorLogin = a.Author.Login
	}
	return out
}

// ListCommitsOptions는 필터 옵션.
//
// Since: 이 시점 이후 커밋만. zero이면 미적용.
// Until: 이 시점까지의 커밋만. zero이면 미적용.
// Branch: 특정 브랜치 (sha 파라미터). 비우면 default branch.
type ListCommitsOptions struct {
	Since  time.Time
	Until  time.Time
	Branch string
}

// ListCommits는 owner/repo의 커밋을 페이징하여 모두 수집한다.
// per_page=100 + Link 헤더 추적. 결과는 GitHub API default(시간 역순) 그대로.
//
// 7일치 활성 레포 기준 보통 50~200건 — 페이징 1~2번이면 충분.
func (c *Client) ListCommits(ctx context.Context, owner, repo string, opts ListCommitsOptions) ([]Commit, error) {
	if owner == "" || repo == "" {
		return nil, fmt.Errorf("github: owner/repo is empty")
	}
	q := url.Values{}
	q.Set("per_page", "100")
	if !opts.Since.IsZero() {
		q.Set("since", opts.Since.UTC().Format(time.RFC3339))
	}
	if !opts.Until.IsZero() {
		q.Set("until", opts.Until.UTC().Format(time.RFC3339))
	}
	if opts.Branch != "" {
		q.Set("sha", opts.Branch)
	}

	u := fmt.Sprintf("%s/repos/%s/%s/commits?%s", c.baseURL, owner, repo, q.Encode())
	var all []Commit
	for u != "" {
		page, next, err := c.fetchCommitPage(ctx, u)
		if err != nil {
			return nil, err
		}
		all = append(all, page...)
		u = next
	}
	return all, nil
}

func (c *Client) fetchCommitPage(ctx context.Context, u string) (page []Commit, next string, err error) {
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
		return nil, "", fmt.Errorf("github: commits request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		// 빈 레포(409 Conflict, "Git Repository is empty")는 정상 케이스 — 빈 결과로 처리.
		if resp.StatusCode == http.StatusConflict {
			return nil, "", nil
		}
		return nil, "", fmt.Errorf("github: %s %d: %s", u, resp.StatusCode, string(body))
	}
	var raw []apiCommit
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, "", fmt.Errorf("github: decode commits: %w", err)
	}
	page = make([]Commit, len(raw))
	for i, r := range raw {
		page[i] = r.toCommit()
	}
	return page, parseNextLink(resp.Header.Get("Link")), nil
}
