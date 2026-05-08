package cmd

import (
	"chatbot-alpha-1/pkg/bot"

	"github.com/spf13/cobra"
)

// newBotCmd는 'bot' 서브커맨드. bot.Run을 명시적으로 호출하고 싶을 때 사용.
// 루트의 RunE와 동일한 동작이지만, 서브커맨드 네이밍이 명시적으로 노출되어
// 배포 스크립트나 systemd unit에서 목적이 분명해진다.
//
// envFileRef는 루트의 persistent flag를 공유하여 전역 일관성을 유지한다.
// (루트에서 --env-file을 설정해도 bot 서브커맨드가 동일 값을 본다)
func newBotCmd(envFileRef *string) *cobra.Command {
	return &cobra.Command{
		Use:   "bot",
		Short: "Discord 봇을 기동한다",
		Long: `Discord 봇을 기동해 스레드 기반 미팅 노트 기능을 제공한다.
SIGINT/SIGTERM을 받을 때까지 포그라운드에서 실행된다.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return bot.Run(*envFileRef)
		},
		SilenceUsage: true,
	}
}
