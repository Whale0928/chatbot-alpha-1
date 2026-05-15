// Package render는 SummarizedContent (LLM 1회 추출 결과)를 4 포맷 markdown으로 변환한다.
//
// 거시 디자인 결정 A: 콘텐츠 1회 추출, 포맷 N회 변환. 이 파일의 함수들은 모두 순수 함수
// (LLM 호출 없음, side effect 없음) — 사용자가 [정리본 토글] 클릭 시 즉시 실행된다.
//
// 예외: SummarizedFreeform은 directive 적용을 위해 LLM 호출 옵션을 가질 수 있다.
// 그 변형은 별도 함수 (RenderSummarizedFreeformLLM 등)로 분리하고, 본 파일은 directive 없이
// SummarizedContent를 자유 합성한 결과만 반환한다.
package render

import (
	"fmt"
	"strings"
	"time"

	"chatbot-alpha-1/pkg/llm"
)

// SummarizedRenderInput은 4 포맷 렌더 공통 입력.
//
// SummarizedContent는 LLM 1회 추출 결과. Date/Speakers/Tags 푸터는 Go가 주입.
// RolesSnapshot은 role-based 렌더에서 그룹핑 시 fallback (action.OriginRoles가 비어있을 때).
type SummarizedRenderInput struct {
	Content       *llm.SummarizedContent
	Date          time.Time
	Speakers      []string
	RolesSnapshot map[string][]string // role-based 렌더 전용. 다른 포맷은 무시.
}

// =====================================================================
// RenderSummarizedDecisionStatus — 포맷 1 (결정 + 4분할 진행 + 액션)
// =====================================================================

// RenderSummarizedDecisionStatus는 SummarizedContent를 결정+진행보고 마크다운으로 변환한다.
//
// 섹션 순서: H1 → 결정사항 → 완료 → 진행 중 → 예정 → 이슈 → 미정 → 액션 → 푸터
// 빈 섹션은 생략 (자리 채움 마크다운 안 만든다 — UX/검증 양쪽 이득).
//
// Deprecated: Stage 4 LLM (summarize.RenderFormat)으로 대체. fallback 용도로만 호출 가능 (LLM 장애 시).
func RenderSummarizedDecisionStatus(in SummarizedRenderInput) string {
	if in.Content == nil {
		return ""
	}
	c := in.Content
	var b strings.Builder

	fmt.Fprintf(&b, "# %s 미팅 노트\n\n", in.Date.Format("2006-01-02"))

	// 결정사항 — Decision은 Title + Context 자식 구조 (v1.4 규칙)
	if len(c.Decisions) > 0 {
		b.WriteString("## 결정사항\n\n")
		for _, d := range c.Decisions {
			fmt.Fprintf(&b, "- **%s**\n", d.Title)
			for _, ctx := range d.Context {
				fmt.Fprintf(&b, "  - %s\n", ctx)
			}
		}
		b.WriteString("\n")
	}

	writeMDBulletSection(&b, "완료", c.Done)
	writeMDBulletSection(&b, "진행 중", c.InProgress)
	writeMDBulletSection(&b, "예정", c.Planned)
	writeMDBulletSection(&b, "이슈/블로커", c.Blockers)
	writeMDBulletSection(&b, "미정 질문", c.OpenQuestions)

	// 액션 — SummaryAction을 markdown checkbox로
	if len(c.Actions) > 0 {
		b.WriteString("## 액션\n\n")
		for _, a := range c.Actions {
			b.WriteString("- " + formatActionLine(a) + "\n")
		}
		b.WriteString("\n")
	}

	writeSummarizedFooter(&b, in.Speakers, c.Tags)
	return b.String()
}

// =====================================================================
// RenderSummarizedDiscussion — 포맷 2 (토픽별 논의 흐름)
// =====================================================================

// RenderSummarizedDiscussion은 SummarizedContent.Topics를 토픽별 마크다운으로 변환한다.
//
// 섹션: H1 → ## 토픽1 (Flow bullets + Insights) → ## 토픽2 → ... → 미정 → 푸터
// Topics가 비어있으면 헤더만 출력 (사용자가 토글했는데 빈 결과 = LLM이 토픽 클러스터링 실패한 것을 보여주는 디버깅 단서).
//
// Deprecated: Stage 4 LLM (summarize.RenderFormat)으로 대체. fallback 용도로만 호출 가능 (LLM 장애 시).
func RenderSummarizedDiscussion(in SummarizedRenderInput) string {
	if in.Content == nil {
		return ""
	}
	c := in.Content
	var b strings.Builder

	fmt.Fprintf(&b, "# %s 미팅 — 논의 정리\n\n", in.Date.Format("2006-01-02"))

	if len(c.Topics) == 0 {
		b.WriteString("_(토픽이 추출되지 않았습니다 — 회의 톤이 토픽 분리에 적합하지 않을 수 있습니다)_\n\n")
	}
	for _, t := range c.Topics {
		fmt.Fprintf(&b, "## %s\n\n", t.Title)
		if len(t.Flow) > 0 {
			b.WriteString("**흐름**\n")
			for _, f := range t.Flow {
				fmt.Fprintf(&b, "- %s\n", f)
			}
			b.WriteString("\n")
		}
		if len(t.Insights) > 0 {
			b.WriteString("**도출 관점**\n")
			for _, i := range t.Insights {
				fmt.Fprintf(&b, "- %s\n", i)
			}
			b.WriteString("\n")
		}
	}

	writeMDBulletSection(&b, "미정 질문", c.OpenQuestions)
	writeSummarizedFooter(&b, in.Speakers, c.Tags)
	return b.String()
}

// =====================================================================
// 공용 helper
// =====================================================================

// writeMDBulletSection은 비어있지 않은 string slice를 ## 헤더 + bullet 리스트로 출력한다.
// 빈 slice면 출력 없음 (자리 채움 마크다운 방지).
func writeMDBulletSection(b *strings.Builder, title string, items []string) {
	if len(items) == 0 {
		return
	}
	fmt.Fprintf(b, "## %s\n\n", title)
	for _, it := range items {
		fmt.Fprintf(b, "- %s\n", it)
	}
	b.WriteString("\n")
}

// formatActionLine은 SummaryAction 1건을 한 줄 markdown checkbox로 변환한다.
//
// 규칙:
//   - 본인 발화 (OriginRoles == TargetRoles 또는 TargetRoles 비어있음):
//     "[ ] {Origin} — {What} (기한: ...)"
//   - cross-role 발화 (OriginRoles ≠ TargetRoles):
//     "[ ] {TargetUser 또는 TargetRoles 묶음} — {What} (from: {Origin}({OriginRoles 묶음}), 기한: ...)"
//   - Origin 비어있음 (LLM이 누락): "[ ] {What} (기한: ...)"
//
// formatter는 환각 방어 게이트 통과를 가정 — Origin이 Speakers 목록 안임을 보장하지 않으므로
// 잘못된 입력이면 그대로 출력되지만 validate 단계에서 WARN log 남는다.
func formatActionLine(a llm.SummaryAction) string {
	// assignee 결정 — "개인 가시성"을 우선. 자기 발의에도 "BACKEND" 같은 그룹 라벨만 보여주면
	// 실제 책임자가 누구인지 모호 (그룹은 사람이 아님 — Copilot review 지적사항).
	// cross-role과 self를 명확히 분리:
	//   - cross-role: 대상 표시 우선 (TargetUser/TargetRoles), 발화자는 from 메타
	//   - self / ambiguous: 발화자(Origin) 우선 표시, from 메타 X (redundancy 회피)
	isCrossRole := isCrossRoleAction(a)
	var assignee string
	switch {
	case isCrossRole && a.TargetUser != "" && len(a.TargetRoles) > 0:
		// cross-role 개인 지목 + role 둘 다 명시 — 그룹 정체성 가시성 보존.
		// 예: "hyejungpark(FRONTEND) — ..."
		assignee = fmt.Sprintf("%s(%s)", a.TargetUser, strings.Join(a.TargetRoles, ","))
	case isCrossRole && a.TargetUser != "":
		assignee = a.TargetUser
	case isCrossRole && len(a.TargetRoles) > 0:
		// cross-role 그룹 대상 (개인 지목 X) — role 라벨 단독, 발화자는 from 메타.
		assignee = strings.Join(a.TargetRoles, "/")
	case a.Origin != "" && len(a.OriginRoles) > 0:
		// self-initiated 또는 ambiguous — 발화자 + role.
		// 예: "deadwhale(BACKEND) — 큐레이션 order ..."
		assignee = fmt.Sprintf("%s(%s)", a.Origin, strings.Join(a.OriginRoles, ","))
	case a.Origin != "":
		assignee = a.Origin
	case len(a.TargetRoles) > 0:
		// 발화자 정보 0인 그룹 단위 액션 (드문 케이스).
		assignee = strings.Join(a.TargetRoles, "/")
	}

	line := "[ ]"
	if assignee != "" {
		line += " " + assignee + " —"
	}
	line += " " + a.What

	// 메타 (from / deadline) — cross-role일 때만 from 표시 (self는 assignee가 이미 origin이라 redundancy).
	var meta []string
	if isCrossRole && a.Origin != "" {
		if len(a.OriginRoles) > 0 {
			meta = append(meta, fmt.Sprintf("from: %s(%s)", a.Origin, strings.Join(a.OriginRoles, ",")))
		} else {
			meta = append(meta, fmt.Sprintf("from: %s", a.Origin))
		}
	}
	if a.Deadline != "" {
		meta = append(meta, "기한: "+a.Deadline)
	}
	if len(meta) > 0 {
		line += " (" + strings.Join(meta, ", ") + ")"
	}
	return line
}

// isCrossRoleAction은 액션의 발의자 role과 대상 role이 다른지 (cross-role 요청인지) 판단한다.
//
// 룰:
//   - TargetRoles가 비어 있으면: cross-role 아님 (대상 모호 — 본인 발의로 간주)
//   - TargetUser가 명시되어 있고 Origin과 다르면: cross-role (개인 지목 요청)
//   - OriginRoles와 TargetRoles의 교집합이 TargetRoles 전체와 다르면: cross-role (일부라도 외부 그룹 대상)
func isCrossRoleAction(a llm.SummaryAction) bool {
	if len(a.TargetRoles) == 0 {
		if a.TargetUser != "" && a.TargetUser != a.Origin {
			return true
		}
		return false
	}
	originSet := make(map[string]bool, len(a.OriginRoles))
	for _, r := range a.OriginRoles {
		originSet[r] = true
	}
	for _, t := range a.TargetRoles {
		if !originSet[t] {
			return true
		}
	}
	return false
}

// writeSummarizedFooter는 참석자 + 태그 풋터를 출력한다 (Date 헤더와 짝).
//
// 형식:
//
//	---
//	참석자: alice, bob
//	태그: #foo #bar
func writeSummarizedFooter(b *strings.Builder, speakers []string, tags []string) {
	if len(speakers) == 0 && len(tags) == 0 {
		return
	}
	b.WriteString("---\n")
	if len(speakers) > 0 {
		fmt.Fprintf(b, "참석자: %s\n", strings.Join(speakers, ", "))
	}
	if len(tags) > 0 {
		// 태그 앞에 # prefix가 없는 토큰만 prefix 추가 (LLM이 #foo로 줄 수도, foo로 줄 수도)
		formatted := make([]string, len(tags))
		for i, t := range tags {
			if strings.HasPrefix(t, "#") {
				formatted[i] = t
			} else {
				formatted[i] = "#" + t
			}
		}
		fmt.Fprintf(b, "태그: %s\n", strings.Join(formatted, " "))
	}
}
