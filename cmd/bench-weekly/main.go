// Weekly Report 모델 벤치마크 — chatbot-alpha-1 실제 git log 7일치를 입력으로 사용.
//
// 사용:
//   GPT_API_KEY=... go run ./cmd/bench-weekly | tee /tmp/bench-weekly.log
//
// 동작:
//   - `git log --since='7 days ago'` 출력을 github.Commit으로 변환
//   - prompts.Weekly + buildWeeklyUserMessage 동등 입력 직접 구성
//   - 3 시나리오 (5.5+medium, 5.5+low, 5.4-mini+low) × 1 입력 = 3 호출
//   - elapsed / tokens / highlight 수 / summary 1줄 비교
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"time"

	"chatbot-alpha-1/pkg/llm"
	"chatbot-alpha-1/pkg/llm/prompts"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/shared"
)

// 실제 운영 schema 그대로 사용.
var weeklySchema = llm.GenerateSchema[llm.WeeklyReportResponse]()

func main() {
	apiKey := os.Getenv("GPT_API_KEY")
	if apiKey == "" {
		apiKey = os.Getenv("OPENAI_API_KEY")
	}
	if apiKey == "" {
		log.Fatal("GPT_API_KEY 필요")
	}
	c, err := llm.NewClient(apiKey)
	if err != nil {
		log.Fatalf("NewClient: %v", err)
	}

	// git log 7일치 dump
	commits, err := fetchRecentCommits()
	if err != nil {
		log.Fatalf("git log: %v", err)
	}
	fmt.Printf("입력 commits: %d개 (chatbot-alpha-1 main, 지난 7일)\n", len(commits))

	userMsg := buildUserMessage(commits)

	scenarios := []struct {
		name      string
		model     openai.ChatModel
		reasoning openai.ReasoningEffort
	}{
		{name: "5.5+medium (현 운영)", model: "gpt-5.5", reasoning: openai.ReasoningEffortMedium},
		{name: "5.5+low", model: "gpt-5.5", reasoning: openai.ReasoningEffortLow},
		{name: "5.4-mini+low", model: "gpt-5.4-mini", reasoning: openai.ReasoningEffortLow},
	}

	type result struct {
		name      string
		elapsed   time.Duration
		err       error
		inTok     int64
		outTok    int64
		resp      *llm.WeeklyReportResponse
	}
	var results []result

	for i, sc := range scenarios {
		fmt.Printf("\n=== [%d/%d] %s 호출 중...\n", i+1, len(scenarios), sc.name)
		ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
		start := time.Now()
		resp, callErr := c.API().Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
			Model:               sc.model,
			ReasoningEffort:     sc.reasoning,
			MaxCompletionTokens: openai.Int(3000),
			Messages: []openai.ChatCompletionMessageParamUnion{
				openai.SystemMessage(prompts.Weekly),
				openai.UserMessage(userMsg),
			},
			ResponseFormat: openai.ChatCompletionNewParamsResponseFormatUnion{
				OfJSONSchema: &shared.ResponseFormatJSONSchemaParam{
					JSONSchema: shared.ResponseFormatJSONSchemaJSONSchemaParam{
						Name:   "weekly_report_bench",
						Strict: openai.Bool(true),
						Schema: weeklySchema,
					},
				},
			},
		})
		elapsed := time.Since(start)
		cancel()

		r := result{name: sc.name, elapsed: elapsed}
		if callErr != nil {
			r.err = callErr
			fmt.Printf("    ERR: %v\n", truncErr(callErr.Error()))
		} else {
			r.inTok = int64(resp.Usage.PromptTokens)
			r.outTok = int64(resp.Usage.CompletionTokens)
			if len(resp.Choices) > 0 {
				var parsed llm.WeeklyReportResponse
				if err := json.Unmarshal([]byte(resp.Choices[0].Message.Content), &parsed); err == nil {
					r.resp = &parsed
				} else {
					r.err = fmt.Errorf("unmarshal: %w", err)
				}
			}
			mdRunes := 0
			closeCnt := 0
			if r.resp != nil {
				mdRunes = len([]rune(r.resp.Markdown))
				closeCnt = len(r.resp.Closeable)
			}
			fmt.Printf("    elapsed=%s in=%d out=%d md_runes=%d closeable=%d\n",
				elapsed.Round(10*time.Millisecond), r.inTok, r.outTok, mdRunes, closeCnt)
		}
		results = append(results, r)
		time.Sleep(400 * time.Millisecond)
	}

	// 결과 표
	fmt.Println("\n\n===== Weekly Report 결과 표 =====")
	fmt.Printf("%-22s | %-10s | %-8s | %-8s | %-10s | %s\n", "scenario", "elapsed", "in_tok", "out_tok", "md_runes", "closeable")
	fmt.Println(strings.Repeat("-", 90))
	for _, r := range results {
		st := r.elapsed.Round(10 * time.Millisecond).String()
		if r.err != nil {
			st = "ERR"
		}
		md, cl := 0, 0
		if r.resp != nil {
			md = len([]rune(r.resp.Markdown))
			cl = len(r.resp.Closeable)
		}
		fmt.Printf("%-22s | %-10s | %-8d | %-8d | %-10d | %d\n", r.name, st, r.inTok, r.outTok, md, cl)
	}

	// 응답 비교
	fmt.Println("\n===== 응답 markdown 미리보기 (각 600자) =====")
	for _, r := range results {
		fmt.Printf("\n--- %s ---\n", r.name)
		if r.err != nil {
			fmt.Printf("(error: %v)\n", truncErr(r.err.Error()))
			continue
		}
		if r.resp == nil {
			fmt.Println("(no parsed response)")
			continue
		}
		md := r.resp.Markdown
		rr := []rune(md)
		if len(rr) > 600 {
			md = string(rr[:600]) + "\n...(잘림)"
		}
		fmt.Println(md)
	}

	// 속도 비교
	if len(results) >= 3 && results[0].err == nil {
		base := results[0].elapsed
		fmt.Println("\n===== Weekly 속도 비교 =====")
		for _, r := range results[1:] {
			if r.err != nil {
				continue
			}
			fmt.Printf("- %s vs %s: 속도 %.2f×\n",
				r.name, results[0].name, float64(base)/float64(r.elapsed))
		}
	}
}

// fetchRecentCommits — git log 7일치 → []github.Commit 구조와 동등한 데이터.
type commitRow struct {
	SHA     string
	Date    time.Time
	Author  string
	Message string
}

func fetchRecentCommits() ([]commitRow, error) {
	cmd := exec.Command("git", "log",
		"--since=7 days ago",
		"--pretty=format:%h|%ad|%an|%s",
		"--date=short",
		"main",
	)
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	var rows []commitRow
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		parts := strings.SplitN(line, "|", 4)
		if len(parts) != 4 {
			continue
		}
		date, _ := time.Parse("2006-01-02", parts[1])
		rows = append(rows, commitRow{
			SHA:     parts[0],
			Date:    date,
			Author:  parts[2],
			Message: parts[3],
		})
	}
	return rows, sc.Err()
}

// buildUserMessage — chatbot-alpha-1 git log를 buildWeeklyUserMessage 형식과 동등하게 구성.
func buildUserMessage(commits []commitRow) string {
	var b strings.Builder
	now := time.Now().UTC()
	since := now.AddDate(0, 0, -7)
	b.WriteString("Repository: Whale0928/chatbot-alpha-1\n")
	b.WriteString("Analysis scope: commits — commits only (issue dump intentionally omitted; do not infer issue-side state)\n")
	fmt.Fprintf(&b, "Commit window: %s ~ %s\n", since.Format("2006-01-02"), now.Format("2006-01-02"))
	fmt.Fprintf(&b, "\nCommits in window (count=%d, newest first):\n\n", len(commits))
	for _, c := range commits {
		fmt.Fprintf(&b, "- %s by %s (%s): %s\n", c.SHA, c.Author, c.Date.Format("2006-01-02"), c.Message)
	}
	return b.String()
}

func truncErr(s string) string {
	if len(s) > 180 {
		return s[:180] + "..."
	}
	return s
}
