package summarize

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"chatbot-alpha-1/pkg/llm"
)

func TestBuildContentExtractionUserMessage_SeparatesHumanAndContext(t *testing.T) {
	// given: 5/14 미팅 시나리오 — kimjuye/deadwhale은 사람 발화, [tool]은 weekly dump
	in := ContentExtractionInput{
		Date:     time.Date(2026, 5, 14, 19, 58, 0, 0, time.UTC),
		Speakers: []string{"deadwhale", "kimjuye"}, // Human source 발화자만
		SpeakerRoles: map[string][]string{
			"kimjuye":   {"PM"},
			"deadwhale": {"BACKEND"},
		},
		HumanNotes: []llm.Note{
			{Author: "kimjuye", Content: "workspace 통합 이슈 정리"},
			{Author: "deadwhale", Content: "큐레이션 order 확장"},
			{Author: "kimjuye", Content: "차주 미팅까지 FE 이슈 206 체크"},
		},
		ContextNotes: []llm.Note{
			{Author: "[tool]", Content: "주간 리포트: 어드민 회귀 검증 추천"},
		},
	}

	got := buildContentExtractionUserMessage(in)

	// then: 핵심 섹션 모두 등장
	mustContain := []string{
		"Date: 2026-05-14",
		"Speakers (Human source only): deadwhale, kimjuye",
		"SpeakerRoles:",
		"- kimjuye: PM",
		"- deadwhale: BACKEND",
		"=== HUMAN_NOTES (valid action.origin) ===",
		"[H1] kimjuye: workspace 통합 이슈 정리",
		"[H2] deadwhale: 큐레이션 order 확장",
		"[H3] kimjuye: 차주 미팅까지 FE 이슈 206 체크",
		"=== CONTEXT_NOTES (bot/tool/external results, NEVER action.origin) ===",
		"[C1] [tool]: 주간 리포트: 어드민 회귀 검증 추천",
	}
	for _, sub := range mustContain {
		if !strings.Contains(got, sub) {
			t.Errorf("missing section %q in user message:\n%s", sub, got)
		}
	}

	// then: HUMAN_NOTES와 CONTEXT_NOTES 분리 — [tool]이 HUMAN 섹션에 등장하면 안 됨
	humanIdx := strings.Index(got, "=== HUMAN_NOTES")
	contextIdx := strings.Index(got, "=== CONTEXT_NOTES")
	if humanIdx < 0 || contextIdx < 0 || humanIdx > contextIdx {
		t.Fatal("HUMAN_NOTES/CONTEXT_NOTES 섹션 순서 깨짐")
	}
	humanBlock := got[humanIdx:contextIdx]
	if strings.Contains(humanBlock, "[tool]") {
		t.Error("CONTEXT_NOTES author([tool])가 HUMAN_NOTES 섹션에 누출됨 — 환각 방어 토대 깨짐")
	}
}

func TestBuildContentExtractionUserMessage_EmptyGroupsHandled(t *testing.T) {
	in := ContentExtractionInput{
		Date:         time.Date(2026, 5, 14, 0, 0, 0, 0, time.UTC),
		Speakers:     []string{},
		SpeakerRoles: nil,
		HumanNotes:   nil,
		ContextNotes: nil,
	}

	got := buildContentExtractionUserMessage(in)

	if !strings.Contains(got, "(none)") {
		t.Errorf("빈 노트 그룹이 (none)으로 표시 안 됨:\n%s", got)
	}
	// 두 섹션 모두 (none)이어야 함
	if strings.Count(got, "(none)") != 2 {
		t.Errorf("(none) count = %d, want 2 (HUMAN + CONTEXT 모두 비어 있을 때)", strings.Count(got, "(none)"))
	}
}

func TestBuildContentExtractionUserMessage_SpeakerRolesSortedDeterministic(t *testing.T) {
	// 동일 입력에 대해 매번 같은 출력 — 비결정성은 디버깅 어려움
	in := ContentExtractionInput{
		Date: time.Date(2026, 5, 14, 0, 0, 0, 0, time.UTC),
		SpeakerRoles: map[string][]string{
			"zoe":   {"PM"},
			"alice": {"BACKEND"},
			"bob":   {"FRONTEND"},
		},
	}

	first := buildContentExtractionUserMessage(in)
	for i := 0; i < 5; i++ {
		got := buildContentExtractionUserMessage(in)
		if got != first {
			t.Fatalf("buildContentExtractionUserMessage가 비결정적 (run %d):\n--- first ---\n%s\n--- got ---\n%s", i+1, first, got)
		}
	}

	// 알파벳 순 — alice → bob → zoe
	aliceIdx := strings.Index(first, "alice")
	bobIdx := strings.Index(first, "bob")
	zoeIdx := strings.Index(first, "zoe")
	if !(aliceIdx < bobIdx && bobIdx < zoeIdx) {
		t.Errorf("SpeakerRoles 정렬 깨짐 — alice=%d bob=%d zoe=%d (기대: 오름차순)", aliceIdx, bobIdx, zoeIdx)
	}
}

func TestSummarizedContentSchema_Generates(t *testing.T) {
	// JSON Schema 생성이 panic/empty 안 되는지 + OpenAI strict mode 호환 형태인지 검증.
	// (invopop/jsonschema가 SummarizedContent + SummaryAction에서 잘 동작하는지)
	if summarizedContentSchema == nil {
		t.Fatal("summarizedContentSchema is nil")
	}
	if len(summarizedContentSchema) == 0 {
		t.Fatal("summarizedContentSchema is empty")
	}

	// strict mode 필수 키 — OpenAI structured output strict=true가 요구.
	// 누락 시 API에서 "missing required property" 에러.
	if got, _ := summarizedContentSchema["type"].(string); got != "object" {
		t.Errorf("schema.type = %q, want \"object\" (OpenAI strict 호환 깨짐)", got)
	}
	if _, ok := summarizedContentSchema["additionalProperties"]; !ok {
		t.Error("schema.additionalProperties 누락 — strict mode에서는 false 명시 필요")
	}
	if _, ok := summarizedContentSchema["required"]; !ok {
		t.Error("schema.required 누락 — strict mode에서는 모든 properties가 required여야 함")
	}

	// properties 키 검증 — struct 태그가 정확히 jsonschema에 반영됐는지
	props, ok := summarizedContentSchema["properties"].(map[string]any)
	if !ok {
		t.Fatal("schema.properties 타입 mismatch")
	}
	expected := []string{"decisions", "done", "in_progress", "planned", "blockers",
		"topics", "actions", "shared", "open_questions", "tags",
		"weekly_reports", "release_results", "agent_responses", "external_refs"}
	for _, name := range expected {
		if _, ok := props[name]; !ok {
			t.Errorf("schema.properties.%s 누락", name)
		}
	}

	required, ok := summarizedContentSchema["required"].([]any)
	if !ok {
		t.Fatal("schema.required 타입 mismatch")
	}
	for _, name := range expected {
		if !containsRequiredField(required, name) {
			t.Errorf("schema.required.%s 누락", name)
		}
	}

	botObjects := map[string][]string{
		"weekly_reports":  {"repo", "period_days", "commit_count", "highlights"},
		"release_results": {"module", "prev_version", "new_version", "bump_type", "pr_number", "pr_url", "highlights"},
		"agent_responses": {"question", "highlights"},
		"external_refs":   {"title", "highlights"},
	}
	for field, wantProps := range botObjects {
		itemProps := assertStrictArrayItemSchema(t, props, field)
		for _, name := range wantProps {
			if _, ok := itemProps[name]; !ok {
				t.Errorf("schema.properties.%s.items.properties.%s 누락", field, name)
			}
		}
	}
}

func containsRequiredField(required []any, name string) bool {
	for _, item := range required {
		if item == name {
			return true
		}
	}
	return false
}

func assertStrictArrayItemSchema(t *testing.T, props map[string]any, field string) map[string]any {
	t.Helper()

	fieldSchema, ok := props[field].(map[string]any)
	if !ok {
		t.Fatalf("schema.properties.%s 타입 mismatch", field)
	}
	items, ok := fieldSchema["items"].(map[string]any)
	if !ok {
		t.Fatalf("schema.properties.%s.items 타입 mismatch", field)
	}
	if got, _ := items["type"].(string); got != "object" {
		t.Fatalf("schema.properties.%s.items.type = %q, want object", field, got)
	}
	if got, ok := items["additionalProperties"].(bool); !ok || got {
		t.Fatalf("schema.properties.%s.items.additionalProperties = %v, want false", field, items["additionalProperties"])
	}
	itemProps, ok := items["properties"].(map[string]any)
	if !ok {
		t.Fatalf("schema.properties.%s.items.properties 타입 mismatch", field)
	}
	return itemProps
}

func TestSummarizedContent_BotResultFieldsRoundTripJSON(t *testing.T) {
	in := llm.SummarizedContent{
		WeeklyReports: []llm.WeeklyReportSummary{
			{
				Repo:        "opnd/chatbot-alpha-1",
				PeriodDays:  7,
				CommitCount: 12,
				Highlights:  []string{"릴리즈 플로우 정리", "테스트 보강"},
			},
		},
		ReleaseResults: []llm.ReleaseResultSummary{
			{
				Module:      "product",
				PrevVersion: "1.1.4",
				NewVersion:  "1.1.5",
				BumpType:    "patch",
				PRNumber:    42,
				PRURL:       "https://gitea.example/opnd/product/pulls/42",
				Highlights:  []string{"버전 bump", "릴리즈 노트 생성"},
			},
		},
		AgentResponses: []llm.AgentResponseSummary{
			{
				Question:   "큐레이션 정렬 정책은?",
				Highlights: []string{"order 필드 기준 정렬", "동률이면 생성일 기준"},
			},
		},
		ExternalRefs: []llm.ExternalRefSummary{
			{
				Title:      "Vendor latency 보고서",
				Highlights: []string{"p95 420ms", "한국 리전 지연 증가"},
			},
		},
	}

	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("json.Marshal failed: %v", err)
	}

	var out llm.SummarizedContent
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("json.Unmarshal failed: %v", err)
	}

	if len(out.WeeklyReports) != 1 || out.WeeklyReports[0].Repo != "opnd/chatbot-alpha-1" {
		t.Fatalf("weekly_reports round trip mismatch: %+v", out.WeeklyReports)
	}
	if len(out.ReleaseResults) != 1 || out.ReleaseResults[0].PRNumber != 42 {
		t.Fatalf("release_results round trip mismatch: %+v", out.ReleaseResults)
	}
	if len(out.AgentResponses) != 1 || out.AgentResponses[0].Question != "큐레이션 정렬 정책은?" {
		t.Fatalf("agent_responses round trip mismatch: %+v", out.AgentResponses)
	}
	if len(out.ExternalRefs) != 1 || out.ExternalRefs[0].Title != "Vendor latency 보고서" {
		t.Fatalf("external_refs round trip mismatch: %+v", out.ExternalRefs)
	}
}
