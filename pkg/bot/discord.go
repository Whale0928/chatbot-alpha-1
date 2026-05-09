package bot

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"chatbot-alpha-1/pkg/llm"

	"github.com/bwmarrin/discordgo"
)

// =====================================================================
// 이벤트 엔트리 핸들러
// =====================================================================

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
		respondInteraction(s, i, "세션이 만료되었습니다. 다시 시작해주세요.")
		return
	}

	sess.UpdatedAt = time.Now()

	// 동적 custom_id 처리: weekly_repo:owner/name 등 prefix 매칭은 switch case로 표현 불가하므로 먼저 분기.
	if isWeeklyRepoCustomID(data.CustomID) {
		handleWeeklyRepoSelect(s, i, extractWeeklyRepoFullName(data.CustomID))
		return
	}

	// 주간 follow-up 버튼들. weekly.go의 핸들러에 위임.
	switch data.CustomID {
	case customIDWeeklyDirectiveBtn:
		handleWeeklyDirective(s, i, sess)
		return
	case customIDWeeklyPeriodPromptBtn:
		handleWeeklyPeriodPrompt(s, i, sess)
		return
	case customIDWeeklyPeriod14:
		handleWeeklyPeriod(s, i, sess, 14)
		return
	case customIDWeeklyPeriod30:
		handleWeeklyPeriod(s, i, sess, 30)
		return
	case customIDWeeklyRetryBtn:
		handleWeeklyRetry(s, i, sess)
		return
	case customIDWeeklyToMeetingBtn:
		handleWeeklyToMeeting(s, i, sess)
		return
	case customIDWeeklyCloseStartBtn:
		handleWeeklyCloseStart(s, i, sess)
		return
	case customIDWeeklyCloseConfirmBtn:
		handleWeeklyCloseConfirm(s, i, sess)
		return
	case customIDHomeBtn:
		handleHome(s, i, sess)
		return
	}

	switch data.CustomID {
	// 기능 선택
	case "mode_meeting":
		sess.Mode = ModeMeeting
		sess.State = StateMeeting
		sess.Notes = nil
		sess.Speakers = nil
		sess.NotesAtLastSticky = 0
		sess.StickyMessageID = ""
		sess.Directive = ""
		log.Printf("[미팅/start] 미팅 모드 진입 thread=%s user=%s", sess.ThreadID, sess.UserID)
		respondInteraction(s, i, "미팅을 시작합니다. 메시지를 자유롭게 입력하세요. 하단 [중간 요약] 버튼으로 진행 상황을 정리할 수 있고, [미팅 종료] 버튼 또는 \"미팅 종료\" 입력으로 마무리할 수 있습니다.")
		// 초기 sticky 컨트롤 메시지 전송 - 맨 아래에 [중간 요약][미팅 종료] 버튼이 항상 보이도록.
		sendSticky(s, sess)

	// interim/sticky 메시지 하단 "미팅 종료" 버튼 → 4 포맷 선택 prompt
	case customIDMeetingEndBtn:
		log.Printf("[미팅/end] 종료 버튼 클릭 thread=%s by=%s", sess.ThreadID, i.Member.User.Username)
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
			sess.ThreadID, format, i.Member.User.Username, len([]rune(sess.Directive)))
		respondInteraction(s, i, ack)
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		keep := finalizeMeeting(ctx, s, summarizer, sess, time.Now(), format, sess.Directive)
		if !keep {
			// 성공: 세션을 정리하지 않고 SelectMode로 reset해서 사용자가 같은 스레드에서
			// [처음 메뉴]로 다음 작업을 이어갈 수 있게 한다.
			log.Printf("[미팅/end] 세션 reset (SelectMode) thread=%s", sess.ThreadID)
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

	// [추가 요청] 버튼 → 사용자 정리 지시 입력 대기 상태로 전환
	case customIDDirectiveBtn:
		sess.State = StateMeetingAwaitDirective
		log.Printf("[미팅/directive] 입력 대기 진입 thread=%s by=%s", sess.ThreadID, i.Member.User.Username)
		respondInteraction(s, i,
			"원하는 정리 방식을 한 메시지로 적어주세요.\n"+
				"예) `프론트엔드/백엔드/기획 H3 섹션으로 묶고, 각 항목은 bullet, 부연은 sub-bullet으로`")

	// [지시 다시 입력] 버튼 → 기존 directive 비우고 다시 입력 대기로
	case customIDDirectiveRetryBtn:
		sess.Directive = ""
		sess.State = StateMeetingAwaitDirective
		log.Printf("[미팅/directive] 재입력 진입 thread=%s by=%s", sess.ThreadID, i.Member.User.Username)
		respondInteraction(s, i, "이전 지시를 비웠습니다. 새 정리 지시를 한 메시지로 적어주세요.")

	// sticky/interim 메시지 하단 "중간 요약" 버튼
	case customIDInterimBtn:
		if sess.Mode != ModeMeeting {
			respondInteraction(s, i, "미팅 모드에서만 중간 요약을 사용할 수 있습니다.")
			return
		}
		log.Printf("[미팅/interim] 버튼 클릭 thread=%s by=%s", sess.ThreadID, i.Member.User.Username)
		respondInteraction(s, i, "중간 요약을 정리하는 중입니다...")
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		emitInterim(ctx, s, summarizer, sess, time.Now())
	case "mode_weekly":
		respondInteraction(s, i, "주간 정리할 레포를 선택해주세요.")
		sendWeeklyRepoButtons(s, channelID)
	case customIDAgentBtn:
		handleAgent(s, i, sess)
	case "mode_status":
		respondInteractionWithStatus(s, i)
		// 세션은 정리하지 않음 — 사용자가 [처음 메뉴]로 다른 작업 계속 가능.

	default:
		respondInteraction(s, i, "알 수 없는 동작입니다.")
	}
}

// =====================================================================
// 스레드/세션 생성 + 기능 선택
// =====================================================================

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

	sess := &Session{
		Mode:      ModeNormal,
		State:     StateSelectMode,
		ThreadID:  thread.ID,
		UserID:    m.Author.ID,
		UpdatedAt: time.Now(),
	}
	sessionsMu.Lock()
	sessions[thread.ID] = sess
	sessionsMu.Unlock()

	s.ChannelMessageSendComplex(thread.ID, &discordgo.MessageSend{
		Content: "무엇을 도와드릴까요?",
		Components: []discordgo.MessageComponent{
			discordgo.ActionsRow{
				Components: []discordgo.MessageComponent{
					discordgo.Button{Label: "미팅", Style: discordgo.PrimaryButton, CustomID: "mode_meeting"},
					discordgo.Button{Label: "주간 정리", Style: discordgo.PrimaryButton, CustomID: "mode_weekly"},
					discordgo.Button{Label: "에이전트", Style: discordgo.SuccessButton, CustomID: customIDAgentBtn},
					discordgo.Button{Label: "상태 조회", Style: discordgo.SecondaryButton, CustomID: "mode_status"},
				},
			},
		},
	})
}

// =====================================================================
// 텍스트 기반 세션 라우팅 + 상태별 핸들러
// =====================================================================

func handleSession(s *discordgo.Session, m *discordgo.MessageCreate, sess *Session) {
	switch sess.State {
	case StateSelectMode:
		handleSelectMode(s, m, sess)
	case StateMeeting:
		handleMeetingMessage(s, m, sess)
	case StateMeetingAwaitDirective:
		handleMeetingDirective(s, m, sess)
	case StateWeeklyAwaitDirective:
		handleWeeklyAwaitDirectiveMessage(s, m, sess)
	case StateAgentAwaitInput:
		handleAgentMessage(s, m, sess)
	}
}

// handleSelectMode: 텍스트 입력 폴백 (버튼 못 누른 경우)
func handleSelectMode(s *discordgo.Session, m *discordgo.MessageCreate, sess *Session) {
	content := strings.TrimSpace(m.Content)
	sess.UpdatedAt = time.Now()

	switch content {
	case "1":
		sess.Mode = ModeMeeting
		sess.State = StateMeeting
		sess.Notes = nil
		sess.Speakers = nil
		s.ChannelMessageSend(m.ChannelID, "미팅을 시작합니다. \"미팅 종료\"로 마무리하세요.")
	case "2":
		sendWeeklyRepoButtons(s, m.ChannelID)
	case "3":
		sess.State = StateAgentAwaitInput
		s.ChannelMessageSend(m.ChannelID, "에이전트 모드: 자유롭게 지시를 입력해주세요. (예: 워크스페이스에서 인프라 관련 열려있는 이슈들 가져와)")
	case "4":
		s.ChannelMessageSend(m.ChannelID, "상태 조회 - 버튼을 눌러주세요.")
	default:
		s.ChannelMessageSend(m.ChannelID, "버튼을 선택하거나 1~4를 입력해주세요.")
	}
}

// =====================================================================
// 미팅 관련 텍스트 핸들러
// =====================================================================

func handleMeetingMessage(s *discordgo.Session, m *discordgo.MessageCreate, sess *Session) {
	content := strings.TrimSpace(m.Content)
	sess.UpdatedAt = time.Now()

	if IsMeetingEndCommand(content) {
		handleMeetingEnd(s, m, sess)
		return
	}

	// 조용히 수집 (Author 포함)
	idx := sess.AddNote(m.Author.Username, content)
	log.Printf("[미팅/note] thread=%s idx=%d author=%s runes=%d content=%q",
		sess.ThreadID, idx, m.Author.Username, len([]rune(content)), truncate(content, 80))

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

func respondInteractionWithStatus(s *discordgo.Session, i *discordgo.InteractionCreate) {
	respondInteractionWithRow(s, i, `**[bottle-note 현황]** (목킹)

Open 이슈: 42건
- bug: 9건 / feature: 30건 / fix: 6건
- 담당자 없음: 42건
- 라벨 누락: 19건

최근 활동: 2026-04-09 #223 생성

지연 경고:
- #148 어드민 리뷰 발행 - 4개월 경과, 담당자 없음`,
		discordgo.Button{Label: "처음 메뉴", Style: discordgo.SecondaryButton, CustomID: customIDHomeBtn},
	)
}

