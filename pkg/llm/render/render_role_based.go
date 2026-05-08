package render

import (
	"fmt"
	"strings"
	"time"

	"chatbot-alpha-1/pkg/llm"
)

// RoleBasedRenderInput은 포맷 3번 마크다운 렌더 입력.
type RoleBasedRenderInput struct {
	Date         time.Time
	Participants []string
	Response     *llm.RoleBasedResponse
}

// RenderRoleBased는 역할별 정리형 마크다운을 생성한다.
//
// 출력 구조:
//
//	# YYYY-MM-DD 미팅 노트
//	## 역할별 정리
//	### `speaker`
//	**결정** / **액션** / **공유**   (각 비면 sub-block 생략)
//	## 공통 사항          (SharedItems 있을 때만)
//	## 확인 필요          (있을 때만)
//	---
//	참석자/태그
func RenderRoleBased(in RoleBasedRenderInput) string {
	var b strings.Builder

	fmt.Fprintf(&b, "# %s 미팅 노트\n\n", in.Date.Format("2006-01-02"))

	if in.Response == nil {
		b.WriteString("## 역할별 정리\n- (기록되지 않음)\n\n")
		writeFooter(&b, in.Participants, nil)
		return b.String()
	}

	r := in.Response

	if len(r.Roles) == 0 {
		b.WriteString("## 역할별 정리\n- (기록되지 않음)\n\n")
	} else {
		b.WriteString("## 역할별 정리\n\n")
		for _, role := range r.Roles {
			fmt.Fprintf(&b, "### `%s`\n", role.Speaker)
			if len(role.Decisions) > 0 {
				b.WriteString("**결정**\n")
				for _, d := range role.Decisions {
					fmt.Fprintf(&b, "- %s\n", d)
				}
				b.WriteString("\n")
			}
			if len(role.Actions) > 0 {
				b.WriteString("**액션**\n")
				for _, a := range role.Actions {
					fmt.Fprintf(&b, "- [ ] %s\n", formatNextStep(a))
				}
				b.WriteString("\n")
			}
			if len(role.Shared) > 0 {
				b.WriteString("**공유**\n")
				for _, s := range role.Shared {
					fmt.Fprintf(&b, "- %s\n", s)
				}
				b.WriteString("\n")
			}
		}
	}

	if len(r.SharedItems) > 0 {
		b.WriteString("## 공통 사항\n")
		for _, s := range r.SharedItems {
			fmt.Fprintf(&b, "- %s\n", s)
		}
		b.WriteString("\n")
	}

	writeOpenQuestionsSection(&b, r.OpenQuestions)
	writeFooter(&b, in.Participants, r.Tags)
	return b.String()
}
