package render

import (
	"fmt"
	"strings"
	"time"

	"chatbot-alpha-1/pkg/llm"
)

// FreeformRenderInput은 포맷 4번 마크다운 렌더 입력.
type FreeformRenderInput struct {
	Date         time.Time
	Participants []string
	Response     *llm.FreeformResponse
}

// RenderFreeform은 LLM이 작성한 자율 마크다운을 H1 헤더 + 풋터로 감싼다.
//
// LLM은 ## 헤딩부터 작성한 본문(Markdown 필드)을 반환하고, 헤더와
// 참석자/태그 풋터는 Go가 일관되게 주입한다.
func RenderFreeform(in FreeformRenderInput) string {
	var b strings.Builder

	fmt.Fprintf(&b, "# %s 미팅 노트\n\n", in.Date.Format("2006-01-02"))

	if in.Response == nil || strings.TrimSpace(in.Response.Markdown) == "" {
		b.WriteString("## 본문\n- (기록되지 않음)\n\n")
		writeFooter(&b, in.Participants, nil)
		return b.String()
	}

	body := strings.TrimSpace(in.Response.Markdown)
	b.WriteString(body)
	b.WriteString("\n\n")

	// Freeform은 LLM이 태그를 별도 필드로 주지 않으므로 풋터에 참석자만.
	writeFooter(&b, in.Participants, nil)
	return b.String()
}
