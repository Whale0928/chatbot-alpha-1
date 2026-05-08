package cmd

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"chatbot-alpha-1/pkg/llm"
	"chatbot-alpha-1/pkg/llm/render"
	"chatbot-alpha-1/pkg/llm/summarize"

	"github.com/joho/godotenv"
	"github.com/spf13/cobra"
)

// newLLMBotCmd는 Discord 없이 CLI에서 미팅 시나리오를 시뮬레이션하는 서브커맨드.
//
// 용도:
//   - 프롬프트 튜닝: 같은 시나리오 반복 실행해 출력 품질 비교
//   - 2000자 분할 검증: 긴 입력 → 렌더 결과 길이 확인
//   - 시나리오 파일 기반 자동 테스트: --file로 docs/시나리오_*.md 투입
//
// 동작:
//   - interactive (기본): stdin에서 한 줄씩 읽어 노트 누적. "미팅 종료"로 LLM 호출.
//   - file 모드: --file 지정 시 파일의 각 줄을 노트로 처리. 끝나면 자동 LLM 호출.
//   - 결과는 stdout에 마크다운으로 출력 + rune 수 표시 (2000자 체크용).
func newLLMBotCmd(envFileRef *string) *cobra.Command {
	var (
		filePath  string
		speaker   string
		interim   bool
		format    string
		directive string
	)

	cmd := &cobra.Command{
		Use:   "llm-bot",
		Short: "CLI에서 미팅 시나리오를 시뮬레이션한다 (Discord 불필요)",
		Long: `Discord 없이 CLI 콘솔에서 미팅 메모를 입력하고 LLM 요약 결과를 확인한다.

interactive 모드 (기본):
  한 줄씩 입력 → 노트 누적 → "미팅 종료" 입력 시 LLM 호출

file 모드:
  --file docs/시나리오_01.md 지정 → 파일의 각 줄을 노트로 → 자동 LLM 호출

포맷:
  --format=decision_status (1, 기본) | discussion (2) | role_based (3) | freeform (4)
  legacy 는 기존 v1.4 FinalNoteResponse 사용

용도: 프롬프트 튜닝, 2000자 분할 검증, 4 포맷 골든 비교`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runLLMBot(*envFileRef, filePath, speaker, format, directive, interim)
		},
		SilenceUsage: true,
	}

	cmd.Flags().StringVar(&filePath, "file", "", "시나리오 파일 경로 (없으면 interactive stdin)")
	cmd.Flags().StringVar(&speaker, "speaker", "user", "발화자 이름")
	cmd.Flags().BoolVar(&interim, "interim", false, "종료 전 interim 요약도 출력 (legacy 포맷에서만)")
	cmd.Flags().StringVar(&format, "format", "decision_status", "노트 포맷: decision_status|discussion|role_based|freeform|legacy")
	cmd.Flags().StringVar(&directive, "directive", "", "정리 지시문 (4 포맷에만 적용, legacy/interim에는 미적용)")

	return cmd
}

func runLLMBot(envFile, filePath, speaker, formatStr, directive string, showInterim bool) error {
	// env 로드
	if envFile != "" {
		_ = godotenv.Load(envFile)
	} else {
		_ = godotenv.Load()
	}

	gptKey := os.Getenv("GPT_API_KEY")
	if gptKey == "" {
		return fmt.Errorf("GPT_API_KEY 환경변수가 필요합니다")
	}
	client, err := llm.NewClient(gptKey)
	if err != nil {
		return fmt.Errorf("LLM 클라이언트 초기화 실패: %w", err)
	}

	// 포맷 파싱: "legacy"는 v1.4 FinalNoteResponse 사용 (기존 호환).
	useLegacy := formatStr == "legacy"
	var noteFormat llm.NoteFormat
	if !useLegacy {
		f, ok := llm.ParseNoteFormat(formatStr)
		if !ok {
			return fmt.Errorf("알 수 없는 --format=%q (decision_status|discussion|role_based|freeform|legacy)", formatStr)
		}
		noteFormat = f
	}

	fmt.Println("[llm-bot] LLM 클라이언트 초기화 완료")
	fmt.Printf("[llm-bot] speaker: %s, format: %s\n", speaker, formatStr)
	if directive != "" {
		fmt.Printf("[llm-bot] directive: %s\n", truncateForDisplay(directive, 200))
	}

	// 노트 수집
	var notes []llm.Note

	if filePath != "" {
		notes, err = loadNotesFromFile(filePath, speaker)
		if err != nil {
			return err
		}
		fmt.Printf("[llm-bot] 파일 로드 완료: %s (%d건)\n", filePath, len(notes))
		for i, n := range notes {
			fmt.Printf("  [note %d] %s\n", i+1, truncateForDisplay(n.Content, 80))
		}
	} else {
		fmt.Println("[llm-bot] 미팅 시작. 메시지를 입력하세요. \"미팅 종료\"로 마무리.")
		fmt.Println()

		scanner := bufio.NewScanner(os.Stdin)
		scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

		for {
			fmt.Print("> ")
			if !scanner.Scan() {
				break
			}
			line := strings.TrimSpace(scanner.Text())
			if line == "" {
				continue
			}
			if line == "미팅 종료" || line == "회의 종료" {
				break
			}
			notes = append(notes, llm.Note{Author: speaker, Content: line})
			fmt.Printf("  [note %d] %s\n", len(notes), truncateForDisplay(line, 80))
		}
	}

	if len(notes) == 0 {
		fmt.Println("[llm-bot] 노트가 없습니다.")
		return nil
	}

	// speakers는 노트의 Author 집합으로부터 구성 (역할별 포맷에서 중요).
	speakers := collectSpeakers(notes, speaker)
	now := time.Now()
	ctx := context.Background()

	// Interim은 legacy 포맷에서만 (4 포맷은 interim 미정).
	if showInterim && useLegacy {
		runInterim(ctx, client, notes, speakers, now)
	}

	fmt.Println()
	fmt.Println("━━━ FINAL ━━━")

	if useLegacy {
		if directive != "" {
			fmt.Println("[llm-bot] WARN legacy 포맷은 directive를 반영하지 않습니다.")
		}
		return runLegacyFinal(ctx, client, notes, speakers, now)
	}
	return runFormatFinal(ctx, client, noteFormat, notes, speakers, now, directive)
}

// collectSpeakers는 notes의 Author 집합 + 보강용 fallback speaker로 발화자 목록을 구성한다.
func collectSpeakers(notes []llm.Note, fallback string) []string {
	seen := map[string]bool{}
	var out []string
	for _, n := range notes {
		if n.Author == "" {
			continue
		}
		if !seen[n.Author] {
			seen[n.Author] = true
			out = append(out, n.Author)
		}
	}
	if len(out) == 0 && fallback != "" {
		out = []string{fallback}
	}
	return out
}

func runInterim(ctx context.Context, client *llm.Client, notes []llm.Note, speakers []string, now time.Time) {
	fmt.Println()
	fmt.Println("━━━ INTERIM ━━━")
	fmt.Printf("[llm-bot] SummarizeInterim 호출 중... (notes=%d)\n", len(notes))
	start := time.Now()
	interimResp, err := summarize.Interim(ctx, client, notes, speakers, now)
	dur := time.Since(start)
	if err != nil {
		fmt.Printf("[llm-bot] ERR interim: %v\n", err)
		return
	}
	fmt.Printf("[llm-bot] 완료 (elapsed=%s, decisions=%d, open_questions=%d)\n",
		dur.Round(time.Millisecond), len(interimResp.Decisions), len(interimResp.OpenQuestions))
	fmt.Println()
	rendered := render.RenderInterimNote(render.InterimRenderInput{
		Date:         now,
		Participants: speakers,
		Response:     interimResp,
	})
	fmt.Println(rendered)
	printLengthReport("interim", rendered)
}

func runLegacyFinal(ctx context.Context, client *llm.Client, notes []llm.Note, speakers []string, now time.Time) error {
	fmt.Printf("[llm-bot] SummarizeMeeting (legacy) 호출 중... (notes=%d)\n", len(notes))
	start := time.Now()
	resp, err := summarize.Meeting(ctx, client, notes, speakers, now)
	dur := time.Since(start)
	if err != nil {
		return fmt.Errorf("SummarizeMeeting 실패: %w", err)
	}
	fmt.Printf("[llm-bot] 완료 (elapsed=%s, decisions=%d, open_questions=%d, next_steps=%d)\n",
		dur.Round(time.Millisecond), len(resp.Decisions), len(resp.OpenQuestions), len(resp.NextSteps))
	fmt.Println()
	rendered := render.RenderFinalNote(render.RenderInput{Date: now, Participants: speakers, Response: resp})
	fmt.Println(rendered)
	printLengthReport("final", rendered)
	return nil
}

func runFormatFinal(ctx context.Context, client *llm.Client, format llm.NoteFormat, notes []llm.Note, speakers []string, now time.Time, directive string) error {
	fmt.Printf("[llm-bot] format=%s 호출 중... (notes=%d, speakers=%v, directive_runes=%d)\n",
		format, len(notes), speakers, len([]rune(directive)))
	start := time.Now()
	var rendered string
	switch format {
	case llm.FormatDecisionStatus:
		resp, err := summarize.DecisionStatus(ctx, client, notes, speakers, now, directive)
		if err != nil {
			return fmt.Errorf("SummarizeDecisionStatus 실패: %w", err)
		}
		fmt.Printf("[llm-bot] 완료 (elapsed=%s, decisions=%d, done=%d, in_progress=%d, planned=%d, blockers=%d, open_questions=%d, next_steps=%d)\n",
			time.Since(start).Round(time.Millisecond),
			len(resp.Decisions), len(resp.Done), len(resp.InProgress), len(resp.Planned),
			len(resp.Blockers), len(resp.OpenQuestions), len(resp.NextSteps))
		rendered = render.RenderDecisionStatus(render.DecisionStatusRenderInput{Date: now, Participants: speakers, Response: resp})
	case llm.FormatDiscussion:
		resp, err := summarize.Discussion(ctx, client, notes, speakers, now, directive)
		if err != nil {
			return fmt.Errorf("SummarizeDiscussion 실패: %w", err)
		}
		fmt.Printf("[llm-bot] 완료 (elapsed=%s, topics=%d, open_questions=%d)\n",
			time.Since(start).Round(time.Millisecond), len(resp.Topics), len(resp.OpenQuestions))
		rendered = render.RenderDiscussion(render.DiscussionRenderInput{Date: now, Participants: speakers, Response: resp})
	case llm.FormatRoleBased:
		resp, err := summarize.RoleBased(ctx, client, notes, speakers, now, directive)
		if err != nil {
			return fmt.Errorf("SummarizeRoleBased 실패: %w", err)
		}
		fmt.Printf("[llm-bot] 완료 (elapsed=%s, roles=%d, shared_items=%d, open_questions=%d)\n",
			time.Since(start).Round(time.Millisecond), len(resp.Roles), len(resp.SharedItems), len(resp.OpenQuestions))
		rendered = render.RenderRoleBased(render.RoleBasedRenderInput{Date: now, Participants: speakers, Response: resp})
	case llm.FormatFreeform:
		resp, err := summarize.Freeform(ctx, client, notes, speakers, now, directive)
		if err != nil {
			return fmt.Errorf("SummarizeFreeform 실패: %w", err)
		}
		fmt.Printf("[llm-bot] 완료 (elapsed=%s, markdown_runes=%d)\n",
			time.Since(start).Round(time.Millisecond), len([]rune(resp.Markdown)))
		rendered = render.RenderFreeform(render.FreeformRenderInput{Date: now, Participants: speakers, Response: resp})
	default:
		return fmt.Errorf("미지원 format=%v", format)
	}
	fmt.Println()
	fmt.Println(rendered)
	printLengthReport(format.String(), rendered)
	return nil
}

// loadNotesFromFile은 시나리오 파일에서 노트를 추출한다.
// 규칙:
//   - YAML frontmatter (파일 첫 줄 "---"로 시작 → 다음 "---"까지 전체 스킵)
//   - # 으로 시작하는 줄: 헤딩 → 스킵
//   - > 으로 시작하는 줄: 메타/인용 → 스킵
//   - ``` 코드 블록: 전체 스킵
//   - --- 구분선 (frontmatter 밖): 스킵
//   - ~~취소선 텍스트~~: 스킵 (회의 중 취소된 항목)
//   - 빈 줄: 스킵
//   - Obsidian 내부 링크 "[[...]]" 만으로 구성된 줄: 스킵
//   - author 헤더: 줄 시작이 "@username:" 또는 "[username]" 이면 해당 줄에
//     그 이후 텍스트를 노트로, author 필드는 username으로. 헤더만 있고
//     본문이 비면 다음 줄들의 default author로 적용 (heredoc 스타일).
//   - 그 외: 노트 1건 (default speaker로)
func loadNotesFromFile(path, speaker string) ([]llm.Note, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("파일 열기 실패: %w", err)
	}
	defer f.Close()

	var notes []llm.Note
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	inCodeBlock := false
	inFrontmatter := false
	frontmatterSeen := false // frontmatter는 파일 시작에서 한 번만
	lineNum := 0
	currentAuthor := speaker // sticky author: 헤더만 있는 줄은 default author 변경

	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)
		lineNum++

		// YAML frontmatter: 파일 첫 줄이 "---"이면 시작, 다음 "---"에서 끝.
		// Obsidian 미팅 노트는 거의 100% frontmatter가 있어서 이걸 스킵하지 않으면
		// created/author/tags/related 등이 노트에 섞여 토큰 낭비 + 오분류 유발.
		if trimmed == "---" {
			if !frontmatterSeen && lineNum <= 2 {
				inFrontmatter = true
				frontmatterSeen = true
				continue
			}
			if inFrontmatter {
				inFrontmatter = false
				continue
			}
			continue // frontmatter 밖의 --- 구분선도 스킵
		}
		if inFrontmatter {
			continue
		}

		// 코드 블록 토글
		if strings.HasPrefix(trimmed, "```") {
			inCodeBlock = !inCodeBlock
			continue
		}
		if inCodeBlock {
			continue
		}

		// 스킵 대상
		if trimmed == "" {
			continue
		}
		if strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, ">") {
			continue
		}
		// ~~취소선 전체~~ 줄 스킵 (회의 중 취소된 항목)
		if strings.HasPrefix(trimmed, "~~") && strings.HasSuffix(trimmed, "~~") {
			continue
		}
		// 구분선 변형 (----, ===, ***)
		if len(trimmed) >= 3 && (allSameChar(trimmed, '-') || allSameChar(trimmed, '=') || allSameChar(trimmed, '*')) {
			continue
		}

		// author 헤더: "@username: 본문" 또는 "[username] 본문"
		// 헤더만 있고 본문이 비면 currentAuthor만 갱신 후 노트는 추가하지 않음.
		if author, content, ok := parseAuthorHeader(trimmed); ok {
			currentAuthor = author
			if content == "" {
				continue
			}
			notes = append(notes, llm.Note{Author: author, Content: content})
			continue
		}

		notes = append(notes, llm.Note{Author: currentAuthor, Content: trimmed})
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("파일 읽기 실패: %w", err)
	}
	return notes, nil
}

// parseAuthorHeader는 다음 형식을 파싱:
//
//	"@username: 본문" → ("username", "본문", true)
//	"@username:"       → ("username", "",     true)   (헤더만)
//	"[username] 본문"  → ("username", "본문", true)
//	"[username]"       → ("username", "",     true)
//
// username은 영문자/숫자/_/- 만 허용 (보수적). 매칭 안 되면 ok=false.
func parseAuthorHeader(line string) (author, content string, ok bool) {
	// "@username: ..." 또는 "@username:"
	if strings.HasPrefix(line, "@") {
		// "@" 이후 ":"의 첫 등장 위치
		colon := strings.Index(line, ":")
		if colon < 2 {
			return "", "", false
		}
		name := line[1:colon]
		if !isValidAuthorName(name) {
			return "", "", false
		}
		return name, strings.TrimSpace(line[colon+1:]), true
	}
	// "[username] ..." 또는 "[username]"
	if strings.HasPrefix(line, "[") {
		end := strings.Index(line, "]")
		if end < 2 {
			return "", "", false
		}
		name := line[1:end]
		if !isValidAuthorName(name) {
			return "", "", false
		}
		return name, strings.TrimSpace(line[end+1:]), true
	}
	return "", "", false
}

// isValidAuthorName은 username이 영문자/숫자/_/- 로만 구성되었는지 검사.
// 회의 노트 본문의 일반 텍스트와 구분하기 위해 보수적으로 제한.
func isValidAuthorName(s string) bool {
	if len(s) == 0 || len(s) > 32 {
		return false
	}
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '_' || r == '-' {
			continue
		}
		return false
	}
	return true
}

func allSameChar(s string, ch byte) bool {
	for i := 0; i < len(s); i++ {
		if s[i] != ch {
			return false
		}
	}
	return true
}

func truncateForDisplay(s string, maxRunes int) string {
	r := []rune(s)
	if len(r) <= maxRunes {
		return strings.ReplaceAll(s, "\n", "\\n")
	}
	return strings.ReplaceAll(string(r[:maxRunes]), "\n", "\\n") + "..."
}

func printLengthReport(label, rendered string) {
	runes := len([]rune(rendered))
	status := "OK"
	if runes > 2000 {
		status = fmt.Sprintf("OVER (분할 필요: %d chunks)", (runes/2000)+1)
	}
	fmt.Printf("[llm-bot] %s 렌더 결과: %d runes / 2000 limit → %s\n", label, runes, status)
	log.Printf("[llm-bot] %s runes=%d bytes=%d", label, runes, len(rendered))
}
