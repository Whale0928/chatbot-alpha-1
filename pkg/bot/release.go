package bot

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"chatbot-alpha-1/pkg/github"
	"chatbot-alpha-1/pkg/llm/render"
	"chatbot-alpha-1/pkg/llm/summarize"
	"chatbot-alpha-1/pkg/release"

	"github.com/bwmarrin/discordgo"
)

// =====================================================================
// 릴리즈 흐름
// =====================================================================
//
// 메뉴 단계:
//   [릴리즈]                                 → 라인 선택
//   [백엔드/프론트엔드]                       → 모듈 선택 (BE: product/admin/batch)
//   [모듈]                                   → GetFile + ListTags → 버전 정보 + bump 버튼
//   [메이저/마이너/패치]                      → 확인 prompt (메이저는 danger 강조)
//   [확인]                                   → 진행 5단계 (UpdateFile/tag/branch/PR)
//                                              auto-merge 없음. 머지는 사람.
//
// 진행 5단계 메시지는 단일 progress 메시지를 in-place 편집하며 갱신.

const (
	// 첫 메뉴 [릴리즈]
	customIDReleaseEntry = "mode_release"

	// 라인 / 모듈 / bump custom_id prefix
	customIDReleaseLinePrefix   = "release_line:"   // be / fe
	customIDReleaseModulePrefix = "release_module:" // product / admin / batch
	customIDReleaseBumpPrefix   = "release_bump:"   // major / minor / patch
	customIDReleaseConfirm      = "release_confirm"
	customIDReleaseBackLine     = "release_back_line"   // 모듈 화면에서 라인 화면으로
	customIDReleaseBackModule   = "release_back_module" // 버전 화면에서 모듈 화면으로
)

// ReleaseContext는 진행 중인 릴리즈 흐름의 누적 상태.
// 라인/모듈/bump 선택 단계에서 채워지고, 진행 단계 + 폴링 단계에서도 참조된다.
type ReleaseContext struct {
	Owner string
	Repo  string

	Module           release.Module
	Bump             release.BumpType
	PrevTag          string
	PrevTagCommitSHA string // release/* 첫 생성 시 분기점
	PrevVersion      release.Version
	NewVersion       release.Version
	FileSHA          string // GetFile 결과 — UpdateFile 의 if-match
	CommitCount      int    // CompareCommits 결과

	// 진행 결과
	NewCommitSHA string
	NewTag       string
	PRNumber     int
	PRURL        string
	PRHeadSHA    string // CreatePullRequest 응답의 head sha — check-runs 조회 대상

	// 진행 메시지 ID — in-place 편집 대상
	ProgressMsgID string

	// 폴링 패널 메시지 ID 와 cancel 함수.
	// [폴링 중단] 클릭 시 PollCancel() 호출 → goroutine 종료.
	PollMsgID  string
	PollCancel context.CancelFunc

	// LastStep 은 runReleaseFlow 에서 마지막으로 시도한 step 번호 (1-based, 0=시작전).
	// 실패 시 renderReleaseProgress 에 -LastStep 으로 전달해 어디서 실패했는지 시각화.
	LastStep int
}

// =====================================================================
// [릴리즈] 첫 클릭 — 라인 선택 prompt
// =====================================================================

func handleReleaseEntry(s *discordgo.Session, i *discordgo.InteractionCreate, sess *Session) {
	if githubClient == nil {
		respondInteraction(s, i, "GITHUB_TOKEN 이 설정되어 있지 않아 릴리즈 흐름을 시작할 수 없습니다.")
		return
	}
	if llmClient == nil {
		respondInteraction(s, i, "LLM 클라이언트가 초기화되지 않았습니다.")
		return
	}
	sess.ReleaseCtx = &ReleaseContext{}
	respondInteractionWithComponents(s, i,
		"어떤 라인을 릴리즈할까요?",
		releaseLineComponents(),
	)
}

func releaseLineComponents() []discordgo.MessageComponent {
	return []discordgo.MessageComponent{
		discordgo.ActionsRow{Components: []discordgo.MessageComponent{
			discordgo.Button{Label: "백엔드", Style: discordgo.PrimaryButton, CustomID: customIDReleaseLinePrefix + "be"},
			discordgo.Button{Label: "프론트엔드", Style: discordgo.PrimaryButton, CustomID: customIDReleaseLinePrefix + "fe"},
			homeButton(),
		}},
	}
}

// =====================================================================
// 라인 선택 — 모듈 prompt
// =====================================================================

func handleReleaseLine(s *discordgo.Session, i *discordgo.InteractionCreate, sess *Session, lineTok string) {
	if sess.ReleaseCtx == nil {
		respondInteraction(s, i, "릴리즈 컨텍스트가 만료되었습니다. [처음 메뉴]에서 다시 시작해주세요.")
		return
	}
	var (
		line  release.Line
		label string
	)
	switch lineTok {
	case "be":
		line, label = release.LineBackend, "백엔드"
	case "fe":
		line, label = release.LineFrontend, "프론트엔드"
	default:
		respondInteraction(s, i, fmt.Sprintf("알 수 없는 라인: %q", lineTok))
		return
	}
	modules := release.ModulesByLine(line)
	if len(modules) == 0 {
		respondInteractionWithRow(s, i,
			fmt.Sprintf("%s 라인에 등록된 모듈이 없습니다.", label),
			discordgo.Button{Label: "← 라인 다시", Style: discordgo.SecondaryButton, CustomID: customIDReleaseEntry},
			homeButton(),
		)
		return
	}
	respondInteractionWithComponents(s, i,
		fmt.Sprintf("%s — 어느 모듈을 릴리즈할까요?", label),
		releaseModuleComponents(modules),
	)
}

// releaseModuleComponents는 모듈 버튼 + [라인 다시] 버튼 행을 만든다.
// 최대 5 버튼/row 제약에 맞춰 모듈 + 뒤로 가기 = 5개 이하 가정.
func releaseModuleComponents(modules []release.Module) []discordgo.MessageComponent {
	btns := make([]discordgo.MessageComponent, 0, len(modules)+2)
	for _, m := range modules {
		btns = append(btns, discordgo.Button{
			Label:    m.DisplayName,
			Style:    discordgo.PrimaryButton,
			CustomID: customIDReleaseModulePrefix + m.Key,
		})
	}
	btns = append(btns,
		discordgo.Button{Label: "← 뒤로", Style: discordgo.SecondaryButton, CustomID: customIDReleaseBackLine},
		homeButton(),
	)
	return []discordgo.MessageComponent{discordgo.ActionsRow{Components: btns}}
}

// =====================================================================
// 모듈 선택 — 버전 정보 prompt
// =====================================================================

func handleReleaseModule(s *discordgo.Session, i *discordgo.InteractionCreate, sess *Session, moduleKey string) {
	if sess.ReleaseCtx == nil {
		respondInteraction(s, i, "릴리즈 컨텍스트가 만료되었습니다. [처음 메뉴]에서 다시 시작해주세요.")
		return
	}
	module, ok := release.FindModule(moduleKey)
	if !ok {
		respondInteraction(s, i, fmt.Sprintf("알 수 없는 모듈: %q", moduleKey))
		return
	}
	sess.ReleaseCtx.Module = module
	sess.ReleaseCtx.Owner = module.Owner
	sess.ReleaseCtx.Repo = module.Repo

	// 사용자 응답이 3초 안에 와야 하므로 일단 defer로 ack
	if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
	}); err != nil {
		log.Printf("[릴리즈/module] ack 실패 thread=%s: %v", sess.ThreadID, err)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// VERSION 파일 + 직전 tag 조회
	fc, err := githubClient.GetFile(ctx, sess.ReleaseCtx.Owner, sess.ReleaseCtx.Repo, module.VersionPath, "main")
	if err != nil {
		followupErr(s, i, fmt.Sprintf("VERSION 파일 조회 실패: %v", err))
		return
	}
	curVer, err := release.ParseVersion(string(fc.Content))
	if err != nil {
		followupErr(s, i, fmt.Sprintf("VERSION 파싱 실패: %v", err))
		return
	}
	sess.ReleaseCtx.FileSHA = fc.SHA
	sess.ReleaseCtx.PrevVersion = curVer

	tags, err := githubClient.ListTags(ctx, sess.ReleaseCtx.Owner, sess.ReleaseCtx.Repo)
	if err != nil {
		followupErr(s, i, fmt.Sprintf("ListTags 실패: %v", err))
		return
	}
	names := make([]string, len(tags))
	for i, tg := range tags {
		names[i] = tg.Name
	}
	latest, found := release.ResolveLatestTag(names, module)
	prevTag := "(없음 — 첫 릴리즈)"
	prevTagSHA := ""
	if found {
		prevTag = latest.TagName
		for _, tg := range tags {
			if tg.Name == latest.TagName {
				prevTagSHA = tg.Commit.SHA
				break
			}
		}
	}
	sess.ReleaseCtx.PrevTag = prevTag
	sess.ReleaseCtx.PrevTagCommitSHA = prevTagSHA

	// 정보 카드 메시지
	body := fmt.Sprintf(
		"**%s** (`%s`) — 현재 버전을 확인하고 bump 타입을 선택해주세요.\n\n"+
			"• 현재 VERSION: `%s`\n"+
			"• 직전 tag: `%s`\n"+
			"• 비교 base ↔ head: `%s` ↔ `main`",
		module.DisplayName, module.Key, curVer, prevTag, prevTag)
	if !module.HasDeploy {
		body += "\n\n주의: 모듈 `" + module.Key + "` 는 HasDeploy=false (prod 자동배포 워크플로우 없음, 릴리즈 노트만 생성)."
	}

	if _, err := s.FollowupMessageCreate(i.Interaction, true, &discordgo.WebhookParams{
		Content:    body,
		Components: releaseBumpComponents(curVer),
	}); err != nil {
		log.Printf("[릴리즈/module] followup 실패 thread=%s: %v", sess.ThreadID, err)
	}
}

// releaseBumpComponents는 [메이저][마이너][패치][뒤로][처음] 버튼 행을 만든다.
// 라벨에 미리 새 버전을 박아 사용자가 클릭 전에 결과를 인지하도록 한다.
func releaseBumpComponents(cur release.Version) []discordgo.MessageComponent {
	major, _ := cur.Bump(release.BumpMajor)
	minor, _ := cur.Bump(release.BumpMinor)
	patch, _ := cur.Bump(release.BumpPatch)
	return []discordgo.MessageComponent{
		discordgo.ActionsRow{Components: []discordgo.MessageComponent{
			discordgo.Button{Label: fmt.Sprintf("메이저 (v%s)", major), Style: discordgo.DangerButton, CustomID: customIDReleaseBumpPrefix + "major"},
			discordgo.Button{Label: fmt.Sprintf("마이너 (v%s)", minor), Style: discordgo.PrimaryButton, CustomID: customIDReleaseBumpPrefix + "minor"},
			discordgo.Button{Label: fmt.Sprintf("패치 (v%s)", patch), Style: discordgo.SuccessButton, CustomID: customIDReleaseBumpPrefix + "patch"},
			discordgo.Button{Label: "← 뒤로", Style: discordgo.SecondaryButton, CustomID: customIDReleaseBackModule},
			homeButton(),
		}},
	}
}

// =====================================================================
// bump 선택 — 확인 prompt
// =====================================================================

func handleReleaseBump(s *discordgo.Session, i *discordgo.InteractionCreate, sess *Session, bumpTok string) {
	if sess.ReleaseCtx == nil || sess.ReleaseCtx.Module.Key == "" {
		respondInteraction(s, i, "릴리즈 컨텍스트가 만료되었습니다. [처음 메뉴]에서 다시 시작해주세요.")
		return
	}
	bump, ok := release.ParseBumpType(bumpTok)
	if !ok {
		respondInteraction(s, i, fmt.Sprintf("알 수 없는 bump: %q", bumpTok))
		return
	}
	newVer, err := sess.ReleaseCtx.PrevVersion.Bump(bump)
	if err != nil {
		respondInteraction(s, i, fmt.Sprintf("버전 계산 실패: %v", err))
		return
	}
	sess.ReleaseCtx.Bump = bump
	sess.ReleaseCtx.NewVersion = newVer
	sess.ReleaseCtx.NewTag = newVer.Tag(sess.ReleaseCtx.Module)

	body := fmt.Sprintf(
		"아래 작업을 진행합니다. 이상 없으면 [확인]을 눌러주세요.\n\n"+
			"• 모듈: `%s` (%s)\n"+
			"• 변경: `v%s` → `v%s` (%s)\n"+
			"• 비교 base: `%s` ↔ `main`\n"+
			"• 작업: VERSION + tag push → PR 생성 (LLM 본문) — auto-merge 없음, 머지는 직접",
		sess.ReleaseCtx.Module.Key, sess.ReleaseCtx.Module.DisplayName,
		sess.ReleaseCtx.PrevVersion, newVer, bump,
		sess.ReleaseCtx.PrevTag)
	confirmStyle := discordgo.SuccessButton
	confirmLabel := "확인"
	if bump == release.BumpMajor {
		body = "⚠️ **메이저 버전은 호환성 깨짐을 의미합니다.** 정말 진행할까요?\n\n" + body
		confirmStyle = discordgo.DangerButton
		confirmLabel = "메이저 진행"
	}
	respondInteractionWithRow(s, i, body,
		discordgo.Button{Label: confirmLabel, Style: confirmStyle, CustomID: customIDReleaseConfirm},
		discordgo.Button{Label: "← 다시 선택", Style: discordgo.SecondaryButton, CustomID: customIDReleaseBackModule},
		discordgo.Button{Label: "취소", Style: discordgo.SecondaryButton, CustomID: customIDHomeBtn},
	)
}

// =====================================================================
// [확인] — 진행 5단계 실행
// =====================================================================

func handleReleaseConfirm(s *discordgo.Session, i *discordgo.InteractionCreate, sess *Session) {
	rc := sess.ReleaseCtx
	if rc == nil || rc.Module.Key == "" || rc.Bump == release.BumpUnknown {
		respondInteraction(s, i, "릴리즈 컨텍스트가 만료되었습니다. [처음 메뉴]에서 다시 시작해주세요.")
		return
	}

	// ack — 진행 표시 메시지는 별도 channel send
	respondInteraction(s, i, fmt.Sprintf("`%s` 릴리즈를 진행합니다. (`v%s` → `v%s`)",
		rc.Module.Key, rc.PrevVersion, rc.NewVersion))

	// progress 메시지 초기 전송 — 이후 in-place edit
	msg, err := s.ChannelMessageSend(sess.ThreadID, renderReleaseProgress(rc, 0, ""))
	if err != nil {
		log.Printf("[릴리즈/progress] 초기 전송 실패 thread=%s: %v", sess.ThreadID, err)
		return
	}
	rc.ProgressMsgID = msg.ID

	go runReleaseFlow(s, sess, rc)
}

// runReleaseFlow는 진행 5단계를 비동기로 실행한다 (goroutine).
// 단계별로 ChannelMessageEdit 으로 progress 메시지를 갱신한다.
func runReleaseFlow(s *discordgo.Session, sess *Session, rc *ReleaseContext) {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	updateProgress := func(step int, note string) {
		rc.LastStep = step
		_, err := s.ChannelMessageEdit(sess.ThreadID, rc.ProgressMsgID, renderReleaseProgress(rc, step, note))
		if err != nil {
			log.Printf("[릴리즈/progress] edit 실패 step=%d: %v", step, err)
		}
	}

	// Step 1: CompareCommits (LLM 입력 준비)
	updateProgress(1, "직전 tag ↔ main diff 수집 중...")
	if rc.PrevTag == "(없음 — 첫 릴리즈)" {
		updateProgressError(s, sess, rc, "첫 릴리즈는 봇 흐름이 미지원입니다. CLI `release-bot --base-tag=...` 또는 수동 태그 생성 후 다시 시도해주세요.")
		return
	}
	cmp, err := githubClient.CompareCommits(ctx, rc.Owner, rc.Repo, rc.PrevTag, "main")
	if err != nil {
		updateProgressError(s, sess, rc, fmt.Sprintf("CompareCommits 실패: %v", err))
		return
	}
	rc.CommitCount = len(cmp.Commits)
	if rc.CommitCount == 0 {
		updateProgressError(s, sess, rc, "직전 tag ↔ main 사이 커밋이 0건입니다. 변경사항 없이는 릴리즈를 만들 수 없습니다.")
		return
	}

	// Step 2: LLM 노트 생성
	updateProgress(2, "LLM 으로 릴리즈 노트 본문 생성 중...")
	resp, err := summarize.Release(ctx, llmClient, summarize.ReleaseInput{
		ModuleKey:   rc.Module.Key,
		DisplayName: rc.Module.DisplayName,
		PrevTag:     rc.PrevTag,
		PrevVersion: rc.PrevVersion.String(),
		NewVersion:  rc.NewVersion.String(),
		BumpLabel:   rc.Bump.String(),
		Commits:     cmp.Commits,
		Files:       cmp.Files,
	})
	if err != nil {
		updateProgressError(s, sess, rc, fmt.Sprintf("summarize.Release 실패: %v", err))
		return
	}
	prBody := render.RenderReleasePRBody(render.ReleaseRenderInput{
		ModuleDisplayName: rc.Module.DisplayName,
		NewVersion:        rc.NewVersion.String(),
		PrevTag:           rc.PrevTag,
		NewTag:            rc.NewTag,
		CommitCount:       rc.CommitCount,
		BumpLabel:         rc.Bump.String(),
		Response:          resp,
	})

	// Step 3: VERSION 파일 갱신
	updateProgress(3, fmt.Sprintf("VERSION 파일 main 에 commit/push 중 (v%s → v%s)...", rc.PrevVersion, rc.NewVersion))
	upd, err := githubClient.UpdateFile(ctx, rc.Owner, rc.Repo, github.UpdateFileInput{
		Path:    rc.Module.VersionPath,
		Content: []byte(rc.NewVersion.String() + "\n"),
		SHA:     rc.FileSHA,
		Message: fmt.Sprintf("chore(%s): bump VERSION to %s", rc.Module.Key, rc.NewVersion),
		Branch:  "main",
	})
	if err != nil {
		updateProgressError(s, sess, rc, fmt.Sprintf("UpdateFile 실패: %v", err))
		return
	}
	rc.NewCommitSHA = upd.CommitSHA

	// Step 4: git tag 생성 + release/* 브랜치 보장
	updateProgress(4, fmt.Sprintf("git tag `%s` 생성 + release/* 브랜치 확인 중...", rc.NewTag))
	if _, err := githubClient.CreateRef(ctx, rc.Owner, rc.Repo, "refs/tags/"+rc.NewTag, rc.NewCommitSHA); err != nil {
		if !errors.Is(err, github.ErrAlreadyExists) {
			updateProgressError(s, sess, rc, fmt.Sprintf("CreateRef(tag) 실패: %v", err))
			return
		}
		log.Printf("[릴리즈] tag %s 이미 존재 — 진행 계속", rc.NewTag)
	}
	if _, err := githubClient.GetRef(ctx, rc.Owner, rc.Repo, "heads/"+rc.Module.ReleaseBranch); err != nil {
		if !errors.Is(err, github.ErrNotFound) {
			updateProgressError(s, sess, rc, fmt.Sprintf("GetRef(release branch) 실패: %v", err))
			return
		}
		branchSHA := rc.PrevTagCommitSHA
		if branchSHA == "" {
			r, gerr := githubClient.GetRef(ctx, rc.Owner, rc.Repo, "tags/"+rc.PrevTag)
			if gerr != nil {
				updateProgressError(s, sess, rc, fmt.Sprintf("base tag sha 조회 실패: %v", gerr))
				return
			}
			branchSHA = r.Object.SHA
		}
		if _, err := githubClient.CreateRef(ctx, rc.Owner, rc.Repo, "refs/heads/"+rc.Module.ReleaseBranch, branchSHA); err != nil {
			updateProgressError(s, sess, rc, fmt.Sprintf("CreateRef(release branch) 실패: %v", err))
			return
		}
	}

	// Step 5: PR 생성 (또는 기존 open PR 본문 갱신 — 멱등 처리)
	prTitle := fmt.Sprintf("[deploy] %s-v%s", rc.Module.Key, rc.NewVersion)
	updateProgress(5, fmt.Sprintf("PR 생성/갱신 (base=%s ← head=main)...", rc.Module.ReleaseBranch))
	existing, err := githubClient.ListPullRequestsByHead(ctx, rc.Owner, rc.Repo,
		rc.Owner+":main", rc.Module.ReleaseBranch, "open")
	if err != nil {
		updateProgressError(s, sess, rc, fmt.Sprintf("ListPullRequestsByHead 실패: %v", err))
		return
	}
	var pr *github.PullRequest
	if len(existing) > 0 {
		// 동일 head/base open PR 이 이미 있음 → 본문 갱신만.
		pr, err = githubClient.UpdatePullRequest(ctx, rc.Owner, rc.Repo, existing[0].Number, github.UpdatePullRequestInput{
			Title: prTitle,
			Body:  prBody,
		})
		if err != nil {
			updateProgressError(s, sess, rc, fmt.Sprintf("UpdatePullRequest #%d 실패: %v", existing[0].Number, err))
			return
		}
		log.Printf("[릴리즈] 기존 PR #%d 본문 갱신 (멱등) thread=%s", pr.Number, sess.ThreadID)
	} else {
		pr, err = githubClient.CreatePullRequest(ctx, rc.Owner, rc.Repo, github.CreatePullRequestInput{
			Title: prTitle,
			Body:  prBody,
			Head:  "main",
			Base:  rc.Module.ReleaseBranch,
		})
		if err != nil {
			updateProgressError(s, sess, rc, fmt.Sprintf("CreatePullRequest 실패: %v", err))
			return
		}
	}
	rc.PRNumber = pr.Number
	rc.PRURL = pr.HTMLURL
	rc.PRHeadSHA = pr.Head.SHA

	// 완료 — progress 메시지 최종 상태로 갱신 후 별도 결과 메시지 + [처음 메뉴]
	rc.LastStep = len(releaseProgressSteps)
	_, err = s.ChannelMessageEdit(sess.ThreadID, rc.ProgressMsgID, renderReleaseProgress(rc, len(releaseProgressSteps)+1, ""))
	if err != nil {
		log.Printf("[릴리즈/progress] 완료 edit 실패: %v", err)
	}
	sendReleaseResult(s, sess, rc, prBody)

	// 폴링 시작 — 별도 goroutine. context cancel 로 [폴링 중단] 처리.
	pollCtx, cancel := context.WithCancel(context.Background())
	rc.PollCancel = cancel
	go pollReleasePR(pollCtx, s, sess, rc)
}

// updateProgressError는 진행 도중 실패 시 progress 메시지에 실패 표시 + [처음 메뉴] 첨부.
// rc.LastStep 을 음수로 변환해 renderReleaseProgress 에 전달 — 어느 단계에서 막혔는지 시각화.
func updateProgressError(s *discordgo.Session, sess *Session, rc *ReleaseContext, errMsg string) {
	failedSignal := -rc.LastStep
	if rc.LastStep == 0 {
		failedSignal = -1 // 0 step 실패면 임의로 step 1 위치로 표시
	}
	body := renderReleaseProgress(rc, failedSignal, errMsg)
	if _, err := s.ChannelMessageEdit(sess.ThreadID, rc.ProgressMsgID, body); err != nil {
		log.Printf("[릴리즈/progress] error edit 실패: %v", err)
	}
	if _, err := s.ChannelMessageSendComplex(sess.ThreadID, &discordgo.MessageSend{
		Content: "실패 — 위 메시지에 사유 표시. 환경/권한 점검 후 다시 시도해주세요.",
		Components: []discordgo.MessageComponent{
			discordgo.ActionsRow{Components: []discordgo.MessageComponent{homeButton()}},
		},
	}); err != nil {
		log.Printf("[릴리즈/result] error notice 전송 실패: %v", err)
	}
	sess.ReleaseCtx = nil
}

// sendReleaseResult는 완료 후 PR 본문 미리보기 + [PR 열기][처음 메뉴] 안내.
// PR URL 은 plain text 가 아닌 LinkButton 으로 노출해 클릭 동선을 일관화.
func sendReleaseResult(s *discordgo.Session, sess *Session, rc *ReleaseContext, prBody string) {
	header := fmt.Sprintf("PR #%d 생성 완료 — `%s`\n머지는 GitHub 에서 직접 (auto-merge 사용 안 함).\n\n본문 미리보기는 아래 메시지.",
		rc.PRNumber, rc.NewTag)
	if _, err := s.ChannelMessageSendComplex(sess.ThreadID, &discordgo.MessageSend{
		Content:    header,
		Components: releaseDoneComponents(rc.PRURL),
	}); err != nil {
		log.Printf("[릴리즈/result] header 전송 실패: %v", err)
	}
	if err := sendLongMessage(s, sess.ThreadID, prBody); err != nil {
		log.Printf("[릴리즈/result] body 전송 실패: %v", err)
	}
	sess.LastBotSummary = prBody
}

// releaseDoneComponents는 PR 생성 완료 메시지에 첨부할 버튼 행을 만든다.
// [PR 열기] LinkButton + [처음 메뉴] 두 개. 외부 URL 은 항상 버튼으로 노출.
func releaseDoneComponents(prURL string) []discordgo.MessageComponent {
	row := []discordgo.MessageComponent{}
	if prURL != "" {
		row = append(row, discordgo.Button{
			Label: "PR 열기",
			Style: discordgo.LinkButton,
			URL:   prURL,
		})
	}
	row = append(row, homeButton())
	return []discordgo.MessageComponent{discordgo.ActionsRow{Components: row}}
}

// =====================================================================
// 뒤로 가기 핸들러
// =====================================================================

func handleReleaseBackLine(s *discordgo.Session, i *discordgo.InteractionCreate, sess *Session) {
	if sess.ReleaseCtx == nil {
		sess.ReleaseCtx = &ReleaseContext{}
	}
	respondInteractionWithComponents(s, i,
		"어떤 라인을 릴리즈할까요?",
		releaseLineComponents())
}

func handleReleaseBackModule(s *discordgo.Session, i *discordgo.InteractionCreate, sess *Session) {
	if sess.ReleaseCtx == nil {
		respondInteraction(s, i, "릴리즈 컨텍스트가 만료되었습니다. [처음 메뉴]에서 다시 시작해주세요.")
		return
	}
	modules := release.ModulesByLine(release.LineBackend)
	respondInteractionWithComponents(s, i,
		"백엔드 — 어느 모듈을 릴리즈할까요?",
		releaseModuleComponents(modules))
}

// =====================================================================
// progress 메시지 렌더
// =====================================================================

// releaseProgressSteps는 5단계 라벨.
// step 0: 시작 직전 / 1~5: 진행 중 / 6: 완료 / -1: 실패
var releaseProgressSteps = []string{
	"직전 tag ↔ main diff/커밋 수집",
	"LLM 으로 릴리즈 노트 본문 생성",
	"VERSION 파일 main 에 commit/push",
	"git tag 생성 + release/* 브랜치 확인",
	"PR 생성 (auto-merge 없음)",
}

// renderReleaseProgress는 단계별 진행 상태를 마크다운으로 그린다.
//
// current 의미:
//
//	0       : 시작 전 (모든 단계 ·)
//	1..N    : N 번째 단계 진행 중 (▶)
//	N+1     : 모든 단계 완료 (✓)
//	음수    : -current 번째 단계에서 실패 (그 이전 ✓, 그 단계 ✗, 이후 ·)
//
// 실패 시 호출자는 rc.LastStep 값을 음수로 변환해 전달한다.
func renderReleaseProgress(rc *ReleaseContext, current int, note string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "`릴리즈 진행 중` — **%s** v%s → v%s (%s)\n",
		rc.Module.DisplayName, rc.PrevVersion, rc.NewVersion, rc.Bump)
	fmt.Fprintf(&b, "비교: `%s` ↔ `main`\n\n", rc.PrevTag)

	failedStep := 0
	if current < 0 {
		failedStep = -current
	}

	for idx, label := range releaseProgressSteps {
		step := idx + 1
		var marker string
		switch {
		case failedStep > 0:
			if step < failedStep {
				marker = "✓"
			} else if step == failedStep {
				marker = "✗"
			} else {
				marker = "·"
			}
		case current == 0:
			marker = "·"
		case step < current:
			marker = "✓"
		case step == current:
			marker = "▶"
		default:
			marker = "·"
		}
		fmt.Fprintf(&b, "%s %d. %s\n", marker, step, label)
	}
	if note != "" {
		b.WriteString("\n")
		if failedStep > 0 {
			fmt.Fprintf(&b, "**실패 (step %d):** %s\n", failedStep, note)
		} else {
			fmt.Fprintf(&b, "→ %s\n", note)
		}
	}
	if current == len(releaseProgressSteps)+1 {
		b.WriteString("\n✅ 완료. PR 링크는 아래에 따로 안내됩니다.\n")
	}
	return b.String()
}

// followupErr는 deferred ack 이후 발생한 에러를 followup 메시지로 사용자에게 안내한다.
func followupErr(s *discordgo.Session, i *discordgo.InteractionCreate, msg string) {
	if _, err := s.FollowupMessageCreate(i.Interaction, true, &discordgo.WebhookParams{
		Content: msg,
		Components: []discordgo.MessageComponent{
			discordgo.ActionsRow{Components: []discordgo.MessageComponent{homeButton()}},
		},
	}); err != nil {
		log.Printf("[릴리즈/followup-err] 전송 실패: %v", err)
	}
}
