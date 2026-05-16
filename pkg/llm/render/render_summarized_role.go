package render

import (
	"fmt"
	"sort"
	"strings"

	"chatbot-alpha-1/pkg/llm"
)

// =====================================================================
// RenderSummarizedRoleBased — 포맷 3 (역할별 정리)
//
// 거시 디자인 결정 5/B 적용:
//   - 모든 role 그룹에 노출 (한 사람이 여러 role 가지면 모든 그룹에 등장)
//   - cross-role 액션은 발의자 role 그룹의 "자기 발의"가 아니라 대상 role 그룹의 "받은 요청"에 표시
//   - 자기 발의 / 받은 요청 2섹션 분리
//
// 거시 디자인 결정 iii (한 사람 여러 role 시 primary + 다른 그룹 참고 1줄)는 Phase 3 후속에서 적용.
// 현재는 단순화: 각 액션이 해당 role 그룹의 정확한 섹션에 한 번씩 등장.
// =====================================================================

// RenderSummarizedRoleBased는 SummarizedContent를 role 그룹별 마크다운으로 변환한다.
//
// 전략:
//  1. 등장 role 수집 = (모든 actions의 OriginRoles ∪ TargetRoles) ∪ (RolesSnapshot 값)
//  2. role 정렬 후 각 role 섹션:
//     - 자기 발의: Origin role 중 이 role 포함 + TargetRoles에도 이 role 포함 (cross 아님)
//     - 받은 요청: TargetRoles에 이 role 포함 + OriginRoles에 이 role 미포함 (cross)
//  3. 공통 사항 (Shared) + 미정 + 푸터
//
// RolesSnapshot이 비어 있어도 actions 자체에 role 정보가 있으면 동작 가능.
// 그러나 RolesSnapshot이 있으면 "members: alice, bob" 표시로 그룹 가시성 향상.
//
// Deprecated: Stage 4 LLM (summarize.RenderFormat)으로 대체. fallback 용도로만 호출 가능 (LLM 장애 시).
func RenderSummarizedRoleBased(in SummarizedRenderInput) string {
	if in.Content == nil {
		return ""
	}
	c := in.Content
	var b strings.Builder

	fmt.Fprintf(&b, "# %s 미팅 — 역할별 정리\n\n", in.Date.Format("2006-01-02"))

	roles := collectAllRoles(c.Actions, in.RolesSnapshot)
	if len(roles) == 0 {
		b.WriteString("_(role 정보가 없어 그룹핑을 건너뜁니다 — Discord guild role 설정 또는 SpeakerRoles 입력 확인 필요)_\n\n")
	}

	for _, role := range roles {
		fmt.Fprintf(&b, "## %s\n\n", role)

		// members (RolesSnapshot 있을 때만)
		members := membersOfRole(role, in.RolesSnapshot)
		if len(members) > 0 {
			fmt.Fprintf(&b, "_members: %s_\n\n", strings.Join(members, ", "))
		}

		// 자기 발의 / 받은 요청 분리
		var selfActions, receivedActions []llm.SummaryAction
		for _, a := range c.Actions {
			placement := classifyActionForRole(a, role)
			switch placement {
			case placementSelf:
				selfActions = append(selfActions, a)
			case placementReceived:
				receivedActions = append(receivedActions, a)
			}
		}

		if len(selfActions) > 0 {
			b.WriteString("**자기 발의 액션**\n")
			for _, a := range selfActions {
				b.WriteString("- " + formatActionLine(a) + "\n")
			}
			b.WriteString("\n")
		}
		if len(receivedActions) > 0 {
			b.WriteString("**받은 요청**\n")
			for _, a := range receivedActions {
				b.WriteString("- " + formatActionLine(a) + "\n")
			}
			b.WriteString("\n")
		}
		if len(selfActions) == 0 && len(receivedActions) == 0 {
			b.WriteString("_(이 role에 attribute된 액션이 없습니다)_\n\n")
		}
	}

	// 공통 사항 (역할 비귀속)
	if len(c.Shared) > 0 {
		b.WriteString("## 공통 사항\n\n")
		for _, s := range c.Shared {
			fmt.Fprintf(&b, "- %s\n", s)
		}
		b.WriteString("\n")
	}

	writeMDBulletSection(&b, "미정 질문", c.OpenQuestions)
	writeToolReferenceSections(&b, c)
	writeSummarizedFooter(&b, in.Speakers, c.Tags)
	return b.String()
}

// =====================================================================
// helper — role 그룹핑
// =====================================================================

// collectAllRoles는 actions 모든 OriginRoles/TargetRoles + RolesSnapshot 값 합집합 정렬 반환.
// 동작 목적: action이 비어 있는 role 그룹도 (snapshot에 멤버가 있으면) 섹션 노출 →
// "이 role엔 액션 없음" 가시화 (디버깅/관리 단서).
func collectAllRoles(actions []llm.SummaryAction, snapshot map[string][]string) []string {
	seen := make(map[string]bool)
	for _, a := range actions {
		for _, r := range a.OriginRoles {
			seen[r] = true
		}
		for _, r := range a.TargetRoles {
			seen[r] = true
		}
	}
	for _, roles := range snapshot {
		for _, r := range roles {
			seen[r] = true
		}
	}
	out := make([]string, 0, len(seen))
	for r := range seen {
		out = append(out, r)
	}
	sort.Strings(out)
	return out
}

// membersOfRole은 RolesSnapshot에서 이 role을 가진 username들을 정렬 반환.
func membersOfRole(role string, snapshot map[string][]string) []string {
	var out []string
	for user, roles := range snapshot {
		for _, r := range roles {
			if r == role {
				out = append(out, user)
				break
			}
		}
	}
	sort.Strings(out)
	return out
}

// actionPlacement는 단일 액션이 특정 role 그룹에서 어느 섹션에 등장할지 식별.
type actionPlacement int

const (
	placementNone     actionPlacement = iota // 이 role과 무관
	placementSelf                            // 이 role의 사람이 자기 발의 (Origin∩Target 포함)
	placementReceived                        // 이 role이 외부 발의로부터 요청 받음 (cross-role 대상)
)

// classifyActionForRole은 action이 role 그룹의 자기 발의/받은 요청/무관 중 어디에 속할지 판단.
//
// 룰:
//   - role이 OriginRoles와 TargetRoles 모두에 있음 → placementSelf
//   - role이 TargetRoles에 있고 OriginRoles에는 없음 → placementReceived (cross-role 대상)
//   - role이 OriginRoles에만 있고 TargetRoles에 없음 (TargetRoles 비어있지 않은 cross-role)
//     → 발의자 본인 그룹은 액션 사본을 갖지 않음 (받은 요청은 대상 그룹에만 표시)
//   - 그 외 → placementNone
//
// 폴백 케이스:
//   - TargetRoles와 TargetUser 둘 다 비어 있음 (대상 완전 모호): OriginRoles에 있으면 placementSelf
//   - TargetRoles 비어 있고 TargetUser만 명시됨: TargetUser의 role을 모르므로 발의자 그룹에
//     placementSelf로 폴백 노출 (액션이 어떤 role 섹션에도 등장하지 않는 누락 방지).
//     Phase 3에서 RolesSnapshot을 인자로 받아 TargetUser → role 조회로 정밀화.
func classifyActionForRole(a llm.SummaryAction, role string) actionPlacement {
	originHas := containsString(a.OriginRoles, role)
	targetHas := containsString(a.TargetRoles, role)

	// 대상 모호 / TargetUser-only — 발의자 그룹에 자기 발의로 노출 (누락 방지 폴백)
	if len(a.TargetRoles) == 0 {
		if originHas {
			return placementSelf
		}
		return placementNone
	}

	if originHas && targetHas {
		return placementSelf
	}
	if targetHas && !originHas {
		return placementReceived
	}
	return placementNone
}

func containsString(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
