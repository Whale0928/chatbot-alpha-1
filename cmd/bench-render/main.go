// Stage 4 RenderFormat 모델 벤치마크 — gpt-5.5 vs gpt-5.4-mini 4 포맷 실측.
//
// 사용:
//   GPT_API_KEY=... go run ./cmd/bench-render | tee /tmp/bench-render.log
//
// 동작:
//   - 5/14 미팅 SummarizedContent를 4 포맷 (decision_status / discussion / role_based / freeform) 모두 호출
//   - 각 포맷마다 3 시나리오 ((gpt-5.5 medium) / (gpt-5.5 low) / (gpt-5.4-mini low)) 비교
//   - 총 12 호출. 각 호출의 elapsed / token / markdown 길이 기록.
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

const sampleContentJSON = `{
  "decisions": [
    {"title": "workspace 기획·정책 이슈 통합 처리 — 5/15 정리 예정", "context": ["바 정보 관련 이슈 155/157/170/195 묶음"]},
    {"title": "릴리즈는 매주 미팅에서 git-bot으로 검증, 정책적으로 접근", "context": []},
    {"title": "차주 미팅: 2026-05-21 (목) 08:00", "context": []}
  ],
  "actions": [
    {"what": "큐레이션 order spec 확장 / 관리자 order 제어 구현", "origin": "deadwhale", "origin_roles": ["BACKEND"], "target_roles": ["BACKEND"], "target_user": "", "deadline": ""},
    {"what": "차주 큐레이션 커스텀 스펙 구현", "origin": "deadwhale", "origin_roles": ["BACKEND"], "target_roles": ["BACKEND"], "target_user": "", "deadline": "2026-05-21"},
    {"what": "GitHub 이슈 206/207/208 체크", "origin": "kimjuye", "origin_roles": ["PM"], "target_roles": ["FRONTEND"], "target_user": "hyejungpark", "deadline": "2026-05-21"},
    {"what": "workspace 기획·정책 이슈 통합 정리", "origin": "kimjuye", "origin_roles": ["PM"], "target_roles": ["PM"], "target_user": "", "deadline": "2026-05-15"}
  ],
  "done": ["큐레이션 화면 제작 및 전달", "국가 API 스펙 변경 대응 배포 (FE)"],
  "in_progress": ["위스키 캐스크정보 업데이트 90%", "지역 어드민 화면 개발 (FE)"],
  "planned": ["홈화면 토글 UI 개선 배포 (FE)"],
  "blockers": ["distilleries.image_url 컬럼 변경 하위 호환성 검토 필요"],
  "topics": [
    {"title": "workspace 통합 이슈 정리", "flow": ["kimjuye가 묶음 정리 흐름 공유", "5/15 정리 합의"], "insights": ["묶음 기준 사전 합의 유리"]},
    {"title": "큐레이션 order 제어", "flow": ["deadwhale가 생성/수정 spec 불일치 지적", "관리자 기능 확장 합의"], "insights": []}
  ],
  "weekly_reports": [
    {"repo": "bottle-note/bottle-note-api-server", "period_days": 14, "commit_count": 37, "highlights": ["어드민 증류소 CRUD / 이미지 S3 전환 완료", "sortOrder 표준화", "푸시 기능 제거"]}
  ],
  "release_results": [],
  "agent_responses": [],
  "external_refs": [],
  "shared": [],
  "open_questions": ["큐레이션 order spec 적용 범위 — 확인 필요"],
  "tags": ["큐레이션", "위스키데이터", "어드민", "릴리즈운영"]
}`

type renderResp struct {
	Markdown string `json:"markdown" jsonschema_description:"Discord embed.Description에 들어갈 markdown."`
}

var renderSchema = llm.GenerateSchema[renderResp]()

type formatCase struct {
	name         string
	systemPrompt string
	formatLabel  string
}

type scenario struct {
	name      string
	model     openai.ChatModel
	reasoning openai.ReasoningEffort
}

type result struct {
	caseName     string
	scenarioName string
	elapsed      time.Duration
	err          error
	promptTokens int64
	outputTokens int64
	markdownLen  int
}

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

	formats := []formatCase{
		{name: "decision_status", systemPrompt: prompts.FormatRenderDecisionStatus, formatLabel: "decision_status"},
		{name: "discussion", systemPrompt: prompts.FormatRenderDiscussion, formatLabel: "discussion"},
		{name: "role_based", systemPrompt: prompts.FormatRenderRoleBased, formatLabel: "role_based"},
		{name: "freeform", systemPrompt: prompts.FormatRenderFreeform, formatLabel: "freeform"},
	}
	scenarios := []scenario{
		{name: "5.5+medium", model: "gpt-5.5", reasoning: openai.ReasoningEffortMedium},
		{name: "5.5+low", model: "gpt-5.5", reasoning: openai.ReasoningEffortLow},
		{name: "5.4-mini+low", model: "gpt-5.4-mini", reasoning: openai.ReasoningEffortLow},
	}

	var results []result
	totalCalls := len(formats) * len(scenarios)
	callIdx := 0
	for _, f := range formats {
		userMsg := buildUserMessage(f.formatLabel)
		for _, sc := range scenarios {
			callIdx++
			fmt.Printf("\n=== [%d/%d] %s × %s 호출 중...\n", callIdx, totalCalls, f.name, sc.name)
			r := callOnce(c, sc, f.systemPrompt, userMsg)
			r.caseName = f.name
			r.scenarioName = sc.name
			results = append(results, r)
			if r.err != nil {
				fmt.Printf("    ERR: %v\n", truncErr(r.err.Error()))
			} else {
				fmt.Printf("    elapsed=%s in=%d out=%d md_runes=%d\n",
					r.elapsed.Round(10*time.Millisecond), r.promptTokens, r.outputTokens, r.markdownLen)
			}
			time.Sleep(400 * time.Millisecond)
		}
	}

	// ==== 결과 표 ====
	fmt.Println("\n\n===== Stage 4 RenderFormat 결과 표 =====")
	fmt.Printf("%-18s | %-14s | %-10s | %-10s | %-10s | %s\n",
		"format", "scenario", "elapsed", "in_tok", "out_tok", "md_runes")
	fmt.Println(strings.Repeat("-", 90))
	for _, r := range results {
		st := r.elapsed.Round(10 * time.Millisecond).String()
		if r.err != nil {
			st = "ERR"
		}
		fmt.Printf("%-18s | %-14s | %-10s | %-10d | %-10d | %d\n",
			r.caseName, r.scenarioName, st, r.promptTokens, r.outputTokens, r.markdownLen)
	}

	// ==== 시나리오별 합산 ====
	fmt.Println("\n===== 시나리오별 합산 (4 포맷 평균) =====")
	scAgg := map[string]struct {
		total time.Duration
		count int
		outTk int64
	}{}
	for _, r := range results {
		if r.err != nil {
			continue
		}
		a := scAgg[r.scenarioName]
		a.total += r.elapsed
		a.count++
		a.outTk += r.outputTokens
		scAgg[r.scenarioName] = a
	}
	for _, sc := range scenarios {
		a := scAgg[sc.name]
		if a.count == 0 {
			fmt.Printf("%-14s : (no data)\n", sc.name)
			continue
		}
		avg := a.total / time.Duration(a.count)
		fmt.Printf("%-14s : avg elapsed = %-7s | total elapsed = %-7s | avg out_tok = %d\n",
			sc.name, avg.Round(10*time.Millisecond), a.total.Round(10*time.Millisecond), a.outTk/int64(a.count))
	}

	// ==== 권장 ====
	fmt.Println("\n===== Stage 4 권장 =====")
	current := scAgg["5.5+medium"]
	for _, sc := range []string{"5.5+low", "5.4-mini+low"} {
		a := scAgg[sc]
		if a.count == 0 || current.count == 0 {
			continue
		}
		speedup := float64(current.total) / float64(a.total)
		fmt.Printf("- %s vs 5.5+medium: 속도 %.2f× (4 포맷 총 %s → %s)\n",
			sc, speedup, current.total.Round(10*time.Millisecond), a.total.Round(10*time.Millisecond))
	}
}

type payload struct {
	Date         string                 `json:"date"`
	Format       string                 `json:"format"`
	Speakers     []string               `json:"speakers"`
	SpeakerRoles map[string][]string    `json:"speaker_roles"`
	Directive    string                 `json:"directive,omitempty"`
	Content      map[string]interface{} `json:"summarized_content"`
}

func buildUserMessage(formatLabel string) string {
	var content map[string]interface{}
	if err := json.Unmarshal([]byte(sampleContentJSON), &content); err != nil {
		log.Fatalf("sample parse: %v", err)
	}
	p := payload{
		Date:         "2026-05-14 (Thu)",
		Format:       formatLabel,
		Speakers:     []string{"deadwhale", "hyejungpark", "kimjuye"},
		SpeakerRoles: map[string][]string{"deadwhale": {"BACKEND"}, "hyejungpark": {"FRONTEND"}, "kimjuye": {"PM"}},
		Content:      content,
	}
	raw, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		log.Fatalf("payload marshal: %v", err)
	}
	return "Render the following SummarizedContent JSON into the requested markdown format.\n\n" + string(raw)
}

func callOnce(c *llm.Client, sc scenario, systemPrompt, userMsg string) result {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	start := time.Now()
	resp, err := c.API().Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
		Model:               sc.model,
		ReasoningEffort:     sc.reasoning,
		MaxCompletionTokens: openai.Int(2000),
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.SystemMessage(systemPrompt),
			openai.UserMessage(userMsg),
		},
		ResponseFormat: openai.ChatCompletionNewParamsResponseFormatUnion{
			OfJSONSchema: &shared.ResponseFormatJSONSchemaParam{
				JSONSchema: shared.ResponseFormatJSONSchemaJSONSchemaParam{
					Name:   "meeting_format_render_bench",
					Strict: openai.Bool(true),
					Schema: renderSchema,
				},
			},
		},
	})
	elapsed := time.Since(start)
	if err != nil {
		return result{elapsed: elapsed, err: err}
	}
	r := result{
		elapsed:      elapsed,
		promptTokens: int64(resp.Usage.PromptTokens),
		outputTokens: int64(resp.Usage.CompletionTokens),
	}
	if len(resp.Choices) == 0 {
		r.err = fmt.Errorf("empty choices")
		return r
	}
	var parsed renderResp
	if err := json.Unmarshal([]byte(resp.Choices[0].Message.Content), &parsed); err != nil {
		r.err = fmt.Errorf("unmarshal: %w", err)
		return r
	}
	r.markdownLen = len([]rune(parsed.Markdown))
	return r
}

func truncErr(s string) string {
	if len(s) > 150 {
		return s[:150] + "..."
	}
	return s
}
