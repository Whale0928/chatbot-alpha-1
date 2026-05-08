package render

import (
	"testing"
	"time"

	"chatbot-alpha-1/pkg/llm"
)

func TestRenderFreeform_WrapsHeaderAndFooter(t *testing.T) {
	in := FreeformRenderInput{
		Date:         time.Date(2026, 4, 21, 0, 0, 0, 0, time.UTC),
		Participants: []string{"hgkim"},
		Response: &llm.FreeformResponse{
			Markdown: "## 핵심\n자율 본문",
		},
	}
	out := RenderFreeform(in)
	mustContain(t, out, "# 2026-04-21 미팅 노트")
	mustContain(t, out, "## 핵심")
	mustContain(t, out, "자율 본문")
	mustContain(t, out, "참석자: `hgkim`")
}

func TestRenderFreeform_EmptyMarkdown(t *testing.T) {
	in := FreeformRenderInput{
		Date:         time.Date(2026, 4, 21, 0, 0, 0, 0, time.UTC),
		Participants: []string{"hgkim"},
		Response:     &llm.FreeformResponse{Markdown: "   "},
	}
	out := RenderFreeform(in)
	mustContain(t, out, "## 본문\n- (기록되지 않음)")
}

func TestRenderFreeform_NilResponse(t *testing.T) {
	out := RenderFreeform(FreeformRenderInput{
		Date:         time.Date(2026, 4, 21, 0, 0, 0, 0, time.UTC),
		Participants: []string{"hgkim"},
		Response:     nil,
	})
	mustContain(t, out, "## 본문\n- (기록되지 않음)")
}
