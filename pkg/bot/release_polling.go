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
		Content:    fmt.Sprintf("🔄 **PR #%d** 머지 추적을 시작합니다... (첫 갱신 대기)", rc.PRNumber),
		Components: releasePollComponents(rc.PRURL, false),
	})
	if err != nil {
		log.Printf("[릴리즈/polling] 초기 패널 전송 실패 thread=%s: %v", sess.ThreadID, err)
		return
	}
	rc.PollMsgID = msg.ID

	deadline := time.Now().Add(releasePollMaxDuration)

	// 첫 한 번은 즉시
	if done := runOnePollCycle(ctx, s, sess, rc, time.Since(time.Now())+0, 1, deadline); done {
		return
	}

	ticker := time.NewTicker(releasePollInterval)
	defer ticker.Stop()
	tick := 1
	start := time.Now()

	for {
		select {
		case <-ctx.Done():
			// 사용자 [폴링 중단] 또는 봇 종료
			finalizePollPanel(s, sess, rc, "🛑 폴링 중단됨. GitHub 에서 직접 머지 상태를 확인해주세요.", true)
			return
		case <-ticker.C:
			tick++
			if time.Now().After(deadline) {
				finalizePollPanel(s, sess, rc, "⏱ 폴링 시간 초과 (30분). GitHub 에서 직접 확인해주세요.", true)
				return
			}
			if done := runOnePollCycle(ctx, s, sess, rc, time.Since(start), tick, deadline); done {
				return
			}
		}
	}
}

// runOnePollCycle 은 1회 폴링 (PR/checks/reviews 조회) 후 메시지를 갱신한다.
// PR 상태가 종료(merged/closed)면 true 반환해 caller 가 폴링을 끝낸다.
func runOnePollCycle(ctx context.Context, s *discordgo.Session, sess *Session, rc *ReleaseContext, elapsed time.Duration, tick int, deadline time.Time) (terminate bool) {
	callCtx, cancel := context.WithTimeout(ctx, releasePollCallTimeout)
	defer cancel()

	pr, err := githubClient.GetPullRequest(callCtx, rc.Owner, rc.Repo, rc.PRNumber)
	if err != nil {
		log.Printf("[릴리즈/polling] GetPullRequest tick=%d 실패: %v", tick, err)
		updatePollPanel(s, sess, rc, fmt.Sprintf("⚠️ GetPullRequest 일시 실패: %v\n(다음 주기에서 재시도)", err), elapsed, tick, false)
		return false
	}

	// PR 종료 케이스 — merged 또는 closed without merge.
	if pr.Merged {
		finalizePollPanel(s, sess, rc, renderMergedSummary(rc, pr), true)
		return true
	}
	if pr.State == "closed" {
		finalizePollPanel(s, sess, rc, fmt.Sprintf("🚫 **PR #%d** 가 머지되지 않고 닫혔습니다. (state=closed, merged=false)", rc.PRNumber), true)
		return true
	}

	// CI/리뷰 조회 — head SHA 기준
	head := pr.Head.SHA
	if head == "" {
		head = rc.PRHeadSHA
	}
	checks, cErr := githubClient.ListCheckRuns(callCtx, rc.Owner, rc.Repo, head)
	if cErr != nil {
		log.Printf("[릴리즈/polling] ListCheckRuns tick=%d 실패: %v", tick, cErr)
	}
	reviews, rErr := githubClient.ListReviews(callCtx, rc.Owner, rc.Repo, rc.PRNumber)
	if rErr != nil {
		log.Printf("[릴리즈/polling] ListReviews tick=%d 실패: %v", tick, rErr)
	}

	body := renderPollPanel(rc, pr, checks, reviews, elapsed, tick)
	updatePollPanel(s, sess, rc, body, elapsed, tick, false)
	return false
}

// releasePollComponents는 폴링 패널의 버튼 행을 만든다.
// prURL이 비어있지 않으면 GitHub PR로 바로 이동하는 LinkButton 을 맨 앞에 노출.
// stopped=true 면 [폴링 중단] 버튼을 제거하고 [PR 열기][처음 메뉴] 만 남긴다.
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
	row = append(row, homeButton())
	return []discordgo.MessageComponent{discordgo.ActionsRow{Components: row}}
}

// updatePollPanel 은 폴링 메시지를 in-place 편집한다 (진행 중 상태).
func updatePollPanel(s *discordgo.Session, sess *Session, rc *ReleaseContext, body string, elapsed time.Duration, tick int, stopped bool) {
	_, err := s.ChannelMessageEditComplex(&discordgo.MessageEdit{
		Channel:    sess.ThreadID,
		ID:         rc.PollMsgID,
		Content:    &body,
		Components: ptrComponents(releasePollComponents(rc.PRURL, stopped)),
	})
	if err != nil {
		log.Printf("[릴리즈/polling] 패널 edit 실패 tick=%d: %v", tick, err)
	}
}

// finalizePollPanel 은 폴링 종료 메시지를 출력하고 [폴링 중단] 버튼을 제거 + [처음 메뉴] 만 노출.
func finalizePollPanel(s *discordgo.Session, sess *Session, rc *ReleaseContext, body string, stopped bool) {
	_, err := s.ChannelMessageEditComplex(&discordgo.MessageEdit{
		Channel:    sess.ThreadID,
		ID:         rc.PollMsgID,
		Content:    &body,
		Components: ptrComponents(releasePollComponents(rc.PRURL, true)),
	})
	if err != nil {
		log.Printf("[릴리즈/polling] 종료 edit 실패: %v", err)
	}
	if rc.PollCancel != nil {
		rc.PollCancel()
	}
	_ = stopped // 향후 stop reason 분기에 사용 가능 (현재는 메시지 본문에 통합)
}

// ptrComponents 는 discordgo.MessageEdit 의 Components 가 슬라이스 포인터를 요구하므로 보조.
func ptrComponents(cs []discordgo.MessageComponent) *[]discordgo.MessageComponent {
	return &cs
}

// =====================================================================
// 패널 본문 렌더
// =====================================================================

// renderPollPanel 은 진행 중인 PR 상태를 한 메시지로 정리한다.
// 디스코드 2000자 제한 안에서 핵심만.
func renderPollPanel(rc *ReleaseContext, pr *github.PullRequest, checks []github.CheckRun, reviews []github.Review, elapsed time.Duration, tick int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "🔄 **PR #%d** 머지 추적 — `%s`\n\n", rc.PRNumber, rc.Module.Key+"-v"+rc.NewVersion.String())

	// CI
	ciStatus, ciDetail := summarizeChecks(checks)
	b.WriteString("• CI: ")
	b.WriteString(ciStatus)
	if ciDetail != "" {
		b.WriteString(" — ")
		b.WriteString(ciDetail)
	}
	b.WriteString("\n")

	// 리뷰
	revStatus := summarizeReviews(reviews)
	b.WriteString("• 리뷰: ")
	b.WriteString(revStatus)
	b.WriteString("\n")

	// Mergeable
	b.WriteString("• 머지 가능: ")
	b.WriteString(summarizeMergeable(pr))
	b.WriteString("\n")

	fmt.Fprintf(&b, "\n경과 %s · 주기 %ds · 갱신 #%d",
		fmtElapsed(elapsed), int(releasePollInterval/time.Second), tick)
	return b.String()
}

// renderMergedSummary 는 PR 머지 완료 시 최종 메시지.
func renderMergedSummary(rc *ReleaseContext, pr *github.PullRequest) string {
	var b strings.Builder
	fmt.Fprintf(&b, "✅ **PR #%d** 머지 완료 — `%s`\n\n",
		rc.PRNumber, rc.Module.Key+"-v"+rc.NewVersion.String())
	fmt.Fprintf(&b, "• base: `%s` ← head: `main`\n", pr.Base.Ref)
	if rc.Module.HasDeploy {
		fmt.Fprintf(&b, "• 후속: `release/%s` 머지로 deploy_v2_release_* 워크플로우가 자동 트리거됩니다 (sandbox 라 실제 배포 영향은 없음).\n",
			strings.TrimPrefix(rc.Module.ReleaseBranch, "release/"))
	} else {
		b.WriteString("• 주의: 모듈 HasDeploy=false — prod 자동배포 워크플로우 없음.\n")
	}
	return b.String()
}

// summarizeChecks 는 check-runs 를 한 줄로 압축한다.
// 반환 1: 한 단어 status — running/success/failure/queued/no_checks
// 반환 2: 상세 — "N/M 완료" + 실패 항목 노출
func summarizeChecks(checks []github.CheckRun) (string, string) {
	if len(checks) == 0 {
		return "no_checks", "체크 없음"
	}
	completed := 0
	failed := 0
	failedNames := []string{}
	runningNames := []string{}
	for _, ch := range checks {
		if ch.Status == "completed" {
			completed++
			switch ch.Conclusion {
			case "failure", "timed_out", "action_required", "cancelled":
				failed++
				failedNames = append(failedNames, ch.Name)
			}
		} else {
			runningNames = append(runningNames, ch.Name)
		}
	}
	total := len(checks)
	if failed > 0 {
		// 실패 노출 — 핵심 정보
		return "❌ failure", fmt.Sprintf("%d/%d 완료, 실패: %s", completed, total, joinTruncate(failedNames, 3))
	}
	if completed == total {
		return "✓ success", fmt.Sprintf("%d/%d 완료", completed, total)
	}
	// 진행 중
	return "▶ running", fmt.Sprintf("%d/%d 완료, 진행: %s", completed, total, joinTruncate(runningNames, 3))
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
		return fmt.Sprintf("⚠️ %d 명 변경 요청 (%s)", len(changes), joinTruncate(changes, 3))
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
		return "⚠️ behind (state=behind) — base 갱신 필요"
	case "unstable":
		return "⚠️ unstable — 일부 비필수 체크 실패"
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

// fmtElapsed 는 duration 을 "mm:ss" 또는 "Hh Mm" 으로 포맷.
func fmtElapsed(d time.Duration) string {
	total := int(d.Round(time.Second).Seconds())
	if total < 3600 {
		return fmt.Sprintf("%d:%02d", total/60, total%60)
	}
	return fmt.Sprintf("%dh %dm", total/3600, (total%3600)/60)
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
