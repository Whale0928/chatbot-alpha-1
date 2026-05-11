package github

import (
	"context"
	"fmt"
	"net/http"
	"testing"
)

func TestListCheckRuns(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		assertCommonHeaders(t, r)
		if r.URL.Path != "/repos/owner/repo/commits/abc/check-runs" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if r.URL.Query().Get("per_page") != "100" {
			t.Errorf("per_page = %q", r.URL.Query().Get("per_page"))
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{
			"total_count": 3,
			"check_runs": [
				{"name":"ci pipeline / prepare","status":"completed","conclusion":"success","html_url":"u1"},
				{"name":"ci pipeline / unit-tests","status":"completed","conclusion":"success","html_url":"u2"},
				{"name":"ci pipeline / integration-tests","status":"in_progress","conclusion":"","html_url":"u3"}
			]
		}`)
	})
	runs, err := c.ListCheckRuns(context.Background(), "owner", "repo", "abc")
	if err != nil {
		t.Fatalf("ListCheckRuns: %v", err)
	}
	if len(runs) != 3 {
		t.Fatalf("runs = %d, want 3", len(runs))
	}
	if runs[0].Conclusion != "success" || runs[2].Status != "in_progress" {
		t.Errorf("runs = %+v", runs)
	}
}

func TestListReviews(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		assertCommonHeaders(t, r)
		if r.URL.Path != "/repos/owner/repo/pulls/42/reviews" {
			t.Errorf("path = %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `[
			{"user":{"login":"alice"},"state":"COMMENTED","submitted_at":"2026-05-11T16:00:00Z"},
			{"user":{"login":"alice"},"state":"APPROVED","submitted_at":"2026-05-11T16:30:00Z"},
			{"user":{"login":"bob"},"state":"CHANGES_REQUESTED","submitted_at":"2026-05-11T16:35:00Z"}
		]`)
	})
	rs, err := c.ListReviews(context.Background(), "owner", "repo", 42)
	if err != nil {
		t.Fatalf("ListReviews: %v", err)
	}
	if len(rs) != 3 {
		t.Fatalf("reviews = %d, want 3", len(rs))
	}
	if rs[1].State != "APPROVED" || rs[1].User.Login != "alice" {
		t.Errorf("[1] = %+v", rs[1])
	}
}
