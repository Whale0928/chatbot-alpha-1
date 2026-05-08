package render

import (
	"fmt"
	"strings"
	"time"

	"chatbot-alpha-1/pkg/llm"
)

// DecisionStatusRenderInput은 포맷 1번 마크다운 렌더 입력.
type DecisionStatusRenderInput struct {
	Date         time.Time
	Participants []string
	Response     *llm.DecisionStatusResponse
}

// RenderDecisionStatus는 결정+진행보고 통합형 마크다운을 생성한다.
//
// 출력 구조:
//
//	# YYYY-MM-DD 미팅 노트
//	## 결정                  (없으면 "- (기록되지 않음)")
//	## 진행 현황             (4 sub-block; 각 비면 sub-block 생략)
//	  **완료** / **진행 중** / **예정** / **이슈/막힘**
//	## 확인 필요             (있을 때만)
//	## 다음 단계             (항상 - 비면 "(없음)")
//	---
//	참석자: ... / 태그: ...
func RenderDecisionStatus(in DecisionStatusRenderInput) string {
	var b strings.Builder

	fmt.Fprintf(&b, "# %s 미팅 노트\n\n", in.Date.Format("2006-01-02"))

	if in.Response == nil {
		b.WriteString("## 결정\n- (기록되지 않음)\n\n")
		b.WriteString("## 다음 단계\n- (없음)\n\n")
		writeFooter(&b, in.Participants, nil)
		return b.String()
	}

	r := in.Response

	writeDecisionSection(&b, "결정", r.Decisions, "(기록되지 않음)")

	// 진행 현황: 4 sub-block 중 하나라도 있으면 헤더 출력
	if len(r.Done) > 0 || len(r.InProgress) > 0 || len(r.Planned) > 0 || len(r.Blockers) > 0 {
		b.WriteString("## 진행 현황\n")
		writeStatusSubBlock(&b, "완료", r.Done)
		writeStatusSubBlock(&b, "진행 중", r.InProgress)
		writeStatusSubBlock(&b, "예정", r.Planned)
		writeStatusSubBlock(&b, "이슈/막힘", r.Blockers)
	}

	writeOpenQuestionsSection(&b, r.OpenQuestions)

	b.WriteString("## 다음 단계\n")
	if len(r.NextSteps) == 0 {
		b.WriteString("- (없음)\n\n")
	} else {
		for _, ns := range r.NextSteps {
			fmt.Fprintf(&b, "- [ ] %s\n", formatNextStep(ns))
		}
		b.WriteString("\n")
	}

	writeFooter(&b, in.Participants, r.Tags)
	return b.String()
}

// writeStatusSubBlock은 **<라벨>** 헤더 + bullet 리스트. 비면 skip.
func writeStatusSubBlock(b *strings.Builder, label string, items []string) {
	if len(items) == 0 {
		return
	}
	fmt.Fprintf(b, "**%s**\n", label)
	for _, it := range items {
		fmt.Fprintf(b, "- %s\n", it)
	}
	b.WriteString("\n")
}
