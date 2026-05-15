package bot

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// 로거 출력 형식이 깨지면 운영 디버깅 grep 패턴이 모두 망가진다 — 본 테스트가 컨벤션을 lock한다.

func Test_buildLogLine_기본형식(t *testing.T) {
	got := buildLogLine("meeting", "start", "", "super-session 진입",
		[]logField{lf("thread", "t_123"), lf("uid", "u_456"), lf("mode", "meeting")})
	want := `[meeting/start] super-session 진입 thread=t_123 uid=u_456 mode=meeting`
	if got != want {
		t.Errorf("기본 형식 불일치\n got=%q\nwant=%q", got, want)
	}
}

func Test_buildLogLine_ERR레벨_필드_quote(t *testing.T) {
	err := errors.New("boom: detailed reason")
	got := buildLogLine("release", "create_pr_failed", "ERR", "PR 생성 실패",
		[]logField{lf("err", err), lf("module", "frontend"), lf("step", 5)})
	// err 메시지에 콜론/공백 있으니 quote, module은 short ident라 quote 없음, step은 int 그대로.
	wantSubstrings := []string{
		"[release/create_pr_failed] ERR PR 생성 실패",
		`err="boom: detailed reason"`,
		"module=frontend",
		"step=5",
	}
	for _, s := range wantSubstrings {
		if !strings.Contains(got, s) {
			t.Errorf("ERR 출력에 %q 누락:\n%s", s, got)
		}
	}
}

func Test_formatLogValue_quote정책(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want string
	}{
		{"nil", nil, "<nil>"},
		{"short_ident", "thread_123", "thread_123"},
		{"slash_path_ok", "owner/repo:tag", "owner/repo:tag"},
		{"quoted_with_space", "hello world", `"hello world"`},
		{"quoted_with_quote", `say "hi"`, `"say \"hi\""`},
		{"int", 42, "42"},
		{"bool", true, "true"},
		{"duration_ms_round", 1523 * time.Millisecond, "1.523s"},
		{"error", errors.New("oops"), `"oops"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := formatLogValue(c.in); got != c.want {
				t.Errorf("formatLogValue(%v) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func Test_logGuard_reason필드_첫자리(t *testing.T) {
	// reason은 grep 우선순위가 높아서 항상 메시지 직후 첫 필드여야 함.
	// "single_in_progress"는 short ident라 quote 없음 (isShortIdent 정책).
	got := buildLogLine("release", "guard_reject", "", "단일 release 진행 중",
		[]logField{lf("reason", "single_in_progress"), lf("thread", "t_x")})
	if !strings.Contains(got, `reason=single_in_progress thread=t_x`) {
		t.Errorf("reason이 thread보다 앞에 와야 함:\n%s", got)
	}
}

func Test_logState_from_to_필드순서(t *testing.T) {
	// state_change는 from→to를 한눈에 보기 위해 다른 필드보다 앞에 와야 함.
	got := buildLogLine("agent", "state_change", "", "agent → meeting",
		[]logField{lf("from", "agent_await_input"), lf("to", "meeting"), lf("thread", "t_x")})
	if !strings.Contains(got, "from=agent_await_input to=meeting thread=t_x") {
		t.Errorf("from/to 순서 보장 실패:\n%s", got)
	}
}

func Test_isShortIdent_경계(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"thread_123", true},
		{"a-b-c", true},
		{"owner/repo", true},
		{"v1.2.3", true},
		{"key:value", true},
		{"", false},
		{"hello world", false},
		{`with"quote`, false},
		{"한글포함", false},
		{strings.Repeat("a", 65), false},
	}
	for _, c := range cases {
		if got := isShortIdent(c.in); got != c.want {
			t.Errorf("isShortIdent(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}
