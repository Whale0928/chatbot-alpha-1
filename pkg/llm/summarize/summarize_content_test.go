package summarize

import (
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
		"=== CONTEXT_NOTES (background only, NEVER action.origin) ===",
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
		"topics", "actions", "shared", "open_questions", "tags"}
	for _, name := range expected {
		if _, ok := props[name]; !ok {
			t.Errorf("schema.properties.%s 누락", name)
		}
	}
}
