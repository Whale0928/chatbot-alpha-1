package bot

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"chatbot-alpha-1/pkg/db"
	"chatbot-alpha-1/pkg/github"
	"chatbot-alpha-1/pkg/llm/summarize"

	"github.com/bwmarrin/discordgo"
)

// =====================================================================
// 에이전트 모드 — 자유 자연어 지시
// =====================================================================
//
// 흐름 (D1 super-session 통합):
//   sticky [AI에게 질문] 클릭 → discord.go의 customIDSubActionAgent case가 직접
//     PendingAgentUserID 박제 + StateAgentAwaitInput 전환
//   → 본인 발화 → handleAgentMessage
//     → 등록 레포 병렬 fetch (open 이슈 + 14일 커밋)
//     → summarize.Agent 호출 → 결과 전송 + sticky 재발사 (A-8)
//
// /agent instruction:... slash 명령어는 super-session 시작 후 즉시 runAgentInstruction 호출.
//
// 데이터 dump 크기 부담은 토큰 비용으로 부메랑이지만, 의도 분류 1단계 없이도
// LLM이 user request에서 레포/필터를 의미 매칭으로 해결한다.

const (
	agentGitHubTimeout    = 30 * time.Second
	agentLLMTimeout       = 90 * time.Second
	agentCommitWindowDays = 14
)

// customIDAgentBtn ("mode_agent") 폐기 — D1 정책 (UX 재설계 2026-05).
// home menu의 [에이전트] button이 사라지고 super-session sticky의 [AI에게 질문](customIDSubActionAgent)
// 로 통일. 그 button은 discord.go에서 직접 PendingAgentUserID 박제 + StateAgentAwaitInput 전환을
// 처리하므로 별도 handleAgent 함수도 폐기.

// handleAgentMessage는 사용자 자유 텍스트를 받아 검증 + cancel 처리 후 runAgentInstruction에 위임한다.
// 슬래시 /agent instruction:... 직접 실행 흐름과 공통 본체를 공유하기 위해 분리되어 있다.
func handleAgentMessage(s *discordgo.Session, m *discordgo.MessageCreate, sess *Session) {
	content := strings.TrimSpace(m.Content)
	sess.UpdatedAt = time.Now()

	if content == "" {
		s.ChannelMessageSend(m.ChannelID, "지시가 비어 있습니다. 다시 입력해주세요.")
		return
	}
	// D4 button-only — "취소" 텍스트 escape 폐기. agent 입력 대기 상태에서 빠져나오려면
	// sticky의 다른 button을 누르면 됨 (interactionCreate가 PendingAgentUserID clear + State 복귀).
	// 텍스트 입력은 모두 LLM 지시로 처리 (super-session에서는 본인 발화만).
	runAgentInstruction(s, sess, content, m.Author.Username)
}

// runAgentInstruction은 등록 레포 전체 fetch + LLM 호출 + 응답을 수행한다.
// 진입점:
//   - handleAgentMessage (사용자 텍스트 입력)
//   - handleSlashCommand /agent instruction:... (슬래시 즉시 실행)
//
// content는 사전에 trim/검증된 비어있지 않은 지시문이어야 한다.
// authorName은 로그용 (없으면 "system"). 함수 종료 시 sess.State는 항상 StateSelectMode로 복원된다.
func runAgentInstruction(s *discordgo.Session, sess *Session, content, authorName string) {
	if authorName == "" {
		authorName = "system"
	}
	sess.UpdatedAt = time.Now()
	log.Printf("[agent/request] thread=%s by=%s runes=%d preview=%q",
		sess.ThreadID, authorName, len([]rune(content)), truncate(content, 80))

	// === Phase 3 chunk 3B-2c — super-session in-thread 통합 ===
	// weekly와 동일 패턴 (runWeeklyAnalyze 참고). ModeMeeting일 때만 SubAction lifecycle.
	// Context 분리: begin 5s / end 5s / append 5s — runAgentInstruction은 GH fetch + LLM(120s+)이라
	// 단일 ctx로는 항상 cancelled 상태에서 종료.
	var (
		sa            *SubActionContext
		renderedFinal string
	)
	if sess.Mode == ModeMeeting {
		beginCtx, beginCancel := context.WithTimeout(context.Background(), 5*time.Second)
		sa = BeginSubAction(beginCtx, sess, db.SegmentAgent)
		beginCancel()
		defer func() {
			endCtx, endCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer endCancel()
			sa.EndWithArtifact(endCtx, map[string]any{
				"directive_runes": len([]rune(content)),
				"author":          authorName,
				"rendered_runes":  len([]rune(renderedFinal)),
			})
		}()
		// A-8 (D3): sub-action 결과 후 sticky 즉시 재발사 — 다음 button 즉시 클릭 가능.
		// 정상/실패 모든 종료 경로에서 발사. LIFO 순서로 EndWithArtifact 다음에 실행.
		defer sendSticky(s, sess)
	}

	// === Phase 3 chunk 4 — progress 바 (super-session 전용) ===
	var progress *Progress
	if sess.Mode == ModeMeeting {
		progressCtx, progressCancel := context.WithCancel(context.Background())
		defer progressCancel()
		progress = StartProgress(progressCtx, s, sess.ThreadID, "에이전트 실행", 4)
		defer progress.Finish()
		progress.SetStage(1, "GitHub 데이터 수집")
	}

	s.ChannelMessageSend(sess.ThreadID, "데이터 수집 중...")

	ghCtx, ghCancel := context.WithTimeout(context.Background(), agentGitHubTimeout)
	defer ghCancel()
	repos, err := fetchAgentContext(ghCtx)
	if err != nil {
		log.Printf("[agent/fetch] ERR: %v", err)
		s.ChannelMessageSend(sess.ThreadID, fmt.Sprintf("데이터 수집 실패: %v", err))
		// 미팅 모드(super-session)에서 Agent를 호출한 경우 StateMeeting 유지 — 후속 발화가 corpus에
		// 계속 누적되어야 함. legacy(ModeNormal) 흐름은 SelectMode로 복귀.
		if sess.Mode == ModeMeeting {
			sess.State = StateMeeting
		} else {
			sess.State = StateSelectMode
		}
		return
	}
	totalIssues, totalCommits := 0, 0
	for _, r := range repos {
		totalIssues += len(r.Issues)
		totalCommits += len(r.Commits)
	}
	log.Printf("[agent/fetch] ok repos=%d issues=%d commits=%d", len(repos), totalIssues, totalCommits)

	if progress != nil {
		progress.SetStage(2, "LLM 호출 및 응답 대기")
	}
	s.ChannelMessageSend(sess.ThreadID, fmt.Sprintf("이슈 %d건 + 커밋 %d건 (%d 레포)을 분석하는 중...", totalIssues, totalCommits, len(repos)))

	llmCtx, llmCancel := context.WithTimeout(context.Background(), agentLLMTimeout)
	defer llmCancel()
	start := time.Now()
	resp, err := summarize.Agent(llmCtx, llmClient, content, sess.LastBotSummary, repos, time.Now())
	dur := time.Since(start)
	if err != nil {
		log.Printf("[agent/llm] ERR elapsed=%s err=%v", dur, err)
		s.ChannelMessageSend(sess.ThreadID, fmt.Sprintf("LLM 분석 실패: %v", err))
		// 미팅 모드(super-session)에서 Agent를 호출한 경우 StateMeeting 유지 — 후속 발화가 corpus에
		// 계속 누적되어야 함. legacy(ModeNormal) 흐름은 SelectMode로 복귀.
		if sess.Mode == ModeMeeting {
			sess.State = StateMeeting
		} else {
			sess.State = StateSelectMode
		}
		return
	}
	log.Printf("[agent/llm] ok elapsed=%s markdown_runes=%d", dur, len([]rune(resp.Markdown)))

	if progress != nil {
		progress.SetStage(3, "응답 처리")
	}
	rendered := strings.TrimSpace(resp.Markdown)
	if rendered == "" {
		rendered = "_(답변 본문이 비어있습니다)_"
	}
	if progress != nil {
		progress.SetStage(4, "메시지 전송")
	}
	sendErr := sendLongMessage(s, sess.ThreadID, rendered)
	if sendErr != nil {
		log.Printf("[agent/send] ERR (corpus 미누적): %v", sendErr)
	}

	// super-session corpus 누적 — 전송 성공 시에만 (사용자 미수신 분석이 정리본에 영향 X).
	renderedFinal = rendered
	if sa != nil && sendErr == nil {
		appendCtx, appendCancel := context.WithTimeout(context.Background(), 5*time.Second)
		sa.AppendResult(appendCtx, sess, "[agent]", db.SourceAgentOutput, rendered)
		appendCancel()
	}
	// D1 정책 (UX 재설계 2026-05): [처음 메뉴] button row 폐기. 후속 작업은 super-session sticky로.
	// 미팅 모드에서는 후속 발화가 corpus에 계속 누적되어야 하므로 StateMeeting 유지.
	if sess.Mode == ModeMeeting {
		sess.State = StateMeeting
	} else {
		sess.State = StateSelectMode
	}
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
