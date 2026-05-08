package cmd

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"chatbot-alpha-1/pkg/github"
	"chatbot-alpha-1/pkg/llm"
	"chatbot-alpha-1/pkg/llm/render"
	"chatbot-alpha-1/pkg/llm/summarize"

	"github.com/joho/godotenv"
	"github.com/spf13/cobra"
)

// newGitBotCmd는 디스코드 없이 GitHub 호출 + LLM 정제 파이프라인을 stdout에서 검증하는 서브커맨드.
//
// 입력은 봇과 동일: open 이슈 전체 + 지난 N일(default 14) 커밋.
//
// 용도:
//   - 시스템 프롬프트 튜닝: 같은 입력 반복 호출해 출력 품질 비교
//   - directive 효과 검증: --directive로 결과 변화 관찰
//   - 토큰/시간 측정: prompt_bytes / completion_tokens / elapsed 직접 확인
//   - 2000자 분할 검증: rune 수 출력
func newGitBotCmd(envFileRef *string) *cobra.Command {
	var (
		repoFullName string
		days         int
		directive    string
	)

	cmd := &cobra.Command{
		Use:   "git-bot",
		Short: "Discord 없이 GitHub + LLM 정제 파이프라인을 검증한다",
		Long: `Discord 봇을 띄우지 않고 [주간 정리]의 핵심 흐름만 stdout에서 시뮬레이션.

입력:
  - open 이슈 전체 (시간 윈도우 무관)
  - 지난 N일(default 14) 커밋

예시:
  go run . git-bot --repo bottle-note/workspace
  go run . git-bot --repo bottle-note/bottle-note-api-server --days 30
  go run . git-bot --repo bottle-note/bottle-note-frontend --directive "프론트 라벨 정합성 더 보수적으로"`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runGitBot(*envFileRef, repoFullName, days, directive)
		},
		SilenceUsage: true,
	}

	cmd.Flags().StringVar(&repoFullName, "repo", "", "분석할 레포 (owner/name 형식, 필수)")
	cmd.Flags().IntVar(&days, "days", 14, "커밋 분석 기간 (일). 이슈는 open 전체라 무관.")
	cmd.Flags().StringVar(&directive, "directive", "", "사용자 추가 지시 (선택)")
	_ = cmd.MarkFlagRequired("repo")

	return cmd
}

func runGitBot(envFile, repoFullName string, days int, directive string) error {
	if envFile != "" {
		_ = godotenv.Load(envFile)
	} else {
		_ = godotenv.Load()
	}

	owner, name, ok := splitRepoFullName(repoFullName)
	if !ok {
		return fmt.Errorf("--repo는 owner/name 형식이어야 합니다 (받음: %q)", repoFullName)
	}

	ghToken := os.Getenv("GITHUB_TOKEN")
	if ghToken == "" {
		return fmt.Errorf("GITHUB_TOKEN 환경변수가 필요합니다")
	}
	gptKey := os.Getenv("GPT_API_KEY")
	if gptKey == "" {
		return fmt.Errorf("GPT_API_KEY 환경변수가 필요합니다")
	}

	gh, err := github.NewClient(ghToken)
	if err != nil {
		return fmt.Errorf("GitHub 클라이언트 초기화 실패: %w", err)
	}
	llmCl, err := llm.NewClient(gptKey)
	if err != nil {
		return fmt.Errorf("LLM 클라이언트 초기화 실패: %w", err)
	}

	fmt.Printf("[git-bot] repo=%s/%s commit_days=%d directive_runes=%d\n",
		owner, name, days, len([]rune(directive)))

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	now := time.Now()
	since := now.Add(-time.Duration(days) * 24 * time.Hour)

	// 1) open 이슈 전체
	fmt.Println("[git-bot] ListIssues (state=open) ...")
	t0 := time.Now()
	issues, err := gh.ListIssues(ctx, owner, name, github.ListIssuesOptions{
		State: "open",
	})
	if err != nil {
		return fmt.Errorf("ListIssues 실패: %w", err)
	}
	fmt.Printf("[git-bot] ListIssues 완료: %d건 (elapsed=%s)\n", len(issues), time.Since(t0).Round(time.Millisecond))

	// 2) 지난 N일 커밋
	fmt.Printf("[git-bot] ListCommits since=%s ...\n", since.UTC().Format(time.RFC3339))
	t0 = time.Now()
	commits, err := gh.ListCommits(ctx, owner, name, github.ListCommitsOptions{
		Since: since,
		Until: now,
	})
	if err != nil {
		return fmt.Errorf("ListCommits 실패: %w", err)
	}
	fmt.Printf("[git-bot] ListCommits 완료: %d건 (elapsed=%s)\n", len(commits), time.Since(t0).Round(time.Millisecond))

	if len(issues) == 0 && len(commits) == 0 {
		fmt.Println("[git-bot] 이슈 0 + 커밋 0 — LLM 호출 skip")
		return nil
	}

	repo := owner + "/" + name
	fmt.Printf("[git-bot] summarize.Weekly 호출 중... (open_issues=%d commits=%d)\n", len(issues), len(commits))
	t0 = time.Now()
	resp, err := summarize.Weekly(ctx, llmCl, repo, since, now, issues, commits, directive)
	dur := time.Since(t0)
	if err != nil {
		return fmt.Errorf("summarize.Weekly 실패: %w", err)
	}
	fmt.Printf("[git-bot] LLM 완료 (elapsed=%s, markdown_runes=%d, closeable=%d)\n",
		dur.Round(time.Millisecond), len([]rune(resp.Markdown)), len(resp.Closeable))

	rendered := render.RenderWeekly(render.WeeklyRenderInput{
		RepoFullName: repo,
		Since:        since,
		Until:        now,
		IssueCount:   len(issues),
		CommitCount:  len(commits),
		Response:     resp,
	})

	fmt.Println()
	fmt.Println("━━━ FINAL ━━━")
	fmt.Println(rendered)

	runes := len([]rune(rendered))
	status := "OK"
	if runes > 2000 {
		status = fmt.Sprintf("OVER (분할 필요: %d chunks)", (runes/2000)+1)
	}
	fmt.Printf("\n[git-bot] 렌더 결과: %d runes / 2000 limit → %s\n", runes, status)
	return nil
}

// splitRepoFullName은 "owner/name" → (owner, name, true). slash 없으면 ok=false.
func splitRepoFullName(full string) (owner, name string, ok bool) {
	idx := strings.IndexByte(full, '/')
	if idx <= 0 || idx == len(full)-1 {
		return "", "", false
	}
	return full[:idx], full[idx+1:], true
}
