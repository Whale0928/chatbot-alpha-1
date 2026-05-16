package summarize

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"chatbot-alpha-1/pkg/llm"
	"chatbot-alpha-1/pkg/llm/prompts"
	"chatbot-alpha-1/pkg/llm/validate"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/shared"
)

// summarizedContentSchema는 llm.SummarizedContent 타입에서 생성한 JSON Schema 캐시.
// SummarizedContent에 봇 결과 필드가 추가되면 response_format 스키마도 함께 갱신된다.
var summarizedContentSchema = llm.GenerateSchema[llm.SummarizedContent]()

// ContentExtractionInput은 정리본 1회 추출의 입력.
//
// 거시 디자인 결정 6 (환각 방어) 호출 계약:
//   - HumanNotes는 Source.IsAttributionCandidate()=true인 노트만 (Source=Human)
//   - ContextNotes는 corpus엔 포함되지만 attribution 후보 아닌 노트
//     ([weekly] / [release] / [agent] / ExternalPaste)
//   - Speakers는 HumanNotes의 author 집합 (validate가 action.origin을 이 안으로 강제)
//   - SpeakerRoles는 발화자 username → Discord guild role snapshot
//
// 호출자(pkg/bot)가 위 분리를 보장한다. 분리가 깨지면 환각 케이스 (deadwhale의 weekly dump가
// 본인 액션으로 attribute) 재발 가능.
type ContentExtractionInput struct {
	Date         time.Time
	Speakers     []string
	SpeakerRoles map[string][]string
	HumanNotes   []llm.Note
	ContextNotes []llm.Note
}

// ExtractContent는 미팅 corpus에서 SummarizedContent를 1회 추출한다.
//
// LLM 호출 1회 (gpt-5.5 + reasoning=medium). 결과는 후속 4 포맷 렌더의 단일 source of truth가 된다.
// freeform 포맷은 directive별 다양성을 위해 별도 LLM 호출 가능 (이 함수 외).
func ExtractContent(ctx context.Context, c *llm.Client, in ContentExtractionInput) (*llm.SummarizedContent, error) {
	userMsg := buildContentExtractionUserMessage(in)
	log.Printf("[llm/openai] extract_content.new model=%s prompt_bytes=%d human_notes=%d context_notes=%d speakers=%d",
		meetingModel, len(userMsg), len(in.HumanNotes), len(in.ContextNotes), len(in.Speakers))
	log.Printf("[llm/openai] extract_content prompt_preview=%q", previewForLog(userMsg, 300))

	resp, err := c.API().Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
		Model:           meetingModel,
		ReasoningEffort: meetingReasoning,
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.SystemMessage(prompts.SummarizedContent),
			openai.UserMessage(userMsg),
		},
		ResponseFormat: openai.ChatCompletionNewParamsResponseFormatUnion{
			OfJSONSchema: &shared.ResponseFormatJSONSchemaParam{
				JSONSchema: shared.ResponseFormatJSONSchemaJSONSchemaParam{
					Name:   "meeting_summarized_content",
					Strict: openai.Bool(true),
					Schema: summarizedContentSchema,
				},
			},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("llm: extract_content chat completions call failed: %w", err)
	}
	if len(resp.Choices) == 0 {
		return nil, fmt.Errorf("llm: empty choices in extract_content response")
	}

	raw := resp.Choices[0].Message.Content
	log.Printf("[llm/openai] extract_content.ok response_bytes=%d prompt_tokens=%d completion_tokens=%d total_tokens=%d",
		len(raw), resp.Usage.PromptTokens, resp.Usage.CompletionTokens, resp.Usage.TotalTokens)
	log.Printf("[llm/openai] extract_content raw_preview=%q", previewForLog(raw, 500))

	var out llm.SummarizedContent
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, fmt.Errorf("llm: unmarshal SummarizedContent: %w (raw=%q)", err, raw)
	}

	// 환각 1차 방어 — 호출 계약상 Origin은 Speakers 안이어야 한다.
	// corpus 검증은 HumanNotes + ContextNotes 합집합 (모든 텍스트가 토큰 매칭 후보).
	allNotes := make([]llm.Note, 0, len(in.HumanNotes)+len(in.ContextNotes))
	allNotes = append(allNotes, in.HumanNotes...)
	allNotes = append(allNotes, in.ContextNotes...)
	validate.AgainstSummarizedContent(&out, allNotes, in.Speakers)

	return &out, nil
}

// buildContentExtractionUserMessage는 LLM에 전달할 user message를 구성한다.
//
// 구조:
//  1. 날짜 + 참석자 + SpeakerRoles 매핑
//  2. HUMAN_NOTES (action.origin 후보)
//  3. CONTEXT_NOTES (참고만, action.origin 후보 아님)
//
// 두 그룹을 명시 분리하는 것이 환각 방어의 토대 (system prompt의 attribution rule과 짝).
func buildContentExtractionUserMessage(in ContentExtractionInput) string {
	var b strings.Builder

	fmt.Fprintf(&b, "Date: %s\n", in.Date.Format("2006-01-02 (Mon)"))
	fmt.Fprintf(&b, "Speakers (Human source only): %s\n", strings.Join(in.Speakers, ", "))

	// SpeakerRoles 매핑 (정렬 출력으로 결정성 확보)
	if len(in.SpeakerRoles) > 0 {
		b.WriteString("SpeakerRoles:\n")
		ordered := make([]string, 0, len(in.SpeakerRoles))
		for u := range in.SpeakerRoles {
			ordered = append(ordered, u)
		}
		sort.Strings(ordered)
		for _, u := range ordered {
			src := in.SpeakerRoles[u]
			if len(src) == 0 {
				fmt.Fprintf(&b, "  - %s: (no roles)\n", u)
				continue
			}
			// roles 내부도 정렬 — 호출자가 ["PM","BACKEND"]/["BACKEND","PM"] 어느 쪽으로
			// 넘기든 LLM 입력이 동일하도록. 호출 결과 비결정성 차단.
			roles := make([]string, len(src))
			copy(roles, src)
			sort.Strings(roles)
			fmt.Fprintf(&b, "  - %s: %s\n", u, strings.Join(roles, ", "))
		}
	}

	b.WriteString("\n=== HUMAN_NOTES (valid action.origin) ===\n")
	if len(in.HumanNotes) == 0 {
		b.WriteString("(none)\n")
	} else {
		for i, n := range in.HumanNotes {
			fmt.Fprintf(&b, "[H%d] %s: %s\n", i+1, n.Author, n.Content)
		}
	}

	b.WriteString("\n=== CONTEXT_NOTES (bot/tool/external results, NEVER action.origin) ===\n")
	if len(in.ContextNotes) == 0 {
		b.WriteString("(none)\n")
	} else {
		for i, n := range in.ContextNotes {
			fmt.Fprintf(&b, "[C%d] %s: %s\n", i+1, n.Author, n.Content)
		}
	}

	return b.String()
}
