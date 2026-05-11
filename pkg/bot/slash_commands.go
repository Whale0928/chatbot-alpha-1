package bot

import (
	"fmt"
	"log"

	"github.com/bwmarrin/discordgo"
)

// =====================================================================
// 슬래시 명령어 정의 + 등록
// =====================================================================
//
// 흐름:
//   봇 기동 시 registerSlashCommands → Discord에 명령어 등록
//   사용자 /meeting → InteractionApplicationCommand → handleSlashCommand
//     → startSlashSession (스레드 생성 + 세션 등록 + entry별 진입)
//
// 명령어 이름 제약: Discord는 슬래시 이름에 ASCII (영문 소문자/숫자/_/-)만 허용한다.
// 한국어 별칭은 채택 불가하므로 영문 이름 + description만 한국어로 운영한다.

// slashCommands는 Discord에 등록할 명령어 정의. registerSlashCommands에서 순회 등록한다.
var slashCommands = []*discordgo.ApplicationCommand{
	{
		Name:        "meeting",
		Description: "미팅 모드를 즉시 시작합니다 (스레드 생성 + sticky 컨트롤)",
	},
	{
		Name:        "weekly",
		Description: "주간 정리: 등록된 레포 선택 버튼을 띄웁니다",
	},
	{
		Name:        "agent",
		Description: "에이전트: 등록 레포 데이터 + 자연어 지시",
		Options: []*discordgo.ApplicationCommandOption{
			{
				Type:        discordgo.ApplicationCommandOptionString,
				Name:        "instruction",
				Description: "지시문 (생략 시 입력 대기 상태로 진입)",
				Required:    false,
			},
		},
	},
	{
		Name:        "session",
		Description: "빈 봇 세션을 시작합니다 (@멘션과 동일한 메뉴)",
	},
	{
		Name:        "release",
		Description: "릴리즈 봇: 모듈/bump 선택 후 PR 생성 + 머지 추적",
	},
}

// registerSlashCommands는 봇 기동 직후 명령어를 Discord에 등록한다.
// guildID가 비어있으면 글로벌 등록(전파에 최대 1시간), 비어있지 않으면 해당 길드에 즉시 등록한다.
//
// 동일 이름 명령어가 이미 등록되어 있으면 ApplicationCommandCreate가 덮어쓰므로
// 별도 cleanup이 필요하지 않다. 봇 종료 시 명령어를 남겨두면 사용자에게는 그대로 노출되지만
// 봇이 오프라인이면 호출이 실패하는 정상 동작.
func registerSlashCommands(s *discordgo.Session, guildID string) error {
	if s.State == nil || s.State.User == nil {
		return fmt.Errorf("Discord 세션이 아직 준비되지 않았습니다 (s.Open() 이후 호출 필요)")
	}
	appID := s.State.User.ID
	scope := "global"
	if guildID != "" {
		scope = "guild=" + guildID
	}
	for _, def := range slashCommands {
		cmd, err := s.ApplicationCommandCreate(appID, guildID, def)
		if err != nil {
			return fmt.Errorf("슬래시 명령어 등록 실패 /%s: %w", def.Name, err)
		}
		log.Printf("[slash] 등록 완료 /%s (%s, id=%s)", cmd.Name, scope, cmd.ID)
	}
	return nil
}
