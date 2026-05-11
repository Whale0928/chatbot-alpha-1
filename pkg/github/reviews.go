package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Review 는 /pulls/{number}/reviews 응답 항목.
// State: APPROVED / CHANGES_REQUESTED / COMMENTED / DISMISSED / PENDING.
//
// 한 사용자가 여러 리뷰를 남길 수 있으므로 호출자는 user 별 latest 만 살려서
// "유효 리뷰 상태" 를 산정해야 한다.
type Review struct {
	User struct {
		Login string `json:"login"`
	} `json:"user"`
	State       string    `json:"state"`
	SubmittedAt time.Time `json:"submitted_at"`
}

// ListReviews 는 PR 의 모든 리뷰를 페이징하여 수집한다.
func (c *Client) ListReviews(ctx context.Context, owner, repo string, prNumber int) ([]Review, error) {
	if owner == "" || repo == "" {
		return nil, fmt.Errorf("github: listreviews: owner/repo is empty")
	}
	if prNumber <= 0 {
		return nil, fmt.Errorf("github: listreviews: invalid PR number %d", prNumber)
	}
	u := fmt.Sprintf("%s/repos/%s/%s/pulls/%d/reviews?per_page=100", c.baseURL, owner, repo, prNumber)
	var all []Review
	for u != "" {
		page, next, err := c.fetchReviewsPage(ctx, u)
		if err != nil {
			return nil, err
		}
		all = append(all, page...)
		u = next
	}
	return all, nil
}

func (c *Client) fetchReviewsPage(ctx context.Context, u string) ([]Review, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, "", err
	}
	c.setCommonHeaders(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("github: reviews request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, "", fmt.Errorf("github: %s %d: %s", u, resp.StatusCode, string(body))
	}
	var page []Review
	if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
		return nil, "", fmt.Errorf("github: decode reviews: %w", err)
	}
	return page, parseNextLink(resp.Header.Get("Link")), nil
}
