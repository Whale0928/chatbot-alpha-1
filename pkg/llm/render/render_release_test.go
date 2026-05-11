package render

import (
	"strings"
	"testing"

	"chatbot-alpha-1/pkg/llm"
)

func TestRenderReleasePRBody_Basic(t *testing.T) {
	got := RenderReleasePRBody(ReleaseRenderInput{
		ModuleDisplayName: "프로덕트",
		NewVersion:        "1.0.1",
		PrevTag:           "sandbox-product/v1.0.0",
		NewTag:            "sandbox-product/v1.0.1",
		CommitCount:       3,
		BumpLabel:         "패치",
		Response: &llm.ReleaseNoteResponse{
			Markdown: "### 신규 / 개선\n- 항목 A\n\n### 버그 수정\n- 항목 B",
		},
	})

	mustContain := []string{
		"## Release 프로덕트 v1.0.1 (패치)",
		"`sandbox-product/v1.0.0`",
		"3 commits",
		"### 신규 / 개선",
		"- 항목 A",
		"### 버그 수정",
		"- 항목 B",
		"🤖 봇이",
	}
	for _, s := range mustContain {
		if !strings.Contains(got, s) {
			t.Errorf("렌더에 %q 누락:\n%s", s, got)
		}
	}
}

func TestRenderReleasePRBody_EmptyResponse(t *testing.T) {
	got := RenderReleasePRBody(ReleaseRenderInput{
		ModuleDisplayName: "어드민",
		NewVersion:        "2.0.0",
		PrevTag:           "sandbox-admin/v1.5.0",
		NewTag:            "sandbox-admin/v2.0.0",
		CommitCount:       0,
		BumpLabel:         "메이저",
		Response:          nil,
	})
	if !strings.Contains(got, "_(릴리즈 노트 생성 결과 없음") {
		t.Errorf("빈 응답 처리 누락:\n%s", got)
	}
	if !strings.Contains(got, "## Release 어드민 v2.0.0 (메이저)") {
		t.Errorf("헤더 누락:\n%s", got)
	}
}

func TestRenderReleasePRBody_WhitespaceResponse(t *testing.T) {
	got := RenderReleasePRBody(ReleaseRenderInput{
		ModuleDisplayName: "배치",
		NewVersion:        "0.2.0",
		PrevTag:           "sandbox-batch/v0.1.0",
		NewTag:            "sandbox-batch/v0.2.0",
		CommitCount:       5,
		BumpLabel:         "마이너",
		Response:          &llm.ReleaseNoteResponse{Markdown: "   \n\n  "},
	})
	if !strings.Contains(got, "_(릴리즈 노트 생성 결과 없음") {
		t.Errorf("공백만 있는 본문도 빈 처리되어야 함:\n%s", got)
	}
}
