package bot

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"chatbot-alpha-1/pkg/db"
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
	customIDReleaseLinePrefix   = "release_line:"   // be / fe / all (B-3)
	customIDReleaseModulePrefix = "release_module:" // product / admin / batch / frontend / dashboard
	customIDReleaseBumpPrefix   = "release_bump:"   // major / minor / patch
	customIDReleaseConfirm      = "release_confirm"
	customIDReleaseBackLine     = "release_back_line"   // 모듈 화면에서 라인 화면으로
	customIDReleaseBackModule   = "release_back_module" // 버전 화면에서 모듈 화면으로

	// === B-3 batch release ===
	// StringSelectMenu prefix — customID는 batch_release_select:<module_key>
	// (예: batch_release_select:product). 사용자가 옵션 클릭 시 value(major/minor/patch)를 BatchReleaseCtx.Selections에 박제.
	customIDBatchReleaseSelectPrefix = "batch_release_select:"
	// [모두 진행] 버튼 — selection 0건 검증 후 4 goroutine 병렬 발사 (B-4).
	customIDBatchReleaseStart = "batch_release_start"
)

// firstReleaseLabel은 ListTags 결과에 module.TagPrefix 매칭 태그가 0개일 때 ReleaseContext.PrevTag에
// 박제되는 사용자 라벨. handleReleaseModule이 세팅하고 runReleaseFlow의 first-release fallback이
// PrevTagCommitSHA == "" 와 함께 감지 신호로 사용한다.
const firstReleaseLabel = "(없음 — 첫 릴리즈)"

// firstReleaseLookback은 첫 릴리즈일 때 main 커밋을 수집할 윈도우.
// CompareCommits를 쓸 base 태그가 없으므로 임의 윈도우로 lookback. 30일이 기본 (운영 합의).
const firstReleaseLookback = 30 * 24 * time.Hour

// batchReleaseMaxModules는 [전체] batch UI가 한 메시지에 노출 가능한 최대 모듈 수.
//
// 제약: Discord 메시지는 최대 5개 ActionsRow를 가질 수 있고, batchReleaseModuleComponents가
// 모듈마다 1 row(StringSelectMenu) + 마지막 [모두 진행] button row 1개를 사용 → 모듈 4개가 한도.
// release.Modules가 5개 이상으로 커지면 메시지 전송이 silent로 실패하거나 일부 button이 누락될 위험.
//
// 해당 한도를 초과하면 handleReleaseLine("all")이 명시 reject — 페이징 UI는 향후 필요 시 도입.
const batchReleaseMaxModules = 4

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

	// BaseSHA는 release/* 브랜치 생성 시 base로 사용할 git SHA.
	// 정상 케이스(태그 존재): PrevTagCommitSHA 그대로.
	// 첫 릴리즈(B-2): runReleaseSteps Step 1에서 GetRef("heads/main").Object.SHA를 미리 캡처.
	// Batch 모드(P1 isolation): 항상 main HEAD SHA로 캡처 (HeadBranch 생성 base + release 브랜치 first-release base).
	BaseSHA string

	// HeadBranch는 VERSION bump commit이 push될 head 브랜치 이름.
	// 단일 모드: "" (빈 값) → main에 직접 commit (기존 동작).
	// Batch 모드 (codex review 2차 P1 fix): "release-batch/<module-key>-v<new-version>" — 모듈마다 독립.
	//
	// 같은 repo의 여러 batch 모듈이 동일 main에 VERSION bump를 push하면 각 PR이 다른 모듈의 bump 커밋까지
	// 포함하게 되어 cross-contamination 발생 (모듈 A merge 시 모듈 B VERSION도 bump됨).
	// HeadBranch를 모듈마다 분리해 각 PR이 정확히 자기 bump 1건만 포함하도록 보장.
	HeadBranch string

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

	// InProgress는 runReleaseFlow가 실행 중인지 표시 (codex review P2/P3 + 3차 race fix).
	// runReleaseFlow는 background goroutine, 가드 read는 interaction handler goroutine — cross-goroutine
	// access이므로 atomic.Bool 사용 (평범한 bool은 -race 검출됨, 운영 중 가드 flaky 위험).
	// lifecycle: runReleaseFlow Store(true) → defer Store(false). 가드는 Load()로 in-flight 판정.
	InProgress atomic.Bool
}

// IsFirstRelease는 module.TagPrefix 매칭 태그가 0개였던 첫 릴리즈 시나리오인지 판단한다.
// handleReleaseModule이 ResolveLatestTag.found=false에서 PrevTagCommitSHA를 비워두는 게 canonical 신호.
//
// 사용처:
//   - runReleaseFlow Step 1: CompareCommits(base, main) 대신 ListCommits(since=now-30d) 사용
//   - runReleaseFlow Step 4: release/* 브랜치 base sha를 PrevTagCommitSHA가 아닌 main HEAD에서 분기
func (rc *ReleaseContext) IsFirstRelease() bool {
	return rc.PrevTagCommitSHA == ""
}

// =====================================================================
// B-3: batch release ([전체] 라인) 컨텍스트
// =====================================================================

// BatchReleaseContext는 [전체] 라인의 batch release 흐름 누적 상태.
//
// 단일 release(ReleaseContext)와 동시 진행 X — customIDSubActionRelease 가드와 [전체] 진입 가드가
// cross-check해서 한 시점에 하나만 활성. InProgress.Load() == true면 다른 진입 reject.
//
// 동시성 정책 (codex 3차 race fix):
//   - InProgress: atomic.Bool — interaction handler goroutine과 background goroutine이 cross-access.
//   - Selections: mu(sync.Mutex)로 보호 — discordgo가 InteractionCreate를 goroutine마다 dispatch하므로
//     서로 다른 사용자가 동시에 SelectMenu 클릭하면 평범한 map은 "concurrent map writes"로 panic. 운영 패닉 critical.
//     모든 read/write는 SetSelection / SnapshotSelections / HasAnySelection / SelectedCount accessor 경유.
//   - Modules: 한 번 set 후 read-only — 별도 보호 불필요.
//
// 흐름:
//   [전체] 클릭 → handleReleaseLine("all") → BatchReleaseCtx 초기화 + sendBatchReleasePrompt
//   사용자 모듈별 StringSelectMenu 선택 → handleBatchReleaseSelect → SetSelection(key, bump)
//   [모두 진행] 클릭 → handleBatchReleaseStart → SnapshotSelections + B-4 goroutine 병렬 발사
type BatchReleaseContext struct {
	Modules    []release.Module // 대상 모듈 — release.Modules slice 그대로 (4개). 한 번 set 후 read-only.
	InProgress atomic.Bool      // [모두 진행] 발사 후 true (race 방어 — 중복 클릭 reject).

	// mu는 selections 보호 — discordgo의 goroutine-per-interaction dispatch 모델에서 동시 SelectMenu 클릭이
	// 평범한 map을 concurrent write하면 Go runtime이 "fatal error: concurrent map writes"로 프로세스 자체를 panic.
	// 모든 selections 접근은 mu.Lock()/Unlock() 안에서만 (accessor methods 경유).
	mu         sync.Mutex
	selections map[string]release.BumpType // module.Key → bump (없는 키 = 사용자 미선택 = 진행 시 skip).
}

// SetSelection은 모듈 bump 선택을 박제한다 (mu 보호). 동시 호출 안전.
func (bc *BatchReleaseContext) SetSelection(moduleKey string, bump release.BumpType) {
	bc.mu.Lock()
	defer bc.mu.Unlock()
	if bc.selections == nil {
		bc.selections = map[string]release.BumpType{}
	}
	bc.selections[moduleKey] = bump
}

// SnapshotSelections는 selections의 immutable copy를 반환한다 — goroutine spawn 직전에 호출해
// 이후 사용자가 SelectMenu를 더 누르더라도 진행 중인 release flow가 영향받지 않게.
func (bc *BatchReleaseContext) SnapshotSelections() map[string]release.BumpType {
	bc.mu.Lock()
	defer bc.mu.Unlock()
	out := make(map[string]release.BumpType, len(bc.selections))
	for k, v := range bc.selections {
		out[k] = v
	}
	return out
}

// HasAnySelection은 사용자가 1개라도 모듈 bump를 선택했는지 검사한다 (mu 보호).
func (bc *BatchReleaseContext) HasAnySelection() bool {
	bc.mu.Lock()
	defer bc.mu.Unlock()
	for _, b := range bc.selections {
		if b != release.BumpUnknown {
			return true
		}
	}
	return false
}

// SelectedCount는 BumpUnknown이 아닌 선택 개수를 센다 (mu 보호).
func (bc *BatchReleaseContext) SelectedCount() int {
	bc.mu.Lock()
	defer bc.mu.Unlock()
	n := 0
	for _, b := range bc.selections {
		if b != release.BumpUnknown {
			n++
		}
	}
	return n
}

// GetSelection은 특정 모듈의 현재 선택을 반환 (mu 보호). 없으면 BumpUnknown.
// batchReleaseModuleComponents가 default 옵션 표시할 때 사용.
func (bc *BatchReleaseContext) GetSelection(moduleKey string) release.BumpType {
	bc.mu.Lock()
	defer bc.mu.Unlock()
	return bc.selections[moduleKey]
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

// D1 정책: [처음 메뉴] button 폐기 — 흐름 중단은 super-session sticky로.
// B-3: [전체] 추가 — 등록된 모든 모듈을 한 번에 batch release 발사.
func releaseLineComponents() []discordgo.MessageComponent {
	return []discordgo.MessageComponent{
		discordgo.ActionsRow{Components: []discordgo.MessageComponent{
			discordgo.Button{Label: "백엔드", Style: discordgo.PrimaryButton, CustomID: customIDReleaseLinePrefix + "be"},
			discordgo.Button{Label: "프론트엔드", Style: discordgo.PrimaryButton, CustomID: customIDReleaseLinePrefix + "fe"},
			discordgo.Button{Label: "전체", Style: discordgo.SuccessButton, CustomID: customIDReleaseLinePrefix + "all"},
		}},
	}
}

// =====================================================================
// 라인 선택 — 모듈 prompt
// =====================================================================

func handleReleaseLine(s *discordgo.Session, i *discordgo.InteractionCreate, sess *Session, lineTok string) {
	if sess.ReleaseCtx == nil {
		respondInteraction(s, i, "릴리즈 컨텍스트가 만료되었습니다. sticky의 [릴리즈 PR 만들기] button으로 다시 시작해주세요.")
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
	case "all":
		// B-3: [전체] — 단일 release ctx 정리, batch 흐름 진입.
		// codex review P2 fix: 가드는 rc.InProgress 사용 — handleReleaseEntry가 라인 선택 직전에
		// 항상 빈 ReleaseCtx{}를 만들기 때문에 PRNumber==0 가드는 정상 [전체] 진입도 reject.
		// rc.InProgress는 runReleaseFlow가 실제 goroutine 실행 중일 때만 true. 3차 race fix: atomic.Bool.
		if sess.ReleaseCtx != nil && sess.ReleaseCtx.InProgress.Load() {
			logGuard("release/batch", "single_in_progress", "단일 release 진행 중 — [전체] 진입 reject",
				lf("thread", sess.ThreadID), lf("user", interactionCallerUsername(i)),
				lf("module", sess.ReleaseCtx.Module.Key))
			respondInteraction(s, i, "현재 진행 중인 단일 릴리즈가 있습니다. 완료 후 [전체]를 다시 시도해주세요.")
			return
		}
		if sess.BatchReleaseCtx != nil && sess.BatchReleaseCtx.InProgress.Load() {
			logGuard("release/batch", "batch_in_progress", "batch release 진행 중 — 새 [전체] 진입 reject",
				lf("thread", sess.ThreadID), lf("user", interactionCallerUsername(i)),
				lf("selected", sess.BatchReleaseCtx.SelectedCount()))
			respondInteraction(s, i, "이미 batch release가 진행 중입니다.")
			return
		}
		sess.ReleaseCtx = nil
		modules := release.Modules
		if len(modules) == 0 {
			logGuard("release/batch", "no_modules", "release.Modules 비어있음 — [전체] 진입 불가",
				lf("thread", sess.ThreadID))
			respondInteraction(s, i, "등록된 release 모듈이 없습니다 (pkg/release/types.go의 Modules 확인).")
			return
		}
		// codex review 2차 (Copilot 2차 피드백): batch UI는 Discord row max 5 제약상 최대 batchReleaseMaxModules(4)
		// 모듈만 노출 가능. 그 이상이면 silent 메시지 전송 실패/button 누락 위험 — 명시 reject.
		if len(modules) > batchReleaseMaxModules {
			logGuard("release/batch", "too_many_modules",
				fmt.Sprintf("modules=%d > 한도 %d — Discord row 5 제약 위반 위험", len(modules), batchReleaseMaxModules),
				lf("thread", sess.ThreadID), lf("modules", len(modules)), lf("max", batchReleaseMaxModules))
			respondInteraction(s, i, fmt.Sprintf(
				"등록 모듈 %d개가 [전체] UI 한도(%d개)를 초과합니다. Discord 메시지의 ActionsRow 5개 제약 때문에 한 화면에 노출 불가 — 단일 release를 모듈마다 진행하거나 release.Modules에서 일부 모듈을 제외해주세요.",
				len(modules), batchReleaseMaxModules))
			return
		}
		// selections는 SetSelection 호출 시 lazy 초기화 — 빈 map 미리 set 불필요.
		sess.BatchReleaseCtx = &BatchReleaseContext{
			Modules: modules,
		}
		logEvent("release/batch", "start", "[전체] 진입 — 모듈 선택 prompt 발사",
			lf("thread", sess.ThreadID), lf("user", interactionCallerUsername(i)),
			lf("modules", len(modules)))
		sendBatchReleasePrompt(s, i, sess)
		return
	default:
		respondInteraction(s, i, fmt.Sprintf("알 수 없는 라인: %q", lineTok))
		return
	}
	modules := release.ModulesByLine(line)
	if len(modules) == 0 {
		respondInteractionWithRow(s, i,
			fmt.Sprintf("%s 라인에 등록된 모듈이 없습니다.", label),
			discordgo.Button{Label: "← 라인 다시", Style: discordgo.SecondaryButton, CustomID: customIDReleaseEntry},
		)
		return
	}
	respondInteractionWithComponents(s, i,
		fmt.Sprintf("%s — 어느 모듈을 릴리즈할까요?", label),
		releaseModuleComponents(modules),
	)
}

// releaseModuleComponents는 모듈 버튼 + [← 뒤로] 버튼 행을 만든다.
// 최대 5 버튼/row 제약에 맞춰 모듈 + 뒤로 가기 = 5개 이하 가정.
//
// D1 정책: [처음 메뉴] button 폐기 — 흐름 중단은 super-session sticky로.
func releaseModuleComponents(modules []release.Module) []discordgo.MessageComponent {
	btns := make([]discordgo.MessageComponent, 0, len(modules)+1)
	for _, m := range modules {
		btns = append(btns, discordgo.Button{
			Label:    m.DisplayName,
			Style:    discordgo.PrimaryButton,
			CustomID: customIDReleaseModulePrefix + m.Key,
		})
	}
	btns = append(btns,
		discordgo.Button{Label: "← 뒤로", Style: discordgo.SecondaryButton, CustomID: customIDReleaseBackLine},
	)
	return []discordgo.MessageComponent{discordgo.ActionsRow{Components: btns}}
}

// =====================================================================
// 모듈 선택 — 버전 정보 prompt
// =====================================================================

func handleReleaseModule(s *discordgo.Session, i *discordgo.InteractionCreate, sess *Session, moduleKey string) {
	if sess.ReleaseCtx == nil {
		respondInteraction(s, i, "릴리즈 컨텍스트가 만료되었습니다. sticky의 [릴리즈 PR 만들기] button으로 다시 시작해주세요.")
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
	prevTag := firstReleaseLabel
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

	// 정보 카드 메시지 — 첫 릴리즈는 base가 없으므로 비교 라인을 다르게 표기.
	var body string
	if sess.ReleaseCtx.IsFirstRelease() {
		body = fmt.Sprintf(
			"**%s** (`%s`) — **첫 릴리즈** 입니다. 현재 버전을 확인하고 bump 타입을 선택해주세요.\n\n"+
				"• 현재 VERSION: `%s`\n"+
				"• 직전 tag: 없음 (B-2 fallback)\n"+
				"• 분석 범위: 지난 %d일 main 커밋 (CompareCommits 대신 ListCommits)\n"+
				"• release/* 브랜치 base: main HEAD",
			module.DisplayName, module.Key, curVer, int(firstReleaseLookback/(24*time.Hour)))
	} else {
		body = fmt.Sprintf(
			"**%s** (`%s`) — 현재 버전을 확인하고 bump 타입을 선택해주세요.\n\n"+
				"• 현재 VERSION: `%s`\n"+
				"• 직전 tag: `%s`\n"+
				"• 비교 base ↔ head: `%s` ↔ `main`",
			module.DisplayName, module.Key, curVer, prevTag, prevTag)
	}
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

// releaseBumpComponents는 [메이저][마이너][패치][뒤로] 버튼 행을 만든다.
// 라벨에 미리 새 버전을 박아 사용자가 클릭 전에 결과를 인지하도록 한다.
//
// D1 정책: [처음 메뉴] button 폐기 — 흐름 중단은 super-session sticky로.
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
		}},
	}
}

// =====================================================================
// bump 선택 — 확인 prompt
// =====================================================================

func handleReleaseBump(s *discordgo.Session, i *discordgo.InteractionCreate, sess *Session, bumpTok string) {
	if sess.ReleaseCtx == nil || sess.ReleaseCtx.Module.Key == "" {
		respondInteraction(s, i, "릴리즈 컨텍스트가 만료되었습니다. sticky의 [릴리즈 PR 만들기] button으로 다시 시작해주세요.")
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

	confirmStyle := discordgo.SuccessButton
	confirmLabel := "확인"
	if bump == release.BumpMajor {
		confirmStyle = discordgo.DangerButton
		confirmLabel = "메이저 진행"
	}
	embed := renderReleaseConfirmEmbed(sess.ReleaseCtx)
	// D1 정책: [취소]=customIDHomeBtn 폐기 — 진행을 원치 않으면 [확인]을 누르지 않고 흐름 종료.
	s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Embeds: []*discordgo.MessageEmbed{embed},
			Components: []discordgo.MessageComponent{
				discordgo.ActionsRow{Components: []discordgo.MessageComponent{
					discordgo.Button{Label: confirmLabel, Style: confirmStyle, CustomID: customIDReleaseConfirm},
					discordgo.Button{Label: "← 다시 선택", Style: discordgo.SecondaryButton, CustomID: customIDReleaseBackModule},
				}},
			},
		},
	})
}

// =====================================================================
// [확인] — 진행 5단계 실행
// =====================================================================

func handleReleaseConfirm(s *discordgo.Session, i *discordgo.InteractionCreate, sess *Session) {
	rc := sess.ReleaseCtx
	if rc == nil || rc.Module.Key == "" || rc.Bump == release.BumpUnknown {
		respondInteraction(s, i, "릴리즈 컨텍스트가 만료되었습니다. sticky의 [릴리즈 PR 만들기] button으로 다시 시작해주세요.")
		return
	}

	// ack — 진행 표시 메시지는 별도 channel send
	respondInteraction(s, i, fmt.Sprintf("`%s` 릴리즈를 진행합니다. (`v%s` → `v%s`)",
		rc.Module.Key, rc.PrevVersion, rc.NewVersion))

	// progress 메시지 초기 전송 — 이후 in-place edit
	msg, err := s.ChannelMessageSendComplex(sess.ThreadID, &discordgo.MessageSend{
		Embeds: []*discordgo.MessageEmbed{renderReleaseProgress(rc, 0, "")},
	})
	if err != nil {
		log.Printf("[릴리즈/progress] 초기 전송 실패 thread=%s: %v", sess.ThreadID, err)
		return
	}
	rc.ProgressMsgID = msg.ID

	go runReleaseFlow(s, sess, rc)
}

// ReleaseStatusFn은 runReleaseSteps 의 진행 상황 콜백. step은 1-5(진행 중) / 6(완료) / -N(실패 step N).
// 단일 모드는 progress 메시지 in-place edit, 배치 모드는 공유 status table 갱신에 사용.
type ReleaseStatusFn func(step int, note string)

// runReleaseSteps 는 PR 생성 5단계 (커밋 수집 → LLM → VERSION 파일 → tag/branch → PR)를 실행한다.
// 단일 release / batch release 양쪽에서 공유 호출. UI/IO는 statusFn 콜백으로 caller에 위임.
//
// 사전 조건 (caller가 채워둬야 하는 rc 필드):
//   - Module / Owner / Repo (handleReleaseModule 또는 setupReleaseContextForBatch)
//   - PrevTag / PrevTagCommitSHA / FileSHA / PrevVersion (위와 동일)
//   - Bump / NewVersion / NewTag (handleReleaseBump 또는 setupReleaseContextForBatch)
//
// 함수가 채우는 필드: LastStep, CommitCount, NewCommitSHA, PRNumber, PRURL, PRHeadSHA.
//
// 에러 발생 시점 직전 statusFn(step, note)이 호출된 상태이며, 반환된 err는 사용자 노출용 메시지를 포함한다.
// caller는 rc.LastStep으로 어느 step에서 실패했는지 알 수 있다.
func runReleaseSteps(ctx context.Context, rc *ReleaseContext, statusFn ReleaseStatusFn) (string, error) {
	// Step 1: 커밋/파일 diff 수집 (LLM 입력 준비) + base SHA 캡처
	//
	// B-2 first-release fallback: rc.IsFirstRelease() 일 때는 base 태그가 없어 CompareCommits 사용
	// 불가. 대신 ListCommits(since=now-firstReleaseLookback)로 main 최근 윈도우의 커밋을 모은다.
	// 파일 diff는 첫 릴리즈에서 수집할 base가 없으므로 nil로 두고 LLM에 노출 안함 (커밋 기반 노트만).
	//
	// codex review P1 fix: 첫 릴리즈에서 release 브랜치 base는 main HEAD인데 이를 Step 4에서 읽으면
	// Step 3의 UpdateFile이 이미 VERSION bump 커밋을 main에 push해놓아서 base/head가 같은 SHA가 됨
	// → CreatePullRequest "no commits between" 실패. 따라서 BaseSHA는 Step 1 (UpdateFile 이전)에서 캡처.
	rc.LastStep = 1
	var (
		commits []github.Commit
		files   []github.ComparisonFile
	)
	// BaseSHA 정책 (실제 동작):
	//   - 첫 릴리즈(IsFirstRelease): main HEAD를 BaseSHA로 캡처 (release/* 브랜치 base 용도)
	//   - 그 외: PrevTagCommitSHA를 BaseSHA로 사용 (직전 tag commit)
	//
	// Batch 모드(rc.HeadBranch != "")의 head 브랜치 base는 BaseSHA가 아닌 별도 batchHeadBaseSHA로 항상 main HEAD에서 캡처
	// (아래 별도 분기). 같은 repo의 다른 batch 모듈이 main을 forward하기 전에 snapshot을 잡아 isolated head 브랜치 base로 사용.
	// 즉 BaseSHA = release branch base, batchHeadBaseSHA = head branch base — 의미 다름.
	if rc.IsFirstRelease() {
		windowDays := int(firstReleaseLookback / (24 * time.Hour))
		statusFn(1, fmt.Sprintf("첫 릴리즈 — 지난 %d일 main 커밋 수집 중...", windowDays))
		mainRef, err := githubClient.GetRef(ctx, rc.Owner, rc.Repo, "heads/main")
		if err != nil {
			return "", fmt.Errorf("첫 릴리즈 — main HEAD 조회 실패: %w", err)
		}
		rc.BaseSHA = mainRef.Object.SHA
		log.Printf("[릴리즈] 첫 릴리즈 — base SHA 캡처 (UpdateFile 이전 main HEAD): %s", rc.BaseSHA)

		since := time.Now().Add(-firstReleaseLookback)
		cmts, err := githubClient.ListCommits(ctx, rc.Owner, rc.Repo, github.ListCommitsOptions{
			Since:  since,
			Branch: "main",
		})
		if err != nil {
			return "", fmt.Errorf("ListCommits(첫 릴리즈) 실패: %w", err)
		}
		commits = cmts
	} else {
		statusFn(1, "직전 tag ↔ main diff 수집 중...")
		rc.BaseSHA = rc.PrevTagCommitSHA
		cmp, err := githubClient.CompareCommits(ctx, rc.Owner, rc.Repo, rc.PrevTag, "main")
		if err != nil {
			return "", fmt.Errorf("CompareCommits 실패: %w", err)
		}
		commits = cmp.Commits
		files = cmp.Files
	}
	// Batch 모드 isolation 추가 캡처 — 위에서 BaseSHA가 PrevTagCommitSHA로 세팅됐을 수 있는데,
	// HeadBranch base는 main HEAD여야 함 (UpdateFile이 head 브랜치에 commit하면 다른 모듈 bump가 섞이지 않음).
	// 이 경우 별도 batchHeadBaseSHA 변수에 main HEAD를 캡처해 head 브랜치 생성에만 사용.
	var batchHeadBaseSHA string
	if rc.HeadBranch != "" {
		mainRef, err := githubClient.GetRef(ctx, rc.Owner, rc.Repo, "heads/main")
		if err != nil {
			return "", fmt.Errorf("batch — main HEAD 조회 실패 (head 브랜치 base용): %w", err)
		}
		batchHeadBaseSHA = mainRef.Object.SHA
		log.Printf("[릴리즈/batch] head 브랜치 base SHA 캡처 module=%s sha=%s", rc.Module.Key, batchHeadBaseSHA)
	}
	rc.CommitCount = len(commits)
	if rc.CommitCount == 0 {
		if rc.IsFirstRelease() {
			windowDays := int(firstReleaseLookback / (24 * time.Hour))
			return "", fmt.Errorf("첫 릴리즈 윈도우(지난 %d일) 안 main 커밋이 0건입니다", windowDays)
		}
		return "", fmt.Errorf("직전 tag ↔ main 사이 커밋이 0건입니다")
	}

	// Step 2: LLM 노트 생성
	rc.LastStep = 2
	statusFn(2, "LLM 으로 릴리즈 노트 본문 생성 중...")
	resp, err := summarize.Release(ctx, llmClient, summarize.ReleaseInput{
		ModuleKey:   rc.Module.Key,
		DisplayName: rc.Module.DisplayName,
		PrevTag:     rc.PrevTag, // 첫 릴리즈일 때 firstReleaseLabel — LLM에 "첫 릴리즈" 컨텍스트로 작용
		PrevVersion: rc.PrevVersion.String(),
		NewVersion:  rc.NewVersion.String(),
		BumpLabel:   rc.Bump.String(),
		Commits:     commits,
		Files:       files, // 첫 릴리즈는 nil
	})
	if err != nil {
		return "", fmt.Errorf("summarize.Release 실패: %w", err)
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
	rc.LastStep = 3
	// codex review 2차 P1 isolation: batch 모드는 main 직접 commit 대신 모듈별 head 브랜치(rc.HeadBranch)에 commit.
	// head 브랜치는 main HEAD snapshot(batchHeadBaseSHA)에서 분기 — 다른 batch 모듈의 bump가 섞이지 않음.
	// 단일 모드는 rc.HeadBranch == "" → 기존 동작(main에 직접 commit) 유지.
	commitBranch := "main"
	if rc.HeadBranch != "" {
		commitBranch = rc.HeadBranch
		// head 브랜치 ensure — 없으면 main snapshot SHA에서 생성, 있으면 재사용 (idempotent).
		if _, err := githubClient.GetRef(ctx, rc.Owner, rc.Repo, "heads/"+rc.HeadBranch); err != nil {
			if !errors.Is(err, github.ErrNotFound) {
				return "", fmt.Errorf("GetRef(batch head branch) 실패: %w", err)
			}
			if _, err := githubClient.CreateRef(ctx, rc.Owner, rc.Repo, "refs/heads/"+rc.HeadBranch, batchHeadBaseSHA); err != nil {
				return "", fmt.Errorf("CreateRef(batch head branch) 실패: %w", err)
			}
			log.Printf("[릴리즈/batch] head 브랜치 생성 module=%s branch=%s base=%s",
				rc.Module.Key, rc.HeadBranch, batchHeadBaseSHA)
		}
		// rc.FileSHA는 handleReleaseModule/setupReleaseContextForBatch가 main에서 GetFile해 가져온 값.
		// batch head 브랜치는 방금 main HEAD snapshot에서 갈라졌으므로 같은 VERSION 파일 SHA — 그대로 if-match 가능.
	}
	statusFn(3, fmt.Sprintf("VERSION 파일 %s 에 commit/push 중 (v%s → v%s)...", commitBranch, rc.PrevVersion, rc.NewVersion))
	upd, err := githubClient.UpdateFile(ctx, rc.Owner, rc.Repo, github.UpdateFileInput{
		Path:    rc.Module.VersionPath,
		Content: []byte(rc.NewVersion.String() + "\n"),
		SHA:     rc.FileSHA,
		Message: fmt.Sprintf("chore(%s): bump VERSION to %s", rc.Module.Key, rc.NewVersion),
		Branch:  commitBranch,
	})
	if err != nil {
		return "", fmt.Errorf("UpdateFile 실패: %w", err)
	}
	rc.NewCommitSHA = upd.CommitSHA

	// Step 4: git tag 생성 + release/* 브랜치 보장
	rc.LastStep = 4
	statusFn(4, fmt.Sprintf("git tag `%s` 생성 + release/* 브랜치 확인 중...", rc.NewTag))
	if _, err := githubClient.CreateRef(ctx, rc.Owner, rc.Repo, "refs/tags/"+rc.NewTag, rc.NewCommitSHA); err != nil {
		if !errors.Is(err, github.ErrAlreadyExists) {
			return "", fmt.Errorf("CreateRef(tag) 실패: %w", err)
		}
		log.Printf("[릴리즈] tag %s 이미 존재 — 진행 계속", rc.NewTag)
	}
	if _, err := githubClient.GetRef(ctx, rc.Owner, rc.Repo, "heads/"+rc.Module.ReleaseBranch); err != nil {
		if !errors.Is(err, github.ErrNotFound) {
			return "", fmt.Errorf("GetRef(release branch) 실패: %w", err)
		}
		// release/* 브랜치 base SHA: Step 1에서 미리 캡처한 rc.BaseSHA 사용 (codex review P1 fix).
		// 정상 케이스는 PrevTagCommitSHA, 첫 릴리즈는 Step 3 UpdateFile 이전 main HEAD.
		// rc.BaseSHA가 비어있는 비정상 케이스 (정상 흐름에서는 발생 X)는 tags/<PrevTag> 방어 fallback.
		branchSHA := rc.BaseSHA
		if branchSHA == "" {
			r, gerr := githubClient.GetRef(ctx, rc.Owner, rc.Repo, "tags/"+rc.PrevTag)
			if gerr != nil {
				return "", fmt.Errorf("base tag sha 조회 실패: %w", gerr)
			}
			branchSHA = r.Object.SHA
		}
		if _, err := githubClient.CreateRef(ctx, rc.Owner, rc.Repo, "refs/heads/"+rc.Module.ReleaseBranch, branchSHA); err != nil {
			return "", fmt.Errorf("CreateRef(release branch) 실패: %w", err)
		}
	}

	// Step 5: PR 생성 (또는 기존 open PR 본문 갱신 — 멱등 처리)
	rc.LastStep = 5
	// codex review 2차 P1 isolation: PR head는 batch 모드면 모듈별 head 브랜치, 그 외 main.
	prHead := "main"
	if rc.HeadBranch != "" {
		prHead = rc.HeadBranch
	}
	prTitle := fmt.Sprintf("[deploy] %s-v%s", rc.Module.Key, rc.NewVersion)
	statusFn(5, fmt.Sprintf("PR 생성/갱신 (base=%s ← head=%s)...", rc.Module.ReleaseBranch, prHead))
	existing, err := githubClient.ListPullRequestsByHead(ctx, rc.Owner, rc.Repo,
		rc.Owner+":"+prHead, rc.Module.ReleaseBranch, "open")
	if err != nil {
		return "", fmt.Errorf("ListPullRequestsByHead 실패: %w", err)
	}
	var pr *github.PullRequest
	if len(existing) > 0 {
		pr, err = githubClient.UpdatePullRequest(ctx, rc.Owner, rc.Repo, existing[0].Number, github.UpdatePullRequestInput{
			Title: prTitle,
			Body:  prBody,
		})
		if err != nil {
			return "", fmt.Errorf("UpdatePullRequest #%d 실패: %w", existing[0].Number, err)
		}
		log.Printf("[릴리즈] 기존 PR #%d 본문 갱신 (멱등)", pr.Number)
	} else {
		pr, err = githubClient.CreatePullRequest(ctx, rc.Owner, rc.Repo, github.CreatePullRequestInput{
			Title: prTitle,
			Body:  prBody,
			Head:  prHead,
			Base:  rc.Module.ReleaseBranch,
		})
		if err != nil {
			return "", fmt.Errorf("CreatePullRequest 실패: %w", err)
		}
	}
	rc.PRNumber = pr.Number
	rc.PRURL = pr.HTMLURL
	rc.PRHeadSHA = pr.Head.SHA

	// 완료 — caller가 LastStep을 step 6으로 박제하고 UI 마무리.
	rc.LastStep = len(releaseProgressSteps)
	statusFn(len(releaseProgressSteps)+1, "")
	return prBody, nil
}

// runReleaseFlow는 단일 모듈 release를 비동기로 실행한다 (goroutine).
// runReleaseSteps + 단일 모드 progress UI/sendResult/sticky/polling을 wrap한다.
//
// codex review P2/P3 fix: rc.InProgress lifecycle을 명확히 마킹 — sticky [릴리즈 PR 만들기] 가드가
// "in-flight vs abandoned" 구분 가능. 시작 시 true, defer로 false (성공/실패 모두).
// 3차 race fix: atomic.Bool — interaction handler가 동시에 .Load()로 가드 체크하기 때문.
func runReleaseFlow(s *discordgo.Session, sess *Session, rc *ReleaseContext) {
	rc.InProgress.Store(true)
	defer func() {
		rc.InProgress.Store(false)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	// === Phase 3 chunk 3B-2c — super-session in-thread 통합 ===
	// weekly/agent와 동일 패턴. ModeMeeting일 때만 SubAction lifecycle.
	// Context 분리 (begin/end/append 각 5s 독립) — runReleaseFlow는 180s timeout이라
	// 단일 ctx 공유 시 defer 시점에 cancelled 위험.
	var (
		sa             *SubActionContext
		releaseSummary string // PR 생성 성공 시 채워짐 — defer/AppendResult capture
	)
	if sess.Mode == ModeMeeting {
		beginCtx, beginCancel := context.WithTimeout(context.Background(), 5*time.Second)
		sa = BeginSubAction(beginCtx, sess, db.SegmentRelease)
		beginCancel()
		defer func() {
			endCtx, endCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer endCancel()
			sa.EndWithArtifact(endCtx, map[string]any{
				"module":      rc.Module.Key,
				"prev_tag":    rc.PrevTag,
				"new_version": rc.NewVersion.String(),
				"bump":        rc.Bump.String(),
				"pr_number":   rc.PRNumber, // 0이면 PR 생성 전 종료
				"pr_url":      rc.PRURL,
				"summary":     len(releaseSummary), // 0이면 에러 종료
			})
		}()
	}

	// statusFn — 단일 모드는 progress 메시지 in-place edit. step in [1, 5] 진행 중,
	// runReleaseSteps가 마지막에 step=len(releaseProgressSteps)+1로 호출 = 완료 마킹.
	statusFn := func(step int, note string) {
		_, err := s.ChannelMessageEditComplex(&discordgo.MessageEdit{
			Channel: sess.ThreadID,
			ID:      rc.ProgressMsgID,
			Content: ptrString(""),
			Embeds:  ptrEmbeds(renderReleaseProgress(rc, step, note)),
		})
		if err != nil {
			log.Printf("[릴리즈/progress] edit 실패 step=%d: %v", step, err)
		}
	}

	prBody, err := runReleaseSteps(ctx, rc, statusFn)
	if err != nil {
		updateProgressError(s, sess, rc, err.Error())
		return
	}

	sendReleaseResult(s, sess, rc, prBody)

	// === super-session corpus 누적 ===
	// PR 본문(markdown) + URL/Number를 NoteSource=ReleaseResult로 sess에 추가.
	// finalize 시 ContextNotes로 분류되어 LLM에 참고 자료로 전달, attribution 후보 X.
	releaseSummary = fmt.Sprintf("[release] %s %s → PR #%d (%s)\n%s",
		rc.Module.DisplayName, rc.NewVersion.String(), rc.PRNumber, rc.PRURL, prBody)
	if sa != nil {
		appendCtx, appendCancel := context.WithTimeout(context.Background(), 5*time.Second)
		sa.AppendResult(appendCtx, sess, "[release]", db.SourceReleaseResult, releaseSummary)
		appendCancel()
	}

	// A-8 (D3): 결과 메시지 전송 직후 sticky 즉시 재발사 — 사용자가 다음 sub-action button을 화면
	// 하단에서 즉시 클릭 가능. polling 패널은 별도 goroutine에서 갱신되므로 sticky가 그 위로 밀려나도
	// 핵심 컨트롤은 sticky 재발사로 회복.
	if sess.Mode == ModeMeeting {
		sendSticky(s, sess)
	}

	// 폴링 시작 — 별도 goroutine. context cancel 로 [폴링 중단] 처리.
	pollCtx, cancel := context.WithCancel(context.Background())
	rc.PollCancel = cancel
	go pollReleasePR(pollCtx, s, sess, rc)
}

// updateProgressError는 진행 도중 실패 시 progress 메시지에 실패 표시.
// rc.LastStep 을 음수로 변환해 renderReleaseProgress 에 전달 — 어느 단계에서 막혔는지 시각화.
//
// D1 정책: [처음 메뉴] button 폐기 — 후속 작업은 super-session sticky로.
// A-8 (D3): super-session 진행 유지되는 경우 sticky 재발사 — 실패 메시지가 sticky 위로 올라가
// button이 안 보이는 회귀 방어.
func updateProgressError(s *discordgo.Session, sess *Session, rc *ReleaseContext, errMsg string) {
	failedSignal := -rc.LastStep
	if rc.LastStep == 0 {
		failedSignal = -1 // 0 step 실패면 임의로 step 1 위치로 표시
	}
	if _, err := s.ChannelMessageEditComplex(&discordgo.MessageEdit{
		Channel: sess.ThreadID,
		ID:      rc.ProgressMsgID,
		Content: ptrString(""),
		Embeds:  ptrEmbeds(renderReleaseProgress(rc, failedSignal, errMsg)),
	}); err != nil {
		log.Printf("[릴리즈/progress] error edit 실패: %v", err)
	}
	// 3차 race fix: 이전엔 sess.ReleaseCtx = nil로 직접 cleanup했으나 background goroutine에서 sess 포인터를
	// mutate하면 interaction handler 동시 read와 race. runReleaseFlow defer가 InProgress.Store(false)로
	// lifecycle을 표시하므로 가드는 InProgress.Load()로 abandoned 처리 가능. 다음 sticky 진입 시
	// customIDSubActionRelease handler가 sess.ReleaseCtx = nil을 main goroutine에서 정리.
	if sess.Mode == ModeMeeting {
		sendSticky(s, sess)
	}
}

// sendReleaseResult는 완료 후 PR 본문 미리보기 + [PR 열기][처음 메뉴] 안내.
// PR URL 은 plain text 가 아닌 LinkButton 으로 노출해 클릭 동선을 일관화.
func sendReleaseResult(s *discordgo.Session, sess *Session, rc *ReleaseContext, prBody string) {
	embed := renderReleaseResultEmbed(rc, prBody)
	if _, err := s.ChannelMessageSendComplex(sess.ThreadID, &discordgo.MessageSend{
		Embeds:     []*discordgo.MessageEmbed{embed},
		Components: releaseDoneComponents(rc.PRURL),
	}); err != nil {
		log.Printf("[릴리즈/result] 전송 실패: %v", err)
	}
	sess.LastBotSummary = prBody
}

// releaseDoneComponents는 PR 생성 완료 메시지에 첨부할 버튼 행을 만든다.
//
// D1 정책: [처음 메뉴] button 폐기 — [PR 열기] LinkButton만. URL이 비면 row 자체 생략.
func releaseDoneComponents(prURL string) []discordgo.MessageComponent {
	if prURL == "" {
		return nil
	}
	return []discordgo.MessageComponent{
		discordgo.ActionsRow{Components: []discordgo.MessageComponent{
			discordgo.Button{
				Label: "PR 열기",
				Style: discordgo.LinkButton,
				URL:   prURL,
			},
		}},
	}
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

// handleReleaseBackModule는 [← 다시 선택] 클릭 시 모듈 선택 prompt를 다시 띄운다.
//
// codex review 2차 P2 fix: 이전엔 LineBackend를 하드코딩해서 frontend 모듈 진입 후 [← 다시 선택]을
// 누르면 backend 모듈 화면으로 잘못 돌아가는 회귀가 있었음. 현재 ReleaseCtx.Module.Line으로 분기.
//
// 모듈이 아직 선택 안 됐으면(Module.Key=="" — handleReleaseModule 호출 전에 [← 다시 선택]은 normally
// 도달 불가) 라인 선택 화면으로 폴백.
func handleReleaseBackModule(s *discordgo.Session, i *discordgo.InteractionCreate, sess *Session) {
	if sess.ReleaseCtx == nil {
		respondInteraction(s, i, "릴리즈 컨텍스트가 만료되었습니다. sticky의 [릴리즈 PR 만들기] button으로 다시 시작해주세요.")
		return
	}
	if sess.ReleaseCtx.Module.Key == "" {
		// 비정상 경로 — 모듈 선택 전엔 done 안 됨. 안전하게 라인 선택으로 fallback.
		logGuard("release", "back_module_no_module", "ReleaseCtx.Module 미설정 — 라인 화면으로 fallback",
			lf("thread", sess.ThreadID))
		respondInteractionWithComponents(s, i, "어떤 라인을 릴리즈할까요?", releaseLineComponents())
		return
	}
	line := sess.ReleaseCtx.Module.Line
	label := line.String()
	modules := release.ModulesByLine(line)
	respondInteractionWithComponents(s, i,
		fmt.Sprintf("%s — 어느 모듈을 릴리즈할까요?", label),
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

// renderReleaseConfirmEmbed 는 bump 선택 직후 확인 prompt embed 를 만든다.
func renderReleaseConfirmEmbed(rc *ReleaseContext) *discordgo.MessageEmbed {
	embed := releaseEmbed(
		bumpColor(rc.Bump),
		fmt.Sprintf("%s · %s (%s)", rc.Module.Line.String(), rc.Module.Key, rc.Module.DisplayName),
		limitText(fmt.Sprintf("릴리즈 진행 확인 — v%s → v%s (%s)", rc.PrevVersion, rc.NewVersion, rc.Bump.String()), 256),
	)
	embed.Description = "아래 내용으로 진행합니다. 이상 없으면 **확인** 을 눌러주세요."
	embed.Fields = []*discordgo.MessageEmbedField{
		embedField("모듈", limitText(fmt.Sprintf("%s (%s)", rc.Module.Key, rc.Module.DisplayName), 1024), true),
		embedField("Bump", limitText(fmt.Sprintf("%s · v%s", rc.Bump.String(), rc.NewVersion), 1024), true),
		embedField("비교 base", limitText(fmt.Sprintf("`%s ↔ main`", rc.PrevTag), 1024), false),
		embedField("작업", "VERSION commit → tag push → PR 생성 (LLM 본문)", false),
	}
	embed.Footer = &discordgo.MessageEmbedFooter{Text: "auto-merge 없음 · 머지는 GitHub 에서 직접"}
	return embed
}

// renderReleaseProgress는 단계별 진행 상태를 embed 로 그린다.
func renderReleaseProgress(rc *ReleaseContext, current int, note string) *discordgo.MessageEmbed {
	failedStep := 0
	if current < 0 {
		failedStep = -current
	}
	total := len(releaseProgressSteps)
	doneSteps := 0
	runningSteps := 0
	failedCount := 0
	switch {
	case failedStep > 0:
		doneSteps = failedStep - 1
		failedCount = 1
	case current == total+1:
		doneSteps = total
	case current > 0:
		doneSteps = current - 1
		runningSteps = 1
	}

	stripe := colorWarn
	titleSuffix := ""
	footer := fmt.Sprintf("비교: %s ↔ main · PR 링크는 다음 카드", rc.PrevTag)
	if failedStep > 0 {
		stripe = colorBad
		titleSuffix = fmt.Sprintf(" · 실패 (step %d)", failedStep)
		footer = fmt.Sprintf("비교: %s ↔ main · 실패 step 확인 후 재시도", rc.PrevTag)
	} else if current == total+1 {
		stripe = colorOK
		titleSuffix = " · 완료"
	}

	embed := releaseEmbed(
		stripe,
		fmt.Sprintf("%s · %s (%s)", rc.Module.Line.String(), rc.Module.Key, rc.Module.DisplayName),
		limitText(fmt.Sprintf("릴리즈 진행 — v%s → v%s (%s)%s", rc.PrevVersion, rc.NewVersion, rc.Bump.String(), titleSuffix), 256),
	)
	countLine := fmt.Sprintf("완료 %d · 진행 %d", doneSteps, runningSteps)
	if failedCount > 0 {
		countLine += fmt.Sprintf(" · 실패 %d", failedCount)
	}
	embed.Description = fmt.Sprintf("%s\n%s", progressBar(doneSteps, total), countLine)

	var b strings.Builder
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
	if failedStep > 0 && note != "" {
		b.WriteString("\n")
		fmt.Fprintf(&b, "실패 사유: %s\n", note)
	}
	embed.Fields = []*discordgo.MessageEmbedField{
		embedField("단계", limitText(b.String(), 1024), false),
	}
	embed.Footer = &discordgo.MessageEmbedFooter{Text: limitText(footer, 2048)}
	return embed
}

// renderReleaseResultEmbed 는 PR 완료와 LLM 본문 미리보기를 하나의 embed 로 만든다.
func renderReleaseResultEmbed(rc *ReleaseContext, prBody string) *discordgo.MessageEmbed {
	body := strings.TrimLeft(prBody, "\n")
	if idx := strings.IndexByte(body, '\n'); idx >= 0 {
		first := strings.TrimSpace(body[:idx])
		heading := strings.TrimSpace(strings.TrimLeft(first, "# "))
		if strings.HasPrefix(heading, "Release ") {
			body = strings.TrimLeft(body[idx+1:], "\n")
		}
	} else {
		heading := strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(body), "# "))
		if strings.HasPrefix(heading, "Release ") {
			body = ""
		}
	}
	desc := fmt.Sprintf("비교 `%s ↔ main` 기준 LLM 초안 본문.\n\n%s", rc.PrevTag, body)
	embed := releaseEmbed(
		lineColor(rc.Module.Line),
		fmt.Sprintf("%s · %s (%s) · %d commits", rc.Module.Line.String(), rc.Module.Key, rc.Module.DisplayName, rc.CommitCount),
		limitText(fmt.Sprintf("PR #%d · Release %s v%s (%s)", rc.PRNumber, rc.Module.DisplayName, rc.NewVersion, rc.Bump.String()), 256),
	)
	embed.Description = limitText(desc, 4096)
	embed.Footer = &discordgo.MessageEmbedFooter{Text: "봇 diff/커밋 기반 초안 · 머지 전 검토 후 필요 시 직접 편집"}
	return embed
}

// followupErr는 deferred ack 이후 발생한 에러를 followup 메시지로 사용자에게 안내한다.
// D1 정책: [처음 메뉴] button row 폐기 — 후속 작업은 super-session sticky로.
func followupErr(s *discordgo.Session, i *discordgo.InteractionCreate, msg string) {
	if _, err := s.FollowupMessageCreate(i.Interaction, true, &discordgo.WebhookParams{
		Content: msg,
	}); err != nil {
		log.Printf("[릴리즈/followup-err] 전송 실패: %v", err)
	}
}

// =====================================================================
// B-3: batch release UI / handlers
// =====================================================================

// batchReleaseModuleComponents는 BatchReleaseContext.Modules 각각에 대해 한 줄 StringSelectMenu를 만들고
// 마지막 5번째 row에 [모두 진행] button을 둔다.
//
// Discord 제약:
//   - 한 메시지에 ActionsRow 최대 5개
//   - StringSelectMenu는 ActionsRow 안에 단독 배치 (다른 컴포넌트와 같은 row에 못 둠)
//   - 따라서 모듈 batchReleaseMaxModules(4)개 + button 1 row = 5 row 정확히 사용.
//   - 한도 초과는 handleReleaseLine("all")이 진입 시점에 reject (silent 깨짐 방지).
//
// Selections 박제 상태가 있으면 Default 옵션으로 그 bump를 미리 선택해 보여준다 (in-place edit 후 재발사 시).
//
// 3차 race fix: bc.selections 직접 read 대신 SnapshotSelections로 immutable copy를 한 번만 가져와서 사용 —
// rendering 도중 다른 사용자의 SetSelection 호출이 같은 map을 mutate해도 안전.
func batchReleaseModuleComponents(bc *BatchReleaseContext) []discordgo.MessageComponent {
	selections := bc.SnapshotSelections()
	rows := make([]discordgo.MessageComponent, 0, len(bc.Modules)+1)
	selected := 0
	for _, m := range bc.Modules {
		current := selections[m.Key]
		if current != release.BumpUnknown {
			selected++
		}
		opts := []discordgo.SelectMenuOption{
			{Label: "메이저 (major)", Value: "major", Default: current == release.BumpMajor},
			{Label: "마이너 (minor)", Value: "minor", Default: current == release.BumpMinor},
			{Label: "패치 (patch)", Value: "patch", Default: current == release.BumpPatch},
		}
		placeholder := fmt.Sprintf("%s — bump 선택", m.DisplayName)
		if current != release.BumpUnknown {
			placeholder = fmt.Sprintf("%s — 선택: %s", m.DisplayName, current.String())
		}
		rows = append(rows, discordgo.ActionsRow{
			Components: []discordgo.MessageComponent{
				discordgo.SelectMenu{
					MenuType:    discordgo.StringSelectMenu,
					CustomID:    customIDBatchReleaseSelectPrefix + m.Key,
					Placeholder: placeholder,
					Options:     opts,
				},
			},
		})
	}
	// 같은 snapshot 기반으로 카운트 — SelectedCount 별도 호출 시 lock 한 번 더 + race 시점 차이 가능.
	startLabel := fmt.Sprintf("모두 진행 (선택 %d개)", selected)
	rows = append(rows, discordgo.ActionsRow{
		Components: []discordgo.MessageComponent{
			discordgo.Button{
				Label:    startLabel,
				Style:    discordgo.SuccessButton,
				CustomID: customIDBatchReleaseStart,
			},
		},
	})
	return rows
}

// batchReleasePromptHeader는 사용자에게 보여주는 안내 문구 + 현재 선택 요약.
// 매 selection마다 갱신되어 in-place edit으로 재전송된다.
//
// 3차 race fix: SnapshotSelections로 immutable copy 사용.
func batchReleasePromptHeader(bc *BatchReleaseContext) string {
	selections := bc.SnapshotSelections()
	var b strings.Builder
	b.WriteString("**[전체] batch release** — 모듈별로 bump를 선택하고 [모두 진행]을 눌러주세요.\n")
	b.WriteString("미선택 모듈은 release 대상에서 자동 제외됩니다.\n\n")
	for _, m := range bc.Modules {
		cur := selections[m.Key]
		if cur == release.BumpUnknown {
			fmt.Fprintf(&b, "- `%s` (%s): _미선택_\n", m.Key, m.DisplayName)
		} else {
			fmt.Fprintf(&b, "- `%s` (%s): **%s**\n", m.Key, m.DisplayName, cur.String())
		}
	}
	return b.String()
}

// sendBatchReleasePrompt는 batch release UI를 처음 띄울 때 사용한다.
// 진입점: handleReleaseLine("all") 분기 끝.
//
// 동작: deferred channel message ack → FollowupMessageCreate로 prompt 메시지 발사.
// 후속 모듈별 SelectMenu 인터랙션은 handleBatchReleaseSelect가 InteractionResponseUpdateMessage로
// 같은 메시지를 in-place edit하므로 별도 message ID 보관 불필요 (Discord interaction이 자체적으로
// 트리거한 메시지를 추적한다).
func sendBatchReleasePrompt(s *discordgo.Session, i *discordgo.InteractionCreate, sess *Session) {
	if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
	}); err != nil {
		log.Printf("[릴리즈/batch] ack 실패 thread=%s: %v", sess.ThreadID, err)
		return
	}
	// deferred ack는 followup 1건이 필수 — 빈 followup 보내기보단 prompt 메시지를 followup으로 발사.
	msg, err := s.FollowupMessageCreate(i.Interaction, false, &discordgo.WebhookParams{
		Content:    batchReleasePromptHeader(sess.BatchReleaseCtx),
		Components: batchReleaseModuleComponents(sess.BatchReleaseCtx),
	})
	if err != nil {
		log.Printf("[릴리즈/batch] prompt followup 실패 thread=%s: %v", sess.ThreadID, err)
		return
	}
	log.Printf("[릴리즈/batch] prompt 발사 thread=%s msg=%s", sess.ThreadID, msg.ID)
}

// handleBatchReleaseSelect는 모듈별 StringSelectMenu 클릭을 처리한다.
// customID는 batch_release_select:<module_key>, value는 "major"/"minor"/"patch".
//
// Selections에 박제 후 prompt 메시지를 in-place edit (header + components 재생성) — placeholder/Default가
// 새 선택을 반영해 사용자가 자기 선택을 시각적으로 확인할 수 있다.
func handleBatchReleaseSelect(s *discordgo.Session, i *discordgo.InteractionCreate, sess *Session, moduleKey string) {
	if sess.BatchReleaseCtx == nil {
		logGuard("release/batch", "ctx_expired", "BatchReleaseCtx nil — select reject",
			lf("thread", sess.ThreadID), lf("module", moduleKey))
		respondInteractionEphemeral(s, i, "batch release 컨텍스트가 만료되었습니다. sticky의 [릴리즈 PR 만들기] → [전체]로 다시 시작해주세요.")
		return
	}
	if sess.BatchReleaseCtx.InProgress.Load() {
		logGuard("release/batch", "in_progress", "batch 진행 중 — select reject",
			lf("thread", sess.ThreadID), lf("module", moduleKey))
		respondInteractionEphemeral(s, i, "이미 batch release가 진행 중입니다.")
		return
	}
	if _, ok := release.FindModule(moduleKey); !ok {
		logError("release/batch", "unknown_module", "FindModule 실패 — select reject", nil,
			lf("thread", sess.ThreadID), lf("module", moduleKey))
		respondInteractionEphemeral(s, i, fmt.Sprintf("알 수 없는 모듈: %q", moduleKey))
		return
	}
	data := i.MessageComponentData()
	if len(data.Values) == 0 {
		logError("release/batch", "empty_select_value", "SelectMenu values 비어있음", nil,
			lf("thread", sess.ThreadID), lf("module", moduleKey))
		respondInteractionEphemeral(s, i, "선택값이 비어 있습니다.")
		return
	}
	bump, ok := release.ParseBumpType(data.Values[0])
	if !ok {
		logError("release/batch", "unknown_bump", "ParseBumpType 실패", nil,
			lf("thread", sess.ThreadID), lf("module", moduleKey), lf("value", data.Values[0]))
		respondInteractionEphemeral(s, i, fmt.Sprintf("알 수 없는 bump: %q", data.Values[0]))
		return
	}
	// 3차 race fix: 평범한 map 직접 mutate → SetSelection accessor (mu 보호) 경유.
	sess.BatchReleaseCtx.SetSelection(moduleKey, bump)
	logEvent("release/batch", "select", "모듈 bump 선택 박제",
		lf("thread", sess.ThreadID), lf("module", moduleKey), lf("bump", bump.String()),
		lf("selected", sess.BatchReleaseCtx.SelectedCount()))

	// in-place edit으로 prompt 갱신 (header + components 둘 다 새 선택 반영).
	if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseUpdateMessage,
		Data: &discordgo.InteractionResponseData{
			Content:    batchReleasePromptHeader(sess.BatchReleaseCtx),
			Components: batchReleaseModuleComponents(sess.BatchReleaseCtx),
		},
	}); err != nil {
		log.Printf("[릴리즈/batch] update 실패 thread=%s: %v", sess.ThreadID, err)
	}
}

// handleBatchReleaseStart는 [모두 진행] button 클릭을 처리한다.
//
// 검증:
//   - BatchReleaseCtx 만료 reject
//   - InProgress 중복 클릭 reject (race 방어)
//   - HasAnySelection() == false → 0개 선택 안내 (UI 그대로 두기)
//
// 통과 시 InProgress=true 박제 + ack 후 runBatchReleaseFlow goroutine 발사.
func handleBatchReleaseStart(s *discordgo.Session, i *discordgo.InteractionCreate, sess *Session) {
	bc := sess.BatchReleaseCtx
	if bc == nil {
		logGuard("release/batch", "ctx_expired", "[모두 진행] click — BatchReleaseCtx nil",
			lf("thread", sess.ThreadID), lf("user", interactionCallerUsername(i)))
		respondInteraction(s, i, "batch release 컨텍스트가 만료되었습니다. sticky의 [릴리즈 PR 만들기] → [전체]로 다시 시작해주세요.")
		return
	}
	// 3차 race fix: 사전 read-only 가드(SelectedCount 0 / github / llm) 먼저 통과시키고,
	// 마지막에 CompareAndSwap(false, true)으로 race-safe 단일 진입 commit.
	// (CAS를 먼저 두면 중복 클릭은 막아도 read-only fail 시 InProgress=true 박제로 다음 정상 클릭이 막히는 부작용.)
	if !bc.HasAnySelection() {
		logGuard("release/batch", "no_selection", "선택 0개 — 발사 reject",
			lf("thread", sess.ThreadID), lf("user", interactionCallerUsername(i)))
		respondInteractionEphemeral(s, i, "선택된 모듈이 0개입니다. 모듈별 dropdown에서 bump를 1개 이상 선택해주세요.")
		return
	}
	if githubClient == nil {
		logGuard("release/batch", "github_unconfigured", "GITHUB_TOKEN 미설정",
			lf("thread", sess.ThreadID), lf("user", interactionCallerUsername(i)))
		respondInteraction(s, i, "GITHUB_TOKEN이 설정되어 있지 않아 batch release를 시작할 수 없습니다.")
		return
	}
	if llmClient == nil {
		logGuard("release/batch", "llm_unconfigured", "LLM client 미초기화",
			lf("thread", sess.ThreadID), lf("user", interactionCallerUsername(i)))
		respondInteraction(s, i, "LLM 클라이언트가 초기화되지 않아 batch release를 시작할 수 없습니다.")
		return
	}
	if !bc.InProgress.CompareAndSwap(false, true) {
		logGuard("release/batch", "double_start", "[모두 진행] 중복 click — 이미 진행 중",
			lf("thread", sess.ThreadID), lf("user", interactionCallerUsername(i)),
			lf("selected", bc.SelectedCount()))
		respondInteractionEphemeral(s, i, "이미 batch release가 진행 중입니다.")
		return
	}

	// snapshot으로 selected 목록 구성 (race-safe).
	selections := bc.SnapshotSelections()
	selected := make([]string, 0, len(selections))
	for _, m := range bc.Modules {
		if b, ok := selections[m.Key]; ok && b != release.BumpUnknown {
			selected = append(selected, fmt.Sprintf("%s=%s", m.Key, b))
		}
	}
	logEvent("release/batch", "fire", "[모두 진행] click — N goroutine 발사",
		lf("thread", sess.ThreadID), lf("user", interactionCallerUsername(i)),
		lf("selected", len(selected)), lf("modules", strings.Join(selected, ",")))

	respondInteraction(s, i, fmt.Sprintf("Batch release 발사 — 모듈 %d개 (%s)", len(selected), strings.Join(selected, ", ")))
	go runBatchReleaseFlow(s, sess, bc)
}

// =====================================================================
// B-4: batch release 병렬 실행 (4 goroutine + 단일 progress 메시지)
// =====================================================================

// batchModuleJob은 batch release 1 모듈 단위 작업 상태.
// runBatchReleaseFlow가 BatchReleaseContext.Selections에서 채워 넣고, 각 goroutine이 자기 job을 갱신.
type batchModuleJob struct {
	Module release.Module
	Bump   release.BumpType

	// runtime 상태 — batchProgress.mu 보호 (편집 락과 통일).
	rc    *ReleaseContext // setupReleaseContextForBatch 성공 시 채워짐
	step  int             // 0=대기/setup, 1-5=진행, 6=완료, -N=실패 step N (N=0=setup 실패)
	note  string          // 현재 step 라벨 또는 에러 메시지
	err   error           // 최종 에러 (nil이면 성공)
	prURL string          // 성공 시 PR URL
	prNum int             // 성공 시 PR 번호
}

// batchProgress는 batch release 모듈별 진행 상태를 단일 Discord 메시지에 합본 표시한다.
//
// 동시성 정책:
//   - 다수 goroutine이 update()로 자기 job 상태 갱신 — 내부 mu 보호
//   - 단일 ticker goroutine이 1.5s마다 message edit (rate limit 안전)
//   - 모든 작업 종료 후 caller가 finalEdit() 호출 (즉시 마지막 상태 edit)
type batchProgress struct {
	s         *discordgo.Session
	channelID string
	msgID     string

	startedAt time.Time

	mu   sync.Mutex
	jobs []*batchModuleJob

	stop     chan struct{}
	stopOnce sync.Once
	done     chan struct{}
}

// newBatchProgress는 progress 메시지를 즉시 send + ticker goroutine을 시작한다.
// caller는 반드시 finish()를 호출 (defer 권장) — goroutine leak 방지.
func newBatchProgress(s *discordgo.Session, channelID string, jobs []*batchModuleJob) (*batchProgress, error) {
	bp := &batchProgress{
		s:         s,
		channelID: channelID,
		jobs:      jobs,
		startedAt: time.Now(),
		stop:      make(chan struct{}),
		done:      make(chan struct{}),
	}
	msg, err := s.ChannelMessageSend(channelID, bp.renderLocked())
	if err != nil {
		close(bp.done)
		return nil, fmt.Errorf("batch progress 초기 전송 실패: %w", err)
	}
	bp.msgID = msg.ID
	go bp.runTicker()
	return bp, nil
}

// update는 jobIndex의 step/note를 갱신한다 (mu 보호). ticker가 다음 tick에서 메시지에 반영.
// step in [1,5]=진행, 6=완료, -N=실패. note는 사람이 읽는 단계 설명 또는 에러 메시지.
func (bp *batchProgress) update(jobIndex int, step int, note string) {
	bp.mu.Lock()
	defer bp.mu.Unlock()
	if jobIndex < 0 || jobIndex >= len(bp.jobs) {
		return
	}
	bp.jobs[jobIndex].step = step
	bp.jobs[jobIndex].note = note
}

// markDone은 성공 종료를 마킹한다 (PR URL/번호 박제 + step=6).
func (bp *batchProgress) markDone(jobIndex int, prURL string, prNum int) {
	bp.mu.Lock()
	defer bp.mu.Unlock()
	if jobIndex < 0 || jobIndex >= len(bp.jobs) {
		return
	}
	bp.jobs[jobIndex].step = 6
	bp.jobs[jobIndex].note = "완료"
	bp.jobs[jobIndex].prURL = prURL
	bp.jobs[jobIndex].prNum = prNum
}

// markError는 실패 종료를 마킹한다. step은 음수로 박제 (어느 단계에서 실패했는지 표시).
// failedStep == 0 (setup 실패)이면 -1로 정규화.
func (bp *batchProgress) markError(jobIndex int, failedStep int, err error) {
	bp.mu.Lock()
	defer bp.mu.Unlock()
	if jobIndex < 0 || jobIndex >= len(bp.jobs) {
		return
	}
	signal := -failedStep
	if failedStep == 0 {
		signal = -1
	}
	bp.jobs[jobIndex].step = signal
	bp.jobs[jobIndex].note = err.Error()
	bp.jobs[jobIndex].err = err
}

// runTicker는 1.5s 간격으로 message edit. stop 닫히면 종료.
func (bp *batchProgress) runTicker() {
	defer close(bp.done)
	ticker := time.NewTicker(1500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-bp.stop:
			return
		case <-ticker.C:
			bp.editOnce()
		}
	}
}

// editOnce는 현재 상태를 message edit. 내부 mu lock + Discord rate limit는 caller 책임 (ticker가 1.5s 간격으로 호출).
func (bp *batchProgress) editOnce() {
	bp.mu.Lock()
	body := bp.renderLocked()
	mid := bp.msgID
	bp.mu.Unlock()
	if mid == "" {
		return
	}
	if _, err := bp.s.ChannelMessageEdit(bp.channelID, mid, body); err != nil {
		log.Printf("[릴리즈/batch/progress] edit 실패: %v", err)
	}
}

// finish는 ticker goroutine을 멈추고 마지막 한 번 edit한다 (모든 job 종료 후 caller가 호출).
func (bp *batchProgress) finish() {
	select {
	case <-bp.done:
	default:
		bp.stopOnce.Do(func() { close(bp.stop) })
		<-bp.done
	}
	bp.editOnce()
}

// renderLocked는 현재 jobs 상태를 markdown으로 렌더한다. caller가 mu lock 보유한 상태에서 호출해야 함.
func (bp *batchProgress) renderLocked() string {
	var b strings.Builder
	elapsed := time.Since(bp.startedAt).Round(time.Second)
	doneCount, errCount := 0, 0
	for _, j := range bp.jobs {
		switch {
		case j.step == 6:
			doneCount++
		case j.step < 0:
			errCount++
		}
	}
	fmt.Fprintf(&b, "**Batch release** — 모듈 %d개 · 완료 %d · 실패 %d · %s 경과\n```\n",
		len(bp.jobs), doneCount, errCount, elapsed)
	for _, j := range bp.jobs {
		marker := "·" // 대기
		stepLabel := "대기"
		switch {
		case j.step == 6:
			marker = "✓"
			stepLabel = "완료"
		case j.step < 0:
			marker = "✗"
			stepLabel = fmt.Sprintf("실패 step %d", -j.step)
		case j.step >= 1 && j.step <= 5:
			marker = "▶"
			stepLabel = fmt.Sprintf("Step %d/%d", j.step, len(releaseProgressSteps))
		}
		// "✓ frontend (메이저): 완료 · PR #123" / "▶ admin (마이너): Step 3/5 — VERSION 파일 commit 중..."
		bumpLabel := ""
		if j.Bump != release.BumpUnknown {
			bumpLabel = fmt.Sprintf(" (%s)", j.Bump.String())
		}
		line := fmt.Sprintf("%s %s%s: %s", marker, j.Module.Key, bumpLabel, stepLabel)
		if j.note != "" {
			line += " — " + j.note
		}
		b.WriteString(line + "\n")
	}
	b.WriteString("```")
	return b.String()
}

// setupReleaseContextForBatch는 배치 모드에서 1 모듈의 ReleaseContext를 채운다.
// 동작은 handleReleaseModule + handleReleaseBump의 합성 — GetFile/ListTags + bump 적용.
//
// 단일 모드의 handleReleaseModule이 사용자 인터랙션 흐름과 묶여있어서 재사용 불가 — 본 함수에서 별도 구성.
func setupReleaseContextForBatch(ctx context.Context, module release.Module, bump release.BumpType) (*ReleaseContext, error) {
	rc := &ReleaseContext{
		Owner:  module.Owner,
		Repo:   module.Repo,
		Module: module,
	}

	// VERSION 조회
	fc, err := githubClient.GetFile(ctx, module.Owner, module.Repo, module.VersionPath, "main")
	if err != nil {
		return nil, fmt.Errorf("VERSION 파일 조회 실패: %w", err)
	}
	curVer, err := release.ParseVersion(string(fc.Content))
	if err != nil {
		return nil, fmt.Errorf("VERSION 파싱 실패: %w", err)
	}
	rc.FileSHA = fc.SHA
	rc.PrevVersion = curVer

	// 직전 tag 조회
	tags, err := githubClient.ListTags(ctx, module.Owner, module.Repo)
	if err != nil {
		return nil, fmt.Errorf("ListTags 실패: %w", err)
	}
	names := make([]string, len(tags))
	for i, tg := range tags {
		names[i] = tg.Name
	}
	latest, found := release.ResolveLatestTag(names, module)
	rc.PrevTag = firstReleaseLabel
	if found {
		rc.PrevTag = latest.TagName
		for _, tg := range tags {
			if tg.Name == latest.TagName {
				rc.PrevTagCommitSHA = tg.Commit.SHA
				break
			}
		}
	}

	// bump 적용
	newVer, err := curVer.Bump(bump)
	if err != nil {
		return nil, fmt.Errorf("버전 계산 실패: %w", err)
	}
	rc.Bump = bump
	rc.NewVersion = newVer
	rc.NewTag = newVer.Tag(module)

	// codex review 2차 P1 isolation: 모듈마다 독립된 head 브랜치를 사용해 같은 repo의 다른 batch 모듈
	// bump 커밋이 PR에 섞이지 않도록 한다. 브랜치명은 (모듈 키, 새 버전) 기준 deterministic — 같은 release를
	// 두 번 시도해도 동일 브랜치 (idempotent — UpdateFile이 if-match SHA로 충돌 감지).
	rc.HeadBranch = fmt.Sprintf("release-batch/%s-v%s", module.Key, newVer.String())
	return rc, nil
}

// runBatchReleaseFlow는 batch release 흐름의 orchestration 함수. handleBatchReleaseStart에서 goroutine 발사.
//
// 단계:
//  1. Selections에서 batchModuleJob 리스트 구성 (BumpUnknown 제외)
//  2. batchProgress 메시지 send + ticker 시작
//  3. WaitGroup로 N goroutine 병렬 발사 (각각 runOneBatchModule)
//  4. 모든 goroutine done → batchProgress finish (마지막 edit)
//  5. 합본 결과 메시지 send (성공/실패 + PR URL)
//  6. BatchReleaseCtx.InProgress = false (cleanup)
//  7. sendSticky 재발사 (A-8 일관성)
func runBatchReleaseFlow(s *discordgo.Session, sess *Session, bc *BatchReleaseContext) {
	startedAt := time.Now()
	defer func() {
		bc.InProgress.Store(false)
		if sess.Mode == ModeMeeting {
			sendSticky(s, sess)
		}
	}()

	// 1. job 리스트 구성 (선택된 모듈만, original Modules 순서 유지).
	// 3차 race fix: SnapshotSelections로 immutable copy를 한 번만 가져와서 사용 — 진행 중에 사용자가
	// 추가 SelectMenu 클릭하더라도 (이론상 InProgress 가드로 막혀 있지만 race 시점) job 구성에 영향 X.
	selections := bc.SnapshotSelections()
	jobs := make([]*batchModuleJob, 0, len(selections))
	for _, m := range bc.Modules {
		bump, ok := selections[m.Key]
		if !ok || bump == release.BumpUnknown {
			continue
		}
		jobs = append(jobs, &batchModuleJob{Module: m, Bump: bump})
	}
	if len(jobs) == 0 {
		logGuard("release/batch", "no_jobs", "selection 0건 — 진행 중단",
			lf("thread", sess.ThreadID))
		s.ChannelMessageSend(sess.ThreadID, "선택된 모듈이 0개입니다.")
		return
	}

	// 2. batchProgress 시작.
	bp, err := newBatchProgress(s, sess.ThreadID, jobs)
	if err != nil {
		logError("release/batch", "progress_init_failed", "초기 progress 메시지 전송 실패 — 중단", err,
			lf("thread", sess.ThreadID), lf("jobs", len(jobs)))
		s.ChannelMessageSend(sess.ThreadID, fmt.Sprintf("batch progress 메시지 실패 — release 진행 중단: %v", err))
		return
	}
	logEvent("release/batch", "begin", "N goroutine 발사 시작",
		lf("thread", sess.ThreadID), lf("jobs", len(jobs)))

	// 3. 병렬 발사.
	var wg sync.WaitGroup
	for idx, j := range jobs {
		wg.Add(1)
		go func(idx int, j *batchModuleJob) {
			defer wg.Done()
			runOneBatchModule(sess, idx, j, bp)
		}(idx, j)
	}
	wg.Wait()

	// 4. progress 마무리.
	bp.finish()

	// 5. 합본 결과 메시지.
	sendBatchReleaseResult(s, sess, jobs)

	// 6. 종료 로그 (성공/실패 카운트).
	successCount, failCount := 0, 0
	for _, j := range jobs {
		if j.err != nil {
			failCount++
		} else {
			successCount++
		}
	}
	logEvent("release/batch", "end", "모든 goroutine 종료 + 합본 결과 송신",
		lf("thread", sess.ThreadID), lf("jobs", len(jobs)),
		lf("success", successCount), lf("fail", failCount),
		lf("elapsed", time.Since(startedAt)))
}

// runOneBatchModule은 1 모듈의 setup + runReleaseSteps를 실행한다 (병렬 goroutine 본체).
// 진행 상태는 bp.update / markDone / markError로 단일 progress 메시지에 누적.
// SubAction lifecycle (begin/append/end)은 모듈마다 독립 — corpus에 모듈별 segment로 박제.
func runOneBatchModule(sess *Session, idx int, j *batchModuleJob, bp *batchProgress) {
	startedAt := time.Now()
	logEvent("release/batch", "goroutine_start", "모듈 release 시작",
		lf("thread", sess.ThreadID), lf("idx", idx),
		lf("module", j.Module.Key), lf("bump", j.Bump.String()))
	defer func() {
		// 종료 로그 — 성공/실패 무관, elapsed/PR 정보 포함.
		fields := []logField{
			lf("thread", sess.ThreadID), lf("idx", idx),
			lf("module", j.Module.Key), lf("elapsed", time.Since(startedAt)),
		}
		if j.err != nil {
			fields = append(fields, lf("status", "fail"), lf("err", j.err.Error()))
			if j.rc != nil {
				fields = append(fields, lf("step", j.rc.LastStep))
			}
		} else {
			fields = append(fields, lf("status", "ok"), lf("pr", j.prNum), lf("url", j.prURL))
		}
		logEvent("release/batch", "goroutine_end", "모듈 release 종료", fields...)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	bp.update(idx, 0, "VERSION/태그 조회 중...")
	rc, err := setupReleaseContextForBatch(ctx, j.Module, j.Bump)
	if err != nil {
		j.err = err
		bp.markError(idx, 0, err)
		logError("release/batch", "setup_failed", "setupReleaseContextForBatch 실패", err,
			lf("thread", sess.ThreadID), lf("module", j.Module.Key))
		return
	}
	j.rc = rc

	// SubAction lifecycle — ModeMeeting일 때만 적용 (단일 모드 runReleaseFlow와 동일 패턴).
	var (
		sa             *SubActionContext
		releaseSummary string
	)
	if sess.Mode == ModeMeeting {
		beginCtx, beginCancel := context.WithTimeout(context.Background(), 5*time.Second)
		sa = BeginSubAction(beginCtx, sess, db.SegmentRelease)
		beginCancel()
		defer func() {
			endCtx, endCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer endCancel()
			sa.EndWithArtifact(endCtx, map[string]any{
				"module":      rc.Module.Key,
				"prev_tag":    rc.PrevTag,
				"new_version": rc.NewVersion.String(),
				"bump":        rc.Bump.String(),
				"pr_number":   rc.PRNumber,
				"pr_url":      rc.PRURL,
				"summary":     len(releaseSummary),
				"batch":       true,
			})
		}()
	}

	statusFn := func(step int, note string) {
		bp.update(idx, step, note)
	}
	prBody, err := runReleaseSteps(ctx, rc, statusFn)
	if err != nil {
		j.err = err
		bp.markError(idx, rc.LastStep, err)
		return
	}

	bp.markDone(idx, rc.PRURL, rc.PRNumber)

	releaseSummary = fmt.Sprintf("[release/batch] %s %s → PR #%d (%s)\n%s",
		rc.Module.DisplayName, rc.NewVersion.String(), rc.PRNumber, rc.PRURL, prBody)
	if sa != nil {
		appendCtx, appendCancel := context.WithTimeout(context.Background(), 5*time.Second)
		sa.AppendResult(appendCtx, sess, "[release]", db.SourceReleaseResult, releaseSummary)
		appendCancel()
	}
}

// sendBatchReleaseResult는 모든 모듈 종료 후 합본 결과 메시지를 송신한다.
// 성공 모듈은 [PR 열기] LinkButton row를 첨부 (Discord row max 5, LinkButton 5개 row 단위로 묶음).
// 실패 모듈은 본문에 에러 노출.
//
// 폴링은 batch 모드에선 비활성 (사용자 노이즈 방지 — 4 PR 동시 polling은 메시지 폭주). 사용자가
// 필요하면 GitHub UI에서 직접 머지 상태 확인.
func sendBatchReleaseResult(s *discordgo.Session, sess *Session, jobs []*batchModuleJob) {
	successCount, failCount := 0, 0
	var b strings.Builder
	for _, j := range jobs {
		if j.err != nil {
			failCount++
		} else {
			successCount++
		}
	}
	fmt.Fprintf(&b, "**Batch release 완료** — 성공 %d건 · 실패 %d건\n\n", successCount, failCount)
	for _, j := range jobs {
		bumpLabel := ""
		if j.Bump != release.BumpUnknown {
			bumpLabel = fmt.Sprintf(" (%s)", j.Bump.String())
		}
		if j.err != nil {
			fmt.Fprintf(&b, "✗ `%s`%s — %v\n", j.Module.Key, bumpLabel, j.err)
			continue
		}
		fmt.Fprintf(&b, "✓ `%s`%s — `v%s` PR #%d <%s>\n",
			j.Module.Key, bumpLabel, j.rc.NewVersion, j.prNum, j.prURL)
	}

	// 성공 모듈마다 [PR 열기] LinkButton 노출. row max 5 button 제약에 맞춰 그룹핑.
	var rows []discordgo.MessageComponent
	var currentRow []discordgo.MessageComponent
	for _, j := range jobs {
		if j.err != nil || j.prURL == "" {
			continue
		}
		btn := discordgo.Button{
			Label: fmt.Sprintf("PR #%d (%s)", j.prNum, j.Module.Key),
			Style: discordgo.LinkButton,
			URL:   j.prURL,
		}
		currentRow = append(currentRow, btn)
		if len(currentRow) == 5 {
			rows = append(rows, discordgo.ActionsRow{Components: currentRow})
			currentRow = nil
		}
	}
	if len(currentRow) > 0 {
		rows = append(rows, discordgo.ActionsRow{Components: currentRow})
	}

	if _, err := s.ChannelMessageSendComplex(sess.ThreadID, &discordgo.MessageSend{
		Content:    b.String(),
		Components: rows,
	}); err != nil {
		log.Printf("[릴리즈/batch/result] 전송 실패: %v", err)
	}
	sess.LastBotSummary = b.String()
}
