package render

import (
	"strings"
	"testing"
	"time"

	"chatbot-alpha-1/pkg/llm"
)

func TestRenderDecisionStatus_Full(t *testing.T) {
	in := DecisionStatusRenderInput{
		Date:         time.Date(2026, 4, 21, 0, 0, 0, 0, time.UTC),
		Participants: []string{"hgkim"},
		Response: &llm.DecisionStatusResponse{
			Decisions: []llm.Decision{
				{Title: "크론잡은 시스템 타이머로 매일 5:00, 17:00 수집", Context: []string{"이전 작업이 끝나지 않으면 수집 스킵"}},
				{Title: "배치 로직은 내부 트리거 API 기반으로 전환"},
			},
			Done:          []string{"실서버 어플리케이션 배포 세팅 확인", "DB 정상 확인"},
			InProgress:    []string{"배치 로직 내부 트리거 API 전환"},
			Planned:       []string{"발행일자 추출 로직 (오늘 안에 마무리)"},
			Blockers:      []string{"발행일자 추출 미완성"},
			OpenQuestions: []string{"fallback 정책 - 미정. 확인 필요"},
			NextSteps:     []llm.NextStep{{Who: "hgkim", What: "발행일자 추출 마무리", Deadline: "2026-04-21"}},
			Tags:          []string{"크롤러", "배치"},
		},
	}
	out := RenderDecisionStatus(in)

	mustContain(t, out, "# 2026-04-21 미팅 노트")
	mustContain(t, out, "## 결정")
	mustContain(t, out, "- **크론잡은 시스템 타이머로 매일 5:00, 17:00 수집**")
	mustContain(t, out, "  - 이전 작업이 끝나지 않으면 수집 스킵")
	mustContain(t, out, "## 진행 현황")
	mustContain(t, out, "**완료**")
	mustContain(t, out, "- 실서버 어플리케이션 배포 세팅 확인")
	mustContain(t, out, "**진행 중**")
	mustContain(t, out, "**예정**")
	mustContain(t, out, "**이슈/막힘**")
	mustContain(t, out, "## 확인 필요")
	mustContain(t, out, "- fallback 정책 - 미정. 확인 필요")
	mustContain(t, out, "## 다음 단계")
	mustContain(t, out, "- [ ] `hgkim` — 발행일자 추출 마무리 _(기한: 2026-04-21)_")
	mustContain(t, out, "참석자: `hgkim`")
	mustContain(t, out, "태그: #크롤러 #배치")
}

func TestRenderDecisionStatus_AllEmpty(t *testing.T) {
	in := DecisionStatusRenderInput{
		Date:         time.Date(2026, 4, 21, 0, 0, 0, 0, time.UTC),
		Participants: []string{"hgkim"},
		Response:     &llm.DecisionStatusResponse{},
	}
	out := RenderDecisionStatus(in)
	mustContain(t, out, "## 결정\n- (기록되지 않음)")
	mustContain(t, out, "## 다음 단계\n- (없음)")
	mustNotContain(t, out, "## 진행 현황")
	mustNotContain(t, out, "## 확인 필요")
}

func TestRenderDecisionStatus_NilResponse(t *testing.T) {
	out := RenderDecisionStatus(DecisionStatusRenderInput{
		Date:         time.Date(2026, 4, 21, 0, 0, 0, 0, time.UTC),
		Participants: []string{"hgkim"},
		Response:     nil,
	})
	mustContain(t, out, "## 결정\n- (기록되지 않음)")
	mustContain(t, out, "## 다음 단계\n- (없음)")
}

// mustContain은 expected substring이 actual에 포함되어야 한다.
func mustContain(t *testing.T, actual, expected string) {
	t.Helper()
	if !strings.Contains(actual, expected) {
		t.Errorf("expected substring not found:\n  expected: %q\n  actual:\n%s", expected, actual)
	}
}

// mustNotContain은 unwanted substring이 actual에 없어야 한다.
func mustNotContain(t *testing.T, actual, unwanted string) {
	t.Helper()
	if strings.Contains(actual, unwanted) {
		t.Errorf("unexpected substring found:\n  unwanted: %q\n  actual:\n%s", unwanted, actual)
	}
}
