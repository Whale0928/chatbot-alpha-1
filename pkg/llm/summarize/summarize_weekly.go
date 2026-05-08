package summarize

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"chatbot-alpha-1/pkg/github"
	"chatbot-alpha-1/pkg/llm"
	"chatbot-alpha-1/pkg/llm/prompts"
)

// weeklyReportSchema는 WeeklyReportResponse용 strict JSON Schema.
var weeklyReportSchema = llm.GenerateSchema[llm.WeeklyReportResponse]()

// weeklyBodyExcerptRunes는 user 메시지에 포함시키는 이슈 본문 발췌 길이.
// 토큰 절약 위해 짧게 자른다 — 50개 이슈 × 200 룬 ≒ 만 룬 정도.
const weeklyBodyExcerptRunes = 200

// Weekly는 단일 레포의 주간 이슈 + 커밋 dump를 LLM에게 보내 운영 진단 마크다운을 생성한다.
//
// repoFullName은 "owner/name" 형식. since/until은 user 메시지 헤더에만 들어간다.
// issues는 ListIssues 결과 (PR 제외 권장).
// commits는 ListCommits 결과 (그대로). 둘 중 하나가 비어도 OK — 워크스페이스는 이슈 위주,
// BE/FE 레포는 커밋 위주라 비대칭 분포가 정상.
// directive는 사용자 [추가 요청] 입력. 빈 문자열이면 미적용.
func Weekly(
	ctx context.Context,
	c *llm.Client,
	repoFullName string,
	since, until time.Time,
	issues []github.Issue,
	commits []github.Commit,
	directive string,
) (*llm.WeeklyReportResponse, error) {
	userMsg := buildWeeklyUserMessage(repoFullName, since, until, issues, commits, directive)
	raw, err := callMeetingFormat(ctx, c, "weekly", prompts.Weekly, userMsg, "weekly_report", weeklyReportSchema)
	if err != nil {
		return nil, err
	}
	var out llm.WeeklyReportResponse
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, fmt.Errorf("llm: unmarshal weekly: %w (raw=%q)", err, raw)
	}
	return &out, nil
}

// commitMessageFirstLineRunes는 커밋 메시지에서 LLM에 보낼 첫 줄 길이 한도.
// 두 번째 줄부터는 보통 부연이라 토큰 절약 위해 잘라낸다.
const commitMessageFirstLineRunes = 200

// buildWeeklyUserMessage는 LLM에게 줄 user 메시지를 구성한다.
// 형식은 사람이 읽어도 자연스러운 텍스트 — JSON dump보다 LLM이 잘 받음.
//
// directive가 비어있지 않으면 헤더 직후 "Reporting directive ..." 블록이 prepend되어
// 시스템 프롬프트의 default 가이드를 보강한다 (스키마는 못 깨도록).
//
// issues / commits 둘 다 섹션이 들어간다. 한쪽이 비어도 다른 쪽으로 분석을 진행할 수 있도록
// LLM에게 두 데이터 소스의 비대칭 분포가 정상임을 시스템 프롬프트가 가이드한다.
func buildWeeklyUserMessage(repoFullName string, since, until time.Time, issues []github.Issue, commits []github.Commit, directive string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Repository: %s\n", repoFullName)
	fmt.Fprintf(&b, "Commit window: %s ~ %s\n",
		since.UTC().Format("2006-01-02"), until.UTC().Format("2006-01-02"))
	b.WriteString("Issue scope: all currently OPEN issues (no time window)\n")

	if d := strings.TrimSpace(directive); d != "" {
		b.WriteString("\nReporting directive from the operator (priority over default style, but must not violate the schema):\n")
		b.WriteString(d)
		b.WriteString("\n")
	}

	fmt.Fprintf(&b, "\nOpen issues (count=%d, sorted by latest activity):\n\n", len(issues))
	if len(issues) == 0 {
		b.WriteString("(no open issues)\n")
	} else {
		for _, it := range issues {
			writeIssueBlock(&b, it)
		}
	}

	fmt.Fprintf(&b, "\nCommits in window (count=%d, newest first):\n\n", len(commits))
	if len(commits) == 0 {
		b.WriteString("(no commits in this window)\n")
	} else {
		for _, c := range commits {
			writeCommitBlock(&b, c)
		}
	}
	return b.String()
}

// writeCommitBlock은 단일 커밋을 한 줄로 dump한다.
// 형식: "- abc1234 by login (2026-05-08): feat: 어드민 ... (첫 줄 잘림)"
func writeCommitBlock(b *strings.Builder, c github.Commit) {
	sha := c.SHA
	if len(sha) > 7 {
		sha = sha[:7]
	}
	author := c.AuthorLogin
	if author == "" {
		author = c.AuthorName
	}
	if author == "" {
		author = "(unknown)"
	}
	firstLine := commitFirstLine(c.Message)
	fmt.Fprintf(b, "- %s by %s (%s): %s\n",
		sha, author, c.Date.UTC().Format("2006-01-02"), firstLine)
}

// commitFirstLine은 커밋 메시지의 첫 줄만 추출하고 룬 한도로 자른다.
func commitFirstLine(msg string) string {
	msg = strings.TrimSpace(msg)
	if i := strings.IndexByte(msg, '\n'); i >= 0 {
		msg = msg[:i]
	}
	r := []rune(msg)
	if len(r) > commitMessageFirstLineRunes {
		return string(r[:commitMessageFirstLineRunes]) + "…"
	}
	return msg
}

func writeIssueBlock(b *strings.Builder, it github.Issue) {
	stateLabel := "OPEN"
	if it.State == "closed" {
		stateLabel = "CLOSED"
		if it.ClosedAt != nil {
			stateLabel = fmt.Sprintf("CLOSED %s", it.ClosedAt.UTC().Format("2006-01-02"))
		}
	}
	fmt.Fprintf(b, "[%s #%d] %s\n", stateLabel, it.Number, it.Title)
	fmt.Fprintf(b, "- created: %s by %s\n", it.CreatedAt.UTC().Format("2006-01-02"), it.User.Login)
	fmt.Fprintf(b, "- updated: %s\n", it.UpdatedAt.UTC().Format("2006-01-02"))

	if labels := joinLabels(it.Labels); labels != "" {
		fmt.Fprintf(b, "- labels: %s\n", labels)
	}
	if assignees := joinAssignees(it.Assignees); assignees != "" {
		fmt.Fprintf(b, "- assignees: %s\n", assignees)
	} else {
		b.WriteString("- assignees: (none)\n")
	}
	fmt.Fprintf(b, "- comments: %d\n", it.Comments)

	if excerpt := bodyExcerpt(it.Body, weeklyBodyExcerptRunes); excerpt != "" {
		fmt.Fprintf(b, "- body: %s\n", excerpt)
	}
	b.WriteString("\n")
}

func joinLabels(labels []github.Label) string {
	if len(labels) == 0 {
		return ""
	}
	names := make([]string, 0, len(labels))
	for _, l := range labels {
		if l.Name != "" {
			names = append(names, l.Name)
		}
	}
	return strings.Join(names, ", ")
}

func joinAssignees(users []github.User) string {
	if len(users) == 0 {
		return ""
	}
	logins := make([]string, 0, len(users))
	for _, u := range users {
		if u.Login != "" {
			logins = append(logins, u.Login)
		}
	}
	return strings.Join(logins, ", ")
}

// bodyExcerpt는 issue body를 max 룬으로 자르고 개행을 한 줄로 평탄화한다.
// 매우 긴 본문이 토큰을 잡아먹지 않도록 첫 부분만 LLM에게 보여주는 발췌.
func bodyExcerpt(body string, maxRunes int) string {
	body = strings.TrimSpace(body)
	if body == "" {
		return ""
	}
	flat := strings.ReplaceAll(body, "\n", " ")
	flat = strings.ReplaceAll(flat, "\r", "")
	r := []rune(flat)
	if len(r) <= maxRunes {
		return flat
	}
	return string(r[:maxRunes]) + "…"
}
