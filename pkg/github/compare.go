package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// Comparison은 /compare 응답에서 봇이 사용하는 필드.
//
// 250 커밋 한계: GitHub는 base..head 사이 250 커밋까지만 반환한다. 그 이상이면
// 호출자가 ListCommits 로 폴백해야 한다. TotalCommits 가 len(Commits) 보다 크면 추가 분기 필요.
type Comparison struct {
	Status       string           `json:"status"` // ahead/behind/identical/diverged
	AheadBy      int              `json:"ahead_by"`
	BehindBy     int              `json:"behind_by"`
	TotalCommits int              `json:"total_commits"`
	Commits      []Commit         `json:"-"` // apiCommit 으로 받아 정규화
	Files        []ComparisonFile `json:"files"`
}

// ComparisonFile은 changed file 1건의 통계.
type ComparisonFile struct {
	Filename  string `json:"filename"`
	Status    string `json:"status"` // added/modified/removed/renamed
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
	Changes   int    `json:"changes"`
}

// apiComparison은 raw JSON 매핑용.
type apiComparison struct {
	Status       string           `json:"status"`
	AheadBy      int              `json:"ahead_by"`
	BehindBy     int              `json:"behind_by"`
	TotalCommits int              `json:"total_commits"`
	Commits      []apiCommit      `json:"commits"`
	Files        []ComparisonFile `json:"files"`
}

// CompareCommits는 base...head 의 diff/커밋 목록을 반환한다.
// base/head 는 sha, tag, branch 모두 허용. tag 사용 시 "sandbox-product/v1.0.0" 처럼
// slash 포함될 수 있으므로 PathEscape 한다.
//
// 호출자는 TotalCommits > len(Commits) 인 경우 (250건 초과) ListCommits 로 보강하도록 한다.
func (c *Client) CompareCommits(ctx context.Context, owner, repo, base, head string) (*Comparison, error) {
	if owner == "" || repo == "" {
		return nil, fmt.Errorf("github: owner/repo is empty")
	}
	if base == "" || head == "" {
		return nil, fmt.Errorf("github: compare base/head is empty")
	}
	// PathEscape는 slash 도 인코딩하지만 GitHub은 base...head 구분자로 "..." 를 쓴다.
	// base/head 자체에 slash 가 있으면 (태그) 인코딩 필요. "..." 는 separator로 그대로 둔다.
	u := fmt.Sprintf("%s/repos/%s/%s/compare/%s...%s", c.baseURL, owner, repo, url.PathEscape(base), url.PathEscape(head))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", userAgent)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("github: compare request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("github: %s %d: %s", u, resp.StatusCode, string(body))
	}
	var raw apiComparison
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("github: decode compare: %w", err)
	}
	out := &Comparison{
		Status:       raw.Status,
		AheadBy:      raw.AheadBy,
		BehindBy:     raw.BehindBy,
		TotalCommits: raw.TotalCommits,
		Files:        raw.Files,
	}
	out.Commits = make([]Commit, len(raw.Commits))
	for i, ac := range raw.Commits {
		out.Commits[i] = ac.toCommit()
	}
	return out, nil
}
