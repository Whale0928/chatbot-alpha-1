package summarize

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"chatbot-alpha-1/pkg/llm"

	"github.com/openai/openai-go/v3/option"
)

// stubServer는 OpenAI Chat Completions API를 흉내내는 httptest 서버.
func stubServer(t *testing.T, respBody string, statusCode int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if statusCode != http.StatusOK {
			w.WriteHeader(statusCode)
			w.Write([]byte(`{"error": {"message": "stub error"}}`))
			return
		}
		payload := map[string]any{
			"id":      "chatcmpl-stub",
			"object":  "chat.completion",
			"created": 1,
			"model":   "gpt-4o-mini",
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
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(payload)
	}))
}

func newStubClient(t *testing.T, srv *httptest.Server) *llm.Client {
	t.Helper()
	c, err := llm.NewClient("sk-test-dummy",
		option.WithBaseURL(srv.URL+"/"),
	)
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	return c
}

func Test_노트_여러건을_전달할_때_FinalNoteResponse를_반환한다(t *testing.T) {
	stubResp := `{
		"decisions": [
			{"title": "쿼리 수정 방향으로 고정", "context": ["버번 카테고리 노출 현상 확인"]}
		],
		"open_questions": ["수정 기한 확인 필요"],
		"next_steps": [
			{"who": "Whale0928", "deadline": "", "what": "필터 쿼리 수정"}
		],
		"tags": ["backend", "bug"]
	}`
	srv := stubServer(t, stubResp, http.StatusOK)
	defer srv.Close()
	c := newStubClient(t, srv)

	notes := []llm.Note{
		{Author: "hgkim", Content: "라이 필터에서 버번 카테고리가 나온다"},
		{Author: "Whale0928", Content: "내가 쿼리 수정할게"},
	}
	out, err := Meeting(context.Background(), c, notes, []string{"Whale0928", "hgkim"}, time.Now())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out.Decisions) != 1 || len(out.OpenQuestions) != 1 {
		t.Errorf("unexpected shape: %+v", out)
	}
	if out.Decisions[0].Title != "쿼리 수정 방향으로 고정" {
		t.Errorf("unexpected title: %q", out.Decisions[0].Title)
	}
	if len(out.NextSteps) != 1 || out.NextSteps[0].Who != "Whale0928" {
		t.Errorf("unexpected next_steps: %+v", out.NextSteps)
	}
}

func Test_빈_노트_목록을_전달할_때_빈_섹션을_가진_노트를_반환한다(t *testing.T) {
	stubResp := `{"decisions":[],"open_questions":[],"next_steps":[],"tags":[]}`
	srv := stubServer(t, stubResp, http.StatusOK)
	defer srv.Close()
	c := newStubClient(t, srv)

	out, err := Meeting(context.Background(), c, nil, nil, time.Now())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out.NextSteps) != 0 || len(out.Decisions) != 0 || len(out.OpenQuestions) != 0 {
		t.Errorf("expected all empty, got %+v", out)
	}
}

func Test_OpenAI_API가_500을_반환할_때_에러를_래핑하여_반환한다(t *testing.T) {
	srv := stubServer(t, "", http.StatusInternalServerError)
	defer srv.Close()
	c := newStubClient(t, srv)

	_, err := Meeting(context.Background(), c, []llm.Note{{Author: "a", Content: "hi"}}, []string{"a"}, time.Now())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "chat completions call failed") {
		t.Errorf("error should be wrapped: %v", err)
	}
}

func Test_LLM이_참석자_명단에_없는_이름을_넣을_때_경고_로그를_남긴다(t *testing.T) {
	stubResp := `{
		"topic": ["테스트"],
		"discussion": ["노트 내용과 관련 있는 논의"],
		"next_steps": [
			{"who": "unknown_ghost", "deadline": "", "what": "뭔가 한다"}
		],
		"tags": []
	}`
	srv := stubServer(t, stubResp, http.StatusOK)
	defer srv.Close()
	c := newStubClient(t, srv)

	out, err := Meeting(
		context.Background(),
		c,
		[]llm.Note{{Author: "hgkim", Content: "노트 내용과 관련 있는 논의를 하자. 뭔가 한다"}},
		[]string{"hgkim"},
		time.Now(),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.NextSteps[0].Who != "unknown_ghost" {
		t.Errorf("response should still pass through, got %+v", out.NextSteps[0])
	}
}

func Test_buildSummarizeUserMessage는_directive가_있으면_노트_앞에_prepend한다(t *testing.T) {
	got := buildSummarizeUserMessage(
		[]llm.Note{{Author: "hgkim", Content: "본문"}},
		[]string{"hgkim"},
		time.Date(2026, 5, 8, 0, 0, 0, 0, time.UTC),
		"H3 영역별로 묶어줘",
	)
	if !strings.Contains(got, "Formatting directive from the meeting host") {
		t.Errorf("missing directive header: %q", got)
	}
	if !strings.Contains(got, "H3 영역별로 묶어줘") {
		t.Errorf("missing directive body: %q", got)
	}
	directiveIdx := strings.Index(got, "H3 영역별로 묶어줘")
	notesIdx := strings.Index(got, "Notes (chronological):")
	if directiveIdx < 0 || notesIdx < 0 || directiveIdx > notesIdx {
		t.Errorf("directive must appear before notes section\n%s", got)
	}
}

func Test_buildSummarizeUserMessage는_directive가_빈문자열이면_directive블록을_생성하지_않는다(t *testing.T) {
	got := buildSummarizeUserMessage(
		[]llm.Note{{Author: "a", Content: "x"}},
		[]string{"a"},
		time.Now(),
		"",
	)
	if strings.Contains(got, "Formatting directive") {
		t.Errorf("directive block should not appear when empty: %q", got)
	}
}

func Test_buildSummarizeUserMessage는_directive에_공백만있으면_무시한다(t *testing.T) {
	got := buildSummarizeUserMessage(
		[]llm.Note{{Author: "a", Content: "x"}},
		[]string{"a"},
		time.Now(),
		"   \n\t  ",
	)
	if strings.Contains(got, "Formatting directive") {
		t.Errorf("whitespace-only directive should be ignored: %q", got)
	}
}

func Test_SDK_응답이_JSON스키마를_깬_형태일_때_파싱에러를_반환한다(t *testing.T) {
	stubResp := `not a valid json at all`
	srv := stubServer(t, stubResp, http.StatusOK)
	defer srv.Close()
	c := newStubClient(t, srv)

	_, err := Meeting(context.Background(), c, []llm.Note{{Author: "a", Content: "hi"}}, []string{"a"}, time.Now())
	if err == nil {
		t.Fatal("expected parse error, got nil")
	}
	if !strings.Contains(err.Error(), "unmarshal final note") {
		t.Errorf("expected unmarshal error, got: %v", err)
	}
}
