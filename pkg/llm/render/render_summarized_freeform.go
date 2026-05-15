package render

import (
	"fmt"
	"strings"

	"chatbot-alpha-1/pkg/llm"
)

// =====================================================================
// RenderSummarizedFreeform — 포맷 4 (자율, directive 없는 단순 합성)
//
// 거시 디자인 결정 A 예외: freeform은 directive별 다양성을 위해 LLM 호출이 가능하지만,
// "directive 없는" 기본 케이스는 SummarizedContent를 자유 어조로 한 단락씩 합성한 결과로
// 충분하다 — LLM 호출 없이 즉시 렌더 가능.
//
// 사용자가 자연어 directive를 입력한 케이스는 별도 함수 (RenderSummarizedFreeformWithDirective,
// Phase 2 chunk 4 또는 Phase 3에서 추가)가 LLM 호출로 처리한다.
// =====================================================================

// RenderSummarizedFreeform은 SummarizedContent를 자유 어조 마크다운으로 합성한다.
//
// 구조: 한 단락 요약 (decisions / actions / topics 압축) + 핵심 결정 + 핵심 액션 + 토픽 + 푸터.
// 격식 있는 4분할 (decision_status) 보다 풀어 쓴 톤이지만 사실 자체는 동일.
//
// Deprecated: Stage 4 LLM (summarize.RenderFormat)으로 대체. fallback 용도로만 호출 가능 (LLM 장애 시).
func RenderSummarizedFreeform(in SummarizedRenderInput) string {
	if in.Content == nil {
		return ""
	}
	c := in.Content
	var b strings.Builder

	fmt.Fprintf(&b, "# %s 미팅 — 자율 정리\n\n", in.Date.Format("2006-01-02"))

	// 한 단락 요약 — 가장 위에 배치 (스크롤 첫 줄에서 미팅 핵심을 잡게)
	if summary := composeFreeformSummary(c); summary != "" {
		b.WriteString(summary + "\n\n")
	}

	if len(c.Decisions) > 0 {
		b.WriteString("**핵심 결정**\n")
		for _, d := range c.Decisions {
			fmt.Fprintf(&b, "- %s\n", d.Title)
		}
		b.WriteString("\n")
	}

	if len(c.Actions) > 0 {
		b.WriteString("**핵심 액션**\n")
		for _, a := range c.Actions {
			b.WriteString("- " + formatActionLine(a) + "\n")
		}
		b.WriteString("\n")
	}

	if len(c.Topics) > 0 {
		b.WriteString("**논의 흐름**\n")
		for _, t := range c.Topics {
			fmt.Fprintf(&b, "- **%s** — %s\n", t.Title, joinFlowOneLine(t.Flow))
		}
		b.WriteString("\n")
	}

	if len(c.Blockers) > 0 || len(c.OpenQuestions) > 0 {
		b.WriteString("**확인 필요**\n")
		for _, blk := range c.Blockers {
			fmt.Fprintf(&b, "- %s\n", blk)
		}
		for _, q := range c.OpenQuestions {
			fmt.Fprintf(&b, "- %s\n", q)
		}
		b.WriteString("\n")
	}

	writeSummarizedFooter(&b, in.Speakers, c.Tags)
	return b.String()
}

// composeFreeformSummary는 SummarizedContent에서 1-2 문장 한 단락 요약을 합성한다 (pure).
//
// 출력 패턴:
//
//	"결정 N건 / 액션 M건 / 미정 K건. 가장 활발한 토픽: <첫 토픽 제목>."
//
// 모든 카테고리가 비어 있으면 빈 문자열 반환 (호출자가 단락 자체를 생략).
// LLM 추론 없이 카운팅 + 첫 항목 인용만 사용 — 결정성·비용 모두 0.
func composeFreeformSummary(c *llm.SummarizedContent) string {
	if c == nil {
		return ""
	}
	totalActions := len(c.Actions)
	totalDecisions := len(c.Decisions)
	totalOpen := len(c.OpenQuestions)
	totalProgress := len(c.Done) + len(c.InProgress) + len(c.Planned)

	if totalActions+totalDecisions+totalOpen+totalProgress+len(c.Topics) == 0 {
		return ""
	}

	var parts []string
	if totalDecisions > 0 {
		parts = append(parts, fmt.Sprintf("결정 %d건", totalDecisions))
	}
	if totalActions > 0 {
		parts = append(parts, fmt.Sprintf("액션 %d건", totalActions))
	}
	if totalOpen > 0 {
		parts = append(parts, fmt.Sprintf("미정 %d건", totalOpen))
	}
	summary := strings.Join(parts, " · ")
	if summary == "" {
		summary = "진행 사항 정리"
	} else {
		summary += "."
	}
	if len(c.Topics) > 0 {
		summary += " 가장 활발한 토픽: " + c.Topics[0].Title + "."
	}
	return summary
}

// joinFlowOneLine은 Topic.Flow bullet들을 한 줄 자연 문장으로 합친다.
// freeform 톤에서는 bullet 대신 흐름 한 문장이 더 자연스러움.
func joinFlowOneLine(flow []string) string {
	if len(flow) == 0 {
		return ""
	}
	if len(flow) == 1 {
		return flow[0]
	}
	return strings.Join(flow, " → ")
}
