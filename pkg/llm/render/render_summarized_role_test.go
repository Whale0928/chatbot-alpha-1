package render

import (
	"strings"
	"testing"

	"chatbot-alpha-1/pkg/llm"
)

// extractSection은 markdown 문서에서 "## {header}" 라인부터 다음 "## " 헤더 직전까지 슬라이스한다.
// 다음 헤더가 없으면 문서 끝까지. 테스트가 섹션별 내용만 검증하도록 보호 (false-positive 방지).
func extractSection(doc, header string) string {
	marker := "## " + header
	start := strings.Index(doc, marker)
	if start < 0 {
		return ""
	}
	rest := doc[start+len(marker):]
	if end := strings.Index(rest, "\n## "); end >= 0 {
		return rest[:end]
	}
	return rest
}

func TestRenderSummarizedRoleBased_GroupsByRole(t *testing.T) {
	got := RenderSummarizedRoleBased(sampleInput(sample5_14Content()))

	// 모든 등장 role이 섹션 헤더로
	for _, role := range []string{"BACKEND", "FRONTEND", "PM"} {
		if !strings.Contains(got, "## "+role) {
			t.Errorf("role 섹션 %q 누락:\n%s", role, got)
		}
	}
}

func TestRenderSummarizedRoleBased_SelfActionInOriginGroup(t *testing.T) {
	// deadwhale(BE) 자기 발의 액션이 BACKEND "자기 발의 액션" 섹션에 등장
	got := RenderSummarizedRoleBased(sampleInput(sample5_14Content()))
	beBlock := extractSection(got, "BACKEND")
	if beBlock == "" {
		t.Fatalf("BACKEND 섹션 추출 실패:\n%s", got)
	}

	if !strings.Contains(beBlock, "**자기 발의 액션**") {
		t.Errorf("BACKEND 섹션에 '자기 발의 액션' 헤더 누락:\n%s", beBlock)
	}
	if !strings.Contains(beBlock, "큐레이션 order spec 확장") {
		t.Errorf("BACKEND 자기 발의 액션 본문 누락:\n%s", beBlock)
	}
}

func TestRenderSummarizedRoleBased_CrossRoleInTargetGroup(t *testing.T) {
	// kimjuye(PM)의 cross-role "FE 체크 요청"은 FRONTEND "받은 요청"에 등장 (PM 그룹 X)
	got := RenderSummarizedRoleBased(sampleInput(sample5_14Content()))

	// 섹션별 정밀 추출 — extractSection으로 다음 ## 헤더 전까지만
	feBlock := extractSection(got, "FRONTEND")
	pmBlock := extractSection(got, "PM")
	if feBlock == "" || pmBlock == "" {
		t.Fatalf("FRONTEND/PM 섹션 추출 실패")
	}

	if !strings.Contains(feBlock, "**받은 요청**") {
		t.Errorf("FRONTEND 섹션에 '받은 요청' 헤더 누락:\n%s", feBlock)
	}
	if !strings.Contains(feBlock, "GitHub 이슈 206/207/208") {
		t.Errorf("FRONTEND '받은 요청'에 cross-role 액션 누락:\n%s", feBlock)
	}
	if !strings.Contains(feBlock, "from: kimjuye(PM)") {
		t.Errorf("FRONTEND 받은 요청에 from 메타 누락 (cross-role 발의자 식별 깨짐):\n%s", feBlock)
	}

	// PM 섹션엔 cross-role 액션 자체가 등장하면 안 됨 (대상 그룹 FE에만 있어야 함)
	if strings.Contains(pmBlock, "GitHub 이슈 206/207/208") {
		t.Error("cross-role 액션이 발의자(PM) 그룹에도 잘못 등장 — 대상 그룹 분리 정책 깨짐")
	}
}

func TestRenderSummarizedRoleBased_AmbiguousTargetGoesToOriginSelf(t *testing.T) {
	// kimjuye의 "위스키 캐스크 완료"는 TargetRoles 비어있음 → PM 자기 발의에 등장
	got := RenderSummarizedRoleBased(sampleInput(sample5_14Content()))
	pmBlock := extractSection(got, "PM")
	if pmBlock == "" {
		t.Fatal("PM 섹션 추출 실패")
	}

	if !strings.Contains(pmBlock, "**자기 발의 액션**") {
		t.Errorf("PM 섹션에 '자기 발의 액션' 헤더 누락:\n%s", pmBlock)
	}
	if !strings.Contains(pmBlock, "위스키 캐스크정보 업데이트 완료") {
		t.Errorf("PM 자기 발의 (대상 모호 케이스) 누락:\n%s", pmBlock)
	}
}

// TestRenderSummarizedRoleBased_TargetUserOnlyFallsBackToOriginGroup — C1 회귀 검증.
//
// TargetRoles가 비어있고 TargetUser만 명시된 cross-role 액션은 어떤 role 섹션에도 등장하지
// 않는 누락이 있었다. classifyActionForRole의 폴백으로 발의자 그룹에 placementSelf로 노출.
func TestRenderSummarizedRoleBased_TargetUserOnlyFallsBackToOriginGroup(t *testing.T) {
	c := &llm.SummarizedContent{
		Actions: []llm.SummaryAction{{
			What:        "현기님께 큐레이션 화면 전달",
			Origin:      "kimjuye",
			OriginRoles: []string{"PM"},
			TargetUser:  "현기", // TargetRoles 의도적으로 비어있음
		}},
	}
	in := SummarizedRenderInput{
		Content:       c,
		RolesSnapshot: map[string][]string{"kimjuye": {"PM"}},
	}
	got := RenderSummarizedRoleBased(in)

	pmBlock := extractSection(got, "PM")
	if pmBlock == "" {
		t.Fatal("PM 섹션 누락 — 폴백 동작 깨짐")
	}
	if !strings.Contains(pmBlock, "현기님께 큐레이션 화면 전달") {
		t.Errorf("TargetUser-only 액션이 발의자(PM) 그룹에 폴백 노출 안 됨:\n%s", pmBlock)
	}
}

func TestRenderSummarizedRoleBased_MembersListedFromSnapshot(t *testing.T) {
	got := RenderSummarizedRoleBased(sampleInput(sample5_14Content()))

	mustContain := []string{
		"_members: deadwhale_",
		"_members: hyejungpark_",
		"_members: kimjuye_",
	}
	for _, sub := range mustContain {
		if !strings.Contains(got, sub) {
			t.Errorf("members 표시 누락: %q in:\n%s", sub, got)
		}
	}
}

func TestCollectAllRoles_UnionAndSorted(t *testing.T) {
	actions := []llm.SummaryAction{
		{OriginRoles: []string{"BACKEND"}, TargetRoles: []string{"FRONTEND"}},
		{OriginRoles: []string{"PM"}, TargetRoles: []string{"PM"}},
	}
	snapshot := map[string][]string{
		"alice": {"BACKEND", "DESIGN"},
	}
	got := collectAllRoles(actions, snapshot)
	want := []string{"BACKEND", "DESIGN", "FRONTEND", "PM"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("position %d: got %q, want %q", i, got[i], w)
		}
	}
}

func TestRenderSummarizedRoleBased_NoActionsForRoleShowsHint(t *testing.T) {
	c := &llm.SummarizedContent{}
	in := SummarizedRenderInput{
		Content:       c,
		RolesSnapshot: map[string][]string{"alice": {"BACKEND"}},
	}
	got := RenderSummarizedRoleBased(in)
	if !strings.Contains(got, "## BACKEND") {
		t.Fatal("BACKEND 섹션이 액션 없어도 등장해야 함 (디버깅 단서)")
	}
	if !strings.Contains(got, "이 role에 attribute된 액션이 없습니다") {
		t.Errorf("빈 role hint 누락:\n%s", got)
	}
}

// =====================================================================
// RenderSummarizedFreeform
// =====================================================================

func TestRenderSummarizedFreeform_OneParagraphSummary(t *testing.T) {
	got := RenderSummarizedFreeform(sampleInput(sample5_14Content()))

	mustContain := []string{
		"# 2026-05-14 미팅 — 자율 정리",
		"결정 2건 · 액션 3건 · 미정 1건.",
		"가장 활발한 토픽: workspace 통합 이슈 정리.",
		"**핵심 결정**",
		"**핵심 액션**",
		"**논의 흐름**",
		"**확인 필요**",
		"참석자: deadwhale, hyejungpark, kimjuye",
	}
	for _, sub := range mustContain {
		if !strings.Contains(got, sub) {
			t.Errorf("missing %q in:\n%s", sub, got)
		}
	}
}

func TestComposeFreeformSummary_EmptyContentReturnsEmpty(t *testing.T) {
	got := composeFreeformSummary(&llm.SummarizedContent{})
	if got != "" {
		t.Errorf("빈 content → want empty summary, got %q", got)
	}
}

func TestComposeFreeformSummary_NilReturnsEmpty(t *testing.T) {
	if got := composeFreeformSummary(nil); got != "" {
		t.Errorf("nil → want empty, got %q", got)
	}
}

func TestJoinFlowOneLine(t *testing.T) {
	tests := []struct {
		name string
		in   []string
		want string
	}{
		{"empty", nil, ""},
		{"single", []string{"한 줄"}, "한 줄"},
		{"multi joined by arrow", []string{"a", "b", "c"}, "a → b → c"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := joinFlowOneLine(tc.in); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}
