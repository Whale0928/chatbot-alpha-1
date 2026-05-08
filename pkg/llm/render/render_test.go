package render

import (
	"strings"
	"testing"
	"time"

	"chatbot-alpha-1/pkg/llm"
)

func fixedDate() time.Time {
	return time.Date(2026, 4, 16, 13, 0, 0, 0, time.UTC)
}

// === Final Note 렌더 테스트 ===

func Test_빈_FinalNote_렌더링시_결정_다음단계_헤더만_존재한다(t *testing.T) {
	out := RenderFinalNote(RenderInput{
		Date:         fixedDate(),
		Participants: []string{"hgkim"},
		Response:     &llm.FinalNoteResponse{},
	})
	for _, h := range []string{"## 결정", "## 다음 단계"} {
		if !strings.Contains(out, h) {
			t.Errorf("missing %q in:\n%s", h, out)
		}
	}
	if strings.Contains(out, "## 확인 필요") {
		t.Errorf("OpenQuestions 비어있는데 확인 필요 섹션이 렌더됨:\n%s", out)
	}
	if !strings.Contains(out, "# 2026-04-16 미팅 노트") {
		t.Errorf("제목 누락:\n%s", out)
	}
}

func Test_Decision에_Context_자식이_있을때_2단_bullet으로_렌더된다(t *testing.T) {
	out := RenderFinalNote(RenderInput{
		Date:         fixedDate(),
		Participants: []string{"hgkim"},
		Response: &llm.FinalNoteResponse{
			Decisions: []llm.Decision{
				{
					Title:   "캐시 프록시 구현 완료",
					Context: []string{"고객사 제공 기능으로는 부적합", "외부에는 '작업 중'으로 커뮤니케이션"},
				},
				{
					Title:   "메일 포맷은 하나로 고정",
					Context: nil,
				},
			},
		},
	})
	expected := []string{
		"- **캐시 프록시 구현 완료**",
		"  - 고객사 제공 기능으로는 부적합",
		"  - 외부에는 '작업 중'으로 커뮤니케이션",
		"- **메일 포맷은 하나로 고정**",
	}
	for _, exp := range expected {
		if !strings.Contains(out, exp) {
			t.Errorf("missing %q in:\n%s", exp, out)
		}
	}
}

func Test_Decision에_Context가_없을때_title만_렌더된다(t *testing.T) {
	out := RenderFinalNote(RenderInput{
		Date:         fixedDate(),
		Participants: []string{"hgkim"},
		Response: &llm.FinalNoteResponse{
			Decisions: []llm.Decision{
				{Title: "중복 크롤링 금지", Context: nil},
			},
		},
	})
	if !strings.Contains(out, "- **중복 크롤링 금지**") {
		t.Errorf("title 누락:\n%s", out)
	}
	if strings.Contains(out, "  - ") {
		t.Errorf("Context 없는데 자식 bullet이 있음:\n%s", out)
	}
}

func Test_OpenQuestions가_있을때_확인필요_섹션이_렌더된다(t *testing.T) {
	out := RenderFinalNote(RenderInput{
		Date:         fixedDate(),
		Participants: []string{"hgkim"},
		Response: &llm.FinalNoteResponse{
			Decisions:     []llm.Decision{{Title: "x", Context: nil}},
			OpenQuestions: []string{"셀레니움 채택 여부 - 확인 필요", "DB 운영 전환 - 확인 필요"},
		},
	})
	if !strings.Contains(out, "## 확인 필요") {
		t.Errorf("확인 필요 헤더 누락:\n%s", out)
	}
	for _, exp := range []string{"셀레니움 채택 여부", "DB 운영 전환"} {
		if !strings.Contains(out, exp) {
			t.Errorf("missing %q:\n%s", exp, out)
		}
	}
}

func Test_OpenQuestions가_비어있으면_확인필요_섹션_생략(t *testing.T) {
	out := RenderFinalNote(RenderInput{
		Date:         fixedDate(),
		Participants: []string{"hgkim"},
		Response: &llm.FinalNoteResponse{
			Decisions: []llm.Decision{{Title: "x"}},
		},
	})
	if strings.Contains(out, "## 확인 필요") {
		t.Errorf("비어있는데 렌더됨:\n%s", out)
	}
}

func Test_섹션순서는_결정_확인필요_다음단계이다(t *testing.T) {
	out := RenderFinalNote(RenderInput{
		Date:         fixedDate(),
		Participants: []string{"hgkim"},
		Response: &llm.FinalNoteResponse{
			Decisions:     []llm.Decision{{Title: "결정 하나"}},
			OpenQuestions: []string{"확인 필요 하나"},
			NextSteps:     []llm.NextStep{{Who: "hgkim", What: "할 일"}},
		},
	})
	idxDec := strings.Index(out, "## 결정")
	idxOpen := strings.Index(out, "## 확인 필요")
	idxTodo := strings.Index(out, "## 다음 단계")
	if !(idxDec < idxOpen && idxOpen < idxTodo) {
		t.Errorf("순서 오류 decisions=%d open=%d todo=%d\n%s", idxDec, idxOpen, idxTodo, out)
	}
}

func Test_풋터에_참석자와_태그가_렌더된다(t *testing.T) {
	out := RenderFinalNote(RenderInput{
		Date:         fixedDate(),
		Participants: []string{"hgkim", "Whale0928"},
		Response: &llm.FinalNoteResponse{
			Decisions: []llm.Decision{{Title: "x"}},
			Tags:      []string{"크롤러", "DB"},
		},
	})
	if !strings.Contains(out, "---\n") {
		t.Errorf("풋터 구분선 누락:\n%s", out)
	}
	if !strings.Contains(out, "참석자: `hgkim`, `Whale0928`") {
		t.Errorf("참석자 누락:\n%s", out)
	}
	if !strings.Contains(out, "태그: #크롤러 #DB") {
		t.Errorf("태그 누락:\n%s", out)
	}
}

func Test_참석자와_태그_비어있으면_풋터_생략(t *testing.T) {
	out := RenderFinalNote(RenderInput{
		Date:         fixedDate(),
		Participants: nil,
		Response:     &llm.FinalNoteResponse{Decisions: []llm.Decision{{Title: "x"}}},
	})
	if strings.Contains(out, "---\n") {
		t.Errorf("풋터 있으면 안 됨:\n%s", out)
	}
}

func Test_NextStep_기한있을때_italic_parenthetical(t *testing.T) {
	out := RenderFinalNote(RenderInput{
		Date: fixedDate(),
		Response: &llm.FinalNoteResponse{
			NextSteps: []llm.NextStep{{Who: "hgkim", Deadline: "2026-04-18", What: "작업"}},
		},
	})
	if !strings.Contains(out, "_(기한: 2026-04-18)_") {
		t.Errorf("기한 parenthetical 누락:\n%s", out)
	}
}

func Test_NextStep_who_없으면_담당자_미정(t *testing.T) {
	out := RenderFinalNote(RenderInput{
		Date: fixedDate(),
		Response: &llm.FinalNoteResponse{
			NextSteps: []llm.NextStep{{Who: "", Deadline: "", What: "리디자인"}},
		},
	})
	if !strings.Contains(out, "(담당자 미정)") {
		t.Errorf("담당자 미정 누락:\n%s", out)
	}
}

func Test_Response_nil이면_panic없이_빈_섹션(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("panic: %v", r)
		}
	}()
	out := RenderFinalNote(RenderInput{Date: fixedDate(), Response: nil})
	if !strings.Contains(out, "## 결정") {
		t.Errorf("빈 섹션 누락:\n%s", out)
	}
}

// === Interim Note 렌더 테스트 ===

func Test_Interim_Decision_자식_렌더(t *testing.T) {
	out := RenderInterimNote(InterimRenderInput{
		Date:         fixedDate(),
		Participants: []string{"hgkim"},
		Response: &llm.InterimNoteResponse{
			Decisions: []llm.Decision{
				{Title: "Redis 고정", Context: []string{"TTL 1시간"}},
			},
			OpenQuestions: []string{"TTL 근거 - 확인 필요"},
		},
	})
	expected := []string{
		"**현재까지 미팅 정리**",
		"**지금까지 나온 결정**",
		"- **Redis 고정**",
		"  - TTL 1시간",
		"**확인 필요**",
		"TTL 근거 - 확인 필요",
	}
	for _, exp := range expected {
		if !strings.Contains(out, exp) {
			t.Errorf("missing %q in:\n%s", exp, out)
		}
	}
}

func Test_Interim_빈_섹션은_생략(t *testing.T) {
	out := RenderInterimNote(InterimRenderInput{
		Date:         fixedDate(),
		Participants: []string{"hgkim"},
		Response: &llm.InterimNoteResponse{
			Decisions: []llm.Decision{{Title: "x"}},
		},
	})
	if strings.Contains(out, "**확인 필요**") {
		t.Errorf("빈 OpenQuestions 섹션이 렌더됨:\n%s", out)
	}
}
