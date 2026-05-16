package summarize

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"regexp"
	"sort"
	"strings"
	"time"

	"chatbot-alpha-1/pkg/llm"
	"chatbot-alpha-1/pkg/llm/prompts"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/shared"
)

const formatRenderMaxCompletionTokens int64 = 1500

var formatRenderSchema = llm.GenerateSchema[formatRenderResponse]()

// FormatRenderInput는 포맷별 LLM 재렌더 입력.
type FormatRenderInput struct {
	Content      *llm.SummarizedContent
	Format       llm.NoteFormat
	Date         time.Time
	Speakers     []string
	SpeakerRoles map[string][]string
	Directive    string
}

type formatRenderPayload struct {
	Date         string                  `json:"date"`
	Format       string                  `json:"format"`
	Speakers     []string                `json:"speakers"`
	SpeakerRoles map[string][]string     `json:"speaker_roles"`
	Directive    string                  `json:"directive,omitempty"`
	Content      formatRenderContentJSON `json:"summarized_content"`
}

type formatRenderContentJSON struct {
	Decisions      []llm.Decision             `json:"decisions"`
	Done           []string                   `json:"done"`
	InProgress     []string                   `json:"in_progress"`
	Planned        []string                   `json:"planned"`
	Blockers       []string                   `json:"blockers"`
	Topics         []llm.Topic                `json:"topics"`
	Actions        []llm.SummaryAction        `json:"actions"`
	WeeklyReports  []llm.WeeklyReportSummary  `json:"weekly_reports"`
	ReleaseResults []llm.ReleaseResultSummary `json:"release_results"`
	AgentResponses []llm.AgentResponseSummary `json:"agent_responses"`
	ExternalRefs   []llm.ExternalRefSummary   `json:"external_refs"`
	Shared         []string                   `json:"shared"`
	OpenQuestions  []string                   `json:"open_questions"`
	Tags           []string                   `json:"tags"`
}

type formatRenderResponse struct {
	Markdown string `json:"markdown" jsonschema_description:"Discord embed.Description에 들어갈 포맷별 미팅 노트 markdown. H1 제목이나 footer 없이 본문만 포함한다."`
}

// RenderFormat은 SummarizedContent를 선택 포맷의 markdown으로 LLM이 재렌더한다.
func RenderFormat(ctx context.Context, c *llm.Client, in FormatRenderInput) (string, error) {
	if c == nil {
		return "", fmt.Errorf("llm: nil client")
	}
	if in.Content == nil {
		return "", fmt.Errorf("llm: nil SummarizedContent")
	}

	systemPrompt, err := formatRenderPrompt(in.Format)
	if err != nil {
		return "", err
	}
	userMsg, err := buildFormatRenderUserMessage(in)
	if err != nil {
		return "", err
	}

	label := "render_format_" + in.Format.String()
	log.Printf("[llm/openai] %s.new model=%s prompt_bytes=%d", label, meetingModel, len(userMsg))
	log.Printf("[llm/openai] %s prompt_preview=%q", label, previewForLog(userMsg, 300))

	resp, err := c.API().Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
		Model:               meetingModel,
		ReasoningEffort:     meetingReasoning,
		MaxCompletionTokens: openai.Int(formatRenderMaxCompletionTokens),
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.SystemMessage(systemPrompt),
			openai.UserMessage(userMsg),
		},
		ResponseFormat: openai.ChatCompletionNewParamsResponseFormatUnion{
			OfJSONSchema: &shared.ResponseFormatJSONSchemaParam{
				JSONSchema: shared.ResponseFormatJSONSchemaJSONSchemaParam{
					Name:   "meeting_format_render",
					Strict: openai.Bool(true),
					Schema: formatRenderSchema,
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

	var out formatRenderResponse
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return "", fmt.Errorf("llm: unmarshal format render: %w (raw=%q)", err, raw)
	}
	markdown := strings.TrimSpace(out.Markdown)
	if markdown == "" {
		return "", fmt.Errorf("llm: empty markdown in %s response", label)
	}
	if err := validateFormatRenderMarkdown(markdown, in); err != nil {
		return "", err
	}
	return markdown, nil
}

func formatRenderPrompt(format llm.NoteFormat) (string, error) {
	switch format {
	case llm.FormatDecisionStatus:
		return prompts.FormatRenderDecisionStatus, nil
	case llm.FormatDiscussion:
		return prompts.FormatRenderDiscussion, nil
	case llm.FormatRoleBased:
		return prompts.FormatRenderRoleBased, nil
	case llm.FormatFreeform:
		return prompts.FormatRenderFreeform, nil
	default:
		return "", fmt.Errorf("llm: unsupported render format %q", format.String())
	}
}

func buildFormatRenderUserMessage(in FormatRenderInput) (string, error) {
	payload := formatRenderPayload{
		Date:         in.Date.Format("2006-01-02 (Mon)"),
		Format:       in.Format.String(),
		Speakers:     copyStrings(in.Speakers),
		SpeakerRoles: normalizeSpeakerRoles(in.SpeakerRoles),
		Directive:    strings.TrimSpace(in.Directive),
		Content:      normalizeFormatRenderContent(in.Content),
	}

	raw, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return "", fmt.Errorf("llm: marshal format render input: %w", err)
	}
	return "Render the following SummarizedContent JSON into the requested markdown format.\n\n" + string(raw), nil
}

func normalizeFormatRenderContent(c *llm.SummarizedContent) formatRenderContentJSON {
	if c == nil {
		c = &llm.SummarizedContent{}
	}
	return formatRenderContentJSON{
		Decisions:      emptyIfNil(c.Decisions),
		Done:           emptyIfNil(c.Done),
		InProgress:     emptyIfNil(c.InProgress),
		Planned:        emptyIfNil(c.Planned),
		Blockers:       emptyIfNil(c.Blockers),
		Topics:         emptyIfNil(c.Topics),
		Actions:        emptyIfNil(c.Actions),
		WeeklyReports:  emptyIfNil(c.WeeklyReports),
		ReleaseResults: emptyIfNil(c.ReleaseResults),
		AgentResponses: emptyIfNil(c.AgentResponses),
		ExternalRefs:   emptyIfNil(c.ExternalRefs),
		Shared:         emptyIfNil(c.Shared),
		OpenQuestions:  emptyIfNil(c.OpenQuestions),
		Tags:           emptyIfNil(c.Tags),
	}
}

func emptyIfNil[T any](items []T) []T {
	if items == nil {
		return []T{}
	}
	return items
}

func copyStrings(items []string) []string {
	out := append([]string(nil), items...)
	sort.Strings(out)
	return emptyIfNil(out)
}

func normalizeSpeakerRoles(src map[string][]string) map[string][]string {
	if len(src) == 0 {
		return map[string][]string{}
	}
	out := make(map[string][]string, len(src))
	for speaker, roles := range src {
		copied := append([]string(nil), roles...)
		sort.Strings(copied)
		out[speaker] = emptyIfNil(copied)
	}
	return out
}

func validateFormatRenderMarkdown(markdown string, in FormatRenderInput) error {
	// v3.2 정책: 최대 깊이 3 (root + 2 nested). 4뎁스부터 거부.
	// markdown 표준 2-space indent: 1뎁스 0, 2뎁스 2, 3뎁스 4, 4뎁스 6 spaces → 6+ spaces 거부.
	deepBullet := regexp.MustCompile(`(?m)^\s{6,}-`)
	if deepBullet.MatchString(markdown) {
		return fmt.Errorf("llm: format render bullet depth exceeded two nested levels")
	}

	allowed := allowedAttributionNames(in)
	attribution := regexp.MustCompile(`(?m)^\s*-\s+@([A-Za-z0-9_.-]+)\s*:`)
	for _, match := range attribution.FindAllStringSubmatch(markdown, -1) {
		if len(match) < 2 {
			continue
		}
		name := match[1]
		if _, ok := allowed[name]; !ok {
			return fmt.Errorf("llm: unknown attribution %q in format render output", name)
		}
	}
	return nil
}

func allowedAttributionNames(in FormatRenderInput) map[string]struct{} {
	allowed := make(map[string]struct{})
	for _, speaker := range in.Speakers {
		if speaker != "" {
			allowed[speaker] = struct{}{}
		}
	}
	for speaker := range in.SpeakerRoles {
		if speaker != "" {
			allowed[speaker] = struct{}{}
		}
	}
	if in.Content == nil {
		return allowed
	}
	for _, action := range in.Content.Actions {
		if action.Origin != "" {
			allowed[action.Origin] = struct{}{}
		}
		if action.TargetUser != "" {
			allowed[action.TargetUser] = struct{}{}
		}
	}
	return allowed
}
