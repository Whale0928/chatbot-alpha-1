package render

import (
	"strings"
	"testing"
	"time"

	"chatbot-alpha-1/pkg/llm"
)

// 5/14 미팅 데이터 기반 fixture — 거시 디자인 결정의 cross-role 케이스 포함
func sample5_14Content() *llm.SummarizedContent {
	return &llm.SummarizedContent{
		Decisions: []llm.Decision{
			{Title: "workspace 기획·정책 이슈 통합 처리", Context: []string{"이슈 155, 170, 195 묶음", "5/15 정리 예정"}},
			{Title: "릴리즈는 매주 git-bot 검증으로 정책화"},
		},
		Done:       []string{"큐레이션 화면 제작 및 전달", "푸시 기능 제거"},
		InProgress: []string{"위스키 캐스크정보 90%", "지역 어드민 화면 개발 (FE)"},
		Planned:    []string{"홈화면 토글 UI 개선 배포 (FE)", "큐레이션 order spec 확장 (BE)"},
		Blockers:   []string{"order spec 적용 범위 미정"},
		Topics: []llm.Topic{
			{
				Title:    "workspace 통합 이슈 정리",
				Flow:     []string{"kimjuye가 기획·정책 이슈와 바 정보 이슈 묶음 제안", "5/15 정리 합의"},
				Insights: []string{"묶음 기준 합의가 후속 필요"},
			},
		},
		Actions: []llm.SummaryAction{
			// 자기 발의 (BE→BE)
			{
				What: "큐레이션 order spec 확장 / 관리자 order 제어 구현",
				Origin: "deadwhale", OriginRoles: []string{"BACKEND"}, TargetRoles: []string{"BACKEND"},
				Deadline: "2026-05-21",
			},
			// cross-role (PM → FE)
			{
				What: "GitHub 이슈 206/207/208 체크",
				Origin: "kimjuye", OriginRoles: []string{"PM"}, TargetRoles: []string{"FRONTEND"},
				Deadline: "2026-05-21",
			},
			// PM 자기 발의 (target 모호)
			{
				What: "위스키 캐스크정보 업데이트 완료",
				Origin: "kimjuye", OriginRoles: []string{"PM"},
				Deadline: "2026-05-21",
			},
		},
		Shared:        []string{"차주 미팅: 2026-05-21 08:00"},
		OpenQuestions: []string{"workspace 통합 이슈의 묶음 기준 - 확인 필요"},
		Tags:          []string{"workspace", "큐레이션", "위스키", "릴리즈"},
	}
}

func sampleInput(c *llm.SummarizedContent) SummarizedRenderInput {
	return SummarizedRenderInput{
		Content:  c,
		Date:     time.Date(2026, 5, 14, 0, 0, 0, 0, time.UTC),
		Speakers: []string{"deadwhale", "hyejungpark", "kimjuye"},
		RolesSnapshot: map[string][]string{
			"deadwhale":   {"BACKEND"},
			"hyejungpark": {"FRONTEND"},
			"kimjuye":     {"PM"},
		},
	}
}

// =====================================================================
// RenderSummarizedDecisionStatus
// =====================================================================

func TestRenderSummarizedDecisionStatus_AllSectionsPresent(t *testing.T) {
	got := RenderSummarizedDecisionStatus(sampleInput(sample5_14Content()))

	mustContain := []string{
		"# 2026-05-14 미팅 노트",
		"## 결정사항",
		"- **workspace 기획·정책 이슈 통합 처리**",
		"  - 이슈 155, 170, 195 묶음",
		"## 완료",
		"- 큐레이션 화면 제작 및 전달",
		"## 진행 중",
		"- 위스키 캐스크정보 90%",
		"## 예정",
		"## 이슈/블로커",
		"- order spec 적용 범위 미정",
		"## 미정 질문",
		"- workspace 통합 이슈의 묶음 기준 - 확인 필요",
		"## 액션",
		"---",
		"참석자: deadwhale, hyejungpark, kimjuye",
		"태그: #workspace #큐레이션 #위스키 #릴리즈",
	}
	for _, sub := range mustContain {
		if !strings.Contains(got, sub) {
			t.Errorf("missing %q in:\n%s", sub, got)
		}
	}
}

func TestRenderSummarizedDecisionStatus_CrossRoleActionShowsFromMetadata(t *testing.T) {
	// kimjuye(PM)의 cross-role "FE 체크 요청"이 액션 섹션에 from 메타로 표시
	got := RenderSummarizedDecisionStatus(sampleInput(sample5_14Content()))
	want := "FRONTEND — GitHub 이슈 206/207/208 체크 (from: kimjuye(PM), 기한: 2026-05-21)"
	if !strings.Contains(got, want) {
		t.Errorf("cross-role 액션이 누락되거나 형식 다름 — want %q in:\n%s", want, got)
	}
}

func TestRenderSummarizedDecisionStatus_SelfRoleActionShowsOriginNoFromTag(t *testing.T) {
	// deadwhale(BE)의 자기 발의 — Copilot review 지적 반영:
	// 그룹 라벨 단독 ("BACKEND —") 대신 발화자 + 그룹 ("deadwhale(BACKEND) —") 노출.
	// from 메타는 self-initiated라 redundancy로 생략.
	got := RenderSummarizedDecisionStatus(sampleInput(sample5_14Content()))
	beActionLine := "deadwhale(BACKEND) — 큐레이션 order spec 확장 / 관리자 order 제어 구현 (기한: 2026-05-21)"
	if !strings.Contains(got, beActionLine) {
		t.Errorf("자기 발의 액션 형식 다름 — want %q in:\n%s", beActionLine, got)
	}
	// from: deadwhale 표기는 자기 발의에는 등장하지 않아야 함 (assignee가 이미 origin)
	if strings.Contains(got, "from: deadwhale") {
		t.Error("자기 발의 액션에 from 메타가 잘못 등장 (cross-role detection 깨짐)")
	}
}

func TestRenderSummarizedDecisionStatus_EmptySectionsAreSkipped(t *testing.T) {
	c := &llm.SummarizedContent{
		Decisions: []llm.Decision{{Title: "단일 결정"}},
		// done/in_progress/.../actions/topics/shared/tags 모두 빈 채로
	}
	in := SummarizedRenderInput{Content: c, Date: time.Date(2026, 5, 14, 0, 0, 0, 0, time.UTC)}
	got := RenderSummarizedDecisionStatus(in)

	skipped := []string{"## 완료", "## 진행 중", "## 예정", "## 이슈/블로커", "## 미정 질문", "## 액션"}
	for _, sec := range skipped {
		if strings.Contains(got, sec) {
			t.Errorf("빈 섹션 %q가 자리 채움으로 출력됨 (expected skipped):\n%s", sec, got)
		}
	}
}

func TestRenderSummarizedDecisionStatus_NilContentReturnsEmpty(t *testing.T) {
	got := RenderSummarizedDecisionStatus(SummarizedRenderInput{Content: nil})
	if got != "" {
		t.Errorf("nil content → want empty, got %q", got)
	}
}

// =====================================================================
// RenderSummarizedDiscussion
// =====================================================================

func TestRenderSummarizedDiscussion_TopicStructure(t *testing.T) {
	got := RenderSummarizedDiscussion(sampleInput(sample5_14Content()))

	mustContain := []string{
		"# 2026-05-14 미팅 — 논의 정리",
		"## workspace 통합 이슈 정리",
		"**흐름**",
		"- kimjuye가 기획·정책 이슈와 바 정보 이슈 묶음 제안",
		"**도출 관점**",
		"- 묶음 기준 합의가 후속 필요",
		"## 미정 질문",
	}
	for _, sub := range mustContain {
		if !strings.Contains(got, sub) {
			t.Errorf("missing %q in:\n%s", sub, got)
		}
	}
}

func TestRenderSummarizedDiscussion_EmptyTopicsShowsHint(t *testing.T) {
	c := &llm.SummarizedContent{} // 모든 필드 빈 채
	in := SummarizedRenderInput{Content: c, Date: time.Date(2026, 5, 14, 0, 0, 0, 0, time.UTC)}
	got := RenderSummarizedDiscussion(in)

	if !strings.Contains(got, "토픽이 추출되지 않았습니다") {
		t.Errorf("Topics 빈 케이스에 디버깅 단서 누락:\n%s", got)
	}
}

// =====================================================================
// formatActionLine / isCrossRoleAction (pure helper)
// =====================================================================

func TestIsCrossRoleAction(t *testing.T) {
	tests := []struct {
		name string
		a    llm.SummaryAction
		want bool
	}{
		{
			name: "self BE→BE",
			a:    llm.SummaryAction{OriginRoles: []string{"BACKEND"}, TargetRoles: []string{"BACKEND"}},
			want: false,
		},
		{
			name: "cross PM→FE",
			a:    llm.SummaryAction{OriginRoles: []string{"PM"}, TargetRoles: []string{"FRONTEND"}},
			want: true,
		},
		{
			name: "partial cross BE→[BE,FE]",
			a:    llm.SummaryAction{OriginRoles: []string{"BACKEND"}, TargetRoles: []string{"BACKEND", "FRONTEND"}},
			want: true,
		},
		{
			name: "ambiguous target (empty TargetRoles, no TargetUser)",
			a:    llm.SummaryAction{OriginRoles: []string{"PM"}, TargetRoles: nil},
			want: false,
		},
		{
			name: "specific user different from origin",
			a:    llm.SummaryAction{Origin: "kimjuye", OriginRoles: []string{"PM"}, TargetUser: "현기"},
			want: true,
		},
		{
			name: "specific user == origin (self note)",
			a:    llm.SummaryAction{Origin: "kimjuye", OriginRoles: []string{"PM"}, TargetUser: "kimjuye"},
			want: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := isCrossRoleAction(tc.a)
			if got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestFormatActionLine_AmbiguousTargetUsesOriginWithRole(t *testing.T) {
	// TargetRoles/TargetUser 둘 다 비고 OriginRoles가 있으면 Origin(Role) 형태로 표시
	// (Copilot review 지적 — 그룹 식별성 + 개인 식별성 둘 다 보존).
	a := llm.SummaryAction{What: "위스키 캐스크 완료", Origin: "kimjuye", OriginRoles: []string{"PM"}, Deadline: "2026-05-21"}
	got := formatActionLine(a)
	want := "[ ] kimjuye(PM) — 위스키 캐스크 완료 (기한: 2026-05-21)"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestFormatActionLine_OriginOnlyWithoutRoles(t *testing.T) {
	// OriginRoles가 비어있으면 Origin만 표시
	a := llm.SummaryAction{What: "task", Origin: "alice"}
	got := formatActionLine(a)
	want := "[ ] alice — task"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestFormatActionLine_TargetUserAndRolesShownTogether — I1 회귀 검증.
//
// TargetUser와 TargetRoles가 동시에 명시되면 assignee는 "username(ROLE)" 형식으로
// role 그룹 정체성을 함께 표시 (결정 5/B의 그룹 가시성 보존).
func TestFormatActionLine_TargetUserAndRolesShownTogether(t *testing.T) {
	a := llm.SummaryAction{
		What:        "GitHub 이슈 206 체크",
		Origin:      "kimjuye",
		OriginRoles: []string{"PM"},
		TargetUser:  "hyejungpark",
		TargetRoles: []string{"FRONTEND"},
		Deadline:    "2026-05-21",
	}
	got := formatActionLine(a)
	want := "[ ] hyejungpark(FRONTEND) — GitHub 이슈 206 체크 (from: kimjuye(PM), 기한: 2026-05-21)"
	if got != want {
		t.Errorf("got %q\nwant %q", got, want)
	}
}

// TestFormatActionLine_TargetUserSameAsOriginNoFromMeta — I2 회귀 검증.
//
// TargetUser가 Origin과 같은 셀프 노트는 isCrossRoleAction=false → from 메타가 붙지 않아야 함
// (assignee == origin 중복 표시 방지).
func TestFormatActionLine_TargetUserSameAsOriginNoFromMeta(t *testing.T) {
	a := llm.SummaryAction{
		What:        "셀프 체크",
		Origin:      "kimjuye",
		OriginRoles: []string{"PM"},
		TargetUser:  "kimjuye", // self note
	}
	got := formatActionLine(a)
	if strings.Contains(got, "from:") {
		t.Errorf("자기 자신 지목 시 from 메타가 잘못 등장: %q", got)
	}
}
