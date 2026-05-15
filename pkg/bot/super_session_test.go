package bot

import (
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"
)

// =====================================================================
// Phase 3 chunk 3A — super-session sticky UI 단위 테스트
// =====================================================================

func TestSuperSessionStickyComponents_TwoRows(t *testing.T) {
	comps := superSessionStickyComponents()
	if len(comps) != 2 {
		t.Fatalf("expected 2 ActionsRows, got %d", len(comps))
	}
}

func TestSuperSessionStickyComponents_V4Layout_Row1Primary(t *testing.T) {
	// V4 layout — Row 1: [중간 요약] [회의록 정리] (primary 강조 2개)
	comps := superSessionStickyComponents()
	row1, ok := comps[0].(discordgo.ActionsRow)
	if !ok {
		t.Fatalf("expected ActionsRow at index 0, got %T", comps[0])
	}
	if len(row1.Components) != 2 {
		t.Fatalf("V4 row 1 button count = %d, want 2 ([중간 요약]+[회의록 정리])", len(row1.Components))
	}
	wantOrder := []struct {
		id    string
		style discordgo.ButtonStyle
		label string
	}{
		{customIDInterimBtn, discordgo.PrimaryButton, "중간 요약"},
		{customIDFinalizeSummarized, discordgo.SuccessButton, "회의록 정리"},
	}
	for i, want := range wantOrder {
		// type assertion ok-check — Button이 아니면 t.Fatalf로 명확한 원인 출력 (panic 방지).
		btn, ok := row1.Components[i].(discordgo.Button)
		if !ok {
			t.Fatalf("row 1[%d]: expected discordgo.Button, got %T", i, row1.Components[i])
		}
		if btn.CustomID != want.id {
			t.Errorf("row 1[%d] customID = %q, want %q", i, btn.CustomID, want.id)
		}
		if btn.Style != want.style {
			t.Errorf("row 1[%d] style = %v, want %v", i, btn.Style, want.style)
		}
		if btn.Label != want.label {
			t.Errorf("row 1[%d] label = %q, want %q", i, btn.Label, want.label)
		}
	}
}

func TestSuperSessionStickyComponents_V4Layout_Row2SecondaryAndDanger(t *testing.T) {
	// V4 layout — Row 2: [GitHub 주간 분석][릴리즈 PR 만들기][AI에게 질문][외부 문서 첨부][세션 종료]
	comps := superSessionStickyComponents()
	row2, ok := comps[1].(discordgo.ActionsRow)
	if !ok {
		t.Fatalf("expected ActionsRow at index 1, got %T", comps[1])
	}
	if len(row2.Components) != 5 {
		t.Fatalf("V4 row 2 button count = %d, want 5 (자료 4 + 세션 종료)", len(row2.Components))
	}
	wantOrder := []struct {
		id    string
		style discordgo.ButtonStyle
		label string
	}{
		{customIDSubActionWeekly, discordgo.SecondaryButton, "GitHub 주간 분석"},
		{customIDSubActionRelease, discordgo.SecondaryButton, "릴리즈 PR 만들기"},
		{customIDSubActionAgent, discordgo.SecondaryButton, "AI에게 질문"},
		{customIDExternalAttach, discordgo.SecondaryButton, "외부 문서 첨부"},
		{customIDSessionEnd, discordgo.DangerButton, "세션 종료"},
	}
	for i, want := range wantOrder {
		// type assertion ok-check — Button이 아니면 t.Fatalf로 명확한 원인 출력 (panic 방지).
		btn, ok := row2.Components[i].(discordgo.Button)
		if !ok {
			t.Fatalf("row 2[%d]: expected discordgo.Button, got %T", i, row2.Components[i])
		}
		if btn.CustomID != want.id {
			t.Errorf("row 2[%d] customID = %q, want %q", i, btn.CustomID, want.id)
		}
		if btn.Style != want.style {
			t.Errorf("row 2[%d] style = %v, want %v", i, btn.Style, want.style)
		}
		if btn.Label != want.label {
			t.Errorf("row 2[%d] label = %q, want %q", i, btn.Label, want.label)
		}
	}
}

func TestBuildSuperSessionStickyMessageSend_BodyMinimal(t *testing.T) {
	got := buildSuperSessionStickyMessageSend(13)
	// 본문은 헤더 한 줄만 — 7 button 설명 표는 사용자 피드백으로 제거 (button label이 self-evident).
	mustContain := []string{
		"super-session 진행 중",
		"13", // 메모 수
	}
	for _, s := range mustContain {
		if !strings.Contains(got.Content, s) {
			t.Errorf("sticky 본문에 %q 누락:\n%s", s, got.Content)
		}
	}
	// 설명 텍스트가 다시 들어오면 회귀 — 명시 거부.
	mustNotContain := []string{
		"지금까지 회의",
		"4 포맷 정리본",
		"레포 활동",
		"미팅 마무리",
	}
	for _, s := range mustNotContain {
		if strings.Contains(got.Content, s) {
			t.Errorf("sticky 본문에 설명 텍스트 %q 잔존 — 헤더만 노출하기로 했음:\n%s", s, got.Content)
		}
	}
	if len(got.Components) != 2 {
		t.Errorf("Components = %d rows, want 2 (V4)", len(got.Components))
	}
}

// TestSuperSessionCustomIDsAreUniqueAndDistinctFromLegacy
// — 새 customID가 legacy customID와 충돌하지 않음을 검증.
// 라우팅에서 case 분기가 같은 prefix 충돌하면 잘못된 핸들러 호출 위험.
func TestSuperSessionCustomIDsAreUniqueAndDistinctFromLegacy(t *testing.T) {
	allIDs := []string{
		// legacy
		customIDMeetingEndBtn,
		customIDInterimBtn,
		customIDFinalizeDecisionStatus,
		customIDFinalizeDiscussion,
		customIDFinalizeRoleBased,
		customIDFinalizeFreeform,
		customIDDirectiveBtn,
		customIDDirectiveRetryBtn,
		// Phase 2
		customIDFinalizeSummarized,
		customIDFormatToggleDecisionStatus,
		customIDFormatToggleDiscussion,
		customIDFormatToggleRoleBased,
		customIDFormatToggleFreeform,
		// Phase 3 신규
		customIDSubActionWeekly,
		customIDSubActionRelease,
		customIDSubActionAgent,
		customIDSessionEnd,
		customIDExternalAttach,
	}
	seen := make(map[string]bool, len(allIDs))
	for _, id := range allIDs {
		if id == "" {
			t.Errorf("빈 customID 발견")
			continue
		}
		if seen[id] {
			t.Errorf("customID 중복: %q", id)
		}
		seen[id] = true
	}
}
