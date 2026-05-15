package bot

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"chatbot-alpha-1/pkg/db"
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
//
// D1/D2 정책 폐기 customID:
//   - customIDWeeklyDirectiveBtn (weekly_directive)   — D2 weekly directive 흐름 제거
//   - customIDWeeklyPeriodPromptBtn (weekly_period_prompt) — D1 follow-up [기간 변경] 폐기
//   - customIDWeeklyRetryBtn (weekly_retry)            — D1 follow-up [다시 분석] 폐기
//   - customIDWeeklyToMeetingBtn (weekly_to_meeting)   — D1 follow-up [미팅 시작] 폐기
//   - customIDHomeBtn (mode_home)                      — D1 [처음 메뉴] 폐기 (super-session sticky가 항상 표시)
//
// 첫 분석 흐름의 prompt(weekly_period_select/confirm/modal)는 보존 — handleWeeklyScopeSelect 진입점.
const (
	customIDWeeklyRepoPrefix      = "weekly_repo:"             // 레포 클릭 — owner/name
	customIDWeeklyScopePrefix     = "weekly_scope:"            // scope 클릭 — issues/commits/both
	customIDWeeklyPeriodSelect    = "weekly_period_select"     // StringSelect — 일자 옵션 선택
	customIDWeeklyPeriodConfirm   = "weekly_period_confirm"    // [확인] — 선택된 일자로 분석 실행
	customIDWeeklyPeriodModal     = "weekly_period_modal"      // 직접 입력 modal submit custom_id
	customIDWeeklyPeriodModalDate = "weekly_period_modal_date" // modal text input field id
	customIDWeeklyCloseStartBtn   = "weekly_close_start"       // [닫아도 될 이슈 N건 닫기] — 확인 prompt 노출
	customIDWeeklyCloseConfirmBtn = "weekly_close_confirm"     // 확인 후 실제 close API 호출
)

// 드롭다운 옵션 value. weeklyPeriodValueCustom은 "직접 입력" — modal 발사로 분기.
// 그 외 일자는 strconv.Atoi로 파싱 가능한 숫자 문자열.
const weeklyPeriodValueCustom = "custom"

// weeklyMaxCustomDays는 [직접 입력]으로 지정 가능한 시작일의 최대 과거 일수.
// 30일 초과 시작일은 분석 토큰량/품질 양쪽에 부담이 커서 거부한다.
const weeklyMaxCustomDays = 30

// weeklyPeriodDefaultDays는 드롭다운 placeholder + [확인] 클릭 시 사용자가 옵션을 한 번도
// 안 골랐을 때의 기본 일자. 운영에서 가장 자주 보는 "지난 1주" 시나리오로 설정.
const weeklyPeriodDefaultDays = 7

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

// handleWeeklyRepoSelect는 레포 버튼 클릭 시 호출. 즉시 분석하지 않고 scope 선택 prompt를 띄운다.
// 사용자가 [이슈] / [커밋(2주)] / [전체] 중 하나를 눌러야 실제 fetch + LLM 호출이 진행된다.
//
// 선택된 fullName은 sess.PendingWeeklyRepo에 박제하고, scope 클릭 시 handleWeeklyScopeSelect가
// 이 값을 꺼내서 runWeeklyAnalyze에 전달한다.
func handleWeeklyRepoSelect(s *discordgo.Session, i *discordgo.InteractionCreate, fullName string) {
	log.Printf("[주간/select] channel=%s repo=%s by=%s",
		i.ChannelID, fullName, i.Member.User.Username)

	sess := lookupSession(i.ChannelID)
	if sess == nil {
		respondInteraction(s, i, "세션이 만료되었습니다. 다시 시작해주세요.")
		return
	}

	sess.PendingWeeklyRepo = fullName
	respondInteractionWithComponents(s, i,
		fmt.Sprintf("`%s` 어떤 범위로 분석할까요?\n\n"+
			"**이슈**: 현재 OPEN 이슈 전체 (시간 윈도우 무관)\n"+
			"**커밋**: 커밋만 (기간은 다음 단계에서 선택)\n"+
			"**전체**: 이슈 + 커밋 둘 다 (운영 진단 풀 리포트)", fullName),
		weeklyScopeComponents(),
	)
}

// handleWeeklyScopeSelect는 scope 버튼 클릭 시 호출.
// scope=Issues는 기간 선택 단계 없이 즉시 분석 (이슈는 시간 윈도우 무관).
// scope=Commits/Both는 기간을 사용자가 명시 선택할 수 있도록 [1주][2주][📅 캘린더] prompt를 띄운다.
func handleWeeklyScopeSelect(s *discordgo.Session, i *discordgo.InteractionCreate, scope llm.WeeklyScope) {
	sess := lookupSession(i.ChannelID)
	if sess == nil {
		respondInteraction(s, i, "세션이 만료되었습니다. 다시 시작해주세요.")
		return
	}
	if sess.PendingWeeklyRepo == "" {
		respondInteraction(s, i, "선택된 레포가 없습니다. [주간 정리]부터 다시 시작해주세요.")
		return
	}
	fullName := sess.PendingWeeklyRepo

	log.Printf("[주간/scope] channel=%s repo=%s scope=%s by=%s",
		i.ChannelID, fullName, scope, i.Member.User.Username)

	if scope == llm.WeeklyScopeIssues {
		// 이슈 전용은 기간 선택 없이 즉시 분석 — Pending 컨텍스트 정리.
		clearPendingWeekly(sess)
		respondInteraction(s, i, fmt.Sprintf("`%s` %s 분석 중...", fullName, scopeAckLabel(scope)))
		now := time.Now()
		runWeeklyAnalyze(s, sess, fullName, now.Add(-commitWindow), now, "", scope)
		return
	}

	// commits / both: 기간 선택 prompt 노출 (드롭다운 + [확인]). PendingWeeklyScope 박제, days는 0(default 7).
	sess.PendingWeeklyScope = scope
	sess.PendingPeriodDays = 0
	respondInteractionWithComponents(s, i,
		fmt.Sprintf("`%s` %s — 분석할 기간을 고르고 [확인]을 눌러주세요. (기본값: 지난 %d일)",
			fullName, scopeAckLabel(scope), weeklyPeriodDefaultDays),
		weeklyPeriodPromptComponents(0),
	)
}

// scopeAckLabel은 ack 메시지에 들어가는 짧은 라벨.
func scopeAckLabel(scope llm.WeeklyScope) string {
	switch scope {
	case llm.WeeklyScopeIssues:
		return "open 이슈 전체 수집"
	case llm.WeeklyScopeCommits:
		return "커밋 수집"
	default:
		return "open 이슈 + 커밋 수집"
	}
}

// weeklyScopeComponents는 레포 선택 직후 노출되는 [이슈] [커밋] [전체] 버튼 행.
// D1 정책: [처음 메뉴] button 폐기 — super-session sticky가 항상 노출됨.
func weeklyScopeComponents() []discordgo.MessageComponent {
	return []discordgo.MessageComponent{
		discordgo.ActionsRow{Components: []discordgo.MessageComponent{
			discordgo.Button{Label: "이슈", Style: discordgo.PrimaryButton, CustomID: customIDWeeklyScopePrefix + "issues"},
			discordgo.Button{Label: "커밋", Style: discordgo.PrimaryButton, CustomID: customIDWeeklyScopePrefix + "commits"},
			discordgo.Button{Label: "전체", Style: discordgo.SuccessButton, CustomID: customIDWeeklyScopePrefix + "both"},
		}},
	}
}

// isWeeklyScopeCustomID는 custom_id가 weekly_scope:* 패턴인지 검사한다.
func isWeeklyScopeCustomID(id string) bool {
	return strings.HasPrefix(id, customIDWeeklyScopePrefix)
}

// extractWeeklyScope는 weekly_scope:<token>에서 WeeklyScope를 추출한다.
// 알 수 없는 token이면 ok=false.
func extractWeeklyScope(id string) (llm.WeeklyScope, bool) {
	tok := strings.TrimPrefix(id, customIDWeeklyScopePrefix)
	return llm.ParseWeeklyScope(tok)
}

// runWeeklyAnalyze는 주간 분석의 핵심 로직. handleWeeklyScopeSelect 외에도
// follow-up 핸들러 ([다시 분석] / [기간 변경] / [추가 요청])들이 같은 함수를 다른
// 인자로 호출한다.
//
// scope에 따라 fetch가 분기된다 — Issues는 커밋 fetch를 생략, Commits는 이슈 fetch 생략, Both는 둘 다.
// 결과 메시지 끝에 follow-up 버튼을 자동 첨부하고 sess에 컨텍스트(scope 포함)를 박제한다.
func runWeeklyAnalyze(s *discordgo.Session, sess *Session, fullName string, since, until time.Time, directive string, scope llm.WeeklyScope) {
	// === Phase 3 chunk 3B-2b — super-session in-thread 통합 ===
	// 미팅 모드(super-session)에서 호출됐을 때만 SubAction lifecycle 적용:
	//   - segments row insert (BeginSubAction)
	//   - 결과를 NoteSource=WeeklyDump로 sess.Notes 누적 (AppendResult)
	//   - segment 종료 + artifact persist (defer EndWithArtifact)
	// legacy 흐름(메인 채널 [주간 정리])은 ModeNormal이라 후크 미발동 = 기존 동작 그대로 보존.
	//
	// Context 정책 (review C1 반영):
	//   - Begin: 5초 짧은 timeout (DB insert 1회만 보호)
	//   - Append/End: 함수 수명 독립의 새 context — runWeeklyAnalyze는 GitHub fetch(30s) + LLM(90s)
	//     이라 begin context를 그대로 쓰면 항상 cancelled 상태에서 종료 → DB persist 실패.
	var (
		sa            *SubActionContext
		issues        []github.Issue
		commits       []github.Commit
		renderedFinal string
	)
	if sess.Mode == ModeMeeting {
		beginCtx, beginCancel := context.WithTimeout(context.Background(), 5*time.Second)
		sa = BeginSubAction(beginCtx, sess, db.SegmentWeeklySummary)
		beginCancel()
		defer func() {
			// EndCtx는 runWeeklyAnalyze 수명과 독립 — 함수 끝난 후에도 짧게 살아있어야 DB end가 성공.
			endCtx, endCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer endCancel()
			sa.EndWithArtifact(endCtx, map[string]any{
				"repo":           fullName,
				"since":          since.Unix(),
				"until":          until.Unix(),
				"scope":          scope.String(),
				"issues_count":   len(issues),
				"commits_count":  len(commits),
				"rendered_runes": len([]rune(renderedFinal)),
			})
		}()
		// A-8 (D3): sub-action 결과 후 sticky 즉시 재발사 — 사용자가 다음 button을 화면 하단에서
		// 즉시 클릭 가능. 정상/실패 모든 종료 경로에서 발사 (corpus 살아있는데 sticky 위로 올라가서
		// 안 보이는 회귀 방어). LIFO 순서로 EndWithArtifact 다음에 실행.
		defer sendSticky(s, sess)
	}

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

	// === Phase 3 chunk 4 — progress 바 (super-session에서만 노출) ===
	// 단계: GitHub fetch → LLM 호출 → render → 메시지 전송 → corpus 누적
	// FinalizeSummarized와 동일 패턴 (1.5s 간격 edit, rate limit 안전).
	var progress *Progress
	if sess.Mode == ModeMeeting {
		progressCtx, progressCancel := context.WithCancel(context.Background())
		defer progressCancel()
		progress = StartProgress(progressCtx, s, sess.ThreadID, fmt.Sprintf("주간 분석 (%s)", fullName), 4)
		defer progress.Finish()
		progress.SetStage(1, "GitHub 데이터 수집")
	}

	// 1) GitHub 데이터 수집 — scope에 따라 분기. 비포함 소스는 nil/0건으로 LLM에 전달된다.
	ghCtx, ghCancel := context.WithTimeout(context.Background(), weeklyGitHubTimeout)
	defer ghCancel()

	// issues/commits는 super-session 통합용 outer var (위 SubAction defer가 capture).
	// 단순 weekly에서도 동일 변수 사용.
	var err error
	if scope.IncludesIssues() {
		issues, err = githubClient.ListIssues(ghCtx, owner, name, github.ListIssuesOptions{State: "open"})
		if err != nil {
			log.Printf("[주간/issues] ERR repo=%s err=%v", fullName, err)
			s.ChannelMessageSend(sess.ThreadID, fmt.Sprintf("이슈 조회 실패: %v", err))
			return
		}
	}
	if scope.IncludesCommits() {
		commits, err = githubClient.ListCommits(ghCtx, owner, name, github.ListCommitsOptions{Since: since, Until: until})
		if err != nil {
			log.Printf("[주간/commits] ERR repo=%s err=%v", fullName, err)
			s.ChannelMessageSend(sess.ThreadID, fmt.Sprintf("커밋 조회 실패: %v", err))
			return
		}
	}
	log.Printf("[주간/data] repo=%s scope=%s commit_since=%s issues_open=%d commits=%d directive_runes=%d",
		fullName, scope, since.UTC().Format(time.RFC3339), len(issues), len(commits), len([]rune(directive)))

	if len(issues) == 0 && len(commits) == 0 {
		s.ChannelMessageSend(sess.ThreadID, fmt.Sprintf("`%s` 레포에서 해당 범위(%s) 활동이 없습니다.", fullName, scope))
		// 빈 결과여도 세션 유지 — super-session sticky가 다음 작업을 안내.
		sess.State = StateSelectMode
		return
	}

	// 2) LLM 분석
	if progress != nil {
		progress.SetStage(2, "LLM 호출 및 응답 대기")
	}
	s.ChannelMessageSend(sess.ThreadID, weeklyAnalyzingMessage(scope, len(issues), len(commits)))
	llmCtx, llmCancel := context.WithTimeout(context.Background(), weeklyLLMTimeout)
	defer llmCancel()

	start := time.Now()
	resp, err := summarize.Weekly(llmCtx, llmClient, fullName, since, until, issues, commits, directive, scope)
	dur := time.Since(start)
	if err != nil {
		log.Printf("[주간/llm] ERR repo=%s elapsed=%s err=%v", fullName, dur, err)
		s.ChannelMessageSend(sess.ThreadID, fmt.Sprintf("LLM 분석 실패: %v", err))
		return
	}
	log.Printf("[주간/llm] ok repo=%s scope=%s elapsed=%s markdown_runes=%d closeable=%d",
		fullName, scope, dur, len([]rune(resp.Markdown)), len(resp.Closeable))

	if progress != nil {
		progress.SetStage(3, "렌더링")
	}
	// 3) 렌더링 + 분할 전송. 마지막 chunk에 follow-up 버튼 첨부.
	rendered := render.RenderWeekly(render.WeeklyRenderInput{
		RepoFullName: fullName,
		Since:        since,
		Until:        until,
		IssueCount:   len(issues),
		CommitCount:  len(commits),
		Response:     resp,
	})
	if progress != nil {
		progress.SetStage(4, "메시지 전송")
	}
	sendErr := func() error {
		_, e := sendLongMessageWithComponents(s, sess.ThreadID, rendered, weeklyFollowupComponents(len(resp.Closeable), scope))
		return e
	}()
	if sendErr != nil {
		log.Printf("[주간/send] ERR repo=%s (corpus 미누적 — 사용자 미수신): %v", fullName, sendErr)
	}

	// === Phase 3 chunk 3B-2b — super-session corpus 누적 ===
	// rendered markdown을 NoteSource=WeeklyDump로 sess에 추가 → finalize의 ContextNotes로 분류 →
	// 정리본 추출 시 LLM에 참고 자료로 전달되되 action.origin 후보 X (환각 방어).
	//
	// 전송 실패 시 누적 스킵 (review I3 반영) — 사용자가 보지 못한 분석이 정리본에 영향을 주는 것 차단.
	renderedFinal = rendered // defer EndWithArtifact가 capture
	if sa != nil && sendErr == nil {
		appendCtx, appendCancel := context.WithTimeout(context.Background(), 5*time.Second)
		sa.AppendResult(appendCtx, sess, "[weekly]", db.SourceWeeklyDump, rendered)
		appendCancel()
	}

	// 4) 세션 컨텍스트 박제 + 처음 메뉴 사용 가능 상태로 돌려놓음
	sess.LastWeeklyRepo = fullName
	sess.LastWeeklySince = since
	sess.LastWeeklyUntil = until
	sess.LastWeeklyDirective = directive
	sess.LastWeeklyScope = scope
	sess.LastWeeklyResponse = resp
	sess.LastWeeklyCloseable = resp.Closeable
	sess.LastBotSummary = rendered
	sess.State = StateSelectMode
}

// weeklyAnalyzingMessage는 fetch 직후 "분석 중..." 안내 문구를 scope에 맞춰 생성한다.
func weeklyAnalyzingMessage(scope llm.WeeklyScope, issueCount, commitCount int) string {
	switch scope {
	case llm.WeeklyScopeIssues:
		return fmt.Sprintf("이슈 %d건을 분석하는 중...", issueCount)
	case llm.WeeklyScopeCommits:
		return fmt.Sprintf("커밋 %d건을 분석하는 중...", commitCount)
	default:
		return fmt.Sprintf("이슈 %d건 + 커밋 %d건을 분석하는 중...", issueCount, commitCount)
	}
}

// weeklyFollowupComponents는 분석 결과 메시지 하단에 첨부하는 액션 버튼들을 만든다.
//
// D1 정책 (UX 재설계 2026-05): 네비게이션류 follow-up button [다시 분석][기간 변경][미팅 시작][처음 메뉴]
// 모두 폐기. 후속 작업은 super-session sticky 또는 다시 [GitHub 주간 분석]을 처음부터 누르는 식.
//
// 남는 행은 closeableCount > 0 일 때만 [닫아도 될 이슈 N건 닫기] 한 줄 — destructive action이라
// 결과 메시지 컨텍스트(closeable 후보 목록)에 묶여 있어야 안전하므로 sticky로 옮길 수 없음.
//
// scope 인자는 D1 이전엔 [기간 변경] 노출 여부 가르는 용도였지만 현재는 미사용. 시그니처는 호출측
// 변경 최소화 위해 유지 — 향후 필요해지면 활용.
func weeklyFollowupComponents(closeableCount int, _ llm.WeeklyScope) []discordgo.MessageComponent {
	if closeableCount <= 0 {
		return nil
	}
	return []discordgo.MessageComponent{
		discordgo.ActionsRow{Components: []discordgo.MessageComponent{
			discordgo.Button{
				Label:    fmt.Sprintf("닫아도 될 이슈 %d건 닫기", closeableCount),
				Style:    discordgo.DangerButton,
				CustomID: customIDWeeklyCloseStartBtn,
			},
		}},
	}
}

// =====================================================================
// follow-up 핸들러
// =====================================================================

// D1/D2 정책 폐기 함수/state (UX 재설계 2026-05):
//   - handleWeeklyDirective / handleWeeklyAwaitDirectiveMessage (D2 — directive 입력 대기 state 제거)
//   - handleWeeklyPeriodPrompt (D1 — follow-up [기간 변경] 폐기, 첫 분석 prompt만 유지)
//   - handleWeeklyRetry        (D1 — follow-up [다시 분석] 폐기)
//   - handleWeeklyToMeeting    (D1 — follow-up [미팅 시작] 폐기, 미팅 시작은 메인 채널에서)
//   - handleHome               (D1 — [처음 메뉴] 폐기, super-session sticky가 항상 노출)
//
// "더 깊게/다른 기간으로" 분석 원하면 super-session sticky의 [GitHub 주간 분석] button을 다시 클릭
// (처음부터). directive 입력 대기 / follow-up navigation은 사용자를 button 없는 modal-like state에
// 가두는 회귀(2026-05-15)의 근본 원인이라 제거.
//
// 보존: sess.LastWeekly* 필드는 finalize 시 ContextNotes corpus 재구성용으로 유지.
//        sess.LastWeeklyDirective는 D2 폐기 후 zero 값으로만 채워짐 (구조적 호환).

// weeklyPeriodPromptComponents는 [드롭다운][확인] 두 row를 만든다 (첫 분석 흐름 전용).
// currentDays는 드롭다운에서 어느 옵션을 default로 표시할지 결정 — 0 또는 음수면 weeklyPeriodDefaultDays(7)로 fallback.
//
// D1 정책: [처음 메뉴] button 폐기. 흐름 중단은 super-session sticky 또는 새 button 클릭으로.
func weeklyPeriodPromptComponents(currentDays int) []discordgo.MessageComponent {
	if currentDays <= 0 {
		currentDays = weeklyPeriodDefaultDays
	}
	options := []discordgo.SelectMenuOption{
		{Label: "지난 3일", Value: "3", Default: currentDays == 3},
		{Label: "지난 7일", Value: "7", Default: currentDays == 7},
		{Label: "지난 14일", Value: "14", Default: currentDays == 14},
		{Label: "지난 21일", Value: "21", Default: currentDays == 21},
		{Label: "지난 30일", Value: "30", Default: currentDays == 30},
		{
			Label:       "직접 입력",
			Value:       weeklyPeriodValueCustom,
			Description: fmt.Sprintf("임의 시작일 (YYYY-MM-DD, 최대 %d일 이내)", weeklyMaxCustomDays),
			Emoji:       &discordgo.ComponentEmoji{Name: "✏️"},
		},
	}
	return []discordgo.MessageComponent{
		discordgo.ActionsRow{Components: []discordgo.MessageComponent{
			discordgo.SelectMenu{
				MenuType:    discordgo.StringSelectMenu,
				CustomID:    customIDWeeklyPeriodSelect,
				Placeholder: fmt.Sprintf("지난 %d일", currentDays),
				Options:     options,
			},
		}},
		discordgo.ActionsRow{Components: []discordgo.MessageComponent{
			discordgo.Button{
				Label:    "확인",
				Style:    discordgo.SuccessButton,
				CustomID: customIDWeeklyPeriodConfirm,
			},
		}},
	}
}

// handleWeeklyPeriodSelect는 드롭다운에서 옵션 선택 시 호출된다.
//   - "직접 입력" → modal 발사 (handleWeeklyPeriodCalendar 재사용)
//   - 일자 옵션  → sess.PendingPeriodDays 박제 + UpdateMessage로 default 옵션 갱신
//
// [확인] 버튼이 별도로 있으니 여기서 분석을 발사하지 않는다.
func handleWeeklyPeriodSelect(s *discordgo.Session, i *discordgo.InteractionCreate, sess *Session) {
	_, scope, _, ok := resolveWeeklyPeriodContext(sess)
	if !ok {
		respondInteractionEphemeral(s, i, "이전 주간 분석 정보가 없습니다. 다시 [주간 정리]부터 시작해주세요.")
		return
	}
	if !scope.IncludesCommits() {
		respondInteractionEphemeral(s, i, "이슈 전용 분석에는 기간 선택이 적용되지 않습니다.")
		return
	}
	data := i.MessageComponentData()
	if len(data.Values) == 0 {
		respondInteractionEphemeral(s, i, "선택값이 비어 있습니다.")
		return
	}
	val := data.Values[0]
	if val == weeklyPeriodValueCustom {
		// "직접 입력" → modal 발사. PendingPeriodDays는 비워둠 — modal submit이 즉시 분석하므로
		// [확인] 버튼 흐름에 의존하지 않는다.
		handleWeeklyPeriodCalendar(s, i, sess)
		return
	}
	days, err := strconv.Atoi(val)
	if err != nil || days < 1 || days > weeklyMaxCustomDays {
		respondInteractionEphemeral(s, i, fmt.Sprintf("지원하지 않는 기간 값입니다: %q", val))
		return
	}
	sess.PendingPeriodDays = days

	// UpdateMessage로 같은 메시지의 컴포넌트만 교체 — 사용자가 본 default 옵션이 그대로 유지된다.
	if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseUpdateMessage,
		Data: &discordgo.InteractionResponseData{
			Content:    fmt.Sprintf("선택: 지난 **%d일**. [확인]을 눌러 분석을 시작하세요.", days),
			Components: weeklyPeriodPromptComponents(days),
		},
	}); err != nil {
		log.Printf("[주간/period/select] ERR update: %v", err)
	}
}

// handleWeeklyPeriodConfirm은 [확인] 버튼 클릭 시 sess.PendingPeriodDays(없으면 default 7)로 분석을 트리거한다.
// 첫 분석/follow-up 양쪽에서 호출되며 컨텍스트는 resolveWeeklyPeriodContext가 결정.
func handleWeeklyPeriodConfirm(s *discordgo.Session, i *discordgo.InteractionCreate, sess *Session) {
	repo, scope, directive, ok := resolveWeeklyPeriodContext(sess)
	if !ok {
		respondInteraction(s, i, "이전 주간 분석 정보가 없습니다. 다시 [주간 정리]부터 시작해주세요.")
		return
	}
	if !scope.IncludesCommits() {
		respondInteraction(s, i, "이슈 전용 분석에는 기간 변경이 적용되지 않습니다.")
		return
	}
	days := sess.PendingPeriodDays
	if days <= 0 {
		days = weeklyPeriodDefaultDays
	}
	clearPendingWeekly(sess)
	respondInteraction(s, i, fmt.Sprintf("`%s` 레포를 지난 %d일로 분석합니다.", repo, days))
	now := time.Now()
	since := now.Add(-time.Duration(days) * 24 * time.Hour)
	runWeeklyAnalyze(s, sess, repo, since, now, directive, scope)
}

// resolveWeeklyPeriodContext는 기간 핸들러가 사용할 (repo, scope, directive)를 결정한다.
// 우선순위:
//  1. PendingWeeklyRepo가 세팅되어 있으면 첫 분석 흐름 — Pending* 사용 (directive는 항상 빈 값 — D2 폐기)
//  2. 그 외엔 LastWeekly* fallback (D1 폐기 후 도달 경로 없음 — 방어용으로만 유지)
//
// 두 컨텍스트 모두 비어 있으면 ok=false.
func resolveWeeklyPeriodContext(sess *Session) (repo string, scope llm.WeeklyScope, directive string, ok bool) {
	if sess.PendingWeeklyRepo != "" {
		return sess.PendingWeeklyRepo, sess.PendingWeeklyScope, "", true
	}
	if sess.LastWeeklyRepo != "" {
		return sess.LastWeeklyRepo, sess.LastWeeklyScope, sess.LastWeeklyDirective, true
	}
	return "", 0, "", false
}

// clearPendingWeekly는 첫 분석 흐름의 임시 컨텍스트를 비운다.
// 분석 실행 직전에 호출해서 같은 prompt를 두 번 누르더라도 한 번만 처리되도록 한다.
func clearPendingWeekly(sess *Session) {
	sess.PendingWeeklyRepo = ""
	sess.PendingWeeklyScope = 0
	sess.PendingPeriodDays = 0
}

// handleWeeklyPeriodCalendar는 [캘린더] 버튼 클릭 시 modal을 띄워 임의 시작일을 입력 받는다.
// placeholder는 "오늘 - 14일"의 실제 날짜 — 빈 칸이면 포맷 마찰이 생기므로 한 글자만 고치면 되도록 실시한다.
// 첫 분석/follow-up 양쪽에서 호출되므로 resolveWeeklyPeriodContext로 컨텍스트 검증.
func handleWeeklyPeriodCalendar(s *discordgo.Session, i *discordgo.InteractionCreate, sess *Session) {
	_, scope, _, ok := resolveWeeklyPeriodContext(sess)
	if !ok {
		respondInteractionEphemeral(s, i, "이전 주간 분석 정보가 없습니다. 다시 [주간 정리]부터 시작해주세요.")
		return
	}
	if !scope.IncludesCommits() {
		respondInteractionEphemeral(s, i, "이슈 전용 분석에는 기간 변경이 적용되지 않습니다.")
		return
	}
	placeholder := time.Now().Add(-14 * 24 * time.Hour).Format("2006-01-02")
	if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseModal,
		Data: &discordgo.InteractionResponseData{
			CustomID: customIDWeeklyPeriodModal,
			Title:    "커밋 분석 시작일",
			Components: []discordgo.MessageComponent{
				discordgo.ActionsRow{
					Components: []discordgo.MessageComponent{
						discordgo.TextInput{
							CustomID:    customIDWeeklyPeriodModalDate,
							Label:       fmt.Sprintf("시작일 (YYYY-MM-DD, 최대 %d일 이내)", weeklyMaxCustomDays),
							Style:       discordgo.TextInputShort,
							Placeholder: placeholder,
							Required:    true,
							MinLength:   10,
							MaxLength:   10,
						},
					},
				},
			},
		},
	}); err != nil {
		log.Printf("[주간/calendar] ERR modal 발사 실패: %v", err)
	}
}

// handleWeeklyPeriodModalSubmit는 사용자가 [캘린더] modal에 시작일을 입력하고 제출했을 때 호출된다.
// 검증 순서:
//  1. 형식 (YYYY-MM-DD strict)
//  2. 미래 날짜 거부
//  3. 30일 초과 거부
//
// 통과 시 첫 분석/follow-up 컨텍스트를 resolveWeeklyPeriodContext로 결정해 runWeeklyAnalyze 호출.
// 검증 실패는 ephemeral 응답으로 사용자에게만 안내해 채널을 어지럽히지 않는다.
func handleWeeklyPeriodModalSubmit(s *discordgo.Session, i *discordgo.InteractionCreate, sess *Session) {
	repo, scope, directive, ok := resolveWeeklyPeriodContext(sess)
	if !ok {
		respondInteractionEphemeral(s, i, "이전 주간 분석 정보가 없습니다. 다시 [주간 정리]부터 시작해주세요.")
		return
	}
	if !scope.IncludesCommits() {
		respondInteractionEphemeral(s, i, "이슈 전용 분석에는 기간 변경이 적용되지 않습니다.")
		return
	}
	raw := strings.TrimSpace(extractModalTextValue(i, customIDWeeklyPeriodModalDate))
	if raw == "" {
		respondInteractionEphemeral(s, i, "시작일이 비어 있습니다.")
		return
	}
	since, err := time.ParseInLocation("2006-01-02", raw, time.Local)
	if err != nil {
		respondInteractionEphemeral(s, i, fmt.Sprintf("날짜 형식이 올바르지 않습니다 (입력=%q). YYYY-MM-DD 형식으로 다시 입력해주세요.", raw))
		return
	}
	now := time.Now()
	if since.After(now) {
		respondInteractionEphemeral(s, i, fmt.Sprintf("시작일이 미래입니다 (입력=%s, 오늘=%s).",
			since.Format("2006-01-02"), now.Format("2006-01-02")))
		return
	}
	maxAge := time.Duration(weeklyMaxCustomDays) * 24 * time.Hour
	if now.Sub(since) > maxAge {
		respondInteractionEphemeral(s, i, fmt.Sprintf("최대 %d일 이내 시작일만 지원합니다 (입력=%s, 한도=%s).",
			weeklyMaxCustomDays, since.Format("2006-01-02"), now.Add(-maxAge).Format("2006-01-02")))
		return
	}

	clearPendingWeekly(sess)
	respondInteraction(s, i, fmt.Sprintf("`%s` 레포를 %s 이후 커밋으로 분석합니다.",
		repo, since.Format("2006-01-02")))
	log.Printf("[주간/calendar] thread=%s repo=%s since=%s scope=%s",
		sess.ThreadID, repo, since.Format(time.RFC3339), scope)
	runWeeklyAnalyze(s, sess, repo, since, now, directive, scope)
}

// extractModalTextValue는 ModalSubmit interaction에서 지정한 customID의 TextInput 값을 꺼낸다.
// 찾지 못하면 "" 반환.
func extractModalTextValue(i *discordgo.InteractionCreate, fieldID string) string {
	data := i.ModalSubmitData()
	for _, row := range data.Components {
		actionRow, ok := row.(*discordgo.ActionsRow)
		if !ok {
			continue
		}
		for _, comp := range actionRow.Components {
			if input, ok := comp.(*discordgo.TextInput); ok && input.CustomID == fieldID {
				return input.Value
			}
		}
	}
	return ""
}

// handleWeeklyRetry / handleWeeklyToMeeting 폐기 — D1 정책 (UX 재설계 2026-05).
// 후속 작업은 super-session sticky 또는 새 button 클릭으로.

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

	// D1: [취소] button 폐기 — 닫기 진행을 원치 않으면 단순히 [확인]을 누르지 않으면 됨.
	respondInteractionWithRow(s, i, b.String(),
		discordgo.Button{Label: "확인", Style: discordgo.DangerButton, CustomID: customIDWeeklyCloseConfirmBtn},
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

	if _, err := s.ChannelMessageSend(sess.ThreadID, b.String()); err != nil {
		log.Printf("[주간/close] ERR send result: %v", err)
	}

	// 같은 후보로 두 번 누르지 못하게 비움.
	sess.LastWeeklyCloseable = nil
}

// handleHome 폐기 — D1 정책 (UX 재설계 2026-05).
// super-session에서는 sticky의 [세션 종료] button만, legacy(ModeNormal)에서는 신규 mention으로 새 스레드.

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
