package bot

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"chatbot-alpha-1/pkg/db"
	"chatbot-alpha-1/pkg/llm"

	"github.com/bwmarrin/discordgo"
)

// =====================================================================
// 이벤트 엔트리 핸들러
// =====================================================================

// userIDFromInteraction은 InteractionCreate에서 호출자 user ID를 안전하게 추출한다.
// per-user pending state(PendingAgentUserID/PendingExternalPasteUserID)에 사용.
// guild는 i.Member.User.ID, DM은 i.User.ID. 둘 다 없으면 "" 반환.
func userIDFromInteraction(i *discordgo.InteractionCreate) string {
	if i == nil {
		return ""
	}
	if i.Member != nil && i.Member.User != nil {
		return i.Member.User.ID
	}
	if i.User != nil {
		return i.User.ID
	}
	return ""
}

// interactionCallerUsername은 InteractionCreate에서 호출자 username을 안전하게 추출한다.
// guild interaction은 i.Member.User에, DM interaction은 i.User에 사용자 정보가 들어감.
// 둘 다 nil이면 "?" fallback (로그용 — panic 방지).
func interactionCallerUsername(i *discordgo.InteractionCreate) string {
	if i == nil {
		return "?"
	}
	if i.Member != nil && i.Member.User != nil {
		return i.Member.User.Username
	}
	if i.User != nil {
		return i.User.Username
	}
	return "?"
}

func messageCreate(s *discordgo.Session, m *discordgo.MessageCreate) {
	if m.Author.ID == s.State.User.ID {
		return
	}

	// [v1.1] 세션 우선 조회: 세션 맵에 해당 채널이 있으면 isThread 판별 없이 바로 핸들러로.
	// Discord state 캐시 전파 지연이나 일시적 API 실패로 isThread가 false를 반환해도
	// 메시지가 드롭되지 않도록 한다 (v1 포스트모템 참조).
	if sess := lookupSession(m.ChannelID); sess != nil {
		handleSession(s, m, sess)
		return
	}

	isBotChannel := botChannelID != "" && m.ChannelID == botChannelID

	mentioned := false
	for _, u := range m.Mentions {
		if u.ID == s.State.User.ID {
			mentioned = true
			break
		}
	}

	if !mentioned && !isBotChannel {
		return
	}

	content := stripMention(m.Content, s.State.User.ID)

	log.Printf("[%s] %s: %s", m.ChannelID, m.Author.Username, content)

	// 항상 스레드 생성 후 안에서 진행
	openThread(s, m, content)
}

// interactionCreate: 슬래시 명령어 + 버튼 클릭 처리.
// ApplicationCommand는 채널에서 호출되어 스레드를 새로 생성하고,
// MessageComponent는 이미 존재하는 스레드 안의 세션 버튼 흐름을 처리한다.
func interactionCreate(s *discordgo.Session, i *discordgo.InteractionCreate) {
	switch i.Type {
	case discordgo.InteractionApplicationCommand:
		handleSlashCommand(s, i)
		return
	case discordgo.InteractionModalSubmit:
		handleModalSubmit(s, i)
		return
	case discordgo.InteractionMessageComponent:
		// 아래 기존 버튼 처리 흐름으로 진행
	default:
		return
	}

	data := i.MessageComponentData()
	channelID := i.ChannelID

	sessionsMu.RLock()
	sess, exists := sessions[channelID]
	sessionsMu.RUnlock()

	if !exists {
		logGuard("discord", "session_expired", "button click — 세션 만료",
			lf("custom_id", data.CustomID), lf("channel", channelID),
			lf("uid", userIDFromInteraction(i)), lf("user", interactionCallerUsername(i)))
		respondInteraction(s, i, "세션이 만료되었습니다. 다시 시작해주세요.")
		return
	}

	sess.UpdatedAt = time.Now()

	// 모든 button click 진입 로그 — kubectl logs grep '\[discord/click\]' 으로 사용자 클릭 흐름 재구성.
	// custom_id + state + mode를 박제해 race condition 추적 가능 (같은 thread 동시 클릭 시 순서 확인).
	logEvent("discord", "click", "",
		lf("thread", sess.ThreadID), lf("custom_id", data.CustomID),
		lf("uid", userIDFromInteraction(i)), lf("user", interactionCallerUsername(i)),
		lf("mode", sess.Mode.String()), lf("state", sess.State.String()))

	// === Phase 3 chunk 3C — Pending* 자동 취소 (per-user 게이트) ===
	// pending 취소는 본인이 다른 button을 누른 경우만 — 다른 참석자가 sticky를 만져도 A의 pending이
	// 사라지지 않게 (codex 8차 리뷰 P2). clickerUID와 pending owner가 일치할 때만 clear.
	clickerUID := userIDFromInteraction(i)
	if data.CustomID != customIDExternalAttach && sess.PendingExternalPasteUserID != "" &&
		sess.PendingExternalPasteUserID == clickerUID {
		logEvent("meeting", "pending_clear", "ExternalPaste pending 자동 취소 (다른 button click)",
			lf("thread", sess.ThreadID), lf("uid", clickerUID), lf("triggered_by", data.CustomID))
		sess.PendingExternalPasteUserID = ""
	}
	if data.CustomID != customIDSubActionAgent && sess.PendingAgentUserID != "" &&
		sess.PendingAgentUserID == clickerUID {
		// agent pending 취소 시 super-session에서는 State도 Meeting으로 복귀 — StateAgentAwaitInput
		// 잔존 시 다음 미팅 발화가 agent로 라우팅되는 race 방어 (codex 4차 리뷰 P2).
		if sess.Mode == ModeMeeting && sess.State == StateAgentAwaitInput {
			logState("agent", "agent pending 취소로 state 복귀", "agent_await_input", "meeting",
				lf("thread", sess.ThreadID), lf("uid", clickerUID))
			sess.State = StateMeeting
		}
		logEvent("agent", "pending_clear", "Agent pending 자동 취소 (다른 button click)",
			lf("thread", sess.ThreadID), lf("uid", clickerUID), lf("triggered_by", data.CustomID))
		sess.PendingAgentUserID = ""
	}

	// 동적 custom_id 처리: weekly_repo:owner/name 등 prefix 매칭은 switch case로 표현 불가하므로 먼저 분기.
	if isWeeklyRepoCustomID(data.CustomID) {
		handleWeeklyRepoSelect(s, i, extractWeeklyRepoFullName(data.CustomID))
		return
	}
	if isWeeklyScopeCustomID(data.CustomID) {
		scope, ok := extractWeeklyScope(data.CustomID)
		if !ok {
			respondInteraction(s, i, "알 수 없는 분석 범위입니다.")
			return
		}
		handleWeeklyScopeSelect(s, i, scope)
		return
	}
	// 릴리즈 흐름 prefix 매칭
	if strings.HasPrefix(data.CustomID, customIDReleaseLinePrefix) {
		handleReleaseLine(s, i, sess, strings.TrimPrefix(data.CustomID, customIDReleaseLinePrefix))
		return
	}
	if strings.HasPrefix(data.CustomID, customIDReleaseModulePrefix) {
		handleReleaseModule(s, i, sess, strings.TrimPrefix(data.CustomID, customIDReleaseModulePrefix))
		return
	}
	if strings.HasPrefix(data.CustomID, customIDReleaseBumpPrefix) {
		handleReleaseBump(s, i, sess, strings.TrimPrefix(data.CustomID, customIDReleaseBumpPrefix))
		return
	}
	// B-3 batch release: 모듈별 StringSelectMenu prefix 매칭.
	if strings.HasPrefix(data.CustomID, customIDBatchReleaseSelectPrefix) {
		handleBatchReleaseSelect(s, i, sess, strings.TrimPrefix(data.CustomID, customIDBatchReleaseSelectPrefix))
		return
	}

	// 주간 첫 분석 흐름의 라우팅. weekly.go의 핸들러에 위임.
	//
	// D1/D2 폐기 라우팅 (UX 재설계 2026-05):
	//   - customIDWeeklyDirectiveBtn  (D2)
	//   - customIDWeeklyPeriodPromptBtn / customIDWeeklyRetryBtn / customIDWeeklyToMeetingBtn (D1 follow-up)
	//   - customIDHomeBtn (D1 — handleHome 자체 폐기)
	switch data.CustomID {
	case customIDWeeklyPeriodSelect:
		handleWeeklyPeriodSelect(s, i, sess)
		return
	case customIDWeeklyPeriodConfirm:
		handleWeeklyPeriodConfirm(s, i, sess)
		return
	case customIDWeeklyCloseStartBtn:
		handleWeeklyCloseStart(s, i, sess)
		return
	case customIDWeeklyCloseConfirmBtn:
		handleWeeklyCloseConfirm(s, i, sess)
		return
	}

	// D1 폐기 case (UX 재설계 2026-05): home menu의 5 button — openThread/enterSlashMode가 super-session
	// 즉시 시작으로 통일됐기 때문에 더 이상 노출 X.
	//   - "mode_meeting" / "mode_weekly" / "mode_status" / customIDAgentBtn (legacy entry)
	// 보존: customIDReleaseEntry는 release.go의 [← 라인 다시] button 진입점으로 super-session 안에서 사용 중.
	switch data.CustomID {
	// interim/sticky 메시지 하단 "미팅 종료" 버튼 → 4 포맷 선택 prompt
	case customIDMeetingEndBtn:
		log.Printf("[미팅/end] 종료 버튼 클릭 thread=%s by=%s", sess.ThreadID, interactionCallerUsername(i))
		deleteStickyIfPresent(s, sess)
		respondInteractionWithComponents(s, i,
			"어떤 포맷으로 정리할까요?\n\n"+
				"**결정+진행**: 결정사항과 완료/진행/예정/이슈 4분할 (스프린트, 작업 공유)\n"+
				"**논의**: 토픽별 논의 흐름 + 도출 관점 (1on1, 회고, 브레인스토밍)\n"+
				"**역할별**: 참석자별 결정/액션/공유 (역할 분담, 스탠드업)\n"+
				"**자율**: LLM이 회의 성격 보고 자유 정리",
			finalizePromptComponents(),
		)

	// 4 포맷 선택 버튼 → finalize
	case customIDFinalizeDecisionStatus,
		customIDFinalizeDiscussion,
		customIDFinalizeRoleBased,
		customIDFinalizeFreeform:
		format, ok := formatFromCustomID(data.CustomID)
		if !ok {
			respondInteraction(s, i, "알 수 없는 포맷입니다.")
			return
		}
		sess.NoteFormat = format
		ack := fmt.Sprintf("미팅을 [%s] 포맷으로 정리하는 중입니다...", labelForFormat(format))
		if sess.Directive != "" {
			ack += fmt.Sprintf("\n(추가 요청 반영: %s)", truncate(sess.Directive, 80))
		}
		log.Printf("[미팅/end] 포맷 선택 thread=%s format=%s by=%s directive_runes=%d",
			sess.ThreadID, format, interactionCallerUsername(i), len([]rune(sess.Directive)))
		respondInteraction(s, i, ack)
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		keep := finalizeMeeting(ctx, s, summarizer, sess, time.Now(), format, sess.Directive)
		if !keep {
			// 성공: 세션을 정리하지 않고 SelectMode로 reset해서 사용자가 같은 스레드에서
			// [처음 메뉴]로 다음 작업을 이어갈 수 있게 한다.
			log.Printf("[미팅/end] 세션 reset (SelectMode) thread=%s", sess.ThreadID)
			// === Phase 1: DB 세션 CLOSE ===
			// 현재 모델에서 finalize 성공 = 미팅 종료. DB sessions row를 CLOSED로 전환하고
			// in-memory 세션은 SelectMode로 reset해 다음 작업 진입 시 새 DB 세션을 시작한다.
			// (Phase 3 super-session 모델에서는 finalize가 종료가 아니라 도구 호출이 되므로 재검토 필요.)
			persistSessionClose(context.Background(), sess)
			sess.DBSessionID = ""
			sess.Mode = ModeNormal
			sess.State = StateSelectMode
			sess.Notes = nil
			sess.Speakers = nil
			sess.NotesAtLastSticky = 0
			sess.StickyMessageID = ""
			sess.Directive = ""
		} else {
			log.Printf("[미팅/end] 세션 보존 (재시도 대기) thread=%s", sess.ThreadID)
		}

	// === Phase 3 chunk 3B-2b — [주간 추가] in-thread 통합 ===
	// super-session sticky의 [주간 추가] 클릭 → 같은 스레드에 레포 선택 button 노출.
	// 사용자가 레포 선택하면 기존 handleWeeklyRepoSelect → runWeeklyAnalyze 흐름 발동.
	// runWeeklyAnalyze는 sess.Mode == ModeMeeting 감지 시 SubAction lifecycle 적용 + 결과를
	// NoteSource=WeeklyDump로 sess.Notes에 누적 (corpus의 ContextNotes로 분류).
	//
	// TODO(Phase 3 후속): customIDSubActionWeekly로 발급된 레포 button과 legacy mode_weekly로
	// 발급된 button이 customID(weekly_repo:owner/name)만 봐서는 구분 불가. 현재는 super-session
	// sticky가 ModeMeeting 스레드에만 노출되므로 피해 제한적이지만, 구조적 취약점 — 향후 customID에
	// 진입 모드 정보 포함(예: weekly_repo:in_thread:owner/name) 또는 sess.PendingWeeklyRepo에 mode
	// 정보 함께 박제하는 식으로 보강 필요.
	case customIDSubActionWeekly:
		log.Printf("[미팅/subaction_weekly] thread=%s by=%s", sess.ThreadID, interactionCallerUsername(i))
		if len(weeklyRepos) == 0 {
			respondInteraction(s, i, "분석 가능한 레포가 등록되어 있지 않습니다 (git_server.go의 weeklyRepos 확인).")
			return
		}
		header := fmt.Sprintf("주간 분석할 레포를 선택해주세요. 결과는 이 스레드의 corpus에 누적됩니다. (등록된 레포: %d개)", len(weeklyRepos))
		respondInteractionWithComponents(s, i, header, buildWeeklyRepoRows(weeklyRepos))

	// === Phase 3 chunk 3B-2c — [릴리즈 추가] in-thread 통합 ===
	// 기존 handleReleaseEntry 흐름 재사용 — 같은 스레드에서 라인 선택 → 모듈 → bump → 진행.
	// runReleaseFlow가 sess.Mode==ModeMeeting 감지 시 SubAction(Release) lifecycle + 결과 corpus 누적.
	//
	// race 가드 (codex 6차): sess.ReleaseCtx는 단일 인스턴스 — 다중 참석자가 동시에 release 시작하면
	// ctx 덮어쓰기 위험 (A의 confirm button이 B의 모듈로 실행). 진행 중 release가 있으면 reject.
	//
	// stale 처리 (codex 7차): release 완료(PRNumber>0)된 ctx는 새 release를 막지 않도록 reset.
	// 진행 중(PRNumber=0)인 ctx만 reject — 같은 미팅에서 release 여러 번 가능.
	case customIDSubActionRelease:
		// codex review P3 fix: rc.InProgress 사용 — abandoned ReleaseCtx (bump 선택 후 [확인] 안 누름)는
		// 더 이상 사용자를 가두지 않고 새 release 진입 허용. 기존 PRNumber==0 가드는 abandoned/in-flight 구분 X.
		// 3차 race fix: atomic.Bool .Load() — runReleaseFlow goroutine과 cross-access 안전.
		if sess.ReleaseCtx != nil && sess.ReleaseCtx.InProgress.Load() {
			logGuard("release", "single_in_progress", "단일 release 진행 중 — 새 진입 reject",
				lf("thread", sess.ThreadID), lf("user", interactionCallerUsername(i)),
				lf("module", sess.ReleaseCtx.Module.Key))
			respondInteraction(s, i, "현재 진행 중인 릴리즈가 있습니다. 완료 후 다시 시작해주세요. (race 방어 — 동시에 여러 release를 같은 스레드에서 진행 X)")
			return
		}
		// B-3 추가 가드: batch release(InProgress=true)도 단일 release 진입 reject.
		// Selections만 박제된 미진행 batch ctx는 사용자가 마음 바뀐 케이스 — 덮어쓰기 허용.
		if sess.BatchReleaseCtx != nil && sess.BatchReleaseCtx.InProgress.Load() {
			logGuard("release", "batch_in_progress", "batch release 진행 중 — 단일 release 진입 reject",
				lf("thread", sess.ThreadID), lf("user", interactionCallerUsername(i)),
				lf("selected", sess.BatchReleaseCtx.SelectedCount()))
			respondInteraction(s, i, "현재 batch release가 진행 중입니다. 완료 후 다시 시작해주세요.")
			return
		}
		// 진행 중이 아닌 ReleaseCtx (abandoned 또는 완료) / 미진행 batch ctx 모두 reset.
		sess.ReleaseCtx = nil
		sess.BatchReleaseCtx = nil
		handleReleaseEntry(s, i, sess)

	// === Phase 3 chunk 3B-2c — [에이전트] in-thread 통합 ===
	// agent 모드 진입 (사용자 자유 자연어 지시 입력 대기). 메시지 입력 시 runAgentInstruction 호출 →
	// SubAction(Agent) lifecycle + 결과 corpus 누적.
	case customIDSubActionAgent:
		// agent 가드 — GITHUB_TOKEN 미설정 시 fetchAgentContext가 nil deref panic 위험.
		if githubClient == nil {
			logGuard("agent", "github_unconfigured", "GITHUB_TOKEN 미설정 — agent 진입 reject",
				lf("thread", sess.ThreadID), lf("user", interactionCallerUsername(i)))
			respondInteraction(s, i, "GITHUB_TOKEN이 설정되어 있지 않아 에이전트 기능을 사용할 수 없습니다.")
			return
		}
		// LLM 가드 — GPT_API_KEY 누락 시 summarize.Agent에서 c.API() nil deref panic.
		if llmClient == nil {
			logGuard("agent", "llm_unconfigured", "LLM client 미초기화 — agent 진입 reject",
				lf("thread", sess.ThreadID), lf("user", interactionCallerUsername(i)))
			respondInteraction(s, i, "LLM 클라이언트가 초기화되지 않아 에이전트 기능을 사용할 수 없습니다.")
			return
		}
		// per-user pending — 다중 참석자 회의에서 다른 사용자의 일반 발화를 agent 지시로 잘못 처리 방지.
		// 발화 시점에 m.Author.ID와 비교해 일치하는 사람의 다음 1건 메시지만 agent 입력으로 소비.
		clickerUID := userIDFromInteraction(i)
		sess.PendingAgentUserID = clickerUID
		logState("agent", "[AI에게 질문] click — agent 입력 대기 진입", sess.State.String(), "agent_await_input",
			lf("thread", sess.ThreadID), lf("uid", clickerUID), lf("user", interactionCallerUsername(i)))
		logEvent("agent", "pending_set", "agent 입력 owner 박제 (per-user gate)",
			lf("thread", sess.ThreadID), lf("uid", clickerUID))
		sess.State = StateAgentAwaitInput
		respondInteraction(s, i, "에이전트 모드: 자유 자연어 지시를 입력하세요 (예: \"인프라 관련 열려있는 이슈 가져와\"). 결과는 이 스레드의 corpus에 누적됩니다. (button을 누른 본인의 다음 메시지만 지시로 처리)")

	// === Phase 3 chunk 3C — [외부 자료 첨부] 명시 button ===
	// per-user pending — 다중 참석자 회의에서 다른 사용자의 발화가 잘못 ExternalPaste로 분류되는 race 방어.
	// 클릭한 본인의 다음 1건 메시지만 ExternalPaste로 분류 + clear.
	case customIDExternalAttach:
		uid := userIDFromInteraction(i)
		sess.PendingExternalPasteUserID = uid
		logEvent("meeting", "pending_set", "ExternalPaste 활성화 (다음 발화 1건 = 외부 자료)",
			lf("thread", sess.ThreadID), lf("uid", uid), lf("user", interactionCallerUsername(i)))
		respondInteraction(s, i, "본인이 보내는 다음 메시지 1건을 외부 자료로 분류합니다. 회의록·외부 문서 등을 그대로 paste 해주세요. (취소: 다른 button 클릭 또는 발화 1건 후 자동 해제)")

	// === Phase 3 chunk 3D — 명시 [세션 종료] button ===
	// finalize와 분리된 명시 종료. HandleSessionEnd가 sticky 제거 + DB close + in-memory 정리.
	case customIDSessionEnd:
		log.Printf("[세션/end] 클릭 thread=%s by=%s", sess.ThreadID, interactionCallerUsername(i))
		respondInteraction(s, i, "세션을 종료합니다...")
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		HandleSessionEnd(ctx, s, sess)

	// === Phase 2 chunk 3c — 정리본 메시지의 4 포맷 토글 button ===
	// DB에서 SummarizedContent 조회 → 새 포맷 렌더 → 메시지 edit. LLM 재호출 없음.
	case customIDFormatToggleDecisionStatus,
		customIDFormatToggleDiscussion,
		customIDFormatToggleRoleBased,
		customIDFormatToggleFreeform:
		log.Printf("[미팅/format_toggle] 클릭 thread=%s customID=%s by=%s", sess.ThreadID, data.CustomID, interactionCallerUsername(i))
		// 즉시 ack (defer 응답) — 토글은 UX상 매우 빠름 (DB read + render 순수)
		if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseDeferredMessageUpdate,
		}); err != nil {
			log.Printf("[미팅/format_toggle] ERR ack 실패: %v", err)
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		HandleFormatToggle(ctx, s, i, sess, data.CustomID)

	// === Phase 2 chunk 3b — [정리본 통합·토글] 버튼 → SummarizedContent 1회 추출 ===
	// LLM 1회 호출 후 default(decision_status)로 렌더 + 4 포맷 토글 button row 첨부.
	case customIDFinalizeSummarized:
		log.Printf("[미팅/finalize_summarized] 클릭 thread=%s by=%s", sess.ThreadID, interactionCallerUsername(i))
		respondInteraction(s, i, "정리본을 추출하는 중입니다...")
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		keep := FinalizeSummarized(ctx, s, summarizer, sess, time.Now())
		if !keep {
			log.Printf("[미팅/finalize_summarized] 세션 reset thread=%s", sess.ThreadID)
			persistSessionClose(context.Background(), sess)
			sess.DBSessionID = ""
			sess.Mode = ModeNormal
			sess.State = StateSelectMode
			sess.Notes = nil
			sess.Speakers = nil
			sess.NotesAtLastSticky = 0
			sess.StickyMessageID = ""
			sess.Directive = ""
		} else {
			log.Printf("[미팅/finalize_summarized] 세션 보존 (재시도 대기) thread=%s", sess.ThreadID)
		}

	// [추가 요청] 버튼 → 사용자 정리 지시 입력 대기 상태로 전환
	case customIDDirectiveBtn:
		sess.State = StateMeetingAwaitDirective
		log.Printf("[미팅/directive] 입력 대기 진입 thread=%s by=%s", sess.ThreadID, interactionCallerUsername(i))
		respondInteraction(s, i,
			"원하는 정리 방식을 한 메시지로 적어주세요.\n"+
				"예) `프론트엔드/백엔드/기획 H3 섹션으로 묶고, 각 항목은 bullet, 부연은 sub-bullet으로`")

	// [지시 다시 입력] 버튼 → 기존 directive 비우고 다시 입력 대기로
	case customIDDirectiveRetryBtn:
		sess.Directive = ""
		sess.State = StateMeetingAwaitDirective
		log.Printf("[미팅/directive] 재입력 진입 thread=%s by=%s", sess.ThreadID, interactionCallerUsername(i))
		respondInteraction(s, i, "이전 지시를 비웠습니다. 새 정리 지시를 한 메시지로 적어주세요.")

	// sticky/interim 메시지 하단 "중간 요약" 버튼
	case customIDInterimBtn:
		if sess.Mode != ModeMeeting {
			respondInteraction(s, i, "미팅 모드에서만 중간 요약을 사용할 수 있습니다.")
			return
		}
		log.Printf("[미팅/interim] 버튼 클릭 thread=%s by=%s", sess.ThreadID, interactionCallerUsername(i))
		respondInteraction(s, i, "중간 요약을 정리하는 중입니다...")
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		emitInterim(ctx, s, summarizer, sess, time.Now())
	case customIDReleaseEntry:
		// release.go의 [← 라인 다시] button 진입점 — super-session 안에서 release 흐름 재시작.
		handleReleaseEntry(s, i, sess)
	case customIDReleaseConfirm:
		handleReleaseConfirm(s, i, sess)
	case customIDReleaseBackLine:
		handleReleaseBackLine(s, i, sess)
	case customIDReleaseBackModule:
		handleReleaseBackModule(s, i, sess)
	case customIDReleasePollStop:
		handleReleasePollStop(s, i, sess)
	case customIDBatchReleaseStart:
		// B-3 batch release [모두 진행] button — selection 0건 검증 + InProgress 박제 후 (B-4 미구현시)
		// placeholder 안내. B-4 구현 시 실제 4 goroutine 병렬 발사를 호출하도록 교체.
		handleBatchReleaseStart(s, i, sess)

	default:
		respondInteraction(s, i, "알 수 없는 동작입니다.")
	}
}

// handleModalSubmit는 InteractionModalSubmit 타입 인터랙션을 customID로 라우팅한다.
// 모달은 채널/스레드 안에서만 띄울 수 있고, sess는 ChannelID로 조회한다.
//
// 현재 처리하는 모달:
//   - customIDWeeklyPeriodModal: [캘린더] 시작일 입력 — handleWeeklyPeriodModalSubmit
//
// 미정의 customID는 ephemeral 안내 + 로그만 남기고 무시한다.
func handleModalSubmit(s *discordgo.Session, i *discordgo.InteractionCreate) {
	id := i.ModalSubmitData().CustomID

	sess := lookupSession(i.ChannelID)
	if sess == nil {
		respondInteractionEphemeral(s, i, "세션이 만료되었습니다. 다시 시작해주세요.")
		return
	}
	sess.UpdatedAt = time.Now()

	switch id {
	case customIDWeeklyPeriodModal:
		handleWeeklyPeriodModalSubmit(s, i, sess)
	default:
		log.Printf("[modal] WARN 알 수 없는 custom_id=%q channel=%s", id, i.ChannelID)
		respondInteractionEphemeral(s, i, "처리할 수 없는 모달 응답입니다.")
	}
}

// =====================================================================
// 스레드/세션 생성 + 기능 선택
// =====================================================================

// openThread는 mention 한 번이면 super-session(ModeMeeting)을 즉시 시작한다.
//
// D1 정책 (UX 재설계 2026-05): "무엇을 도와드릴까요?" 5 button 메뉴 폐기 — 모든 진입은 super-session.
// 사용자는 sticky button(중간 요약/회의록 정리/GitHub 주간 분석/릴리즈 PR 만들기/AI에게 질문/외부 문서 첨부/
// 세션 종료)으로 모든 작업을 in-thread 수행한다.
func openThread(s *discordgo.Session, m *discordgo.MessageCreate, content string) {
	threadName := "봇 세션"
	if content != "" {
		threadName = truncate(content, 50)
	}

	thread, err := s.MessageThreadStartComplex(m.ChannelID, m.ID, &discordgo.ThreadStart{
		Name:                threadName,
		AutoArchiveDuration: 60,
		Type:                discordgo.ChannelTypeGuildPublicThread,
	})
	if err != nil {
		log.Printf("[discord/openThread] ERR 스레드 생성 실패 channel=%s: %v", m.ChannelID, err)
		s.ChannelMessageSend(m.ChannelID, "이 채널에서는 스레드를 만들 수 없습니다. 텍스트 채널에서 다시 시도해주세요.")
		return
	}

	now := time.Now()
	sess := &Session{
		Mode:      ModeMeeting, // D1: 즉시 super-session
		State:     StateMeeting,
		ThreadID:  thread.ID,
		UserID:    m.Author.ID,
		GuildID:   m.GuildID,
		UpdatedAt: now,
		StartedAt: now, // Phase 3: lifecycle 경과 측정용 (UpdatedAt과 분리)
	}
	sessionsMu.Lock()
	sessions[thread.ID] = sess
	sessionsMu.Unlock()

	// DB 영속화 (best-effort) — 실패해도 in-memory로 진행.
	persistSessionStart(context.Background(), sess)

	log.Printf("[미팅/start] super-session 즉시 진입 thread=%s user=%s (mention 진입)", sess.ThreadID, sess.UserID)
	s.ChannelMessageSend(thread.ID,
		"super-session을 시작합니다. 메시지를 자유롭게 입력하세요. "+
			"하단 sticky button으로 [중간 요약]/[회의록 정리]/[GitHub 주간 분석]/[릴리즈 PR 만들기]/"+
			"[AI에게 질문]/[외부 문서 첨부]/[세션 종료]를 실행할 수 있습니다.")
	sendSticky(s, sess)
}

// =====================================================================
// 텍스트 기반 세션 라우팅 + 상태별 핸들러
// =====================================================================

func handleSession(s *discordgo.Session, m *discordgo.MessageCreate, sess *Session) {
	// D4 button-only 정책: 모든 명령은 sticky button만. universal escape 가드 폐기.
	// 사용자가 "세션 종료" 텍스트 입력해도 일반 미팅 노트로 처리. 종료는 [세션 종료] button만.
	switch sess.State {
	case StateSelectMode:
		handleSelectMode(s, m, sess)
	case StateMeeting:
		handleMeetingMessage(s, m, sess)
	case StateMeetingAwaitDirective:
		handleMeetingDirective(s, m, sess)
	// StateWeeklyAwaitDirective case 폐기 — D2 정책 (handleWeeklyAwaitDirectiveMessage 제거됨)
	case StateAgentAwaitInput:
		// === Phase 3 chunk 3C — Agent per-user 게이트 (super-session safe) ===
		// 정책:
		//   - PendingAgentUserID와 m.Author.ID 일치 → 본인 발화, agent 지시로 소비
		//   - 다른 사용자 발화 → super-session이면 미팅 노트로 우회, 그렇지 않으면 (legacy) 그대로 agent
		//
		// 추가 race 방어 (codex 5차): super-session에서 agent 호출 직전에 State를 StateMeeting으로
		// 복귀시키고 PendingAgentUserID clear. runAgentInstruction은 30~90초 long-running이라 그 동안
		// 도착하는 다른 발화가 StateAgentAwaitInput 상태에서 agent로 잘못 라우팅되는 것 방지.
		if sess.PendingAgentUserID != "" && sess.PendingAgentUserID != m.Author.ID {
			if sess.Mode == ModeMeeting {
				logEvent("agent", "race_deflected", "agent 입력 대기 중 다른 사용자 발화 → 미팅 노트로 우회",
					lf("thread", sess.ThreadID), lf("speaker_uid", m.Author.ID),
					lf("pending_owner_uid", sess.PendingAgentUserID))
				handleMeetingMessage(s, m, sess)
				return
			}
			// 미팅 모드 아닌 legacy는 그대로 agent (이 분기 자체가 super-session 가정 외)
		}
		// 본인 발화 또는 legacy — agent 지시로 소비.
		// super-session: long-running agent 호출 동안 다른 발화 race 차단을 위해 미리 StateMeeting 복귀.
		if sess.Mode == ModeMeeting {
			logState("agent", "agent 입력 소비 직전 — long-running 보호 위해 state 미리 복귀",
				"agent_await_input", "meeting",
				lf("thread", sess.ThreadID), lf("uid", m.Author.ID))
			sess.State = StateMeeting
		}
		logEvent("agent", "pending_consume", "agent 입력 소비 (본인 발화 1건)",
			lf("thread", sess.ThreadID), lf("uid", m.Author.ID), lf("user", m.Author.Username))
		sess.PendingAgentUserID = ""
		handleAgentMessage(s, m, sess)
	}
}

// handleSelectMode는 D1 정책 폐기 후 정상 흐름에서 도달 불가능한 fallback.
//
// openThread/enterSlashMode가 즉시 super-session(StateMeeting)으로 진입하므로 신규 세션은
// StateSelectMode를 거치지 않는다. 다만 finalize 성공 후 sess.State = StateSelectMode로 reset되는
// 경로가 남아있어 (Phase 3+에서 정리 예정), 그 경우 사용자 발화를 그대로 미팅 노트로 처리해
// corpus가 끊기지 않도록 한다 — D4 button-only 정책에 맞춰 텍스트 1/2/3/4 분기는 폐기.
func handleSelectMode(s *discordgo.Session, m *discordgo.MessageCreate, sess *Session) {
	sess.UpdatedAt = time.Now()
	// 사용자가 sticky button을 누르지 않고 텍스트만 보냈다 — D1/D4 정책상 정상 흐름이 아니지만
	// 발화를 잃지 않도록 ModeMeeting/StateMeeting 복원 후 일반 미팅 노트로 처리.
	sess.Mode = ModeMeeting
	sess.State = StateMeeting
	if sess.StickyMessageID == "" {
		sendSticky(s, sess)
	}
	handleMeetingMessage(s, m, sess)
}

// =====================================================================
// 미팅 관련 텍스트 핸들러
// =====================================================================

func handleMeetingMessage(s *discordgo.Session, m *discordgo.MessageCreate, sess *Session) {
	content := strings.TrimSpace(m.Content)
	sess.UpdatedAt = time.Now()

	// D4 button-only — IsMeetingEndCommand 분기 폐기. "미팅 종료" 텍스트는 일반 미팅 노트로 누적.
	// 종료는 sticky [세션 종료] button만 (HandleSessionEnd 경유).

	// === Phase 1 — Source 자동 분류 + role snapshot + DB persist ===
	// 거시 디자인 결정 F(자동) + 결정 6(Source 라벨로 환각 방어).
	// === Phase 3 chunk 3C — 명시 분류 우선 적용 (per-user) ===
	// PendingExternalPasteUserID가 m.Author.ID와 일치할 때만 ExternalPaste 강제.
	// 다중 참석자 회의에서 다른 사용자 발화가 잘못 분류되는 race 방어. 1회성 — 사용 후 clear.
	var source db.NoteSource
	if sess.PendingExternalPasteUserID != "" && sess.PendingExternalPasteUserID == m.Author.ID {
		source = db.SourceExternalPaste
		sess.PendingExternalPasteUserID = ""
		logEvent("meeting", "pending_consume", "ExternalPaste 명시 분류 적용 (button-set 발화 1건)",
			lf("thread", sess.ThreadID), lf("uid", m.Author.ID), lf("user", m.Author.Username),
			lf("runes", len([]rune(content))))
	} else {
		source = classifyMessageSource(content)
	}
	authorRoles := sess.GetOrFetchRoles(s, m.Author.ID)

	// === Phase 4 — cross-role 키워드 detect (best-effort, log만) ===
	// 발화 시점에 키워드 매칭으로 임시 라벨링 — 정리본 추출 시 LLM이 최종 확정.
	// 채팅마다 LLM 호출 X (결정 7). 단순 substring 매칭으로 80% 케이스 잡음.
	var detectedTargets []string
	if source == db.SourceHuman {
		detectedTargets = DetectTargetRoles(content)
		if len(detectedTargets) > 0 && IsCrossRoleHint(authorRoles, detectedTargets) {
			log.Printf("[미팅/cross-role] detected thread=%s author=%s origin_roles=%v target_roles=%v",
				sess.ThreadID, m.Author.Username, authorRoles, detectedTargets)
		}
	}

	note := Note{
		Author:      m.Author.Username,
		Content:     content,
		AuthorID:    m.Author.ID,
		AuthorRoles: authorRoles,
		Source:      source,
	}
	idx, stored := sess.AddNoteWithMeta(note)
	// stored는 정규화 적용된 노트 사본 (Timestamp 등 채워진 상태). 락 밖에서 안전하게 persist.
	persistNote(context.Background(), sess, stored)

	log.Printf("[미팅/note] thread=%s idx=%d author=%s roles=%v source=%s targets=%v runes=%d content=%q",
		sess.ThreadID, idx, m.Author.Username, authorRoles, source, detectedTargets,
		len([]rune(content)), truncate(content, 80))

	// Sticky 컨트롤 메시지 threshold 체크: N개마다 [중간 요약][미팅 종료] 버튼을 최신 하단으로 재전송.
	maybeRefreshSticky(s, sess)
}

// handleMeetingDirective는 사용자가 [추가 요청] 클릭 후 입력하는 정리 지시를 받는다.
// 입력된 메시지를 sess.Directive에 저장하고 다시 미팅 상태로 복귀하면서
// directive 적용 후 prompt(4 포맷 + [지시 다시 입력])를 노출한다.
//
// 빈 입력은 무시하고 다시 안내. "취소" 입력 시 directive 미적용 상태로 일반 prompt 복귀.
func handleMeetingDirective(s *discordgo.Session, m *discordgo.MessageCreate, sess *Session) {
	content := strings.TrimSpace(m.Content)
	sess.UpdatedAt = time.Now()

	if content == "" {
		s.ChannelMessageSend(m.ChannelID, "지시가 비어 있습니다. 다시 입력해주세요.")
		return
	}
	if content == "취소" {
		sess.Directive = ""
		sess.State = StateMeeting
		log.Printf("[미팅/directive] 취소 thread=%s by=%s", sess.ThreadID, m.Author.Username)
		s.ChannelMessageSendComplex(m.ChannelID, &discordgo.MessageSend{
			Content:    "추가 요청을 취소했습니다. 포맷을 선택해주세요.",
			Components: finalizePromptComponents(),
		})
		return
	}

	sess.Directive = content
	sess.State = StateMeeting
	log.Printf("[미팅/directive] 캡처 thread=%s by=%s runes=%d",
		sess.ThreadID, m.Author.Username, len([]rune(content)))

	s.ChannelMessageSendComplex(m.ChannelID, &discordgo.MessageSend{
		Content:    fmt.Sprintf("지시를 반영했습니다.\n> %s\n\n포맷을 선택하면 위 지시가 함께 적용됩니다.", truncate(content, 200)),
		Components: finalizePromptWithDirectiveComponents(),
	})
}

// handleMeetingEnd는 텍스트 "미팅 종료" 입력 시 호출된다. 즉시 finalize하지
// 않고 4 포맷 선택 prompt를 스레드에 띄운다 (버튼 클릭으로 진입한 케이스와 동일).
// 사용자는 그중 한 버튼을 눌러야 finalize가 실행된다.
func handleMeetingEnd(s *discordgo.Session, m *discordgo.MessageCreate, sess *Session) {
	log.Printf("[미팅/end] 텍스트 종료 명령 수신 thread=%s by=%s notes=%d",
		sess.ThreadID, m.Author.Username, len(sess.SnapshotNotes()))
	deleteStickyIfPresent(s, sess)

	s.ChannelMessageSendComplex(m.ChannelID, &discordgo.MessageSend{
		Content: "어떤 포맷으로 정리할까요?\n\n" +
			"**결정+진행**: 결정사항과 완료/진행/예정/이슈 4분할 (스프린트, 작업 공유)\n" +
			"**논의**: 토픽별 논의 흐름 + 도출 관점 (1on1, 회고, 브레인스토밍)\n" +
			"**역할별**: 참석자별 결정/액션/공유 (역할 분담, 스탠드업)\n" +
			"**자율**: LLM이 회의 성격 보고 자유 정리",
		Components: finalizePromptComponents(),
	})
}

// =====================================================================
// Interaction 응답 헬퍼 + 상태 조회 / 주간 리포트 (목킹)
// =====================================================================

func respondInteraction(s *discordgo.Session, i *discordgo.InteractionCreate, content string) {
	s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Content: content,
		},
	})
}

// respondInteractionWithComponents는 이미 만들어진 component 배열을 그대로
// 붙여서 interaction에 응답한다 (4 포맷 prompt에 사용).
func respondInteractionWithComponents(
	s *discordgo.Session,
	i *discordgo.InteractionCreate,
	content string,
	components []discordgo.MessageComponent,
) {
	s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Content:    content,
			Components: components,
		},
	})
}

// labelForFormat은 사용자에게 표시할 포맷 라벨을 반환한다.
func labelForFormat(f llm.NoteFormat) string {
	switch f {
	case llm.FormatDecisionStatus:
		return "결정+진행"
	case llm.FormatDiscussion:
		return "논의"
	case llm.FormatRoleBased:
		return "역할별"
	case llm.FormatFreeform:
		return "자율"
	default:
		return f.String()
	}
}

// respondInteractionWithStatus 폐기 — D1 정책 (UX 재설계 2026-05).
// "mode_status" 라우팅과 home menu의 [상태 조회] button이 사라져서 진입점 없음.
// 향후 sticky에 별도 [상태 조회] button을 추가하려면 이 함수를 부활시킬 수 있음.

