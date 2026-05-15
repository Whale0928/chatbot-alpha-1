package bot

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
)

// =====================================================================
// 슬래시 명령어 라우팅 + 세션 생성
// =====================================================================
//
// D1 정책 (UX 재설계 2026-05): 모든 슬래시 진입은 super-session(ModeMeeting) + sticky.
// standalone weekly/agent/release 흐름 폐기 — sticky의 sub-action button으로 통일.
//
// /meeting   → super-session 시작 (default 안내)
// /weekly    → super-session 시작 + sticky [GitHub 주간 분석] 안내
// /agent     → super-session 시작 + sticky [AI에게 질문] 안내. instruction 옵션 있으면 즉시 실행
// /release   → super-session 시작 + sticky [릴리즈 PR 만들기] 안내
// /session   → super-session 시작 (default 안내, /meeting과 동일)
//
// 모든 명령어는 채널에서 호출되어 새 스레드를 만들고 그 안에서 흐름을 진행한다.
// 사용자에게는 ephemeral followup으로 스레드 링크만 보내고, 본 흐름은 스레드 안에서.

// sessionEntry는 슬래시 명령어가 어떤 모드로 진입할지 식별한다.
type sessionEntry int

const (
	sessionEntryHome sessionEntry = iota
	sessionEntryMeeting
	sessionEntryWeekly
	sessionEntryAgent
	sessionEntryRelease
)

// handleSlashCommand는 InteractionApplicationCommand를 라우팅한다.
// 명령어별로 entry와 옵션을 결정한 뒤 startSlashSession에 위임한다.
func handleSlashCommand(s *discordgo.Session, i *discordgo.InteractionCreate) {
	data := i.ApplicationCommandData()
	switch data.Name {
	case "meeting":
		startSlashSession(s, i, sessionEntryMeeting, "")
	case "weekly":
		startSlashSession(s, i, sessionEntryWeekly, "")
	case "agent":
		startSlashSession(s, i, sessionEntryAgent, getStringOption(i, "instruction"))
	case "session":
		startSlashSession(s, i, sessionEntryHome, "")
	case "release":
		startSlashSession(s, i, sessionEntryRelease, "")
	default:
		respondInteractionEphemeral(s, i, fmt.Sprintf("알 수 없는 명령어: /%s", data.Name))
	}
}

// startSlashSession은 슬래시 명령어 공통 진입 흐름을 처리한다.
//  1. ephemeral ack (호출자에게만 보임)
//  2. 채널에 새 스레드 생성 (메시지 없는 단독 스레드)
//  3. 세션 등록 (스레드 ID 키)
//  4. 호출자에게 스레드 링크 followup
//  5. entry별 모드 진입 (스레드 안에서 메시지/sticky/버튼 발사)
//
// 스레드 안에서 슬래시 명령어를 호출한 경우(이미 세션 컨텍스트인 곳)는
// 새 스레드를 만들지 않고 사용자에게 안내만 한다 — 세션 중첩을 막기 위함.
func startSlashSession(s *discordgo.Session, i *discordgo.InteractionCreate, entry sessionEntry, agentInstruction string) {
	if i.ChannelID == "" {
		respondInteractionEphemeral(s, i, "채널 정보를 확인할 수 없습니다.")
		return
	}

	// 이미 세션이 있는 채널(=스레드)에서 슬래시를 호출한 경우는 스레드를 또 만들지 않고 차단.
	// 스레드 안에서는 sticky button으로 모든 sub-action을 사용해야 한다 (D1/D4 정책).
	if existing := lookupSession(i.ChannelID); existing != nil {
		respondInteractionEphemeral(s, i,
			"이미 세션이 진행 중인 스레드입니다. 일반 채널에서 슬래시 명령어를 사용해주세요.")
		return
	}

	if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Content: "스레드를 생성합니다...",
			Flags:   discordgo.MessageFlagsEphemeral,
		},
	}); err != nil {
		log.Printf("[slash] ERR ack interaction: %v", err)
		return
	}

	thread, err := s.ThreadStartComplex(i.ChannelID, &discordgo.ThreadStart{
		Name:                slashThreadName(entry),
		AutoArchiveDuration: 60,
		Type:                discordgo.ChannelTypeGuildPublicThread,
	})
	if err != nil {
		log.Printf("[slash] ERR ThreadStartComplex channel=%s: %v", i.ChannelID, err)
		_, _ = s.FollowupMessageCreate(i.Interaction, true, &discordgo.WebhookParams{
			Content: fmt.Sprintf("스레드 생성 실패: %v\n(텍스트 채널인지, 봇 권한이 있는지 확인해주세요)", err),
			Flags:   discordgo.MessageFlagsEphemeral,
		})
		return
	}

	userID := slashCallerUserID(i)
	now := time.Now()
	sess := &Session{
		Mode:      ModeNormal,
		State:     StateSelectMode,
		ThreadID:  thread.ID,
		UserID:    userID,
		GuildID:   i.GuildID,
		UpdatedAt: now,
		StartedAt: now, // Phase 3: lifecycle 경과 측정용
	}
	sessionsMu.Lock()
	sessions[thread.ID] = sess
	sessionsMu.Unlock()
	log.Printf("[slash] 세션 생성 thread=%s entry=%s user=%s guild=%s", thread.ID, entryName(entry), userID, i.GuildID)

	// DB 영속화 (best-effort) — 실패해도 in-memory로 진행.
	persistSessionStart(context.Background(), sess)

	if _, err := s.FollowupMessageCreate(i.Interaction, true, &discordgo.WebhookParams{
		Content: fmt.Sprintf("스레드를 생성했습니다 → <#%s>", thread.ID),
		Flags:   discordgo.MessageFlagsEphemeral,
	}); err != nil {
		log.Printf("[slash] WARN followup 실패 thread=%s: %v", thread.ID, err)
	}

	enterSlashMode(s, sess, entry, agentInstruction)
}

// enterSlashMode는 entry별로 스레드 안에서 super-session 진입을 발사한다.
//
// D1 정책 (UX 재설계 2026-05): 모든 slash 진입은 super-session(ModeMeeting) 시작 + sticky.
// 사용자는 sticky button으로 모든 sub-action을 in-thread 호출. standalone weekly/agent/release
// 흐름은 폐기 — 같은 button을 sticky에서 누르는 게 동일 효과 + corpus 누적의 장점이 있음.
//
// 예외: /agent instruction:... 옵션 즉시 실행은 super-session 진입 + sticky 발사 후 곧바로
// runAgentInstruction을 호출해 사용자가 한 번에 결과를 받게 한다.
func enterSlashMode(s *discordgo.Session, sess *Session, entry sessionEntry, agentInstruction string) {
	// 모든 entry를 super-session으로 통일.
	sess.Mode = ModeMeeting
	sess.State = StateMeeting

	intro := superSessionIntro(entry)
	if _, err := s.ChannelMessageSend(sess.ThreadID, intro); err != nil {
		log.Printf("[slash/intro] ERR thread=%s: %v", sess.ThreadID, err)
	}
	sendSticky(s, sess)

	// /agent instruction:... 옵션은 즉시 실행 — sticky까지 갖춰진 super-session에서 첫 sub-action.
	// runAgentInstruction은 끝나면 본인 흐름에서 sticky 재발사 (A-8).
	if entry == sessionEntryAgent {
		if agentInstruction = strings.TrimSpace(agentInstruction); agentInstruction != "" {
			if githubClient == nil {
				s.ChannelMessageSend(sess.ThreadID, "GITHUB_TOKEN이 설정되어 있지 않아 에이전트 sub-action을 사용할 수 없습니다.")
				return
			}
			if llmClient == nil {
				s.ChannelMessageSend(sess.ThreadID, "LLM 클라이언트가 초기화되지 않았습니다.")
				return
			}
			runAgentInstruction(s, sess, agentInstruction, "slash")
		}
	}
}

// superSessionIntro는 slash entry별로 약간 다른 안내 메시지를 만든다.
// 본문은 동일하게 sticky button 사용을 안내하되, 첫 줄에서 사용자가 입력한 slash 의도를 반영해
// 어색함을 줄인다 (/weekly로 들어왔는데 "미팅을 시작합니다" 처럼 보이는 회귀 방어).
func superSessionIntro(entry sessionEntry) string {
	base := "super-session을 시작합니다. 메시지를 자유롭게 입력하세요. 하단 sticky button으로 [중간 요약]/[회의록 정리]/[GitHub 주간 분석]/[릴리즈 PR 만들기]/[AI에게 질문]/[외부 문서 첨부]/[세션 종료]를 실행할 수 있습니다."
	switch entry {
	case sessionEntryWeekly:
		return "super-session 시작 — 주간 분석을 원하시면 sticky의 [GitHub 주간 분석] button을 눌러주세요.\n\n" + base
	case sessionEntryAgent:
		return "super-session 시작 — 에이전트에게 질문하려면 sticky의 [AI에게 질문] button을 눌러주세요.\n\n" + base
	case sessionEntryRelease:
		return "super-session 시작 — 릴리즈 PR을 만들려면 sticky의 [릴리즈 PR 만들기] button을 눌러주세요.\n\n" + base
	default:
		return base
	}
}

// homeMenuComponents 폐기 — D1 정책 (UX 재설계 2026-05).
// 5 button 메뉴(미팅/주간 정리/에이전트/릴리즈/상태 조회)는 super-session 진입으로 통일됨.
// 동일 기능은 sticky button (중간 요약/회의록 정리/GitHub 주간 분석/릴리즈 PR 만들기/AI에게 질문)으로 노출.

// slashThreadName은 entry별 스레드 이름을 결정한다. Discord에서 스레드 이름이 비면 안 되므로 fallback 포함.
func slashThreadName(entry sessionEntry) string {
	switch entry {
	case sessionEntryMeeting:
		return "미팅"
	case sessionEntryWeekly:
		return "주간 정리"
	case sessionEntryAgent:
		return "에이전트"
	case sessionEntryRelease:
		return "릴리즈"
	default:
		return "봇 세션"
	}
}

// entryName은 로그에 사람이 읽을 수 있는 entry 식별자를 출력한다.
func entryName(entry sessionEntry) string {
	switch entry {
	case sessionEntryMeeting:
		return "meeting"
	case sessionEntryWeekly:
		return "weekly"
	case sessionEntryAgent:
		return "agent"
	case sessionEntryRelease:
		return "release"
	default:
		return "home"
	}
}

// slashCallerUserID는 InteractionCreate에서 호출자 user ID를 추출한다.
// 길드 채널에서는 i.Member.User, DM에서는 i.User에 들어 있다 (둘 다 비어 있으면 "").
func slashCallerUserID(i *discordgo.InteractionCreate) string {
	if i.Member != nil && i.Member.User != nil {
		return i.Member.User.ID
	}
	if i.User != nil {
		return i.User.ID
	}
	return ""
}

// getStringOption은 ApplicationCommandData에서 이름이 일치하는 string 옵션 값을 반환한다.
// 옵션이 없거나 string이 아니면 "" 반환.
func getStringOption(i *discordgo.InteractionCreate, name string) string {
	for _, opt := range i.ApplicationCommandData().Options {
		if opt.Name == name && opt.Type == discordgo.ApplicationCommandOptionString {
			return opt.StringValue()
		}
	}
	return ""
}

// respondInteractionEphemeral는 호출자에게만 보이는 응답을 보낸다.
// 슬래시 명령어 검증 실패/거부 케이스에서 채널을 어지럽히지 않으려 사용.
func respondInteractionEphemeral(s *discordgo.Session, i *discordgo.InteractionCreate, content string) {
	if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Content: content,
			Flags:   discordgo.MessageFlagsEphemeral,
		},
	}); err != nil {
		log.Printf("[slash] ERR ephemeral response: %v", err)
	}
}
