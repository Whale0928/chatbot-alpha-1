package summarize

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"chatbot-alpha-1/pkg/llm"

	"github.com/openai/openai-go/v3/option"
)

type renderFormatRequest struct {
	Messages []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"messages"`
	MaxCompletionTokens int `json:"max_completion_tokens"`
	ResponseFormat      struct {
		Type       string `json:"type"`
		JSONSchema struct {
			Name string `json:"name"`
		} `json:"json_schema"`
	} `json:"response_format"`
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func newRenderFormatStubClient(
	t *testing.T,
	respBody string,
	statusCode int,
	requests *[]renderFormatRequest,
) *llm.Client {
	t.Helper()
	httpClient := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var req renderFormatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("request decode failed: %v", err)
		}
		*requests = append(*requests, req)

		if statusCode != http.StatusOK {
			return renderFormatHTTPResponse(statusCode, `{"error": {"message": "stub error"}}`), nil
		}
		payload := map[string]any{
			"id":      "chatcmpl-render-format-stub",
			"object":  "chat.completion",
			"created": 1,
			"model":   "gpt-5.5",
			"choices": []map[string]any{
				{
					"index": 0,
					"message": map[string]any{
						"role":    "assistant",
						"content": respBody,
					},
					"finish_reason": "stop",
				},
			},
			"usage": map[string]any{
				"prompt_tokens":     100,
				"completion_tokens": 20,
				"total_tokens":      120,
			},
		}
		raw, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("response marshal failed: %v", err)
		}
		return renderFormatHTTPResponse(http.StatusOK, string(raw)), nil
	})}
	c, err := llm.NewClient("sk-test-dummy", option.WithHTTPClient(httpClient))
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	return c
}

func renderFormatHTTPResponse(statusCode int, body string) *http.Response {
	return &http.Response{
		StatusCode: statusCode,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func TestRenderFormat_포맷별_MockLLM응답을_markdown으로_반환한다(t *testing.T) {
	cases := []struct {
		name       string
		format     llm.NoteFormat
		systemHint string
		markdown   string
	}{
		{
			name:       "decision_status",
			format:     llm.FormatDecisionStatus,
			systemHint: "decision_status",
			markdown:   "## 📊 주간 분석\n- 주간 리포트 확인\n\n## 🗣️ 사람 결정사항\n- `alice`: 배포는 금요일 진행",
		},
		{
			name:       "discussion",
			format:     llm.FormatDiscussion,
			systemHint: "discussion",
			markdown:   "## 💬 토픽\n- 배포 전략 논의\n\n## 📊 봇 도구 결과\n- 참고 자료로 주간 분석 사용",
		},
		{
			name:       "role_based",
			format:     llm.FormatRoleBased,
			systemHint: "role_based",
			markdown:   "## BACKEND\n- 자기 발의 액션: API 안정화\n\n## 📊 공통 자료\n- 릴리즈 PR #42",
		},
		{
			name:       "freeform",
			format:     llm.FormatFreeform,
			systemHint: "freeform",
			markdown:   "## 회의 흐름\n- 릴리즈와 운영 이슈를 함께 정리\n\n## 참고 자료\n- 외부 자료 검토",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var requests []renderFormatRequest
			stubResp := mustMarkdownJSON(t, tc.markdown)
			c := newRenderFormatStubClient(t, stubResp, http.StatusOK, &requests)

			got, err := RenderFormat(context.Background(), c, sampleFormatRenderInput(tc.format))
			if err != nil {
				t.Fatalf("RenderFormat returned error: %v", err)
			}
			if got != tc.markdown {
				t.Fatalf("markdown mismatch\nwant:\n%s\n\ngot:\n%s", tc.markdown, got)
			}

			if len(requests) != 1 {
				t.Fatalf("requests = %d, want 1", len(requests))
			}
			req := requests[0]
			if req.MaxCompletionTokens != 1500 {
				t.Fatalf("max_completion_tokens = %d, want 1500", req.MaxCompletionTokens)
			}
			if req.ResponseFormat.JSONSchema.Name != "meeting_format_render" {
				t.Fatalf("schema name = %q, want meeting_format_render", req.ResponseFormat.JSONSchema.Name)
			}
			if len(req.Messages) != 2 {
				t.Fatalf("messages = %d, want 2", len(req.Messages))
			}
			if !strings.Contains(req.Messages[0].Content, tc.systemHint) {
				t.Fatalf("system prompt missing format hint %q:\n%s", tc.systemHint, req.Messages[0].Content)
			}
			assertRenderUserMessageHasAllFields(t, req.Messages[1].Content)
		})
	}
}

// v3.2 정책: 빈 SummarizedContent 입력 시 LLM은 "(없음)" 플레이스홀더 없이 단일 문장 "정리할 내용이 없습니다."를 반환해야 한다.
func TestRenderFormat_빈SummarizedContent일때_정리없음_단일문장을_반환한다(t *testing.T) {
	markdown := "정리할 내용이 없습니다."
	var requests []renderFormatRequest
	c := newRenderFormatStubClient(t, mustMarkdownJSON(t, markdown), http.StatusOK, &requests)

	got, err := RenderFormat(context.Background(), c, FormatRenderInput{
		Content: &llm.SummarizedContent{},
		Format:  llm.FormatDecisionStatus,
		Date:    time.Date(2026, 5, 14, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("RenderFormat returned error: %v", err)
	}
	if got != markdown {
		t.Fatalf("expected single-sentence output for empty content, got:\n%s", got)
	}
	if strings.Contains(got, "(없음)") {
		t.Fatalf("v3.2 policy: output must not contain '(없음)' placeholder, got:\n%s", got)
	}
	if !strings.Contains(requests[0].Messages[1].Content, `"decisions": []`) {
		t.Fatalf("empty arrays should be serialized explicitly:\n%s", requests[0].Messages[1].Content)
	}
}

// v3.2 정책: LLM이 "(없음)" 플레이스홀더를 출력하면 validator가 거부한다.
func TestRenderFormat_없음플레이스홀더_출력시_거부한다(t *testing.T) {
	markdown := "## 이번 회의에서 합의한 결정\n- (없음)\n\n## 앞으로 진행할 작업\n- (없음)"
	var requests []renderFormatRequest
	c := newRenderFormatStubClient(t, mustMarkdownJSON(t, markdown), http.StatusOK, &requests)

	_, err := RenderFormat(context.Background(), c, FormatRenderInput{
		Content: &llm.SummarizedContent{},
		Format:  llm.FormatDecisionStatus,
		Date:    time.Date(2026, 5, 14, 0, 0, 0, 0, time.UTC),
	})
	if err == nil {
		t.Fatal("expected validator to reject '(없음)' placeholder, got nil")
	}
	if !strings.Contains(err.Error(), "(없음)") {
		t.Fatalf("expected '(없음)' in error message, got: %v", err)
	}
}

// v3.2 정책: 1뎁스(0 space) + 2뎁스(2 space) + 3뎁스(4 space) OK, 4뎁스(6 space) 거부.
func TestRenderFormat_BulletDepth3단까지_허용(t *testing.T) {
	markdown := "## 결정\n- 1차 bullet\n  - 2차 bullet 허용\n    - 3차 bullet 허용\n- 다음 항목"
	var requests []renderFormatRequest
	c := newRenderFormatStubClient(t, mustMarkdownJSON(t, markdown), http.StatusOK, &requests)

	got, err := RenderFormat(context.Background(), c, sampleFormatRenderInput(llm.FormatFreeform))
	if err != nil {
		t.Fatalf("RenderFormat returned error: %v", err)
	}
	if got != markdown {
		t.Fatalf("markdown mismatch want=%q got=%q", markdown, got)
	}
}

func TestRenderFormat_BulletDepth4단이상_거부(t *testing.T) {
	// 6 space (4뎁스) 이상 bullet은 validator가 거부 → fallback renderer로 떨어진다.
	markdown := "## 결정\n- 1차\n  - 2차\n    - 3차\n      - 4차 (이건 거부됨)"
	var requests []renderFormatRequest
	c := newRenderFormatStubClient(t, mustMarkdownJSON(t, markdown), http.StatusOK, &requests)

	_, err := RenderFormat(context.Background(), c, sampleFormatRenderInput(llm.FormatFreeform))
	if err == nil {
		t.Fatal("expected validator to reject 4-depth bullet, got nil")
	}
	if !strings.Contains(err.Error(), "bullet depth exceeded two nested levels") {
		t.Fatalf("expected 'bullet depth exceeded two nested levels' error, got: %v", err)
	}
}

func TestRenderFormat_Origin에_없는_author_attribution을_거부한다(t *testing.T) {
	markdown := "## 📋 액션 아이템\n- @bob: API 안정화"
	var requests []renderFormatRequest
	c := newRenderFormatStubClient(t, mustMarkdownJSON(t, markdown), http.StatusOK, &requests)

	_, err := RenderFormat(context.Background(), c, FormatRenderInput{
		Content: &llm.SummarizedContent{
			Actions: []llm.SummaryAction{
				{What: "API 안정화", Origin: "alice", OriginRoles: []string{"BACKEND"}, TargetRoles: []string{"BACKEND"}},
			},
		},
		Format:   llm.FormatDecisionStatus,
		Date:     time.Date(2026, 5, 14, 0, 0, 0, 0, time.UTC),
		Speakers: []string{"alice"},
	})
	if err == nil {
		t.Fatal("expected attribution validation error, got nil")
	}
	if !strings.Contains(err.Error(), "unknown attribution") {
		t.Fatalf("expected unknown attribution error, got: %v", err)
	}
}

func TestRenderFormat_AtPrefix없는_일반Bullet은_attribution으로_보지않는다(t *testing.T) {
	markdown := "## 릴리즈\n- product: v1.1.6\n- backend: 큰 변경"
	var requests []renderFormatRequest
	c := newRenderFormatStubClient(t, mustMarkdownJSON(t, markdown), http.StatusOK, &requests)

	got, err := RenderFormat(context.Background(), c, FormatRenderInput{
		Content: &llm.SummarizedContent{
			ReleaseResults: []llm.ReleaseResultSummary{
				{Module: "product", NewVersion: "1.1.6", Highlights: []string{"backend 큰 변경"}},
			},
		},
		Format:   llm.FormatDecisionStatus,
		Date:     time.Date(2026, 5, 14, 0, 0, 0, 0, time.UTC),
		Speakers: []string{"alice"},
	})
	if err != nil {
		t.Fatalf("RenderFormat returned error for non-attribution bullet: %v", err)
	}
	if got != markdown {
		t.Fatalf("markdown mismatch\nwant:\n%s\n\ngot:\n%s", markdown, got)
	}
}

func mustMarkdownJSON(t *testing.T, markdown string) string {
	t.Helper()
	raw, err := json.Marshal(llm.FreeformResponse{Markdown: markdown})
	if err != nil {
		t.Fatalf("json.Marshal failed: %v", err)
	}
	return string(raw)
}

func sampleFormatRenderInput(format llm.NoteFormat) FormatRenderInput {
	return FormatRenderInput{
		Content: &llm.SummarizedContent{
			Decisions: []llm.Decision{
				{Title: "배포는 금요일 진행", Context: []string{"QA 완료 후 진행"}},
			},
			Done:       []string{"API 안정화 완료"},
			InProgress: []string{"프론트 회귀 테스트 진행 중"},
			Planned:    []string{"금요일 배포 예정"},
			Blockers:   []string{"스테이징 간헐 오류"},
			Topics: []llm.Topic{
				{Title: "배포 전략", Flow: []string{"금요일 배포 후보 논의"}, Insights: []string{"QA 완료가 선행 조건"}},
			},
			Actions: []llm.SummaryAction{
				{What: "API 안정화", Origin: "alice", OriginRoles: []string{"BACKEND"}, TargetRoles: []string{"BACKEND"}},
			},
			WeeklyReports: []llm.WeeklyReportSummary{
				{Repo: "opnd/chatbot-alpha-1", PeriodDays: 7, CommitCount: 12, Highlights: []string{"테스트 보강"}},
			},
			ReleaseResults: []llm.ReleaseResultSummary{
				{Module: "product", PrevVersion: "1.1.4", NewVersion: "1.1.5", BumpType: "patch", PRNumber: 42, PRURL: "https://gitea.example/pr/42", Highlights: []string{"릴리즈 노트 생성"}},
			},
			AgentResponses: []llm.AgentResponseSummary{
				{Question: "정렬 정책은?", Highlights: []string{"order 기준"}},
			},
			ExternalRefs: []llm.ExternalRefSummary{
				{Title: "Vendor latency 보고서", Highlights: []string{"p95 420ms"}},
			},
			Shared:        []string{"릴리즈 전 공지 필요"},
			OpenQuestions: []string{"배포 시간대 확인 필요"},
			Tags:          []string{"release", "backend"},
		},
		Format:       format,
		Date:         time.Date(2026, 5, 14, 0, 0, 0, 0, time.UTC),
		Speakers:     []string{"alice", "bob"},
		SpeakerRoles: map[string][]string{"alice": []string{"BACKEND"}, "bob": []string{"FRONTEND"}},
		Directive:    "짧게 정리",
	}
}

func assertRenderUserMessageHasAllFields(t *testing.T, msg string) {
	t.Helper()
	required := []string{
		`"decisions"`,
		`"done"`,
		`"in_progress"`,
		`"planned"`,
		`"blockers"`,
		`"topics"`,
		`"actions"`,
		`"weekly_reports"`,
		`"release_results"`,
		`"agent_responses"`,
		`"external_refs"`,
		`"shared"`,
		`"open_questions"`,
		`"tags"`,
	}
	for _, field := range required {
		if !strings.Contains(msg, field) {
			t.Fatalf("user message missing %s:\n%s", field, msg)
		}
	}
}
