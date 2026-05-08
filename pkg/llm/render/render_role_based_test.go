package render

import (
	"testing"
	"time"

	"chatbot-alpha-1/pkg/llm"
)

func TestRenderRoleBased_Full(t *testing.T) {
	in := RoleBasedRenderInput{
		Date:         time.Date(2026, 4, 21, 0, 0, 0, 0, time.UTC),
		Participants: []string{"hgkim", "whale"},
		Response: &llm.RoleBasedResponse{
			Roles: []llm.RoleSection{
				{
					Speaker:   "hgkim",
					Decisions: []string{"크롤러 안정화를 최우선으로 진행"},
					Actions: []llm.NextStep{
						{Who: "hgkim", What: "발행일자 추출 로직 정리", Deadline: "2026-04-21"},
					},
					Shared: []string{"실서버 정상 확인"},
				},
				{
					Speaker: "whale",
					Actions: []llm.NextStep{
						{Who: "whale", What: "라이 위스키 필터 #223 담당"},
					},
				},
			},
			SharedItems:   []string{"이번 스프린트 마감: 2026-04-25"},
			OpenQuestions: []string{"#148 인계 대상 - 미정. 확인 필요"},
			Tags:          []string{"스프린트", "크롤러"},
		},
	}
	out := RenderRoleBased(in)

	mustContain(t, out, "# 2026-04-21 미팅 노트")
	mustContain(t, out, "## 역할별 정리")
	mustContain(t, out, "### `hgkim`")
	mustContain(t, out, "**결정**")
	mustContain(t, out, "- 크롤러 안정화를 최우선으로 진행")
	mustContain(t, out, "**액션**")
	mustContain(t, out, "- [ ] `hgkim` — 발행일자 추출 로직 정리 _(기한: 2026-04-21)_")
	mustContain(t, out, "**공유**")
	mustContain(t, out, "- 실서버 정상 확인")
	mustContain(t, out, "### `whale`")
	mustContain(t, out, "- [ ] `whale` — 라이 위스키 필터 #223 담당")
	mustContain(t, out, "## 공통 사항")
	mustContain(t, out, "- 이번 스프린트 마감: 2026-04-25")
	mustContain(t, out, "## 확인 필요")
	mustContain(t, out, "참석자: `hgkim`, `whale`")
}

func TestRenderRoleBased_OmitsEmptySubBlocks(t *testing.T) {
	in := RoleBasedRenderInput{
		Date:         time.Date(2026, 4, 21, 0, 0, 0, 0, time.UTC),
		Participants: []string{"whale"},
		Response: &llm.RoleBasedResponse{
			Roles: []llm.RoleSection{
				{Speaker: "whale", Shared: []string{"공유만 있음"}},
			},
		},
	}
	out := RenderRoleBased(in)
	mustContain(t, out, "**공유**")
	mustContain(t, out, "- 공유만 있음")
	mustNotContain(t, out, "**결정**")
	mustNotContain(t, out, "**액션**")
}
