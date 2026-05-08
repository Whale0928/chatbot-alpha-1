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

// agentResponseSchema는 AgentResponse용 strict JSON schema.
var agentResponseSchema = llm.GenerateSchema[llm.AgentResponse]()

// AgentRepoContext는 단일 레포의 dump 입력. 봇이 미리 fetch해서 Agent에 넘긴다.
type AgentRepoContext struct {
	FullName string // "owner/name"
	Label    string // "워크스페이스" 같은 한국어 라벨 (사용자 키워드 매칭에 도움)
	Issues   []github.Issue
	Commits  []github.Commit
}

// Agent는 자유 자연어 지시 + 모든 등록 레포 dump를 LLM에 보내 마크다운 답변을 받는다.
//
// userRequest는 한국어 자유 텍스트. previousSummary는 같은 스레드에서 직전에 봇이
// 보냈던 마크다운(주간 분석 결과 또는 미팅 finalize 결과)이며, 비어 있으면 무시.
// repos는 봇 startup 시점에 결정된 등록 레포의 open 이슈 + 14일 커밋 dump.
// 시스템 프롬프트가 의도 파악과 환각 방지를 가이드한다.
func Agent(
	ctx context.Context,
	c *llm.Client,
	userRequest string,
	previousSummary string,
	repos []AgentRepoContext,
	now time.Time,
) (*llm.AgentResponse, error) {
	userMsg := buildAgentUserMessage(userRequest, previousSummary, repos, now)
	raw, err := callMeetingFormat(ctx, c, "agent", prompts.Agent, userMsg, "agent_response", agentResponseSchema)
	if err != nil {
		return nil, err
	}
	var out llm.AgentResponse
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, fmt.Errorf("llm: unmarshal agent: %w (raw=%q)", err, raw)
	}
	return &out, nil
}

// previousSummaryMaxRunes는 LLM에 동봉할 직전 봇 응답 길이의 상한.
// 넘으면 앞/뒤 절반씩만 남기고 중간을 생략 마커로 대체한다.
// 8000 runes는 한국어 markdown 기준 약 24KB로, 200K급 context window에서 안전한 여유분.
const previousSummaryMaxRunes = 8000

// truncatePreviousSummary는 너무 긴 이전 응답을 머리/꼬리 패턴으로 줄인다.
// 머리(요약/한 줄 헤더)와 꼬리(우선순위/블로커)가 정보가 가장 진하다는 가정.
func truncatePreviousSummary(s string) string {
	rs := []rune(s)
	if len(rs) <= previousSummaryMaxRunes {
		return s
	}
	half := previousSummaryMaxRunes / 2
	return string(rs[:half]) + "\n\n…[중간 생략, 원문이 길어 머리/꼬리만 인용]…\n\n" + string(rs[len(rs)-half:])
}

// buildAgentUserMessage는 사용자 지시 + 멀티 레포 dump를 LLM user 메시지로 직조한다.
// previousSummary가 비어있지 않으면 USER REQUEST 앞에 PREVIOUS BOT OUTPUT 블록을 끼운다.
//
// 형식:
//
//	PREVIOUS BOT OUTPUT (직전 봇 응답, 같은 스레드):
//	{markdown 또는 잘린 머리/꼬리}
//
//	USER REQUEST:
//	{한국어 지시}
//
//	REPOSITORY DUMP (snapshot YYYY-MM-DD):
//
//	=== bottle-note/workspace (워크스페이스) ===
//	Open issues (count=42):
//	  ...
//	Recent commits (count=0):
//	  (none)
//
//	=== bottle-note/bottle-note-api-server (API 서버) ===
//	...
func buildAgentUserMessage(userRequest, previousSummary string, repos []AgentRepoContext, now time.Time) string {
	var b strings.Builder
	if trimmed := strings.TrimSpace(previousSummary); trimmed != "" {
		b.WriteString("PREVIOUS BOT OUTPUT (직전 봇 응답, 같은 스레드):\n")
		b.WriteString(truncatePreviousSummary(trimmed))
		b.WriteString("\n\n")
	}
	b.WriteString("USER REQUEST:\n")
	b.WriteString(strings.TrimSpace(userRequest))
	b.WriteString("\n\n")
	fmt.Fprintf(&b, "REPOSITORY DUMP (snapshot %s):\n\n", now.UTC().Format("2006-01-02"))

	for _, rc := range repos {
		header := rc.FullName
		if rc.Label != "" {
			header = fmt.Sprintf("%s (%s)", rc.FullName, rc.Label)
		}
		fmt.Fprintf(&b, "=== %s ===\n", header)

		fmt.Fprintf(&b, "Open issues (count=%d):\n", len(rc.Issues))
		if len(rc.Issues) == 0 {
			b.WriteString("  (none)\n")
		} else {
			for _, it := range rc.Issues {
				writeIssueBlock(&b, it)
			}
		}

		fmt.Fprintf(&b, "Recent commits (count=%d):\n", len(rc.Commits))
		if len(rc.Commits) == 0 {
			b.WriteString("  (none)\n\n")
		} else {
			for _, c := range rc.Commits {
				writeCommitBlock(&b, c)
			}
			b.WriteString("\n")
		}
	}
	return b.String()
}
