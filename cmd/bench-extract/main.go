// Stage 3 ExtractContent 모델 벤치마크 — 5/14 회의 시뮬레이션 입력 + 환각 방어 핵심 검증.
//
// 사용:
//   GPT_API_KEY=... go run ./cmd/bench-extract | tee /tmp/bench-extract.log
//
// 동작:
//   - 5/14 회의 HumanNotes + ContextNotes 시뮬레이션
//   - prompts.SummarizedContent + buildContentExtractionUserMessage 동등 입력
//   - 3 시나리오 호출 + 결과 SummarizedContent 필드별 개수 비교
//   - 환각 검증: HUMAN_NOTES만 있을 때 봇 필드가 채워지는지, 또는 그 반대 (bleed 차단)
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"chatbot-alpha-1/pkg/llm"
	"chatbot-alpha-1/pkg/llm/prompts"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/shared"
)

var extractSchema = llm.GenerateSchema[llm.SummarizedContent]()

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

	userMsg := buildSampleUserMessage()
	fmt.Printf("입력 길이: %d bytes\n", len(userMsg))

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
		name    string
		elapsed time.Duration
		err     error
		inTok   int64
		outTok  int64
		resp    *llm.SummarizedContent
	}
	var results []result

	for i, sc := range scenarios {
		fmt.Printf("\n=== [%d/%d] %s 호출 중...\n", i+1, len(scenarios), sc.name)
		ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
		start := time.Now()
		resp, callErr := c.API().Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
			Model:               sc.model,
			ReasoningEffort:     sc.reasoning,
			MaxCompletionTokens: openai.Int(4000),
			Messages: []openai.ChatCompletionMessageParamUnion{
				openai.SystemMessage(prompts.SummarizedContent),
				openai.UserMessage(userMsg),
			},
			ResponseFormat: openai.ChatCompletionNewParamsResponseFormatUnion{
				OfJSONSchema: &shared.ResponseFormatJSONSchemaParam{
					JSONSchema: shared.ResponseFormatJSONSchemaJSONSchemaParam{
						Name:   "summarized_content_bench",
						Strict: openai.Bool(true),
						Schema: extractSchema,
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
				var parsed llm.SummarizedContent
				if err := json.Unmarshal([]byte(resp.Choices[0].Message.Content), &parsed); err == nil {
					r.resp = &parsed
				} else {
					r.err = fmt.Errorf("unmarshal: %w", err)
				}
			}
			if r.resp != nil {
				fmt.Printf("    elapsed=%s in=%d out=%d decisions=%d actions=%d weekly=%d done=%d\n",
					elapsed.Round(10*time.Millisecond), r.inTok, r.outTok,
					len(r.resp.Decisions), len(r.resp.Actions), len(r.resp.WeeklyReports), len(r.resp.Done))
			}
		}
		results = append(results, r)
		time.Sleep(500 * time.Millisecond)
	}

	// 결과 표
	fmt.Println("\n\n===== Stage 3 ExtractContent 결과 표 =====")
	fmt.Printf("%-22s | %-10s | %-8s | %-8s | %-9s | %-7s | %-6s | %-4s | %-9s | %-6s | %-6s\n",
		"scenario", "elapsed", "in_tok", "out_tok", "decisions", "actions", "topics", "done", "inProgress", "weekly", "tags")
	fmt.Println(strings.Repeat("-", 130))
	for _, r := range results {
		st := r.elapsed.Round(10 * time.Millisecond).String()
		if r.err != nil {
			st = "ERR"
		}
		var d, a, t, dn, ip, w, tg int
		if r.resp != nil {
			d, a, t, dn, ip, w, tg = len(r.resp.Decisions), len(r.resp.Actions), len(r.resp.Topics),
				len(r.resp.Done), len(r.resp.InProgress), len(r.resp.WeeklyReports), len(r.resp.Tags)
		}
		fmt.Printf("%-22s | %-10s | %-8d | %-8d | %-9d | %-7d | %-6d | %-4d | %-9d | %-6d | %-6d\n",
			r.name, st, r.inTok, r.outTok, d, a, t, dn, ip, w, tg)
	}

	// 환각 방어 검증 — bleed check
	fmt.Println("\n===== 환각 방어 (CONTEXT → HUMAN bleed) 검증 =====")
	for _, r := range results {
		fmt.Printf("\n--- %s ---\n", r.name)
		if r.err != nil || r.resp == nil {
			fmt.Println("(no data)")
			continue
		}
		// weekly highlight ("sortOrder 표준화" 등 봇 출력) 가 done/in_progress/decisions에 새는지 검사
		blockedPhrases := []string{"sortOrder", "어드민 증류소 CRUD", "푸시 기능 제거"}
		bleedFound := false
		for _, phrase := range blockedPhrases {
			for _, item := range r.resp.Done {
				if strings.Contains(item, phrase) {
					fmt.Printf("  ⚠️ BLEED: done[]에 봇 출력 phrase 발견: %q\n", item)
					bleedFound = true
				}
			}
			for _, item := range r.resp.InProgress {
				if strings.Contains(item, phrase) {
					fmt.Printf("  ⚠️ BLEED: in_progress[]에 봇 출력 phrase 발견: %q\n", item)
					bleedFound = true
				}
			}
			for _, d := range r.resp.Decisions {
				if strings.Contains(d.Title, phrase) {
					fmt.Printf("  ⚠️ BLEED: decisions[].title에 봇 출력 phrase 발견: %q\n", d.Title)
					bleedFound = true
				}
			}
		}
		if !bleedFound {
			fmt.Println("  ✓ CONTEXT → HUMAN 필드 bleed 없음 (정상)")
		}
		// weekly 필드가 채워졌는지
		if len(r.resp.WeeklyReports) == 0 {
			fmt.Println("  ⚠️ weekly_reports 비어있음 (CONTEXT_NOTES 분류 실패)")
		} else {
			fmt.Printf("  ✓ weekly_reports %d개 (repo=%q, highlights=%d)\n",
				len(r.resp.WeeklyReports), r.resp.WeeklyReports[0].Repo, len(r.resp.WeeklyReports[0].Highlights))
		}
	}

	// 속도 비교
	if len(results) >= 3 && results[0].err == nil {
		base := results[0].elapsed
		fmt.Println("\n===== Stage 3 속도 비교 =====")
		for _, r := range results[1:] {
			if r.err != nil {
				continue
			}
			fmt.Printf("- %s vs %s: 속도 %.2f×\n",
				r.name, results[0].name, float64(base)/float64(r.elapsed))
		}
	}
}

// buildSampleUserMessage — 5/14 회의 시뮬레이션. HumanNotes 5건 + ContextNotes 2건 (weekly + external).
// buildContentExtractionUserMessage가 만드는 형식과 동등.
func buildSampleUserMessage() string {
	var b strings.Builder
	b.WriteString("Date: 2026-05-14 (Thu)\n")
	b.WriteString("Speakers (Human source only): deadwhale, hyejungpark, kimjuye\n")
	b.WriteString("SpeakerRoles:\n")
	b.WriteString("  - deadwhale: BACKEND\n")
	b.WriteString("  - hyejungpark: FRONTEND\n")
	b.WriteString("  - kimjuye: PM\n")
	b.WriteString("\n=== HUMAN_NOTES (valid action.origin) ===\n")
	b.WriteString("[H1] kimjuye: workspace 이슈에 기획제안성, 정책성 이슈 통합 처리. 예) 이슈 155, 170, 195 등 묶음 정리. 5월 15일까지 정리할 예정\n")
	b.WriteString("[H2] kimjuye: 위스키 캐스크정보 업데이트 90%완료 다음 미팅까지 완료예정. 코덱스로 위스키 테이스팅 정보 수정 진행중\n")
	b.WriteString("[H3] deadwhale: 큐레이션 알코올스 계열들에서 order 제어 기능 필요. 생성에는 있는데 수정 등에서는 없음. 큐레이션 관리자 기능으로서 order spec 확장이 필요함\n")
	b.WriteString("[H4] hyejungpark: [프론트] 국가 api 스펙 변경 대응 배포 완료. 홈화면 토글 ui 개선 배포 예정. 지역 어드민 화면 배포 예정 (개발중)\n")
	b.WriteString("[H5] kimjuye: 차주 미팅까지 깃허브 이슈 206,207,208 프론트엔드 체크 요청\n")
	b.WriteString("[H6] deadwhale: 차주 미팅은 목요일 5/21 8:00시. 백엔드는 차주 큐레이션 커스텀 스펙 구현 필요\n")
	b.WriteString("[H7] kimjuye: 릴리즈 운영은 매주 미팅에서 git-bot으로 검증하고 정책적으로 접근하기로 합의\n")

	b.WriteString("\n=== CONTEXT_NOTES (bot/tool/external results, NEVER action.origin) ===\n")
	b.WriteString("[C1] [weekly]: 2026-05-14 주간 리포트 — bottle-note/bottle-note-api-server\n")
	b.WriteString("한 줄: 어드민 기준정보 API 확장과 정렬 필드 표준화가 가장 활발 (커밋 37건, open 0).\n")
	b.WriteString("주요 활동: 어드민 증류소 CRUD/이미지 S3 전환, sortOrder 표준화, 카테고리 grouped 변경, 푸시 제거 완료.\n")
	b.WriteString("다음 주 우선순위: 어드민 회귀 검증 / 클라이언트 호환성 점검 / 배포 파이프라인 점검 / 푸시 잔여 의존성 검색.\n")
	b.WriteString("[C2] hyejungpark: [외부 paste — 320줄 회의록 — vendor latency 보고서 본문 (시뮬레이션)]\n")
	return b.String()
}

func truncErr(s string) string {
	if len(s) > 180 {
		return s[:180] + "..."
	}
	return s
}
