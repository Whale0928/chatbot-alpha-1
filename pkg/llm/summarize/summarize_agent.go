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
// userRequest는 한국어 자유 텍스트. repos는 봇 startup 시점에 결정된 등록 레포의
// open 이슈 + 14일 커밋 dump. 시스템 프롬프트가 의도 파악과 환각 방지를 가이드한다.
func Agent(
	ctx context.Context,
	c *llm.Client,
	userRequest string,
	repos []AgentRepoContext,
	now time.Time,
) (*llm.AgentResponse, error) {
	userMsg := buildAgentUserMessage(userRequest, repos, now)
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

// buildAgentUserMessage는 사용자 지시 + 멀티 레포 dump를 LLM user 메시지로 직조한다.
//
// 형식:
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
func buildAgentUserMessage(userRequest string, repos []AgentRepoContext, now time.Time) string {
	var b strings.Builder
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
