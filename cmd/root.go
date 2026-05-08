// Package cmd는 cobra 기반 CLI 진입점 모음.
// main 패키지는 이 패키지의 Execute()만 호출한다.
package cmd

import (
	"chatbot-alpha-1/pkg/bot"

	"github.com/spf13/cobra"
)

// Execute는 루트 커맨드를 파싱하고 실행한다. main에서 단 한 번 호출된다.
func Execute() error {
	return newRootCmd().Execute()
}

// newRootCmd는 cobra 루트 커맨드를 생성한다.
// 루트 자체도 RunE를 가져서 "./chatbot-alpha-1" (인자 없음) 호출 시
// "./chatbot-alpha-1 bot"과 동일하게 봇이 기동된다 — 기존 go run . 워크플로우 호환.
func newRootCmd() *cobra.Command {
	var envFile string
	cmd := &cobra.Command{
		Use:   "chatbot-alpha-1",
		Short: "Discord 기반 미팅 보조 봇",
		Long: `회의 메모를 스레드에 쌓고 LLM으로 구조화된 노트를 생성하는 Discord 봇.

기본 실행 시 'bot' 서브커맨드와 동일하게 Discord 봇을 기동한다.`,
		// 서브커맨드 없이 호출되면 봇 기동 (하위 호환)
		RunE: func(cmd *cobra.Command, args []string) error {
			return bot.Run(envFile)
		},
		// 에러 시 Usage 메시지 재출력 방지 - 봇 기동 실패는 로그로 충분하고
		// cobra 기본 Usage dump는 노이즈.
		SilenceUsage: true,
	}
	cmd.PersistentFlags().StringVar(&envFile, "env-file", "", ".env 파일 경로 (기본: 현재 디렉토리)")
	cmd.AddCommand(newBotCmd(&envFile))
	cmd.AddCommand(newLLMBotCmd(&envFile))
	cmd.AddCommand(newGitBotCmd(&envFile))
	return cmd
}
