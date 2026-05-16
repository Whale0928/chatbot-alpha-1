package summarize

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"chatbot-alpha-1/pkg/github"
	"chatbot-alpha-1/pkg/llm"

	"github.com/openai/openai-go/v3/option"
)

// Copilot review (PR #15) P2: Weekly의 model/reasoning 분기 회귀 가드.
// callMeetingFormat의 label=="weekly" 분기가 다시 meetingModel로 회귀하면 실패해야 한다.

type weeklyReqCapture struct {
	Model           string `json:"model"`
	ReasoningEffort string `json:"reasoning_effort"`
}

type capturingRoundTripper struct {
	captured []weeklyReqCapture
}

func (rt *capturingRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}
	var req weeklyReqCapture
	_ = json.Unmarshal(body, &req)
	rt.captured = append(rt.captured, req)

	// 항상 success — Weekly가 unmarshal 가능한 JSON 응답.
	resp := map[string]any{
		"id":      "chatcmpl-stub",
		"object":  "chat.completion",
		"created": 1,
		"model":   "gpt-5.4-mini",
		"choices": []map[string]any{
			{
				"index": 0,
				"message": map[string]any{
					"role":    "assistant",
					"content": `{"markdown": "## 주간 리포트\n- stub", "closeable": []}`,
				},
				"finish_reason": "stop",
			},
		},
	}
	raw, _ := json.Marshal(resp)
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(string(raw))),
		Request:    r,
	}, nil
}

func TestWeekly_사용모델_fastRender(t *testing.T) {
	rt := &capturingRoundTripper{}
	httpClient := &http.Client{Transport: rt}

	c, err := llm.NewClient("sk-test", option.WithHTTPClient(httpClient))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	since := time.Date(2026, 5, 9, 0, 0, 0, 0, time.UTC)
	until := time.Date(2026, 5, 16, 0, 0, 0, 0, time.UTC)
	_, err = Weekly(
		context.Background(),
		c,
		"owner/repo",
		since, until,
		[]github.Issue{}, // no issues
		[]github.Commit{
			{SHA: "abc1234", AuthorLogin: "alice", Date: since.Add(time.Hour), Message: "feat: stub"},
		},
		"",
		llm.WeeklyScopeCommits,
	)
	if err != nil {
		t.Fatalf("Weekly: %v", err)
	}
	if len(rt.captured) != 1 {
		t.Fatalf("captured requests = %d, want 1", len(rt.captured))
	}
	req := rt.captured[0]
	// 핵심 가드 — fastRenderModel + low.
	if req.Model != "gpt-5.4-mini" {
		t.Errorf("Weekly model = %q, want gpt-5.4-mini (fastRenderModel)", req.Model)
	}
	if req.ReasoningEffort != "low" {
		t.Errorf("Weekly reasoning_effort = %q, want low (fastRenderReasoning)", req.ReasoningEffort)
	}
}

// 비교 가드 — DecisionStatus / Discussion / RoleBased / Freeform (legacy finalize 4 포맷)은
// meetingModel(gpt-5.5) + meetingReasoning(medium) 유지. callMeetingFormat 분기가 weekly만 골라야 함.
func TestLegacyFinalize_사용모델_meetingModel유지(t *testing.T) {
	rt := &capturingRoundTripper{}
	httpClient := &http.Client{Transport: rt}

	c, err := llm.NewClient("sk-test", option.WithHTTPClient(httpClient))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	notes := []llm.Note{{Author: "alice", Content: "hi"}}
	speakers := []string{"alice"}
	now := time.Date(2026, 5, 14, 0, 0, 0, 0, time.UTC)

	// 4 포맷 stub은 다른 schema라 unmarshal 실패하지만 — 우리는 첫 request의 model/reasoning만 검증하면 된다.
	// 따라서 error는 무시하고 captured request만 본다.
	_, _ = DecisionStatus(context.Background(), c, notes, speakers, now, "")
	if len(rt.captured) < 1 {
		t.Fatalf("captured = %d, want >= 1", len(rt.captured))
	}
	req := rt.captured[0]
	if req.Model != "gpt-5.5" {
		t.Errorf("DecisionStatus model = %q, want gpt-5.5 (meetingModel 유지)", req.Model)
	}
	if req.ReasoningEffort != "medium" {
		t.Errorf("DecisionStatus reasoning_effort = %q, want medium", req.ReasoningEffort)
	}
}
