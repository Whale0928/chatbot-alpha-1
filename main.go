// chatbot-alpha-1은 Discord 기반 미팅 보조 봇이다.
// 엔트리포인트는 이 main.go 단일 파일에 고정되어 있고,
// CLI 파싱과 실제 동작 로직은 각각 cmd/ 와 pkg/bot/ 패키지로 분리되어 있다.
package main

import (
	"os"

	"chatbot-alpha-1/cmd"
)

func main() {
	if err := cmd.Execute(); err != nil {
		os.Exit(1)
	}
}
