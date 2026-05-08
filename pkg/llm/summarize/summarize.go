package summarize

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"chatbot-alpha-1/pkg/llm"
	"chatbot-alpha-1/pkg/llm/prompts"
	"chatbot-alpha-1/pkg/llm/validate"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/shared"
)

// finalNoteSchema는 llm.FinalNoteResponse 타입에서 생성한 JSON Schema 캐시.
var finalNoteSchema = llm.GenerateSchema[llm.FinalNoteResponse]()

// interimNoteSchema는 llm.InterimNoteResponse용 스키마 캐시.
var interimNoteSchema = llm.GenerateSchema[llm.InterimNoteResponse]()

// v2.0 4 포맷 스키마 캐시.
var (
	decisionStatusSchema = llm.GenerateSchema[llm.DecisionStatusResponse]()
	discussionSchema     = llm.GenerateSchema[llm.DiscussionResponse]()
	roleBasedSchema      = llm.GenerateSchema[llm.RoleBasedResponse]()
	freeformSchema       = llm.GenerateSchema[llm.FreeformResponse]()
)

// meetingModel은 미팅 요약에 사용하는 OpenAI 모델.
// gpt-5.4-mini는 reasoning 기반 모델이라 temperature를 받지 않는다.
// 결정성은 reasoning_effort로 제어한다.
const meetingModel = openai.ChatModelGPT5_4Mini

// meetingReasoning은 reasoning effort. "low"는 가벼운 추론 보조.
var meetingReasoning = openai.ReasoningEffortLow

// Meeting은 수집된 메모와 발화자 목록을 받아 LLM을 호출하고
// 구조화된 llm.FinalNoteResponse를 반환한다 (legacy v1.4 포맷).
//
// 참석자(speakers)는 Go가 수집한 정확한 목록이며 프롬프트 context로만 전달된다.
// LLM은 이 목록에 없는 이름을 "who"로 사용해선 안 된다.
func Meeting(
	ctx context.Context,
	c *llm.Client,
	notes []llm.Note,
	speakers []string,
	date time.Time,
) (*llm.FinalNoteResponse, error) {
	userMsg := buildSummarizeUserMessage(notes, speakers, date, "")
	log.Printf("[llm/openai] chat.completions.new model=%s prompt_bytes=%d notes=%d speakers=%d",
		meetingModel, len(userMsg), len(notes), len(speakers))
	log.Printf("[llm/openai] prompt_preview=%q", previewForLog(userMsg, 300))

	resp, err := c.API().Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
		Model:           meetingModel,
		ReasoningEffort: meetingReasoning,
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.SystemMessage(prompts.Summarize),
			openai.UserMessage(userMsg),
		},
		ResponseFormat: openai.ChatCompletionNewParamsResponseFormatUnion{
			OfJSONSchema: &shared.ResponseFormatJSONSchemaParam{
				JSONSchema: shared.ResponseFormatJSONSchemaJSONSchemaParam{
					Name:   "meeting_final_note",
					Strict: openai.Bool(true),
					Schema: finalNoteSchema,
				},
			},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("llm: chat completions call failed: %w", err)
	}
	if len(resp.Choices) == 0 {
		return nil, fmt.Errorf("llm: empty choices in response")
	}

	raw := resp.Choices[0].Message.Content
	log.Printf("[llm/openai] chat.completions.ok response_bytes=%d prompt_tokens=%d completion_tokens=%d total_tokens=%d",
		len(raw), resp.Usage.PromptTokens, resp.Usage.CompletionTokens, resp.Usage.TotalTokens)
	log.Printf("[llm/openai] raw_preview=%q", previewForLog(raw, 500))

	var out llm.FinalNoteResponse
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, fmt.Errorf("llm: unmarshal final note: %w (raw=%q)", err, raw)
	}

	// 환각 1차 방어선: 원본 노트와의 substring 검증. 실패해도 경고 로그만.
	validate.AgainstNotes(&out, notes, speakers)

	return &out, nil
}

// Interim은 진행 중인 미팅의 중간 스냅샷을 생성한다.
// Meeting과의 차이:
//   - 다른 system prompt (prompts.Interim): 성급한 결정/TODO 금지
//   - 다른 response schema (llm.InterimNoteResponse): NextSteps 없음
//   - "Currently ongoing meeting" 컨텍스트를 user message 앞에 명시
func Interim(
	ctx context.Context,
	c *llm.Client,
	notes []llm.Note,
	speakers []string,
	date time.Time,
) (*llm.InterimNoteResponse, error) {
	userMsg := "[Interim snapshot of an ONGOING meeting]\n\n" + buildSummarizeUserMessage(notes, speakers, date, "")
	log.Printf("[llm/openai] interim.new model=%s prompt_bytes=%d notes=%d speakers=%d",
		meetingModel, len(userMsg), len(notes), len(speakers))
	log.Printf("[llm/openai] interim prompt_preview=%q", previewForLog(userMsg, 300))

	resp, err := c.API().Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
		Model:           meetingModel,
		ReasoningEffort: meetingReasoning,
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.SystemMessage(prompts.Interim),
			openai.UserMessage(userMsg),
		},
		ResponseFormat: openai.ChatCompletionNewParamsResponseFormatUnion{
			OfJSONSchema: &shared.ResponseFormatJSONSchemaParam{
				JSONSchema: shared.ResponseFormatJSONSchemaJSONSchemaParam{
					Name:   "meeting_interim_note",
					Strict: openai.Bool(true),
					Schema: interimNoteSchema,
				},
			},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("llm: interim chat completions call failed: %w", err)
	}
	if len(resp.Choices) == 0 {
		return nil, fmt.Errorf("llm: empty choices in interim response")
	}

	raw := resp.Choices[0].Message.Content
	log.Printf("[llm/openai] interim.ok response_bytes=%d prompt_tokens=%d completion_tokens=%d total_tokens=%d",
		len(raw), resp.Usage.PromptTokens, resp.Usage.CompletionTokens, resp.Usage.TotalTokens)
	log.Printf("[llm/openai] interim raw_preview=%q", previewForLog(raw, 500))

	var out llm.InterimNoteResponse
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, fmt.Errorf("llm: unmarshal interim note: %w (raw=%q)", err, raw)
	}
	return &out, nil
}

// previewForLog는 긴 문자열의 앞부분만 로그용으로 자른다.
// rune 기준으로 잘라 UTF-8 중간 절단을 방지하고, 개행은 이스케이프하여 한 라인 유지.
func previewForLog(s string, maxRunes int) string {
	r := []rune(s)
	truncated := false
	if len(r) > maxRunes {
		r = r[:maxRunes]
		truncated = true
	}
	out := strings.ReplaceAll(string(r), "\n", "\\n")
	if truncated {
		out += "…"
	}
	return out
}

// buildSummarizeUserMessage는 LLM에 보낼 user role 메시지를 구성한다.
// directive가 비어있지 않으면 노트 앞에 "Formatting directive ..." 블록이
// prepend된다. 시스템 프롬프트보다 후순위지만 노트 본문보다 상위 컨텍스트로 작용한다.
func buildSummarizeUserMessage(notes []llm.Note, speakers []string, date time.Time, directive string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Meeting date: %s\n", date.Format("2006-01-02"))
	if len(speakers) > 0 {
		fmt.Fprintf(&b, "Participants (speakers in the thread): %s\n\n", strings.Join(speakers, ", "))
	} else {
		b.WriteString("Participants: (none)\n\n")
	}
	if d := strings.TrimSpace(directive); d != "" {
		b.WriteString("Formatting directive from the meeting host (priority over default style, but must not violate the schema):\n")
		b.WriteString(d)
		b.WriteString("\n\n")
	}
	if len(notes) == 0 {
		b.WriteString("Notes: (no notes were recorded)\n")
	} else {
		b.WriteString("Notes (chronological):\n")
		for i, n := range notes {
			fmt.Fprintf(&b, "%d. [%s] %s\n", i+1, n.Author, n.Content)
		}
	}
	return b.String()
}
