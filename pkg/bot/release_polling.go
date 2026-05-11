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
	if done := runOnePollCycle(ctx, s, sess, rc); done {
		return
	}

	ticker := time.NewTicker(releasePollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			// 사용자 [폴링 중단] 또는 봇 종료
			finalizePollPanel(s, sess, rc, "🛑 폴링 중단됨. GitHub 에서 직접 머지 상태를 확인해주세요.", true)
			return
		case <-ticker.C:
			if time.Now().After(deadline) {
				finalizePollPanel(s, sess, rc, "⏱ 폴링 시간 초과 (30분). GitHub 에서 직접 확인해주세요.", true)
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
		updatePollPanel(s, sess, rc, fmt.Sprintf("⚠️ GetPullRequest 일시 실패: %v\n(다음 주기에서 재시도)", err), false)
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
		log.Printf("[릴리즈/polling] ListCheckRuns 실패: %v", cErr)
	}
	reviews, rErr := githubClient.ListReviews(callCtx, rc.Owner, rc.Repo, rc.PRNumber)
	if rErr != nil {
		log.Printf("[릴리즈/polling] ListReviews 실패: %v", rErr)
	}

	// 머지 가능(clean) 상태 도달 → 폴링 종료. 머지 자체는 사용자가 GitHub 에서 직접.
	if pr.Mergeable != nil && *pr.Mergeable && pr.MergeableState == "clean" {
		finalizePollPanel(s, sess, rc, renderReadyToMerge(rc, checks, reviews), false)
		return true
	}

	body := renderPollPanel(rc, pr, checks, reviews)
	updatePollPanel(s, sess, rc, body, false)
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
func updatePollPanel(s *discordgo.Session, sess *Session, rc *ReleaseContext, body string, stopped bool) {
	_, err := s.ChannelMessageEditComplex(&discordgo.MessageEdit{
		Channel:    sess.ThreadID,
		ID:         rc.PollMsgID,
		Content:    &body,
		Components: ptrComponents(releasePollComponents(rc.PRURL, stopped)),
	})
	if err != nil {
		log.Printf("[릴리즈/polling] 패널 edit 실패: %v", err)
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
// CI 진행은 progress bar + per-job 목록, 리뷰/머지는 한 줄씩.
// 메타 정보(경과/주기/갱신 #)는 노출하지 않는다.
func renderPollPanel(rc *ReleaseContext, pr *github.PullRequest, checks []github.CheckRun, reviews []github.Review) string {
	var b strings.Builder
	fmt.Fprintf(&b, "🔄 **PR #%d** 머지 추적 — `%s`\n\n",
		rc.PRNumber, rc.Module.Key+"-v"+rc.NewVersion.String())

	b.WriteString("**CI Pipeline**\n")
	b.WriteString(renderCIProgress(checks))
	b.WriteString("\n")

	b.WriteString("**리뷰**  ")
	b.WriteString(summarizeReviews(reviews))
	b.WriteString("\n")

	b.WriteString("**머지**  ")
	b.WriteString(summarizeMergeable(pr))
	return b.String()
}

// renderReadyToMerge 는 머지 가능(clean) 도달 시 폴링을 종료하며 노출하는 최종 메시지.
// CI/리뷰 모두 통과한 상태로 표시되고 사용자는 GitHub 에서 머지만 누르면 된다.
func renderReadyToMerge(rc *ReleaseContext, checks []github.CheckRun, reviews []github.Review) string {
	var b strings.Builder
	fmt.Fprintf(&b, "✅ **PR #%d** 모든 게이트 통과 — GitHub 에서 머지하세요\n", rc.PRNumber)
	fmt.Fprintf(&b, "릴리즈: `%s`\n\n", rc.NewTag)

	b.WriteString("**CI Pipeline**\n")
	b.WriteString(renderCIProgress(checks))
	b.WriteString("\n")

	b.WriteString("**리뷰**  ")
	b.WriteString(summarizeReviews(reviews))
	b.WriteString("\n")

	b.WriteString("\n폴링을 종료합니다. 머지 후 알림이 필요하면 GitHub 알림을 켜두세요.\n")
	return b.String()
}

// renderCIProgress 는 check-runs 를 progress bar + per-job 마커로 시각화한다.
// 출력 예시:
//
//	`██████████░░░░░░░░░░` 50% (3/6)
//	✓ ci pipeline / prepare
//	✓ ci pipeline / unit-tests
//	▶ ci pipeline / integration-tests
//	⋯ ci pipeline / rule-tests
const progressBarWidth = 20

func renderCIProgress(checks []github.CheckRun) string {
	if len(checks) == 0 {
		return "_(아직 등록된 체크 없음)_\n"
	}
	total := len(checks)
	completed := 0
	failed := 0
	for _, ch := range checks {
		if ch.Status == "completed" {
			completed++
			switch ch.Conclusion {
			case "failure", "timed_out", "action_required", "cancelled":
				failed++
			}
		}
	}
	pct := 0
	if total > 0 {
		pct = completed * 100 / total
	}
	filled := completed * progressBarWidth / total
	if filled > progressBarWidth {
		filled = progressBarWidth
	}
	bar := strings.Repeat("█", filled) + strings.Repeat("░", progressBarWidth-filled)

	var b strings.Builder
	prefix := "`" + bar + "`"
	if failed > 0 {
		fmt.Fprintf(&b, "%s ❌ %d/%d (실패 %d)\n", prefix, completed, total, failed)
	} else if completed == total {
		fmt.Fprintf(&b, "%s ✓ %d/%d (100%%)\n", prefix, completed, total)
	} else {
		fmt.Fprintf(&b, "%s ▶ %d/%d (%d%%)\n", prefix, completed, total, pct)
	}
	for _, ch := range checks {
		fmt.Fprintf(&b, "%s %s\n", checkMarker(ch), ch.Name)
	}
	return b.String()
}

// checkMarker 는 단일 check-run 의 상태 아이콘을 반환한다.
func checkMarker(ch github.CheckRun) string {
	if ch.Status == "completed" {
		switch ch.Conclusion {
		case "success":
			return "✓"
		case "failure", "timed_out", "action_required", "cancelled":
			return "✗"
		case "neutral", "skipped":
			return "—"
		default:
			return "·"
		}
	}
	if ch.Status == "in_progress" {
		return "▶"
	}
	return "⋯"
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
