package render

import (
	"fmt"
	"strings"
	"time"

	"chatbot-alpha-1/pkg/llm"
)

// WeeklyRenderInput은 WeeklyReportResponse를 마크다운으로 감쌀 때 필요한 메타.
type WeeklyRenderInput struct {
	RepoFullName string // "owner/name"
	RepoURL      string // "https://github.com/owner/name" — 없으면 자동 생성
	Since        time.Time
	Until        time.Time
	IssueCount   int                       // dump에 들어간 이슈 개수 (PR 제외)
	CommitCount  int                       // dump에 들어간 커밋 개수
	Response     *llm.WeeklyReportResponse // nil이면 빈 본문
}

// RenderWeekly는 LLM이 만든 본문 마크다운에 H1 헤더와 메타 풋터를 감싼다.
//
// 출력 구조:
//
//	# YYYY-MM-DD 주간 리포트 — owner/name
//
//	<LLM 본문 (## 섹션들)>
//
//	---
//	레포: https://github.com/owner/name
//	대상: open 이슈 N건 / 커밋 M건 (커밋 윈도우 YYYY-MM-DD ~ YYYY-MM-DD)
func RenderWeekly(in WeeklyRenderInput) string {
	var b strings.Builder

	fmt.Fprintf(&b, "# %s 주간 리포트 — %s\n\n",
		in.Until.Format("2006-01-02"),
		in.RepoFullName,
	)

	if in.Response == nil || strings.TrimSpace(in.Response.Markdown) == "" {
		b.WriteString("_(분석 결과 없음)_\n\n")
	} else {
		body := strings.TrimSpace(in.Response.Markdown)
		b.WriteString(body)
		b.WriteString("\n\n")
	}

	repoURL := in.RepoURL
	if repoURL == "" && in.RepoFullName != "" {
		repoURL = "https://github.com/" + in.RepoFullName
	}
	b.WriteString("---\n")
	if repoURL != "" {
		fmt.Fprintf(&b, "레포: %s\n", repoURL)
	}
	fmt.Fprintf(&b, "대상: open 이슈 %d건 / 커밋 %d건 (커밋 윈도우 %s ~ %s)\n",
		in.IssueCount, in.CommitCount,
		in.Since.Format("2006-01-02"), in.Until.Format("2006-01-02"))

	return b.String()
}
