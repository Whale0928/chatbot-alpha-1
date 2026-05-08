package bot

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"chatbot-alpha-1/pkg/github"
	"chatbot-alpha-1/pkg/llm/summarize"

	"github.com/bwmarrin/discordgo"
)

// =====================================================================
// 에이전트 모드 — 자유 자연어 지시
// =====================================================================
//
// 흐름:
//   [에이전트] 클릭 → handleAgent → StateAgentAwaitInput
//     → 사용자 자유 텍스트 → handleAgentMessage
//       → 4 레포 병렬 fetch (open 이슈 + 14일 커밋)
//       → summarize.Agent 호출 → 결과 전송 + [처음 메뉴]
//
// 데이터 dump 크기 부담은 토큰 비용으로 부메랑이지만, 의도 분류 1단계 없이도
// LLM이 user request에서 레포/필터를 의미 매칭으로 해결한다.

const (
	customIDAgentBtn      = "mode_agent"
	agentGitHubTimeout    = 30 * time.Second
	agentLLMTimeout       = 90 * time.Second
	agentCommitWindowDays = 14
)

// handleAgent는 [에이전트] 버튼 클릭 시 자유 지시 입력 대기 상태로 전환한다.
func handleAgent(s *discordgo.Session, i *discordgo.InteractionCreate, sess *Session) {
	if githubClient == nil {
		respondInteraction(s, i, "GITHUB_TOKEN이 설정되어 있지 않아 에이전트를 시작할 수 없습니다.")
		return
	}
	if llmClient == nil {
		respondInteraction(s, i, "LLM 클라이언트가 초기화되지 않았습니다.")
		return
	}
	sess.State = StateAgentAwaitInput
	respondInteraction(s, i,
		"무엇을 도와드릴까요? 자유롭게 지시해주세요.\n"+
			"예) `워크스페이스에서 인프라 관련 열려있는 이슈들 가져와`\n"+
			"예) `BE에서 지난 14일 동안 가장 큰 변경 요약해줘`\n"+
			"예) `취소` 입력 시 에이전트 종료")
}

// handleAgentMessage는 사용자 자유 텍스트를 받아 4 레포 fetch + LLM 호출 → 답변.
func handleAgentMessage(s *discordgo.Session, m *discordgo.MessageCreate, sess *Session) {
	content := strings.TrimSpace(m.Content)
	sess.UpdatedAt = time.Now()

	if content == "" {
		s.ChannelMessageSend(m.ChannelID, "지시가 비어 있습니다. 다시 입력해주세요.")
		return
	}
	if content == "취소" {
		sess.State = StateSelectMode
		s.ChannelMessageSendComplex(m.ChannelID, &discordgo.MessageSend{
			Content:    "에이전트를 종료했습니다.",
			Components: []discordgo.MessageComponent{discordgo.ActionsRow{Components: []discordgo.MessageComponent{homeButton()}}},
		})
		return
	}

	log.Printf("[agent/request] thread=%s by=%s runes=%d preview=%q",
		sess.ThreadID, m.Author.Username, len([]rune(content)), truncate(content, 80))

	s.ChannelMessageSend(m.ChannelID, "데이터 수집 중...")

	ghCtx, ghCancel := context.WithTimeout(context.Background(), agentGitHubTimeout)
	defer ghCancel()
	repos, err := fetchAgentContext(ghCtx)
	if err != nil {
		log.Printf("[agent/fetch] ERR: %v", err)
		s.ChannelMessageSend(m.ChannelID, fmt.Sprintf("데이터 수집 실패: %v", err))
		sess.State = StateSelectMode
		return
	}
	totalIssues, totalCommits := 0, 0
	for _, r := range repos {
		totalIssues += len(r.Issues)
		totalCommits += len(r.Commits)
	}
	log.Printf("[agent/fetch] ok repos=%d issues=%d commits=%d", len(repos), totalIssues, totalCommits)

	s.ChannelMessageSend(m.ChannelID, fmt.Sprintf("이슈 %d건 + 커밋 %d건 (4 레포)을 분석하는 중...", totalIssues, totalCommits))

	llmCtx, llmCancel := context.WithTimeout(context.Background(), agentLLMTimeout)
	defer llmCancel()
	start := time.Now()
	resp, err := summarize.Agent(llmCtx, llmClient, content, sess.LastBotSummary, repos, time.Now())
	dur := time.Since(start)
	if err != nil {
		log.Printf("[agent/llm] ERR elapsed=%s err=%v", dur, err)
		s.ChannelMessageSend(m.ChannelID, fmt.Sprintf("LLM 분석 실패: %v", err))
		sess.State = StateSelectMode
		return
	}
	log.Printf("[agent/llm] ok elapsed=%s markdown_runes=%d", dur, len([]rune(resp.Markdown)))

	rendered := strings.TrimSpace(resp.Markdown)
	if rendered == "" {
		rendered = "_(답변 본문이 비어있습니다)_"
	}
	if err := sendLongMessage(s, m.ChannelID, rendered); err != nil {
		log.Printf("[agent/send] ERR: %v", err)
	}
	if _, err := s.ChannelMessageSendComplex(m.ChannelID, &discordgo.MessageSend{
		Content: "이어서 다른 작업을 시작하려면 [처음 메뉴]를 눌러주세요.",
		Components: []discordgo.MessageComponent{
			discordgo.ActionsRow{Components: []discordgo.MessageComponent{homeButton()}},
		},
	}); err != nil {
		log.Printf("[agent/send] ERR home button: %v", err)
	}
	sess.State = StateSelectMode
}

// fetchAgentContext는 등록된 weeklyRepos 4개를 병렬로 fetch해 AgentRepoContext slice로 반환한다.
// 각 레포: open 이슈 전체 + 지난 agentCommitWindowDays(14)일 커밋.
func fetchAgentContext(ctx context.Context) ([]summarize.AgentRepoContext, error) {
	now := time.Now()
	since := now.Add(-time.Duration(agentCommitWindowDays) * 24 * time.Hour)

	type result struct {
		idx int
		ctx summarize.AgentRepoContext
		err error
	}
	results := make([]result, len(weeklyRepos))
	var wg sync.WaitGroup
	for i, r := range weeklyRepos {
		wg.Add(1)
		go func(idx int, repo WeeklyRepo) {
			defer wg.Done()
			res := result{idx: idx, ctx: summarize.AgentRepoContext{
				FullName: repo.Owner + "/" + repo.Name,
				Label:    repo.Label,
			}}
			issues, err := githubClient.ListIssues(ctx, repo.Owner, repo.Name, github.ListIssuesOptions{
				State: "open",
			})
			if err != nil {
				res.err = fmt.Errorf("ListIssues %s: %w", repo.Name, err)
				results[idx] = res
				return
			}
			res.ctx.Issues = issues
			commits, err := githubClient.ListCommits(ctx, repo.Owner, repo.Name, github.ListCommitsOptions{
				Since: since,
				Until: now,
			})
			if err != nil {
				res.err = fmt.Errorf("ListCommits %s: %w", repo.Name, err)
				results[idx] = res
				return
			}
			res.ctx.Commits = commits
			results[idx] = res
		}(i, r)
	}
	wg.Wait()

	out := make([]summarize.AgentRepoContext, 0, len(results))
	for _, r := range results {
		if r.err != nil {
			return nil, r.err
		}
		out = append(out, r.ctx)
	}
	return out, nil
}
