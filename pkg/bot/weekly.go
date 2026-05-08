package bot

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"chatbot-alpha-1/pkg/github"
	"chatbot-alpha-1/pkg/llm"
	"chatbot-alpha-1/pkg/llm/render"
	"chatbot-alpha-1/pkg/llm/summarize"

	"github.com/bwmarrin/discordgo"
)

// commitWindow는 주간 정리 default 커밋 분석 기간 (지난 14일).
// 이슈는 별도로 open 전체를 수집하므로 시간 윈도우 미적용.
const commitWindow = 14 * 24 * time.Hour

// weeklyLLMTimeout은 LLM 호출 timeout. 이슈 수십 건 dump 기준.
const weeklyLLMTimeout = 90 * time.Second

// weeklyGitHubTimeout은 ListIssues / ListCommits 호출 timeout.
const weeklyGitHubTimeout = 30 * time.Second

// =====================================================================
// 주간 정리 - 레포 선택
// =====================================================================
//
// 흐름:
//   [주간 정리] 클릭 → mode_weekly
//     → sendWeeklyRepoButtons: git_server.go의 weeklyRepos 4개를 직접 버튼으로 노출
//     → 사용자 [{repo}] 클릭 → handleWeeklyRepoSelect → runWeeklyAnalyze
//     → 결과 메시지에 follow-up 5 + 닫기(closeable>0일 때) 첨부

// 버튼 customID 상수.
const (
	customIDWeeklyRepoPrefix      = "weekly_repo:" // 레포 클릭 — owner/name
	customIDWeeklyDirectiveBtn    = "weekly_directive"
	customIDWeeklyPeriodPromptBtn = "weekly_period_prompt" // [기간 변경] 클릭 — 14/30 sub-prompt 노출
	customIDWeeklyPeriod14        = "weekly_period_14"     // 14일 즉시 재분석
	customIDWeeklyPeriod30        = "weekly_period_30"     // 30일 즉시 재분석
	customIDWeeklyRetryBtn        = "weekly_retry"
	customIDWeeklyToMeetingBtn    = "weekly_to_meeting"
	customIDWeeklyCloseStartBtn   = "weekly_close_start"   // [닫아도 될 이슈 N건 닫기] — 확인 prompt 노출
	customIDWeeklyCloseConfirmBtn = "weekly_close_confirm" // 확인 후 실제 close API 호출
	customIDHomeBtn               = "mode_home"
)

// sendWeeklyRepoButtons는 [주간 정리] 클릭 시 weeklyRepos를 5개씩 ActionsRow로 분할하여
// 직접 버튼으로 노출한다 (그룹 navigation 없음).
func sendWeeklyRepoButtons(s *discordgo.Session, channelID string) {
	if len(weeklyRepos) == 0 {
		s.ChannelMessageSend(channelID, "분석 가능한 레포가 등록되어 있지 않습니다 (git_server.go의 weeklyRepos 확인).")
		return
	}
	rows := buildWeeklyRepoRows(weeklyRepos)
	header := fmt.Sprintf("주간 분석할 레포를 선택해주세요. (등록된 레포: %d개)", len(weeklyRepos))
	if _, err := s.ChannelMessageSendComplex(channelID, &discordgo.MessageSend{
		Content:    header,
		Components: rows,
	}); err != nil {
		log.Printf("[주간/repos] ERR send buttons: %v", err)
	}
}

// buildWeeklyRepoRows는 weeklyRepos를 5개씩 ActionsRow로 묶는다.
func buildWeeklyRepoRows(repos []WeeklyRepo) []discordgo.MessageComponent {
	var rows []discordgo.MessageComponent
	for i := 0; i < len(repos); i += 5 {
		end := i + 5
		if end > len(repos) {
			end = len(repos)
		}
		var btns []discordgo.MessageComponent
		for _, r := range repos[i:end] {
			label := r.Label
			if label == "" {
				label = r.Name
			}
			btns = append(btns, discordgo.Button{
				Label:    truncate(label, 80),
				Style:    discordgo.SecondaryButton,
				CustomID: customIDWeeklyRepoPrefix + r.Owner + "/" + r.Name,
			})
		}
		rows = append(rows, discordgo.ActionsRow{Components: btns})
	}
	return rows
}

// handleWeeklyRepoSelect는 레포 버튼 클릭 시 호출. ack 응답 후 runWeeklyAnalyze로 위임.
// 모든 레포 동일 입력: open 이슈 전체 + 지난 commitWindow(14일) 커밋.
func handleWeeklyRepoSelect(s *discordgo.Session, i *discordgo.InteractionCreate, fullName string) {
	log.Printf("[주간/select] channel=%s repo=%s by=%s",
		i.ChannelID, fullName, i.Member.User.Username)

	sess := lookupSession(i.ChannelID)
	if sess == nil {
		respondInteraction(s, i, "세션이 만료되었습니다. 다시 시작해주세요.")
		return
	}

	respondInteraction(s, i, fmt.Sprintf("`%s` open 이슈 + 지난 14일 커밋 수집 중...", fullName))
	now := time.Now()
	runWeeklyAnalyze(s, sess, fullName, now.Add(-commitWindow), now, "")
}

// runWeeklyAnalyze는 주간 분석의 핵심 로직. handleWeeklyRepoSelect 외에도
// follow-up 핸들러 ([다시 분석] / [기간 변경] / [추가 요청])들이 같은 함수를 다른
// 인자로 호출한다.
//
// 결과 메시지 끝에 follow-up 5개 버튼을 자동 첨부하고 sess에 컨텍스트를 박제한다.
func runWeeklyAnalyze(s *discordgo.Session, sess *Session, fullName string, since, until time.Time, directive string) {
	owner, name, ok := splitRepoFullName(fullName)
	if !ok {
		s.ChannelMessageSend(sess.ThreadID, fmt.Sprintf("잘못된 레포 식별자: `%s`", fullName))
		return
	}
	if githubClient == nil {
		s.ChannelMessageSend(sess.ThreadID, "GITHUB_TOKEN이 설정되어 있지 않아 이슈를 조회할 수 없습니다.")
		return
	}
	if llmClient == nil {
		s.ChannelMessageSend(sess.ThreadID, "LLM 클라이언트가 초기화되지 않았습니다.")
		return
	}

	// 1) GitHub 데이터 수집 — 입력은 모든 레포 동일:
	//    - 이슈: 현재 open 전체 (since 미적용)
	//    - 커밋: 지난 N일 (since~until 윈도우)
	ghCtx, ghCancel := context.WithTimeout(context.Background(), weeklyGitHubTimeout)
	defer ghCancel()
	issues, err := githubClient.ListIssues(ghCtx, owner, name, github.ListIssuesOptions{
		State: "open",
	})
	if err != nil {
		log.Printf("[주간/issues] ERR repo=%s err=%v", fullName, err)
		s.ChannelMessageSend(sess.ThreadID, fmt.Sprintf("이슈 조회 실패: %v", err))
		return
	}
	commits, err := githubClient.ListCommits(ghCtx, owner, name, github.ListCommitsOptions{
		Since: since,
		Until: until,
	})
	if err != nil {
		log.Printf("[주간/commits] ERR repo=%s err=%v", fullName, err)
		s.ChannelMessageSend(sess.ThreadID, fmt.Sprintf("커밋 조회 실패: %v", err))
		return
	}
	log.Printf("[주간/data] repo=%s commit_since=%s issues_open=%d commits=%d directive_runes=%d",
		fullName, since.UTC().Format(time.RFC3339), len(issues), len(commits), len([]rune(directive)))

	if len(issues) == 0 && len(commits) == 0 {
		s.ChannelMessageSendComplex(sess.ThreadID, &discordgo.MessageSend{
			Content: fmt.Sprintf("`%s` 레포에서 해당 기간 활동(이슈 + 커밋)이 없습니다.", fullName),
			Components: []discordgo.MessageComponent{
				discordgo.ActionsRow{Components: []discordgo.MessageComponent{homeButton()}},
			},
		})
		// 빈 결과여도 세션 유지 — 사용자가 다른 메뉴를 바로 누를 수 있게.
		sess.State = StateSelectMode
		return
	}

	// 2) LLM 분석
	s.ChannelMessageSend(sess.ThreadID, fmt.Sprintf("이슈 %d건 + 커밋 %d건을 분석하는 중...", len(issues), len(commits)))
	llmCtx, llmCancel := context.WithTimeout(context.Background(), weeklyLLMTimeout)
	defer llmCancel()

	start := time.Now()
	resp, err := summarize.Weekly(llmCtx, llmClient, fullName, since, until, issues, commits, directive)
	dur := time.Since(start)
	if err != nil {
		log.Printf("[주간/llm] ERR repo=%s elapsed=%s err=%v", fullName, dur, err)
		s.ChannelMessageSend(sess.ThreadID, fmt.Sprintf("LLM 분석 실패: %v", err))
		return
	}
	log.Printf("[주간/llm] ok repo=%s elapsed=%s markdown_runes=%d closeable=%d",
		fullName, dur, len([]rune(resp.Markdown)), len(resp.Closeable))

	// 3) 렌더링 + 분할 전송. 마지막 chunk에 follow-up 버튼 첨부.
	rendered := render.RenderWeekly(render.WeeklyRenderInput{
		RepoFullName: fullName,
		Since:        since,
		Until:        until,
		IssueCount:   len(issues),
		CommitCount:  len(commits),
		Response:     resp,
	})
	if _, err := sendLongMessageWithComponents(s, sess.ThreadID, rendered, weeklyFollowupComponents(len(resp.Closeable))); err != nil {
		log.Printf("[주간/send] ERR repo=%s: %v", fullName, err)
	}

	// 4) 세션 컨텍스트 박제 + 처음 메뉴 사용 가능 상태로 돌려놓음
	sess.LastWeeklyRepo = fullName
	sess.LastWeeklySince = since
	sess.LastWeeklyUntil = until
	sess.LastWeeklyDirective = directive
	sess.LastWeeklyResponse = resp
	sess.LastWeeklyCloseable = resp.Closeable
	sess.State = StateSelectMode
}

// weeklyFollowupComponents는 분석 결과 메시지 하단에 첨부하는 follow-up 버튼들을 만든다.
// closeableCount > 0 일 때만 두 번째 row에 [닫아도 될 이슈 N건 닫기] 버튼을 추가한다.
func weeklyFollowupComponents(closeableCount int) []discordgo.MessageComponent {
	rows := []discordgo.MessageComponent{
		discordgo.ActionsRow{Components: []discordgo.MessageComponent{
			discordgo.Button{Label: "추가 요청", Style: discordgo.PrimaryButton, CustomID: customIDWeeklyDirectiveBtn},
			discordgo.Button{Label: "기간 변경", Style: discordgo.SecondaryButton, CustomID: customIDWeeklyPeriodPromptBtn},
			discordgo.Button{Label: "다시 분석", Style: discordgo.SecondaryButton, CustomID: customIDWeeklyRetryBtn},
			discordgo.Button{Label: "미팅 시작", Style: discordgo.SuccessButton, CustomID: customIDWeeklyToMeetingBtn},
			discordgo.Button{Label: "처음 메뉴", Style: discordgo.SecondaryButton, CustomID: customIDHomeBtn},
		}},
	}
	if closeableCount > 0 {
		rows = append(rows, discordgo.ActionsRow{Components: []discordgo.MessageComponent{
			discordgo.Button{
				Label:    fmt.Sprintf("닫아도 될 이슈 %d건 닫기", closeableCount),
				Style:    discordgo.DangerButton,
				CustomID: customIDWeeklyCloseStartBtn,
			},
		}})
	}
	return rows
}

// homeButton은 follow-up이 부적절한 곳에서 단독 노출하는 [처음 메뉴] 버튼.
func homeButton() discordgo.MessageComponent {
	return discordgo.Button{Label: "처음 메뉴", Style: discordgo.SecondaryButton, CustomID: customIDHomeBtn}
}

// =====================================================================
// follow-up 핸들러
// =====================================================================

// handleWeeklyDirective는 [추가 요청] 클릭 시 directive 입력 대기 상태로 전환한다.
func handleWeeklyDirective(s *discordgo.Session, i *discordgo.InteractionCreate, sess *Session) {
	if sess.LastWeeklyRepo == "" {
		respondInteraction(s, i, "이전 주간 분석 정보가 없습니다. 다시 [주간 정리]부터 시작해주세요.")
		return
	}
	sess.State = StateWeeklyAwaitDirective
	respondInteraction(s, i,
		"원하는 분석 방향을 한 메시지로 적어주세요.\n"+
			"예) `프론트엔드 라벨 이슈만 / 라벨 정합성 더 깊게 / 닫아도 될 이슈 후보 더 보수적으로`")
}

// handleWeeklyAwaitDirectiveMessage는 사용자가 directive 텍스트를 보냈을 때 호출된다.
func handleWeeklyAwaitDirectiveMessage(s *discordgo.Session, m *discordgo.MessageCreate, sess *Session) {
	content := strings.TrimSpace(m.Content)
	sess.UpdatedAt = time.Now()

	if content == "" {
		s.ChannelMessageSend(m.ChannelID, "지시가 비어 있습니다. 다시 입력해주세요.")
		return
	}
	if content == "취소" {
		sess.State = StateSelectMode
		s.ChannelMessageSendComplex(m.ChannelID, &discordgo.MessageSend{
			Content:    "추가 요청을 취소했습니다.",
			Components: []discordgo.MessageComponent{discordgo.ActionsRow{Components: []discordgo.MessageComponent{homeButton()}}},
		})
		return
	}
	if sess.LastWeeklyRepo == "" {
		s.ChannelMessageSend(m.ChannelID, "이전 주간 분석 정보가 없습니다. 다시 [주간 정리]부터 시작해주세요.")
		sess.State = StateSelectMode
		return
	}

	log.Printf("[주간/directive] 캡처 thread=%s by=%s runes=%d",
		sess.ThreadID, m.Author.Username, len([]rune(content)))
	s.ChannelMessageSend(m.ChannelID, fmt.Sprintf("지시 반영하여 다시 분석합니다.\n> %s", truncate(content, 200)))
	now := time.Now()
	runWeeklyAnalyze(s, sess, sess.LastWeeklyRepo, now.Add(-commitWindow), now, content)
}

// handleWeeklyPeriod는 [14일] / [30일] sub-button 클릭 시 즉시 재분석한다.
func handleWeeklyPeriod(s *discordgo.Session, i *discordgo.InteractionCreate, sess *Session, days int) {
	if sess.LastWeeklyRepo == "" {
		respondInteraction(s, i, "이전 주간 분석 정보가 없습니다. 다시 [주간 정리]부터 시작해주세요.")
		return
	}
	respondInteraction(s, i, fmt.Sprintf("`%s` 레포를 지난 %d일로 다시 분석합니다.", sess.LastWeeklyRepo, days))
	now := time.Now()
	since := now.Add(-time.Duration(days) * 24 * time.Hour)
	runWeeklyAnalyze(s, sess, sess.LastWeeklyRepo, since, now, sess.LastWeeklyDirective)
}

// handleWeeklyPeriodPrompt는 [기간 변경] 클릭 시 14/30 sub-prompt를 띄운다.
func handleWeeklyPeriodPrompt(s *discordgo.Session, i *discordgo.InteractionCreate, sess *Session) {
	if sess.LastWeeklyRepo == "" {
		respondInteraction(s, i, "이전 주간 분석 정보가 없습니다. 다시 [주간 정리]부터 시작해주세요.")
		return
	}
	respondInteractionWithRow(s, i, "어느 기간으로 다시 분석할까요?",
		discordgo.Button{Label: "14일", Style: discordgo.SecondaryButton, CustomID: customIDWeeklyPeriod14},
		discordgo.Button{Label: "30일", Style: discordgo.SecondaryButton, CustomID: customIDWeeklyPeriod30},
		homeButton(),
	)
}

// handleWeeklyRetry는 [다시 분석] 클릭 시 같은 repo + 기간 + directive로 재호출한다.
func handleWeeklyRetry(s *discordgo.Session, i *discordgo.InteractionCreate, sess *Session) {
	if sess.LastWeeklyRepo == "" {
		respondInteraction(s, i, "이전 주간 분석 정보가 없습니다. 다시 [주간 정리]부터 시작해주세요.")
		return
	}
	respondInteraction(s, i, fmt.Sprintf("`%s` 레포를 같은 조건으로 다시 분석합니다.", sess.LastWeeklyRepo))
	runWeeklyAnalyze(s, sess, sess.LastWeeklyRepo, sess.LastWeeklySince, sess.LastWeeklyUntil, sess.LastWeeklyDirective)
}

// handleWeeklyToMeeting는 [미팅 시작] 클릭 시 분석 결과 마크다운을 미팅 첫 노트로 주입한 뒤 미팅 모드 진입.
func handleWeeklyToMeeting(s *discordgo.Session, i *discordgo.InteractionCreate, sess *Session) {
	if sess.LastWeeklyResponse == nil || strings.TrimSpace(sess.LastWeeklyResponse.Markdown) == "" {
		respondInteraction(s, i, "주입할 주간 분석 결과가 없습니다.")
		return
	}
	header := fmt.Sprintf("[주간 분석 결과 — %s, %s ~ %s]",
		sess.LastWeeklyRepo,
		sess.LastWeeklySince.Format("2006-01-02"),
		sess.LastWeeklyUntil.Format("2006-01-02"))
	body := header + "\n" + strings.TrimSpace(sess.LastWeeklyResponse.Markdown)

	// 미팅 모드 초기화 후 분석 본문을 첫 노트로 추가 (Author는 가상 "weekly_report")
	sess.Mode = ModeMeeting
	sess.State = StateMeeting
	sess.Notes = nil
	sess.Speakers = nil
	sess.NotesAtLastSticky = 0
	sess.StickyMessageID = ""
	sess.Directive = ""

	sess.AddNote("weekly_report", body)

	respondInteraction(s, i, "분석 결과를 미팅 첫 노트로 주입했습니다. 메시지를 자유롭게 입력하세요.\n\"미팅 종료\"로 마무리.")
	sendSticky(s, sess)
}

// handleWeeklyCloseStart는 [닫아도 될 이슈 N건 닫기] 클릭 시 확인 prompt를 노출한다.
func handleWeeklyCloseStart(s *discordgo.Session, i *discordgo.InteractionCreate, sess *Session) {
	if len(sess.LastWeeklyCloseable) == 0 {
		respondInteraction(s, i, "닫을 후보 이슈가 없습니다.")
		return
	}
	if sess.LastWeeklyRepo == "" {
		respondInteraction(s, i, "이전 주간 분석 정보가 없습니다.")
		return
	}

	var b strings.Builder
	fmt.Fprintf(&b, "다음 **%d건**을 GitHub에서 닫습니다 (state=closed, reason=completed):\n\n", len(sess.LastWeeklyCloseable))
	for _, c := range sess.LastWeeklyCloseable {
		fmt.Fprintf(&b, "- #%d %s\n  사유: %s\n", c.Number, c.Title, c.Reason)
	}
	b.WriteString("\n진행할까요?")

	respondInteractionWithRow(s, i, b.String(),
		discordgo.Button{Label: "확인", Style: discordgo.DangerButton, CustomID: customIDWeeklyCloseConfirmBtn},
		discordgo.Button{Label: "취소", Style: discordgo.SecondaryButton, CustomID: customIDHomeBtn},
	)
}

// handleWeeklyCloseConfirm은 [확인] 클릭 시 실제 GitHub close API를 호출한다.
func handleWeeklyCloseConfirm(s *discordgo.Session, i *discordgo.InteractionCreate, sess *Session) {
	if len(sess.LastWeeklyCloseable) == 0 {
		respondInteraction(s, i, "닫을 후보 이슈가 없습니다.")
		return
	}
	if githubClient == nil {
		respondInteraction(s, i, "GITHUB_TOKEN이 설정되어 있지 않아 닫기를 진행할 수 없습니다.")
		return
	}
	owner, name, ok := splitRepoFullName(sess.LastWeeklyRepo)
	if !ok {
		respondInteraction(s, i, fmt.Sprintf("잘못된 레포 식별자: `%s`", sess.LastWeeklyRepo))
		return
	}

	respondInteraction(s, i, fmt.Sprintf("`%s` 이슈 %d건을 닫는 중...", sess.LastWeeklyRepo, len(sess.LastWeeklyCloseable)))

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	type closeResult struct {
		issue llm.ClosableIssue
		err   error
	}
	var results []closeResult
	for _, c := range sess.LastWeeklyCloseable {
		err := githubClient.CloseIssue(ctx, owner, name, c.Number)
		results = append(results, closeResult{issue: c, err: err})
		if err != nil {
			log.Printf("[주간/close] ERR repo=%s #%d: %v", sess.LastWeeklyRepo, c.Number, err)
		} else {
			log.Printf("[주간/close] ok repo=%s #%d", sess.LastWeeklyRepo, c.Number)
		}
	}

	successCount := 0
	for _, r := range results {
		if r.err == nil {
			successCount++
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "닫기 결과: 성공 **%d**건 / 실패 **%d**건\n\n", successCount, len(results)-successCount)
	for _, r := range results {
		if r.err == nil {
			fmt.Fprintf(&b, "✓ #%d %s\n", r.issue.Number, r.issue.Title)
		} else {
			fmt.Fprintf(&b, "✗ #%d %s — %v\n", r.issue.Number, r.issue.Title, r.err)
		}
	}

	if _, err := s.ChannelMessageSendComplex(sess.ThreadID, &discordgo.MessageSend{
		Content: b.String(),
		Components: []discordgo.MessageComponent{
			discordgo.ActionsRow{Components: []discordgo.MessageComponent{homeButton()}},
		},
	}); err != nil {
		log.Printf("[주간/close] ERR send result: %v", err)
	}

	// 같은 후보로 두 번 누르지 못하게 비움.
	sess.LastWeeklyCloseable = nil
}

// handleHome는 [처음 메뉴] 클릭 시 세션을 SelectMode로 reset하고 초기 메뉴를 다시 노출한다.
func handleHome(s *discordgo.Session, i *discordgo.InteractionCreate, sess *Session) {
	sess.Mode = ModeNormal
	sess.State = StateSelectMode
	sess.Notes = nil
	sess.Speakers = nil
	sess.NotesAtLastSticky = 0
	sess.StickyMessageID = ""
	sess.Directive = ""
	respondInteractionWithRow(s, i, "무엇을 도와드릴까요?",
		discordgo.Button{Label: "미팅", Style: discordgo.PrimaryButton, CustomID: "mode_meeting"},
		discordgo.Button{Label: "주간 정리", Style: discordgo.PrimaryButton, CustomID: "mode_weekly"},
		discordgo.Button{Label: "에이전트", Style: discordgo.SuccessButton, CustomID: customIDAgentBtn},
		discordgo.Button{Label: "상태 조회", Style: discordgo.SecondaryButton, CustomID: "mode_status"},
	)
}

// =====================================================================
// 헬퍼
// =====================================================================

// respondInteractionWithRow는 한 줄에 임의 버튼들을 첨부해 ack한다 (ActionsRow 1개).
func respondInteractionWithRow(s *discordgo.Session, i *discordgo.InteractionCreate, content string, btns ...discordgo.MessageComponent) {
	s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Content: content,
			Components: []discordgo.MessageComponent{
				discordgo.ActionsRow{Components: btns},
			},
		},
	})
}

// splitRepoFullName은 "owner/name" → (owner, name, true). slash 없으면 ok=false.
func splitRepoFullName(full string) (owner, name string, ok bool) {
	idx := strings.IndexByte(full, '/')
	if idx <= 0 || idx == len(full)-1 {
		return "", "", false
	}
	return full[:idx], full[idx+1:], true
}

// isWeeklyRepoCustomID는 custom_id가 weekly_repo:* 패턴인지 검사한다.
func isWeeklyRepoCustomID(id string) bool {
	return strings.HasPrefix(id, customIDWeeklyRepoPrefix)
}

// extractWeeklyRepoFullName은 weekly_repo:owner/name 에서 "owner/name"을 추출한다.
func extractWeeklyRepoFullName(id string) string {
	return strings.TrimPrefix(id, customIDWeeklyRepoPrefix)
}
