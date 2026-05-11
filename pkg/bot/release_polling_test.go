package bot

import (
	"strings"
	"testing"
	"time"

	"chatbot-alpha-1/pkg/github"
)

func TestSummarizeChecks(t *testing.T) {
	cases := []struct {
		name      string
		checks    []github.CheckRun
		wantStat  string
		wantDetIn string // substring of detail (empty = don't care)
	}{
		{"empty", nil, "no_checks", "체크 없음"},
		{
			"all success",
			[]github.CheckRun{
				{Name: "prepare", Status: "completed", Conclusion: "success"},
				{Name: "unit", Status: "completed", Conclusion: "success"},
			},
			"✓ success", "2/2 완료",
		},
		{
			"partial running",
			[]github.CheckRun{
				{Name: "prepare", Status: "completed", Conclusion: "success"},
				{Name: "unit", Status: "in_progress"},
				{Name: "rule", Status: "queued"},
			},
			"▶ running", "1/3 완료",
		},
		{
			"failure",
			[]github.CheckRun{
				{Name: "prepare", Status: "completed", Conclusion: "success"},
				{Name: "unit", Status: "completed", Conclusion: "failure"},
			},
			"❌ failure", "실패: unit",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotStat, gotDet := summarizeChecks(c.checks)
			if gotStat != c.wantStat {
				t.Errorf("status = %q, want %q", gotStat, c.wantStat)
			}
			if c.wantDetIn != "" && !strings.Contains(gotDet, c.wantDetIn) {
				t.Errorf("detail = %q (substring %q 없음)", gotDet, c.wantDetIn)
			}
		})
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

func TestFmtElapsed(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{0, "0:00"},
		{45 * time.Second, "0:45"},
		{83 * time.Second, "1:23"},
		{3601 * time.Second, "1h 0m"},
		{4500 * time.Second, "1h 15m"},
	}
	for _, c := range cases {
		got := fmtElapsed(c.d)
		if got != c.want {
			t.Errorf("fmtElapsed(%v) = %q, want %q", c.d, got, c.want)
		}
	}
}

func boolPtr(b bool) *bool { return &b }
