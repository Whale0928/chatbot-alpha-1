package summarize

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"chatbot-alpha-1/pkg/llm"
	"chatbot-alpha-1/pkg/llm/prompts"
	"chatbot-alpha-1/pkg/llm/validate"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/shared"
)

// DecisionStatus는 포맷 1번. 결정+진행보고 통합형.
// directive는 사용자 [추가 요청] 입력. 빈 문자열이면 미적용.
func DecisionStatus(
	ctx context.Context,
	c *llm.Client,
	notes []llm.Note,
	speakers []string,
	date time.Time,
	directive string,
) (*llm.DecisionStatusResponse, error) {
	userMsg := buildSummarizeUserMessage(notes, speakers, date, directive)
	raw, err := callMeetingFormat(ctx, c, "decision_status", prompts.DecisionStatus, userMsg, "meeting_decision_status", decisionStatusSchema)
	if err != nil {
		return nil, err
	}
	var out llm.DecisionStatusResponse
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, fmt.Errorf("llm: unmarshal decision_status: %w (raw=%q)", err, raw)
	}
	validate.DecisionStatus(&out, notes, speakers)
	return &out, nil
}

// Discussion은 포맷 2번. 토픽별 논의.
func Discussion(
	ctx context.Context,
	c *llm.Client,
	notes []llm.Note,
	speakers []string,
	date time.Time,
	directive string,
) (*llm.DiscussionResponse, error) {
	userMsg := buildSummarizeUserMessage(notes, speakers, date, directive)
	raw, err := callMeetingFormat(ctx, c, "discussion", prompts.Discussion, userMsg, "meeting_discussion", discussionSchema)
	if err != nil {
		return nil, err
	}
	var out llm.DiscussionResponse
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, fmt.Errorf("llm: unmarshal discussion: %w (raw=%q)", err, raw)
	}
	validate.Discussion(&out, notes, speakers)
	return &out, nil
}

// RoleBased는 포맷 3번. 역할별 정리.
func RoleBased(
	ctx context.Context,
	c *llm.Client,
	notes []llm.Note,
	speakers []string,
	date time.Time,
	directive string,
) (*llm.RoleBasedResponse, error) {
	userMsg := buildSummarizeUserMessage(notes, speakers, date, directive)
	raw, err := callMeetingFormat(ctx, c, "role_based", prompts.RoleBased, userMsg, "meeting_role_based", roleBasedSchema)
	if err != nil {
		return nil, err
	}
	var out llm.RoleBasedResponse
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, fmt.Errorf("llm: unmarshal role_based: %w (raw=%q)", err, raw)
	}
	if vErr := validate.RoleBased(&out, notes, speakers); vErr != nil {
		return nil, vErr
	}
	return &out, nil
}

// Freeform은 포맷 4번. LLM 자율 마크다운 (단일 markdown 필드 강제).
func Freeform(
	ctx context.Context,
	c *llm.Client,
	notes []llm.Note,
	speakers []string,
	date time.Time,
	directive string,
) (*llm.FreeformResponse, error) {
	userMsg := buildSummarizeUserMessage(notes, speakers, date, directive)
	raw, err := callMeetingFormat(ctx, c, "freeform", prompts.Freeform, userMsg, "meeting_freeform", freeformSchema)
	if err != nil {
		return nil, err
	}
	var out llm.FreeformResponse
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, fmt.Errorf("llm: unmarshal freeform: %w (raw=%q)", err, raw)
	}
	validate.Freeform(&out, notes)
	_ = speakers
	return &out, nil
}

// callMeetingFormat은 4 포맷 공통 chat.completions 호출 헬퍼.
// model/reasoning은 label에 따라 분기 — "weekly"는 fastRenderModel (벤치 결과 3.26× 빠름, 핵심 보존).
// 나머지 (legacy 4 포맷 finalize)는 meetingModel 유지.
func callMeetingFormat(
	ctx context.Context,
	c *llm.Client,
	label, systemPrompt, userMsg, schemaName string,
	schema map[string]any,
) (string, error) {
	model := meetingModel
	reasoning := meetingReasoning
	if label == "weekly" {
		model = fastRenderModel
		reasoning = fastRenderReasoning
	}
	log.Printf("[llm/openai] %s.new model=%s prompt_bytes=%d", label, model, len(userMsg))
	log.Printf("[llm/openai] %s prompt_preview=%q", label, previewForLog(userMsg, 300))

	resp, err := c.API().Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
		Model:           model,
		ReasoningEffort: reasoning,
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.SystemMessage(systemPrompt),
			openai.UserMessage(userMsg),
		},
		ResponseFormat: openai.ChatCompletionNewParamsResponseFormatUnion{
			OfJSONSchema: &shared.ResponseFormatJSONSchemaParam{
				JSONSchema: shared.ResponseFormatJSONSchemaJSONSchemaParam{
					Name:   schemaName,
					Strict: openai.Bool(true),
					Schema: schema,
				},
			},
		},
	})
	if err != nil {
		return "", fmt.Errorf("llm: %s chat completions failed: %w", label, err)
	}
	if len(resp.Choices) == 0 {
		return "", fmt.Errorf("llm: empty choices in %s response", label)
	}

	raw := resp.Choices[0].Message.Content
	log.Printf("[llm/openai] %s.ok response_bytes=%d prompt_tokens=%d completion_tokens=%d total_tokens=%d",
		label, len(raw), resp.Usage.PromptTokens, resp.Usage.CompletionTokens, resp.Usage.TotalTokens)
	log.Printf("[llm/openai] %s raw_preview=%q", label, previewForLog(raw, 500))
	return raw, nil
}
