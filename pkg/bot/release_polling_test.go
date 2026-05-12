package bot

import (
	"strings"
	"testing"
	"time"

	"chatbot-alpha-1/pkg/github"
	"chatbot-alpha-1/pkg/release"
)

func TestRenderPollPanelEmbed(t *testing.T) {
	module, ok := release.FindModule("admin")
	if !ok {
		t.Fatal("admin module not found")
	}
	rc := &ReleaseContext{
		Module:     module,
		NewVersion: release.Version{Major: 1, Minor: 1, Patch: 3},
		PRNumber:   591,
	}
	pr := &github.PullRequest{Mergeable: boolPtr(true), MergeableState: "blocked"}
	checks := []github.CheckRun{
		{Name: "unit", Status: "completed", Conclusion: "success"},
		{Name: "integration", Status: "in_progress"},
		{Name: "rule", Status: "completed", Conclusion: "failure"},
	}

	got := renderPollPanel(rc, pr, checks, nil)

	if got.Title != "머지 추적 — 실패 (rule)" {
		t.Fatalf("title = %q", got.Title)
	}
	if got.Color != colorBad {
		t.Fatalf("color = %#x", got.Color)
	}
	if got.Author == nil || got.Author.Name != "백엔드 · admin/v1.1.3 · PR #591" {
		t.Fatalf("author = %#v", got.Author)
	}
	if !strings.Contains(got.Description, "통과 1 · 진행 1 · 실패 1") {
		t.Fatalf("description = %q", got.Description)
	}
	if len(got.Fields) != 4 {
		t.Fatalf("fields = %d", len(got.Fields))
	}
	if !strings.Contains(got.Fields[0].Value, "✓ unit") {
		t.Fatalf("passed field = %q", got.Fields[0].Value)
	}
	if !strings.Contains(got.Fields[1].Value, "▶ integration") || !strings.Contains(got.Fields[1].Value, "✗ rule") {
		t.Fatalf("running/failed field = %q", got.Fields[1].Value)
	}
}

func TestSummarizeReviews(t *testing.T) {
	base := time.Date(2026, 5, 11, 16, 0, 0, 0, time.UTC)
	mk := func(login, state string, off time.Duration) github.Review {
		r := github.Review{State: state, SubmittedAt: base.Add(off)}
		r.User.Login = login
		return r
	}
	cases := []struct {
		name string
		in   []github.Review
		want string
	}{
		{"empty", nil, "0 명 승인"},
		{
			"single approval",
			[]github.Review{mk("alice", "APPROVED", time.Minute)},
			"1 명 승인 (alice)",
		},
		{
			"user latest wins — comment then approval",
			[]github.Review{
				mk("alice", "COMMENTED", time.Minute),
				mk("alice", "APPROVED", 2*time.Minute),
			},
			"1 명 승인 (alice)",
		},
		{
			"changes requested takes priority",
			[]github.Review{
				mk("alice", "APPROVED", time.Minute),
				mk("bob", "CHANGES_REQUESTED", 2*time.Minute),
			},
			"1 명 변경 요청 (bob)",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := summarizeReviews(c.in)
			if !strings.Contains(got, c.want) {
				t.Errorf("got %q, want substring %q", got, c.want)
			}
		})
	}
}

func TestSummarizeMergeable(t *testing.T) {
	cases := []struct {
		name string
		pr   github.PullRequest
		want string
	}{
		{"nil", github.PullRequest{Mergeable: nil}, "체크 대기"},
		{"clean", github.PullRequest{Mergeable: boolPtr(true), MergeableState: "clean"}, "✓ 머지 가능"},
		{"blocked", github.PullRequest{Mergeable: boolPtr(true), MergeableState: "blocked"}, "블록됨"},
		{"dirty", github.PullRequest{Mergeable: boolPtr(false), MergeableState: "dirty"}, "머지 불가"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := summarizeMergeable(&c.pr)
			if !strings.Contains(got, c.want) {
				t.Errorf("got %q, want substring %q", got, c.want)
			}
		})
	}
}

func TestJoinTruncate(t *testing.T) {
	cases := []struct {
		in   []string
		max  int
		want string
	}{
		{[]string{}, 3, ""},
		{[]string{"a", "b"}, 3, "a, b"},
		{[]string{"a", "b", "c", "d", "e"}, 3, "a, b, c 외 2건"},
	}
	for _, c := range cases {
		got := joinTruncate(c.in, c.max)
		if got != c.want {
			t.Errorf("joinTruncate(%v, %d) = %q, want %q", c.in, c.max, got, c.want)
		}
	}
}

func boolPtr(b bool) *bool { return &b }
