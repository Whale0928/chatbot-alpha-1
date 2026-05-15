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

func TestSuperSessionStickyComponents_Row1HasFiveButtons(t *testing.T) {
	comps := superSessionStickyComponents()
	row1, ok := comps[0].(discordgo.ActionsRow)
	if !ok {
		t.Fatalf("expected ActionsRow at index 0, got %T", comps[0])
	}
	if len(row1.Components) != 5 {
		t.Errorf("row 1 button count = %d, want 5 (Discord row max)", len(row1.Components))
	}

	// 정확한 customID 셋 — 순서/위치까지 검증 (UX 가시성)
	wantOrder := []string{
		customIDInterimBtn,
		customIDSubActionWeekly,
		customIDSubActionRelease,
		customIDSubActionAgent,
		customIDFinalizeSummarized,
	}
	for i, want := range wantOrder {
		btn, ok := row1.Components[i].(discordgo.Button)
		if !ok {
			t.Errorf("row 1 position %d: expected Button, got %T", i, row1.Components[i])
			continue
		}
		if btn.CustomID != want {
			t.Errorf("row 1 position %d: customID = %q, want %q", i, btn.CustomID, want)
		}
	}
}

func TestSuperSessionStickyComponents_Row2HasSessionEnd(t *testing.T) {
	comps := superSessionStickyComponents()
	row2, ok := comps[1].(discordgo.ActionsRow)
	if !ok {
		t.Fatalf("expected ActionsRow at index 1, got %T", comps[1])
	}
	if len(row2.Components) != 1 {
		t.Errorf("row 2 button count = %d, want 1 (세션 종료만)", len(row2.Components))
	}
	btn, ok := row2.Components[0].(discordgo.Button)
	if !ok {
		t.Fatalf("row 2: expected Button, got %T", row2.Components[0])
	}
	if btn.CustomID != customIDSessionEnd {
		t.Errorf("row 2 customID = %q, want %q", btn.CustomID, customIDSessionEnd)
	}
	if btn.Style != discordgo.DangerButton {
		t.Errorf("row 2 style = %v, want DangerButton (종료는 위험 색)", btn.Style)
	}
}

func TestSuperSessionStickyComponents_FinalizeButtonHighlighted(t *testing.T) {
	// 정리본은 가장 자주 쓰는 핵심 button — SuccessButton (강조 녹색)
	comps := superSessionStickyComponents()
	row1 := comps[0].(discordgo.ActionsRow)
	for _, c := range row1.Components {
		btn := c.(discordgo.Button)
		if btn.CustomID == customIDFinalizeSummarized {
			if btn.Style != discordgo.SuccessButton {
				t.Errorf("정리본 button style = %v, want SuccessButton", btn.Style)
			}
			return
		}
	}
	t.Errorf("정리본 button을 row 1에서 찾지 못함")
}

func TestBuildSuperSessionStickyMessageSend_ContainsNoteCount(t *testing.T) {
	got := buildSuperSessionStickyMessageSend(13)
	if !strings.Contains(got.Content, "13") {
		t.Errorf("content가 노트 수 13을 포함해야 함, got: %q", got.Content)
	}
	if !strings.Contains(got.Content, "super-session") {
		t.Errorf("content가 'super-session' 라벨을 포함해야 함, got: %q", got.Content)
	}
	if len(got.Components) != 2 {
		t.Errorf("Components = %d rows, want 2", len(got.Components))
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
