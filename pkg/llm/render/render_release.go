package render

import (
	"fmt"
	"strings"

	"chatbot-alpha-1/pkg/llm"
)

// ReleaseRenderInput은 ReleaseNoteResponse를 PR 본문 마크다운으로 감쌀 때 필요한 메타.
//
// LLM은 ### 섹션부터만 생성하고, H2 헤더("## Release ...")와 비교 메타 풋터는 Go가 주입한다.
// 이렇게 분리하면 LLM이 메타 값(태그명/커밋수)을 환각으로 잘못 채우는 위험이 사라진다.
type ReleaseRenderInput struct {
	ModuleDisplayName string // "프로덕트"
	NewVersion        string // "1.0.1"
	PrevTag           string // "sandbox-product/v1.0.0"
	NewTag            string // "sandbox-product/v1.0.1"
	CommitCount       int
	BumpLabel         string // "메이저" / "마이너" / "패치"
	Response          *llm.ReleaseNoteResponse
}

// RenderReleasePRBody는 GitHub PR 본문에 들어갈 마크다운을 만든다.
//
// 출력 구조:
//
//	## Release {모듈} v{version} ({bump})
//
//	**비교:** {PrevTag} ↔ main ({N} commits)
//
//	<LLM 본문 (### 섹션들)>
//
//	---
//	🤖 봇이 diff/커밋 기반으로 생성한 초안입니다. 머지 전 검토 후 필요 시 직접 편집해주세요.
func RenderReleasePRBody(in ReleaseRenderInput) string {
	var b strings.Builder

	fmt.Fprintf(&b, "## Release %s v%s (%s)\n\n",
		in.ModuleDisplayName, in.NewVersion, in.BumpLabel)
	fmt.Fprintf(&b, "**비교:** `%s` ↔ `main` (%d commits)\n\n",
		in.PrevTag, in.CommitCount)

	if in.Response == nil || strings.TrimSpace(in.Response.Markdown) == "" {
		b.WriteString("_(릴리즈 노트 생성 결과 없음 — 커밋이 비어있거나 LLM 응답이 비어있음)_\n\n")
	} else {
		b.WriteString(strings.TrimSpace(in.Response.Markdown))
		b.WriteString("\n\n")
	}

	b.WriteString("---\n")
	b.WriteString("🤖 봇이 diff/커밋 기반으로 생성한 초안입니다. 머지 전 검토 후 필요 시 직접 편집해주세요.\n")

	return b.String()
}
