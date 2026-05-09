package bot

import (
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
// /meeting   → 즉시 미팅 모드 진입 + sticky
// /weekly    → 주간 정리 레포 선택 버튼
// /agent     → instruction 옵션 있으면 즉시 실행, 없으면 입력 대기
// /session   → @멘션과 동일한 빈 세션 + 메뉴
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
	// 스레드 안에서는 [처음 메뉴] 버튼이나 텍스트 명령으로 모드 전환을 사용해야 한다.
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
	sess := &Session{
		Mode:      ModeNormal,
		State:     StateSelectMode,
		ThreadID:  thread.ID,
		UserID:    userID,
		UpdatedAt: time.Now(),
	}
	sessionsMu.Lock()
	sessions[thread.ID] = sess
	sessionsMu.Unlock()
	log.Printf("[slash] 세션 생성 thread=%s entry=%s user=%s", thread.ID, entryName(entry), userID)

	if _, err := s.FollowupMessageCreate(i.Interaction, true, &discordgo.WebhookParams{
		Content: fmt.Sprintf("스레드를 생성했습니다 → <#%s>", thread.ID),
		Flags:   discordgo.MessageFlagsEphemeral,
	}); err != nil {
		log.Printf("[slash] WARN followup 실패 thread=%s: %v", thread.ID, err)
	}

	enterSlashMode(s, sess, entry, agentInstruction)
}

// enterSlashMode는 entry별로 스레드 안에서 첫 메시지/버튼/sticky를 발사한다.
// 사전 조건: sess가 sessions 맵에 등록되어 있고 thread가 생성된 직후.
func enterSlashMode(s *discordgo.Session, sess *Session, entry sessionEntry, agentInstruction string) {
	switch entry {
	case sessionEntryHome:
		if _, err := s.ChannelMessageSendComplex(sess.ThreadID, &discordgo.MessageSend{
			Content:    "무엇을 도와드릴까요?",
			Components: homeMenuComponents(),
		}); err != nil {
			log.Printf("[slash/home] ERR send menu: %v", err)
		}

	case sessionEntryMeeting:
		sess.Mode = ModeMeeting
		sess.State = StateMeeting
		if _, err := s.ChannelMessageSend(sess.ThreadID,
			"미팅을 시작합니다. 메시지를 자유롭게 입력하세요. "+
				"하단 [중간 요약] 버튼으로 진행 상황을 정리할 수 있고, "+
				"[미팅 종료] 버튼 또는 \"미팅 종료\" 입력으로 마무리할 수 있습니다."); err != nil {
			log.Printf("[slash/meeting] ERR send intro: %v", err)
		}
		sendSticky(s, sess)

	case sessionEntryWeekly:
		sendWeeklyRepoButtons(s, sess.ThreadID)

	case sessionEntryAgent:
		if agentInstruction = strings.TrimSpace(agentInstruction); agentInstruction != "" {
			runAgentInstruction(s, sess, agentInstruction, "slash")
			return
		}
		// instruction 옵션이 없으면 텍스트 입력 대기 흐름. 검증은 handleAgent와 동일하게 수행.
		if githubClient == nil {
			s.ChannelMessageSend(sess.ThreadID, "GITHUB_TOKEN이 설정되어 있지 않아 에이전트를 시작할 수 없습니다.")
			return
		}
		if llmClient == nil {
			s.ChannelMessageSend(sess.ThreadID, "LLM 클라이언트가 초기화되지 않았습니다.")
			return
		}
		sess.State = StateAgentAwaitInput
		s.ChannelMessageSend(sess.ThreadID,
			"에이전트 모드로 진입했습니다. 자유롭게 지시해주세요.\n"+
				"예) `워크스페이스에서 인프라 관련 열려있는 이슈들 가져와`\n"+
				"`취소` 입력 시 종료")
	}
}

// homeMenuComponents는 [처음 메뉴]에 노출되는 4 버튼 행을 만든다.
// openThread/handleHome과 동일한 셋. 슬래시 /session에서 재사용한다.
func homeMenuComponents() []discordgo.MessageComponent {
	return []discordgo.MessageComponent{
		discordgo.ActionsRow{
			Components: []discordgo.MessageComponent{
				discordgo.Button{Label: "미팅", Style: discordgo.PrimaryButton, CustomID: "mode_meeting"},
				discordgo.Button{Label: "주간 정리", Style: discordgo.PrimaryButton, CustomID: "mode_weekly"},
				discordgo.Button{Label: "에이전트", Style: discordgo.SuccessButton, CustomID: customIDAgentBtn},
				discordgo.Button{Label: "상태 조회", Style: discordgo.SecondaryButton, CustomID: "mode_status"},
			},
		},
	}
}

// slashThreadName은 entry별 스레드 이름을 결정한다. Discord에서 스레드 이름이 비면 안 되므로 fallback 포함.
func slashThreadName(entry sessionEntry) string {
	switch entry {
	case sessionEntryMeeting:
		return "미팅"
	case sessionEntryWeekly:
		return "주간 정리"
	case sessionEntryAgent:
		return "에이전트"
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
