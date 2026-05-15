package bot

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"chatbot-alpha-1/pkg/github"

	"github.com/bwmarrin/discordgo"
)

// =====================================================================
// 릴리즈 PR 머지 추적 폴링
// =====================================================================
//
// PR 생성 후 별도 goroutine 에서 주기적으로 GitHub 를 호출해 CI/리뷰/머지 상태를
// 디스코드 메시지 in-place 편집으로 갱신한다. auto-merge 는 하지 않으며 사용자가
// GitHub UI 에서 직접 머지하면 봇이 그 사실을 감지해 완료 메시지로 마무리.

const (
	customIDReleasePollStop = "release_poll_stop"

	// 폴링 주기 — GitHub rate limit (5000/h) 충분히 여유.
	// 한 번 폴링당 3 호출 (PR/checks/reviews) × 30s = 시간당 360 호출.
	releasePollInterval = 30 * time.Second
	// 최대 폴링 시간. 사용자가 자리 비웠을 때 무한 폴링 방지.
	releasePollMaxDuration = 30 * time.Minute
	// 호출 1건당 timeout. 30초 주기보다 짧아야 한다.
	releasePollCallTimeout = 15 * time.Second
)

// pollReleasePR 은 PR 생성 직후 호출되어 폴링 패널을 운영한다.
// 종료 조건: merged / closed without merge / max duration / 사용자 [폴링 중단] (ctx cancel).
//
// 첫 호출은 즉시 한 번 실행해 패널이 빨리 노출되도록 한다 (사용자 체감 지연 최소화).
func pollReleasePR(ctx context.Context, s *discordgo.Session, sess *Session, rc *ReleaseContext) {
	// 초기 패널 전송 (loading state)
	msg, err := s.ChannelMessageSendComplex(sess.ThreadID, &discordgo.MessageSend{
		Embeds:     []*discordgo.MessageEmbed{renderPollNotice(rc, colorWarn, "머지 추적 — 시작", "첫 갱신을 기다리는 중입니다.", "자동 갱신 중 · 머지 가능 시 자동 종료")},
		Components: releasePollComponents(rc.PRURL, false),
	})
	if err != nil {
		log.Printf("[릴리즈/polling] 초기 패널 전송 실패 thread=%s: %v", sess.ThreadID, err)
		return
	}
	rc.PollMsgID = msg.ID

	deadline := time.Now().Add(releasePollMaxDuration)

	// 첫 한 번은 즉시
	if done := runOnePollCycle(ctx, s, sess, rc); done {
		return
	}

	ticker := time.NewTicker(releasePollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			// 사용자 [폴링 중단] 또는 봇 종료
			finalizePollPanel(s, sess, rc, renderPollNotice(rc, colorWarn, "머지 추적 — 중단됨", "폴링이 중단되었습니다. GitHub 에서 직접 머지 상태를 확인해주세요.", "폴링 종료"))
			return
		case <-ticker.C:
			if time.Now().After(deadline) {
				finalizePollPanel(s, sess, rc, renderPollNotice(rc, colorWarn, "머지 추적 — 시간 초과", "폴링 시간 초과 (30분). GitHub 에서 직접 확인해주세요.", "폴링 종료"))
				return
			}
			if done := runOnePollCycle(ctx, s, sess, rc); done {
				return
			}
		}
	}
}

// runOnePollCycle 은 1회 폴링 (PR/checks/reviews 조회) 후 메시지를 갱신한다.
// PR 상태가 종료(merged/closed/ready-to-merge)면 true 반환해 caller 가 폴링을 끝낸다.
func runOnePollCycle(ctx context.Context, s *discordgo.Session, sess *Session, rc *ReleaseContext) (terminate bool) {
	callCtx, cancel := context.WithTimeout(ctx, releasePollCallTimeout)
	defer cancel()

	pr, err := githubClient.GetPullRequest(callCtx, rc.Owner, rc.Repo, rc.PRNumber)
	if err != nil {
		log.Printf("[릴리즈/polling] GetPullRequest 실패: %v", err)
		updatePollPanel(s, sess, rc, renderPollNotice(rc, colorWarn, "머지 추적 — 조회 재시도", fmt.Sprintf("GetPullRequest 일시 실패: %v\n다음 주기에서 재시도합니다.", err), "자동 갱신 중 · 머지 가능 시 자동 종료"), false)
		return false
	}

	// PR 종료 케이스 — merged 또는 closed without merge.
	if pr.Merged {
		finalizePollPanel(s, sess, rc, renderMergedSummary(rc, pr))
		return true
	}
	if pr.State == "closed" {
		finalizePollPanel(s, sess, rc, renderPollNotice(rc, colorBad, "머지 추적 — 닫힘", fmt.Sprintf("PR #%d 가 머지되지 않고 닫혔습니다. (state=closed, merged=false)", rc.PRNumber), "폴링 종료"))
		return true
	}

	// CI/리뷰 조회 — head SHA 기준
	head := pr.Head.SHA
	if head == "" {
		head = rc.PRHeadSHA
	}
	checks, cErr := githubClient.ListCheckRuns(callCtx, rc.Owner, rc.Repo, head)
	if cErr != nil {
		log.Printf("[릴리즈/polling] ListCheckRuns 실패: %v", cErr)
	}
	reviews, rErr := githubClient.ListReviews(callCtx, rc.Owner, rc.Repo, rc.PRNumber)
	if rErr != nil {
		log.Printf("[릴리즈/polling] ListReviews 실패: %v", rErr)
	}

	// 머지 가능(clean) 상태 도달 → 폴링 종료. 머지 자체는 사용자가 GitHub 에서 직접.
	if pr.Mergeable != nil && *pr.Mergeable && pr.MergeableState == "clean" {
		finalizePollPanel(s, sess, rc, renderReadyToMerge(rc, checks, reviews))
		return true
	}

	embed := renderPollPanel(rc, pr, checks, reviews)
	updatePollPanel(s, sess, rc, embed, false)
	return false
}

// releasePollComponents는 폴링 패널의 버튼 행을 만든다.
// prURL이 비어있지 않으면 GitHub PR로 바로 이동하는 LinkButton 을 맨 앞에 노출.
// stopped=true 면 [폴링 중단] 버튼을 제거하고 [PR 열기]만 남긴다.
//
// D1 정책: [처음 메뉴] button 폐기 — 후속 작업은 super-session sticky로.
// row가 비면 nil 반환 (Discord component row 0개를 강제하지 않음).
func releasePollComponents(prURL string, stopped bool) []discordgo.MessageComponent {
	row := []discordgo.MessageComponent{}
	if prURL != "" {
		row = append(row, discordgo.Button{
			Label: "PR 열기",
			Style: discordgo.LinkButton,
			URL:   prURL,
		})
	}
	if !stopped {
		row = append(row, discordgo.Button{
			Label:    "폴링 중단",
			Style:    discordgo.DangerButton,
			CustomID: customIDReleasePollStop,
		})
	}
	if len(row) == 0 {
		return nil
	}
	return []discordgo.MessageComponent{discordgo.ActionsRow{Components: row}}
}

// updatePollPanel 은 폴링 메시지를 in-place 편집한다 (진행 중 상태).
func updatePollPanel(s *discordgo.Session, sess *Session, rc *ReleaseContext, embed *discordgo.MessageEmbed, stopped bool) {
	_, err := s.ChannelMessageEditComplex(&discordgo.MessageEdit{
		Channel:    sess.ThreadID,
		ID:         rc.PollMsgID,
		Content:    ptrString(""),
		Embeds:     ptrEmbeds(embed),
		Components: ptrComponents(releasePollComponents(rc.PRURL, stopped)),
	})
	if err != nil {
		log.Printf("[릴리즈/polling] 패널 edit 실패: %v", err)
	}
}

// finalizePollPanel 은 폴링 종료 메시지를 출력하고 [폴링 중단] 버튼을 제거 + [처음 메뉴] 만 노출.
func finalizePollPanel(s *discordgo.Session, sess *Session, rc *ReleaseContext, embed *discordgo.MessageEmbed) {
	_, err := s.ChannelMessageEditComplex(&discordgo.MessageEdit{
		Channel:    sess.ThreadID,
		ID:         rc.PollMsgID,
		Content:    ptrString(""),
		Embeds:     ptrEmbeds(embed),
		Components: ptrComponents(releasePollComponents(rc.PRURL, true)),
	})
	if err != nil {
		log.Printf("[릴리즈/polling] 종료 edit 실패: %v", err)
	}
	if rc.PollCancel != nil {
		rc.PollCancel()
	}
}

// ptrComponents 는 discordgo.MessageEdit 의 Components 가 슬라이스 포인터를 요구하므로 보조.
func ptrComponents(cs []discordgo.MessageComponent) *[]discordgo.MessageComponent {
	return &cs
}

// ptrString 는 MessageEdit 의 문자열 포인터 필드용 보조 함수다.
func ptrString(s string) *string {
	return &s
}

// ptrEmbeds 는 MessageEdit 의 embed 포인터 필드용 보조 함수다.
func ptrEmbeds(embed *discordgo.MessageEmbed) *[]*discordgo.MessageEmbed {
	return &[]*discordgo.MessageEmbed{embed}
}

// =====================================================================
// 패널 본문 렌더
// =====================================================================

// renderPollPanel 은 진행 중인 PR 상태를 embed 카드로 정리한다.
func renderPollPanel(rc *ReleaseContext, pr *github.PullRequest, checks []github.CheckRun, reviews []github.Review) *discordgo.MessageEmbed {
	passed, running, failed := groupChecks(checks)
	passedCount := groupedCheckCount(passed)
	runningCount := groupedCheckCount(running)
	failedCount := groupedCheckCount(failed)
	total := passedCount + runningCount + failedCount
	done := passedCount + failedCount

	stripe := colorWarn
	title := "머지 추적 — 진행 중"
	if failedCount > 0 {
		stripe = colorBad
		title = limitText(fmt.Sprintf("머지 추적 — 실패 (%s)", failed[0].Name), 256)
	}

	embed := releaseEmbed(stripe, releasePollAuthor(rc), title)
	embed.Description = fmt.Sprintf("%s\n통과 %d · 진행 %d · 실패 %d",
		progressBar(done, total), passedCount, runningCount, failedCount)
	embed.Fields = []*discordgo.MessageEmbedField{
		embedField(fmt.Sprintf("통과 (%d개)", passedCount), formatCheckLines("✓", passed), false),
		embedField("진행 / 실패", formatRunningFailedLines(running, failed), false),
		embedField("리뷰", limitText(summarizeReviews(reviews), 1024), true),
		embedField("머지", limitText(summarizeMergeable(pr), 1024), true),
	}
	embed.Footer = &discordgo.MessageEmbedFooter{Text: "자동 갱신 중 · 머지 가능 시 자동 종료"}
	return embed
}

// renderReadyToMerge 는 머지 가능(clean) 도달 시 노출하는 최종 embed 다.
func renderReadyToMerge(rc *ReleaseContext, checks []github.CheckRun, reviews []github.Review) *discordgo.MessageEmbed {
	total := len(checks)
	embed := releaseEmbed(colorOK, releasePollAuthor(rc), "머지 추적 — 머지 가능")
	embed.Description = fmt.Sprintf("%s\n모든 체크 통과 · %s · 머지 상태 clean",
		progressBar(total, total), summarizeReviews(reviews))
	embed.Footer = &discordgo.MessageEmbedFooter{Text: "폴링 종료 · GitHub 에서 직접 머지"}
	return embed
}

// renderMergedSummary 는 PR 머지 완료 시 최종 embed 를 만든다.
func renderMergedSummary(rc *ReleaseContext, pr *github.PullRequest) *discordgo.MessageEmbed {
	base := pr.Base.Ref
	if base == "" {
		base = rc.Module.ReleaseBranch
	}
	mergedAt := "알 수 없음"
	if pr.MergedAt != nil {
		mergedAt = pr.MergedAt.Local().Format("2006-01-02 15:04:05 MST")
	}
	sha := pr.MergeCommitSHA
	if sha == "" {
		sha = "알 수 없음"
	}

	embed := releaseEmbed(colorOK, releasePollAuthor(rc), "머지 추적 — 머지 완료")
	embed.Description = fmt.Sprintf("머지 시각 %s · 머지 SHA `%s` · base `%s`", mergedAt, sha, base)
	embed.Footer = &discordgo.MessageEmbedFooter{Text: "폴링 종료"}
	return embed
}

// renderPollNotice 는 오류/중단 같은 단일 상태 embed 를 만든다.
func renderPollNotice(rc *ReleaseContext, stripe int, title, description, footer string) *discordgo.MessageEmbed {
	embed := releaseEmbed(stripe, releasePollAuthor(rc), limitText(title, 256))
	embed.Description = limitText(description, 4096)
	embed.Footer = &discordgo.MessageEmbedFooter{Text: limitText(footer, 2048)}
	return embed
}

// releasePollAuthor 는 라인/릴리즈/PR 정보를 author 라벨로 만든다.
func releasePollAuthor(rc *ReleaseContext) string {
	prefix := rc.Module.TagPrefix
	if prefix == "" {
		prefix = rc.Module.Key
	}
	return fmt.Sprintf("%s · %s/v%s · PR #%d", rc.Module.Line.String(), prefix, rc.NewVersion, rc.PRNumber)
}

// groupedCheckCount 는 묶인 check 개수 합계를 반환한다.
func groupedCheckCount(groups []groupedCheck) int {
	total := 0
	for _, group := range groups {
		total += group.Count
	}
	return total
}

// formatCheckLines 는 check 그룹을 mono field 값으로 만든다.
func formatCheckLines(marker string, groups []groupedCheck) string {
	lines := groupedCheckLines(marker, groups)
	if len(lines) == 0 {
		return "`없음`"
	}
	body := limitText(strings.Join(lines, "\n"), 1016)
	return "```\n" + body + "\n```"
}

// formatRunningFailedLines 는 진행/실패 check 그룹을 field 값으로 만든다.
func formatRunningFailedLines(running, failed []groupedCheck) string {
	lines := groupedCheckLines("▶", running)
	lines = append(lines, groupedCheckLines("✗", failed)...)
	if len(lines) == 0 {
		return "없음"
	}
	return limitText(strings.Join(lines, "\n"), 1024)
}

// groupedCheckLines 는 check 그룹을 마커가 붙은 줄 목록으로 변환한다.
func groupedCheckLines(marker string, groups []groupedCheck) []string {
	lines := make([]string, 0, len(groups))
	for _, group := range groups {
		name := group.Name
		if name == "" {
			name = "(이름 없음)"
		}
		line := fmt.Sprintf("%s %s", marker, name)
		if group.Count > 1 {
			line += fmt.Sprintf(" (×%d)", group.Count)
		}
		lines = append(lines, line)
	}
	return lines
}

// limitText 는 Discord embed 한도에 맞춰 문자열 길이를 제한한다.
func limitText(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	if max <= 3 {
		return string(runes[:max])
	}
	return string(runes[:max-3]) + "..."
}

// summarizeReviews 는 user 별 latest 리뷰만 살려 승인 수를 센다.
// COMMENTED 는 카운트에서 제외.
func summarizeReviews(reviews []github.Review) string {
	if len(reviews) == 0 {
		return "0 명 승인 — GitHub 에서 직접 승인 필요"
	}
	// user → latest review (state 기준 우선순위는 없으므로 시간순)
	latest := make(map[string]github.Review)
	for _, r := range reviews {
		cur, ok := latest[r.User.Login]
		if !ok || r.SubmittedAt.After(cur.SubmittedAt) {
			latest[r.User.Login] = r
		}
	}
	approved := []string{}
	changes := []string{}
	for login, r := range latest {
		switch r.State {
		case "APPROVED":
			approved = append(approved, login)
		case "CHANGES_REQUESTED":
			changes = append(changes, login)
		}
	}
	sort.Strings(approved)
	sort.Strings(changes)

	switch {
	case len(changes) > 0:
		return fmt.Sprintf("✗ %d 명 변경 요청 (%s)", len(changes), joinTruncate(changes, 3))
	case len(approved) > 0:
		return fmt.Sprintf("✓ %d 명 승인 (%s)", len(approved), joinTruncate(approved, 3))
	default:
		return "0 명 승인 — GitHub 에서 직접 승인 필요"
	}
}

// summarizeMergeable 은 PR 의 mergeable 상태를 한 줄로.
func summarizeMergeable(pr *github.PullRequest) string {
	if pr.Mergeable == nil {
		return "체크 대기 (GitHub 계산 중)"
	}
	if !*pr.Mergeable {
		return fmt.Sprintf("✗ 머지 불가 (state=%s) — 충돌 또는 보호 규칙 위반", pr.MergeableState)
	}
	switch pr.MergeableState {
	case "clean":
		return "✓ 머지 가능 — GitHub 에서 직접 머지하세요"
	case "blocked":
		return "✗ 블록됨 (state=blocked) — 필수 체크/리뷰 대기"
	case "behind":
		return "▶ behind (state=behind) — base 갱신 필요"
	case "unstable":
		return "▶ unstable — 일부 비필수 체크 실패"
	default:
		return fmt.Sprintf("? state=%s", pr.MergeableState)
	}
}

// joinTruncate 는 names 를 ", " 로 join 하되 max 를 넘으면 "외 N건" 으로 압축.
func joinTruncate(names []string, max int) string {
	if len(names) <= max {
		return strings.Join(names, ", ")
	}
	return strings.Join(names[:max], ", ") + fmt.Sprintf(" 외 %d건", len(names)-max)
}

// =====================================================================
// 사용자 [폴링 중단] 핸들러
// =====================================================================

func handleReleasePollStop(s *discordgo.Session, i *discordgo.InteractionCreate, sess *Session) {
	if sess.ReleaseCtx == nil || sess.ReleaseCtx.PollCancel == nil {
		respondInteraction(s, i, "활성 폴링이 없습니다.")
		return
	}
	respondInteraction(s, i, "폴링을 중단합니다.")
	sess.ReleaseCtx.PollCancel()
}
