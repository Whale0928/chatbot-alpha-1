package render

import (
	"fmt"
	"strings"
	"time"

	"chatbot-alpha-1/pkg/llm"
)

// RenderInput은 llm.FinalNoteResponse를 마크다운으로 렌더링할 때 필요한 메타데이터 묶음.
// Participants는 Go가 수집한 발화자 목록으로, LLM이 생성한 필드가 아니다.
type RenderInput struct {
	Date         time.Time
	Participants []string
	Response     *llm.FinalNoteResponse
}

// InterimRenderInput은 interim(진행 중) 노트 렌더링 입력.
type InterimRenderInput struct {
	Date         time.Time
	Participants []string
	Response     *llm.InterimNoteResponse
}

// RenderFinalNote는 v1.4 "결정 중심" 템플릿에 맞춰 최종 미팅 노트의 마크다운을
// 생성한다.
//
// 포맷 원칙:
//   - 제목: H1 `# YYYY-MM-DD 미팅 노트`
//   - `## 결정` 섹션이 가장 먼저. 각 결정은 **bold title** + 2단 자식 bullet
//   - `## 확인 필요` 섹션은 있을 때만. 평평한 리스트
//   - `## 다음 단계` 섹션은 항상 존재 (비면 "(없음)")
//   - 참석자/태그는 `---` 구분선 뒤 풋터
//
// 출력 예시:
//
//	# 2026-04-16 미팅 노트
//
//	## 결정
//	- **캐시 프록시 구현 완료**
//	  - 고객사 제공 기능으로는 부적합
//	  - 외부에는 '작업 중'으로 커뮤니케이션
//	- **메일 포맷은 하나로 고정**
//
//	## 확인 필요
//	- 셀레니움 채택 여부 - 확인 필요
//
//	## 다음 단계
//	- (없음)
//
//	---
//	참석자: `@hgkim`
//	태그: #캐시 #프록시
func RenderFinalNote(in RenderInput) string {
	var b strings.Builder

	fmt.Fprintf(&b, "# %s 미팅 노트\n\n", in.Date.Format("2006-01-02"))

	if in.Response == nil {
		writeEmptyFinalSections(&b)
		writeFooter(&b, in.Participants, nil)
		return b.String()
	}

	writeDecisionSection(&b, "결정", in.Response.Decisions, "(기록되지 않음)")
	writeOpenQuestionsSection(&b, in.Response.OpenQuestions)

	b.WriteString("## 다음 단계\n")
	if len(in.Response.NextSteps) == 0 {
		b.WriteString("- (없음)\n\n")
	} else {
		for _, ns := range in.Response.NextSteps {
			fmt.Fprintf(&b, "- [ ] %s\n", formatNextStep(ns))
		}
		b.WriteString("\n")
	}

	writeFooter(&b, in.Participants, in.Response.Tags)

	return b.String()
}

// RenderInterimNote는 진행 중 미팅의 중간 정리를 렌더링한다.
//
// Final과 다른 점:
//   - 헤더: `**현재까지 미팅 정리** · HH:MM:SS` (임시 메시지임을 시각 구분)
//   - H1 대신 bold 라벨 + 섹션 헤더도 `**bold**`
//   - 다음 단계 섹션 없음 (interim은 결정/미정 스냅샷만)
//   - 버튼은 이 함수가 아니라 호출부에서 첨부
//
// 출력 예시:
//
//	**현재까지 미팅 정리** · 14:15:32
//
//	**지금까지 나온 결정**
//	- **캐시 스토어는 Redis로 고정**
//	  - TTL은 1시간
//
//	**확인 필요**
//	- Prometheus 메트릭 - 확인 필요
//
//	_참석자_ `@hgkim`  _태그_ #캐시 #Redis
func RenderInterimNote(in InterimRenderInput) string {
	var b strings.Builder

	fmt.Fprintf(&b, "**현재까지 미팅 정리** · %s\n\n", in.Date.Format("15:04:05"))

	if in.Response == nil {
		b.WriteString("_(생성 실패)_\n")
		return b.String()
	}

	writeInterimDecisionSection(&b, "지금까지 나온 결정", in.Response.Decisions)
	writeInterimOpenQuestions(&b, "확인 필요", in.Response.OpenQuestions)

	writeInterimFooter(&b, in.Participants, in.Response.Tags)

	return b.String()
}

// === 내부 렌더 헬퍼 ===

// writeDecisionSection은 `## <header>` + bold title + 자식 bullet 리스트를 쓴다.
// Decisions가 비어있으면 emptyFallback을 평평한 bullet으로 출력.
func writeDecisionSection(b *strings.Builder, header string, decisions []llm.Decision, emptyFallback string) {
	fmt.Fprintf(b, "## %s\n", header)
	if len(decisions) == 0 {
		fmt.Fprintf(b, "- %s\n\n", emptyFallback)
		return
	}
	for _, d := range decisions {
		fmt.Fprintf(b, "- **%s**\n", d.Title)
		for _, ctx := range d.Context {
			fmt.Fprintf(b, "  - %s\n", ctx)
		}
	}
	b.WriteString("\n")
}

// writeInterimDecisionSection은 interim 용 `**<header>**` + 자식 bullet.
// 비어있으면 섹션 자체 생략.
func writeInterimDecisionSection(b *strings.Builder, header string, decisions []llm.Decision) {
	if len(decisions) == 0 {
		return
	}
	fmt.Fprintf(b, "**%s**\n", header)
	for _, d := range decisions {
		fmt.Fprintf(b, "- **%s**\n", d.Title)
		for _, ctx := range d.Context {
			fmt.Fprintf(b, "  - %s\n", ctx)
		}
	}
	b.WriteString("\n")
}

// writeOpenQuestionsSection은 final 용 `## 확인 필요` 섹션. 비어있으면 생략.
func writeOpenQuestionsSection(b *strings.Builder, questions []string) {
	if len(questions) == 0 {
		return
	}
	b.WriteString("## 확인 필요\n")
	for _, q := range questions {
		fmt.Fprintf(b, "- %s\n", q)
	}
	b.WriteString("\n")
}

// writeInterimOpenQuestions은 interim 용 `**<header>**` + 평평한 리스트.
func writeInterimOpenQuestions(b *strings.Builder, header string, questions []string) {
	if len(questions) == 0 {
		return
	}
	fmt.Fprintf(b, "**%s**\n", header)
	for _, q := range questions {
		fmt.Fprintf(b, "- %s\n", q)
	}
	b.WriteString("\n")
}

// writeFooter는 최종 노트의 하단 메타 (참석자 + 태그)를 `---` 구분선 뒤에 쓴다.
// 둘 다 비어있으면 아무것도 안 쓴다.
func writeFooter(b *strings.Builder, participants []string, tags []string) {
	if len(participants) == 0 && len(tags) == 0 {
		return
	}
	b.WriteString("---\n")
	if len(participants) > 0 {
		parts := make([]string, len(participants))
		for i, p := range participants {
			parts[i] = "`" + p + "`"
		}
		fmt.Fprintf(b, "참석자: %s\n", strings.Join(parts, ", "))
	}
	if len(tags) > 0 {
		tagStrs := make([]string, len(tags))
		for i, t := range tags {
			tagStrs[i] = "#" + t
		}
		fmt.Fprintf(b, "태그: %s\n", strings.Join(tagStrs, " "))
	}
}

// writeInterimFooter는 interim 용 한 줄 메타 풋터.
func writeInterimFooter(b *strings.Builder, participants []string, tags []string) {
	parts := []string{}
	if len(participants) > 0 {
		ps := make([]string, len(participants))
		for i, p := range participants {
			ps[i] = "`" + p + "`"
		}
		parts = append(parts, "_참석자_ "+strings.Join(ps, ", "))
	}
	if len(tags) > 0 {
		ts := make([]string, len(tags))
		for i, t := range tags {
			ts[i] = "#" + t
		}
		parts = append(parts, "_태그_ "+strings.Join(ts, " "))
	}
	if len(parts) > 0 {
		b.WriteString(strings.Join(parts, "  ") + "\n")
	}
}

// formatNextStep은 단일 llm.NextStep을 한 줄 문자열로 변환.
// 기본 포맷: `who` — what
// Deadline 있을 때: `who` — what _(기한: YYYY-MM-DD)_
// Who 없을 때: (담당자 미정) — what
// What 없을 때: (내용 미정)
//
// `@` prefix는 의도적으로 붙이지 않는다 — 노트는 단일 서기가 대신 적은 형태라
// Discord 멘션이 발송되면 안 된다. username은 inline code로만 식별 가능하게.
func formatNextStep(ns llm.NextStep) string {
	who := "(담당자 미정)"
	if ns.Who != "" {
		who = "`" + ns.Who + "`"
	}
	what := ns.What
	if what == "" {
		what = "(내용 미정)"
	}
	if ns.Deadline != "" {
		return fmt.Sprintf("%s — %s _(기한: %s)_", who, what, ns.Deadline)
	}
	return fmt.Sprintf("%s — %s", who, what)
}

// writeEmptyFinalSections는 Response가 nil일 때 빈 섹션 헤더만 출력.
func writeEmptyFinalSections(b *strings.Builder) {
	b.WriteString("## 결정\n- (기록되지 않음)\n\n")
	b.WriteString("## 다음 단계\n- (없음)\n\n")
}
