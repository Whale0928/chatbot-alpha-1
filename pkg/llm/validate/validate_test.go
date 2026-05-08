package validate

import (
	"testing"

	"chatbot-alpha-1/pkg/llm"
)

func Test_Decision_title이_원본_노트와_토큰겹침이_있을_때_경고없이_통과한다(t *testing.T) {
	notes := []llm.Note{
		{Author: "hgkim", Content: "라이 위스키 필터에서 버번 카테고리 노출"},
	}
	note := &llm.FinalNoteResponse{
		Decisions: []llm.Decision{{Title: "라이 위스키 필터 버그", Context: nil}},
	}
	AgainstNotes(note, notes, []string{"hgkim"})
}

func Test_토큰_추출이_길이_2자_미만을_걸러낸다(t *testing.T) {
	toks := tokenize("a b cd ef")
	if len(toks) != 2 {
		t.Fatalf("expected 2 tokens (cd, ef), got %v", toks)
	}
}

func Test_한글_2자_이상_토큰을_추출한다(t *testing.T) {
	toks := tokenize("라이 필터 버 그")
	// "라이", "필터"는 유지, "버", "그"는 1자라 제외
	if len(toks) != 2 {
		t.Fatalf("expected 2 tokens, got %v", toks)
	}
}

func Test_hasAnyTokenOverlap은_겹치는_토큰이_하나라도_있으면_true이다(t *testing.T) {
	corpus := "라이 위스키 필터 버그"
	if !hasAnyTokenOverlap("필터 수정", corpus) {
		t.Error("expected overlap on 필터")
	}
	if hasAnyTokenOverlap("완전히 무관한 내용", corpus) {
		t.Error("expected no overlap")
	}
}
