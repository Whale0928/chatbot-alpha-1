package bot

import (
	"strings"
	"testing"
	"time"
)

func TestBuildProgressBar(t *testing.T) {
	tests := []struct {
		name  string
		step  int
		total int
		want  string
	}{
		{"start", 1, 6, "[●○○○○○]"},
		{"middle", 3, 6, "[●●●○○○]"},
		{"complete", 6, 6, "[●●●●●●]"},
		{"single step total 1", 1, 1, "[●]"},
		{"3 of 4", 3, 4, "[●●●○]"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := buildProgressBar(tc.step, tc.total)
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestProgress_FormatLine_ContainsAllFields(t *testing.T) {
	// pure 함수 검증 — Progress 인스턴스 만들어 formatLine 호출.
	// goroutine 시작 안 함 (Messenger 의존 X).
	p := &Progress{
		label:       "정리본 추출",
		startedAt:   timeNowFor(t).Add(-12),
		totalSteps:  6,
		currentStep: 3,
		currentMsg:  "응답 대기",
	}
	p.mu.Lock()
	line := p.formatLineLocked()
	p.mu.Unlock()

	mustContain := []string{
		"●",   // ASCII 바
		"○",   // ASCII 바
		"정리본 추출",
		"단계 3/6",
		"응답 대기",
		"경과",
	}
	for _, sub := range mustContain {
		if !strings.Contains(line, sub) {
			t.Errorf("missing %q in: %q", sub, line)
		}
	}
}

func TestProgress_FormatLine_ClampsOutOfRange(t *testing.T) {
	tests := []struct {
		name        string
		current     int
		total       int
		wantBarLen  int // ●○ 합계
		wantContain string
	}{
		{"current 0 → clamp to 1", 0, 6, 6, "단계 1/6"},
		{"current > total → clamp to total", 99, 6, 6, "단계 6/6"},
		{"total 0 → clamp to 1", 1, 0, 1, "단계 1/1"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := &Progress{
				label:       "x",
				totalSteps:  tc.total,
				currentStep: tc.current,
				currentMsg:  "test",
			}
			p.mu.Lock()
	line := p.formatLineLocked()
	p.mu.Unlock()
			if !strings.Contains(line, tc.wantContain) {
				t.Errorf("missing %q in: %q", tc.wantContain, line)
			}
			circles := strings.Count(line, "●") + strings.Count(line, "○")
			if circles != tc.wantBarLen {
				t.Errorf("bar length = %d, want %d (line: %q)", circles, tc.wantBarLen, line)
			}
		})
	}
}

// timeNowFor는 testing.T 의존 시각 팩토리 — 시각 비교 시 ms-단위 안정성 위해 분 단위 truncate.
func timeNowFor(t *testing.T) time.Time {
	t.Helper()
	return time.Now().Truncate(time.Minute)
}
