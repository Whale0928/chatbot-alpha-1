package render

import (
	"fmt"
	"strings"
	"time"

	"chatbot-alpha-1/pkg/llm"
)

// DiscussionRenderInput은 포맷 2번 마크다운 렌더 입력.
type DiscussionRenderInput struct {
	Date         time.Time
	Participants []string
	Response     *llm.DiscussionResponse
}

// RenderDiscussion은 논의 중심형 마크다운을 생성한다.
//
// 출력 구조:
//
//	# YYYY-MM-DD 미팅 노트
//	## 논의 토픽
//	### 1. <llm.Topic.Title>
//	- flow item
//	- flow item
//	**도출된 관점**       (Insights 있을 때만)
//	- insight
//	## 확인 필요          (있을 때만)
//	---
//	참석자/태그
func RenderDiscussion(in DiscussionRenderInput) string {
	var b strings.Builder

	fmt.Fprintf(&b, "# %s 미팅 노트\n\n", in.Date.Format("2006-01-02"))

	if in.Response == nil {
		b.WriteString("## 논의 토픽\n- (기록되지 않음)\n\n")
		writeFooter(&b, in.Participants, nil)
		return b.String()
	}

	r := in.Response

	if len(r.Topics) == 0 {
		b.WriteString("## 논의 토픽\n- (기록되지 않음)\n\n")
	} else {
		b.WriteString("## 논의 토픽\n\n")
		for i, t := range r.Topics {
			fmt.Fprintf(&b, "### %d. %s\n", i+1, t.Title)
			for _, f := range t.Flow {
				fmt.Fprintf(&b, "- %s\n", f)
			}
			if len(t.Insights) > 0 {
				b.WriteString("\n**도출된 관점**\n")
				for _, ins := range t.Insights {
					fmt.Fprintf(&b, "- %s\n", ins)
				}
			}
			b.WriteString("\n")
		}
	}

	writeOpenQuestionsSection(&b, r.OpenQuestions)
	writeFooter(&b, in.Participants, r.Tags)

	return b.String()
}
