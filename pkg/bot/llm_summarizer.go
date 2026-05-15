package bot

import (
	"context"
	"time"

	"chatbot-alpha-1/pkg/llm"
	"chatbot-alpha-1/pkg/llm/summarize"
)

// llmSummarizer는 *llm.Client를 MeetingSummarizer 인터페이스로 어댑팅한다.
// summarize 패키지가 free function만 노출하고 *llm.Client에 메서드를
// 정의하지 않으므로 (순환 import 회피 + 패키지 분리), 이 어댑터가
// "*llm.Client + MeetingSummarizer" 가교 역할을 한다.
//
// Run 진입 시 한 번 생성되어 전역 summarizer 변수에 저장된다.
type llmSummarizer struct{ c *llm.Client }

func (a llmSummarizer) SummarizeMeeting(ctx context.Context, notes []llm.Note, speakers []string, date time.Time) (*llm.FinalNoteResponse, error) {
	return summarize.Meeting(ctx, a.c, notes, speakers, date)
}

func (a llmSummarizer) SummarizeDecisionStatus(ctx context.Context, notes []llm.Note, speakers []string, date time.Time, directive string) (*llm.DecisionStatusResponse, error) {
	return summarize.DecisionStatus(ctx, a.c, notes, speakers, date, directive)
}

func (a llmSummarizer) SummarizeDiscussion(ctx context.Context, notes []llm.Note, speakers []string, date time.Time, directive string) (*llm.DiscussionResponse, error) {
	return summarize.Discussion(ctx, a.c, notes, speakers, date, directive)
}

func (a llmSummarizer) SummarizeRoleBased(ctx context.Context, notes []llm.Note, speakers []string, date time.Time, directive string) (*llm.RoleBasedResponse, error) {
	return summarize.RoleBased(ctx, a.c, notes, speakers, date, directive)
}

func (a llmSummarizer) SummarizeFreeform(ctx context.Context, notes []llm.Note, speakers []string, date time.Time, directive string) (*llm.FreeformResponse, error) {
	return summarize.Freeform(ctx, a.c, notes, speakers, date, directive)
}

func (a llmSummarizer) SummarizeInterim(ctx context.Context, notes []llm.Note, speakers []string, date time.Time) (*llm.InterimNoteResponse, error) {
	return summarize.Interim(ctx, a.c, notes, speakers, date)
}

// ExtractContent는 Phase 2 정리본 1회 추출의 어댑터 메서드.
// summarize.ExtractContent 직접 위임 — pkg/bot/handler.go의 MeetingSummarizer 인터페이스 구현.
func (a llmSummarizer) ExtractContent(ctx context.Context, in summarize.ContentExtractionInput) (*llm.SummarizedContent, error) {
	return summarize.ExtractContent(ctx, a.c, in)
}
