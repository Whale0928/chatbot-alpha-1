package bot

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"chatbot-alpha-1/pkg/llm"
	"chatbot-alpha-1/pkg/llm/summarize"

	"github.com/bwmarrin/discordgo"
)

// --- Test doubles ---

type sentMessage struct {
	ChannelID string
	Content   string
}

type fakeMessenger struct {
	sent        []sentMessage
	sentComplex []sentMessage
	edited      []sentMessage // edited messages (ChannelID/Content + MsgID는 별도)
	editedIDs   []string      // channelID:messageID 형식
	deleted     []string      // deleted message IDs (channelID:messageID 형식)
	nextMsgID   int           // 연속적인 mock 메시지 ID 할당
	mu          sync.Mutex    // progress goroutine과 동시 접근 보호
}

func (f *fakeMessenger) ChannelMessageSend(channelID, content string, _ ...discordgo.RequestOption) (*discordgo.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, sentMessage{ChannelID: channelID, Content: content})
	f.nextMsgID++
	return &discordgo.Message{ID: fmt.Sprintf("mock-msg-%d", f.nextMsgID)}, nil
}

func (f *fakeMessenger) ChannelMessageSendComplex(channelID string, data *discordgo.MessageSend, _ ...discordgo.RequestOption) (*discordgo.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sentComplex = append(f.sentComplex, sentMessage{ChannelID: channelID, Content: data.Content})
	f.nextMsgID++
	return &discordgo.Message{ID: fmt.Sprintf("mock-msg-%d", f.nextMsgID)}, nil
}

func (f *fakeMessenger) ChannelMessageEdit(channelID, messageID, content string, _ ...discordgo.RequestOption) (*discordgo.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.edited = append(f.edited, sentMessage{ChannelID: channelID, Content: content})
	f.editedIDs = append(f.editedIDs, channelID+":"+messageID)
	return &discordgo.Message{ID: messageID}, nil
}

func (f *fakeMessenger) ChannelMessageDelete(channelID, messageID string, _ ...discordgo.RequestOption) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, channelID+":"+messageID)
	return nil
}

type fakeSummarizer struct {
	// legacy
	calls    int
	response *llm.FinalNoteResponse
	err      error

	// 4 포맷 — 각 호출 카운터 + 응답/에러 주입
	decisionStatusCalls int
	decisionStatusResp  *llm.DecisionStatusResponse
	decisionStatusErr   error

	discussionCalls int
	discussionResp  *llm.DiscussionResponse
	discussionErr   error

	roleBasedCalls int
	roleBasedResp  *llm.RoleBasedResponse
	roleBasedErr   error

	freeformCalls int
	freeformResp  *llm.FreeformResponse
	freeformErr   error

	interimResp  *llm.InterimNoteResponse
	interimErr   error
	interimCalls int

	// Phase 2 — ExtractContent
	extractResp  *llm.SummarizedContent
	extractErr   error
	extractCalls int
	lastExtract  summarize.ContentExtractionInput

	// capture inputs
	lastNotes     []llm.Note
	lastSpeakers  []string
	lastDirective string
}

func (f *fakeSummarizer) SummarizeMeeting(_ context.Context, notes []llm.Note, speakers []string, _ time.Time) (*llm.FinalNoteResponse, error) {
	f.calls++
	f.lastNotes = notes
	f.lastSpeakers = speakers
	return f.response, f.err
}

func (f *fakeSummarizer) SummarizeDecisionStatus(_ context.Context, notes []llm.Note, speakers []string, _ time.Time, directive string) (*llm.DecisionStatusResponse, error) {
	f.decisionStatusCalls++
	f.lastNotes = notes
	f.lastSpeakers = speakers
	f.lastDirective = directive
	return f.decisionStatusResp, f.decisionStatusErr
}

func (f *fakeSummarizer) SummarizeDiscussion(_ context.Context, notes []llm.Note, speakers []string, _ time.Time, directive string) (*llm.DiscussionResponse, error) {
	f.discussionCalls++
	f.lastNotes = notes
	f.lastSpeakers = speakers
	f.lastDirective = directive
	return f.discussionResp, f.discussionErr
}

func (f *fakeSummarizer) SummarizeRoleBased(_ context.Context, notes []llm.Note, speakers []string, _ time.Time, directive string) (*llm.RoleBasedResponse, error) {
	f.roleBasedCalls++
	f.lastNotes = notes
	f.lastSpeakers = speakers
	f.lastDirective = directive
	return f.roleBasedResp, f.roleBasedErr
}

func (f *fakeSummarizer) SummarizeFreeform(_ context.Context, notes []llm.Note, speakers []string, _ time.Time, directive string) (*llm.FreeformResponse, error) {
	f.freeformCalls++
	f.lastNotes = notes
	f.lastSpeakers = speakers
	f.lastDirective = directive
	return f.freeformResp, f.freeformErr
}

func (f *fakeSummarizer) SummarizeInterim(_ context.Context, notes []llm.Note, speakers []string, _ time.Time) (*llm.InterimNoteResponse, error) {
	f.interimCalls++
	f.lastNotes = notes
	f.lastSpeakers = speakers
	return f.interimResp, f.interimErr
}

// ExtractContent는 Phase 2 정리본 1회 추출의 fake 구현.
// 호출 횟수와 입력 input을 캡처해 테스트가 검증.
func (f *fakeSummarizer) ExtractContent(_ context.Context, in summarize.ContentExtractionInput) (*llm.SummarizedContent, error) {
	f.extractCalls++
	f.lastExtract = in
	return f.extractResp, f.extractErr
}

func newTestSession() *Session {
	return &Session{
		Mode:     ModeMeeting,
		State:    StateMeeting,
		ThreadID: "thread-xyz",
		UserID:   "user-1",
	}
}

// --- Scenarios ---

func Test_미팅_종료_시_DecisionStatus를_호출하고_결과를_전송한다(t *testing.T) {
	sess := newTestSession()
	sess.AddNote("hgkim", "라이 필터 버그 확인")
	sess.AddNote("Whale0928", "쿼리 수정 필요")

	msg := &fakeMessenger{}
	summ := &fakeSummarizer{
		decisionStatusResp: &llm.DecisionStatusResponse{
			Decisions: []llm.Decision{
				{Title: "필터 쿼리 수정 방향 확정", Context: []string{"버번 카테고리 노출 확인"}},
			},
			NextSteps: []llm.NextStep{
				{Who: "Whale0928", Deadline: "2026-04-18", What: "쿼리 수정"},
			},
			Tags: []string{"backend", "bug"},
		},
	}

	keep := finalizeMeeting(context.Background(), msg, summ, sess, time.Now(), llm.FormatDecisionStatus, "")

	if keep {
		t.Fatal("expected keepSession=false on success")
	}
	if summ.decisionStatusCalls != 1 {
		t.Errorf("expected 1 SummarizeDecisionStatus call, got %d", summ.decisionStatusCalls)
	}
	if len(summ.lastNotes) != 2 {
		t.Errorf("expected 2 notes passed, got %d", len(summ.lastNotes))
	}
	if len(summ.lastSpeakers) != 2 {
		t.Errorf("expected 2 speakers, got %v", summ.lastSpeakers)
	}
	if len(msg.sent) != 1 {
		t.Fatalf("expected 1 message sent, got %d", len(msg.sent))
	}
	if msg.sent[0].ChannelID != "thread-xyz" {
		t.Errorf("wrong channel: %q", msg.sent[0].ChannelID)
	}
	body := msg.sent[0].Content
	for _, exp := range []string{"# 2026-", "## 결정", "## 다음 단계", "`Whale0928`"} {
		if !strings.Contains(body, exp) {
			t.Errorf("missing %q in rendered output:\n%s", exp, body)
		}
	}
}

func Test_LLM_호출이_실패할_때_세션을_정리하지_않고_에러메시지를_전송한다(t *testing.T) {
	sess := newTestSession()
	sess.AddNote("hgkim", "메모 1")
	sess.AddNote("hgkim", "메모 2")
	sess.AddNote("hgkim", "메모 3")

	msg := &fakeMessenger{}
	summ := &fakeSummarizer{decisionStatusErr: errors.New("rate limit")}

	keep := finalizeMeeting(context.Background(), msg, summ, sess, time.Now(), llm.FormatDecisionStatus, "")

	if !keep {
		t.Fatal("expected keepSession=true on LLM failure")
	}
	if len(msg.sent) != 1 {
		t.Fatalf("expected 1 error message, got %d", len(msg.sent))
	}
	if !strings.Contains(msg.sent[0].Content, "오류") {
		t.Errorf("error message should mention 오류: %q", msg.sent[0].Content)
	}
	if !strings.Contains(msg.sent[0].Content, "3건") {
		t.Errorf("error message should mention note count: %q", msg.sent[0].Content)
	}
	// 세션 메모가 여전히 보존되어 있는지
	if len(sess.SnapshotNotes()) != 3 {
		t.Errorf("notes should be preserved, got %d", len(sess.SnapshotNotes()))
	}
}

func Test_빈_Notes로_미팅_종료할_때_LLM을_호출하지_않고_안내_메시지만_전송한다(t *testing.T) {
	sess := newTestSession()

	msg := &fakeMessenger{}
	summ := &fakeSummarizer{}

	keep := finalizeMeeting(context.Background(), msg, summ, sess, time.Now(), llm.FormatDecisionStatus, "")

	if keep {
		t.Error("empty session should not be preserved")
	}
	if summ.decisionStatusCalls != 0 {
		t.Errorf("Summarize should NOT be called for empty notes, got %d calls", summ.decisionStatusCalls)
	}
	if len(msg.sent) != 1 {
		t.Fatalf("expected 1 notice message, got %d", len(msg.sent))
	}
	if !strings.Contains(msg.sent[0].Content, "기록된 메모가 없") {
		t.Errorf("unexpected content: %q", msg.sent[0].Content)
	}
}

// === Interim 시나리오 (수동 트리거 기반) ===

func Test_노트가_0건일_때_emitInterim은_안내메시지를_보내고_LLM을_호출하지_않는다(t *testing.T) {
	sess := newTestSession()

	msg := &fakeMessenger{}
	summ := &fakeSummarizer{
		interimResp: &llm.InterimNoteResponse{Decisions: []llm.Decision{{Title: "x"}}},
	}
	emitInterim(context.Background(), msg, summ, sess, time.Now())

	if summ.interimCalls != 0 {
		t.Errorf("expected 0 interim calls for empty notes, got %d", summ.interimCalls)
	}
	if len(msg.sent) != 1 {
		t.Fatalf("expected 1 plain notice message, got %d", len(msg.sent))
	}
	if !strings.Contains(msg.sent[0].Content, "수집된 메모가 없") {
		t.Errorf("unexpected notice: %q", msg.sent[0].Content)
	}
}

func Test_노트가_있을_때_emitInterim이_SummarizeInterim을_호출하고_버튼메시지를_보낸다(t *testing.T) {
	sess := newTestSession()
	sess.AddNote("hgkim", "메모1")
	sess.AddNote("hgkim", "메모2")
	sess.AddNote("hgkim", "메모3")

	msg := &fakeMessenger{}
	summ := &fakeSummarizer{
		interimResp: &llm.InterimNoteResponse{
			Decisions:     []llm.Decision{{Title: "Redis 고정", Context: []string{"캐시 설계 논의"}}},
			OpenQuestions: []string{"TTL 미정 - 확인 필요"},
		},
	}
	emitInterim(context.Background(), msg, summ, sess, time.Now())

	if summ.interimCalls != 1 {
		t.Fatalf("expected 1 interim call, got %d", summ.interimCalls)
	}
	if len(msg.sentComplex) != 1 {
		t.Fatalf("expected 1 complex message (with button), got %d", len(msg.sentComplex))
	}
	body := msg.sentComplex[0].Content
	for _, exp := range []string{"현재까지 미팅 정리", "지금까지 나온 결정", "Redis 고정", "확인 필요", "TTL 미정"} {
		if !strings.Contains(body, exp) {
			t.Errorf("missing %q in interim body:\n%s", exp, body)
		}
	}
}

func Test_노트_1건만_있어도_사용자_명시_클릭이면_emitInterim이_발사된다(t *testing.T) {
	sess := newTestSession()
	sess.AddNote("hgkim", "유일한 메모")

	msg := &fakeMessenger{}
	summ := &fakeSummarizer{interimResp: &llm.InterimNoteResponse{}}

	emitInterim(context.Background(), msg, summ, sess, time.Now())

	if summ.interimCalls != 1 {
		t.Errorf("manual click should always fire when notes exist, got %d", summ.interimCalls)
	}
}

func Test_emitInterim은_같은_노트_상태에서도_매번_재발사된다(t *testing.T) {
	sess := newTestSession()
	sess.AddNote("hgkim", "메모1")
	sess.AddNote("hgkim", "메모2")
	sess.AddNote("hgkim", "메모3")

	msg := &fakeMessenger{}
	summ := &fakeSummarizer{interimResp: &llm.InterimNoteResponse{}}

	emitInterim(context.Background(), msg, summ, sess, time.Now())
	emitInterim(context.Background(), msg, summ, sess, time.Now())

	if summ.interimCalls != 2 {
		t.Errorf("manual click should fire on every press, got %d", summ.interimCalls)
	}
}

func Test_SummarizeInterim_실패시_재시도가_가능하다(t *testing.T) {
	sess := newTestSession()
	sess.AddNote("hgkim", "a")

	msg := &fakeMessenger{}
	summ := &fakeSummarizer{interimErr: errors.New("boom")}

	emitInterim(context.Background(), msg, summ, sess, time.Now())
	emitInterim(context.Background(), msg, summ, sess, time.Now())

	if summ.interimCalls != 2 {
		t.Errorf("expected 2 attempts on repeated failure, got %d", summ.interimCalls)
	}
}

// === Sticky 컨트롤 메시지 테스트 ===

func Test_미팅_시작_시_sendSticky는_초기_컨트롤_메시지를_전송한다(t *testing.T) {
	sess := newTestSession()
	msg := &fakeMessenger{}

	sendSticky(msg, sess)

	if len(msg.sentComplex) != 1 {
		t.Fatalf("expected 1 sticky message, got %d", len(msg.sentComplex))
	}
	if len(msg.deleted) != 0 {
		t.Errorf("초기 sticky는 delete 호출 없어야 함, got %d", len(msg.deleted))
	}
	if sess.CurrentStickyID() == "" {
		t.Error("sticky message ID가 세션에 저장되어야 함")
	}
	body := msg.sentComplex[0].Content
	if !strings.Contains(body, "미팅 진행 중") || !strings.Contains(body, "0건") {
		t.Errorf("expected '미팅 진행 중 · 0건', got %q", body)
	}
}

func Test_maybeRefreshSticky는_threshold_미달시_발사하지_않는다(t *testing.T) {
	sess := newTestSession()
	sess.AddNote("u", "1")
	sess.AddNote("u", "2")

	msg := &fakeMessenger{}
	maybeRefreshSticky(msg, sess)

	if len(msg.sentComplex) != 0 {
		t.Errorf("threshold=3 미달인데 발사됨: %d", len(msg.sentComplex))
	}
}

func Test_maybeRefreshSticky는_threshold_도달시_delete_후_재전송한다(t *testing.T) {
	sess := newTestSession()
	// 초기 sticky 발사로 ID 세팅
	msg := &fakeMessenger{}
	sendSticky(msg, sess)
	initialID := sess.CurrentStickyID()
	if initialID == "" {
		t.Fatal("initial sticky failed")
	}

	// 3개 노트 추가
	sess.AddNote("u", "1")
	sess.AddNote("u", "2")
	sess.AddNote("u", "3")

	maybeRefreshSticky(msg, sess)

	// 기존 msg 1개 삭제 + 새 msg 전송
	if len(msg.deleted) != 1 {
		t.Errorf("expected 1 delete, got %d", len(msg.deleted))
	}
	if !strings.Contains(msg.deleted[0], initialID) {
		t.Errorf("expected old sticky %s to be deleted, got %v", initialID, msg.deleted)
	}
	if len(msg.sentComplex) != 2 {
		t.Fatalf("expected 2 complex messages (initial + refresh), got %d", len(msg.sentComplex))
	}
	// 새 메시지에 "3건" 포함
	refreshBody := msg.sentComplex[1].Content
	if !strings.Contains(refreshBody, "3건") {
		t.Errorf("refresh body should say '3건': %q", refreshBody)
	}
	// sticky ID 갱신됨
	if sess.CurrentStickyID() == initialID {
		t.Error("sticky ID should have been updated")
	}
}

func Test_sticky는_threshold_주기마다_정확히_한번_발사된다(t *testing.T) {
	sess := newTestSession()
	msg := &fakeMessenger{}
	sendSticky(msg, sess) // initial

	// 2개 노트 → 미달
	sess.AddNote("u", "1")
	maybeRefreshSticky(msg, sess)
	sess.AddNote("u", "2")
	maybeRefreshSticky(msg, sess)

	if len(msg.sentComplex) != 1 {
		t.Errorf("2개 노트에선 initial 1개만 있어야 함: %d", len(msg.sentComplex))
	}

	// 3개째 - 발사
	sess.AddNote("u", "3")
	maybeRefreshSticky(msg, sess)
	if len(msg.sentComplex) != 2 {
		t.Errorf("3개 노트 도달 시 refresh 발사: %d", len(msg.sentComplex))
	}

	// 4, 5개째 - 미달 (3 → 6 필요)
	sess.AddNote("u", "4")
	maybeRefreshSticky(msg, sess)
	sess.AddNote("u", "5")
	maybeRefreshSticky(msg, sess)
	if len(msg.sentComplex) != 2 {
		t.Errorf("4-5개 노트에선 추가 발사 없어야 함: %d", len(msg.sentComplex))
	}

	// 6개째 - 다시 발사
	sess.AddNote("u", "6")
	maybeRefreshSticky(msg, sess)
	if len(msg.sentComplex) != 3 {
		t.Errorf("6개 노트 도달 시 두 번째 refresh: %d", len(msg.sentComplex))
	}
}

func Test_deleteStickyIfPresent는_sticky_있을때_delete하고_세션을_비운다(t *testing.T) {
	sess := newTestSession()
	msg := &fakeMessenger{}
	sendSticky(msg, sess)
	if sess.CurrentStickyID() == "" {
		t.Fatal("initial sticky failed")
	}

	deleteStickyIfPresent(msg, sess)

	if len(msg.deleted) != 1 {
		t.Errorf("expected 1 delete, got %d", len(msg.deleted))
	}
	if sess.CurrentStickyID() != "" {
		t.Errorf("sticky ID should be cleared, got %q", sess.CurrentStickyID())
	}
}

func Test_deleteStickyIfPresent는_sticky_없을때_noop이다(t *testing.T) {
	sess := newTestSession()
	msg := &fakeMessenger{}

	deleteStickyIfPresent(msg, sess)

	if len(msg.deleted) != 0 {
		t.Errorf("noop이어야 하는데 delete 호출됨: %d", len(msg.deleted))
	}
}

func Test_성공적으로_렌더링된_후_Participants가_발화자_목록에서_채워진다(t *testing.T) {
	sess := newTestSession()
	sess.AddNote("alice", "안건 1")
	sess.AddNote("bob", "안건 2")
	sess.AddNote("alice", "추가")

	msg := &fakeMessenger{}
	summ := &fakeSummarizer{
		decisionStatusResp: &llm.DecisionStatusResponse{
			Decisions: []llm.Decision{
				{Title: "안건 처리 방향 확정", Context: []string{"내용 정리"}},
			},
		},
	}

	finalizeMeeting(context.Background(), msg, summ, sess, time.Now(), llm.FormatDecisionStatus, "")

	// Summarizer에 전달된 speakers가 Go 수집 기반인지 확인
	if len(summ.lastSpeakers) != 2 {
		t.Fatalf("expected 2 speakers, got %v", summ.lastSpeakers)
	}
	// 렌더 결과에 alice, bob 참석자가 (멘션 형태가 아닌) inline code로 표시되어야 함
	body := msg.sent[0].Content
	if !strings.Contains(body, "`alice`") || !strings.Contains(body, "`bob`") {
		t.Errorf("rendered output should contain speaker names:\n%s", body)
	}
}

// === 4 포맷 디스패치 테스트 ===

func Test_finalizeMeeting_FormatDiscussion이면_SummarizeDiscussion만_호출한다(t *testing.T) {
	sess := newTestSession()
	sess.AddNote("hgkim", "최근 업무량 얘기")
	sess.AddNote("manager", "분담 제안")

	msg := &fakeMessenger{}
	summ := &fakeSummarizer{
		discussionResp: &llm.DiscussionResponse{
			Topics: []llm.Topic{
				{Title: "업무 부담", Flow: []string{"부담 공유", "분담 제안"}, Insights: []string{"분담 가능 영역 식별 합의"}},
			},
			Tags: []string{"1on1"},
		},
	}

	keep := finalizeMeeting(context.Background(), msg, summ, sess, time.Now(), llm.FormatDiscussion, "")

	if keep {
		t.Fatal("expected keepSession=false")
	}
	if summ.discussionCalls != 1 {
		t.Errorf("expected discussion=1, got %d", summ.discussionCalls)
	}
	if summ.decisionStatusCalls != 0 || summ.roleBasedCalls != 0 || summ.freeformCalls != 0 || summ.calls != 0 {
		t.Errorf("only discussion should be called: decision=%d, role_based=%d, freeform=%d, legacy=%d",
			summ.decisionStatusCalls, summ.roleBasedCalls, summ.freeformCalls, summ.calls)
	}
	body := msg.sent[0].Content
	if !strings.Contains(body, "## 논의 토픽") || !strings.Contains(body, "업무 부담") {
		t.Errorf("expected discussion render, got:\n%s", body)
	}
}

func Test_finalizeMeeting_FormatRoleBased이면_SummarizeRoleBased만_호출한다(t *testing.T) {
	sess := newTestSession()
	sess.AddNote("hgkim", "발행일자 마무리")
	sess.AddNote("whale", "#223 담당")

	msg := &fakeMessenger{}
	summ := &fakeSummarizer{
		roleBasedResp: &llm.RoleBasedResponse{
			Roles: []llm.RoleSection{
				{Speaker: "hgkim", Decisions: []string{"발행일자 우선"}},
				{Speaker: "whale", Actions: []llm.NextStep{{Who: "whale", What: "#223 담당"}}},
			},
		},
	}

	finalizeMeeting(context.Background(), msg, summ, sess, time.Now(), llm.FormatRoleBased, "")

	if summ.roleBasedCalls != 1 {
		t.Errorf("expected role_based=1, got %d", summ.roleBasedCalls)
	}
	if summ.decisionStatusCalls != 0 || summ.discussionCalls != 0 || summ.freeformCalls != 0 {
		t.Errorf("only role_based should be called")
	}
	body := msg.sent[0].Content
	if !strings.Contains(body, "## 역할별 정리") || !strings.Contains(body, "`hgkim`") || !strings.Contains(body, "`whale`") {
		t.Errorf("expected role-based render with both speakers, got:\n%s", body)
	}
}

func Test_finalizeMeeting_FormatFreeform이면_LLM_마크다운을_그대로_감싼다(t *testing.T) {
	sess := newTestSession()
	sess.AddNote("hgkim", "혼합 회의")

	msg := &fakeMessenger{}
	summ := &fakeSummarizer{
		freeformResp: &llm.FreeformResponse{
			Markdown: "## 자율 본문\n자유롭게 정리한 내용",
		},
	}

	finalizeMeeting(context.Background(), msg, summ, sess, time.Now(), llm.FormatFreeform, "")

	if summ.freeformCalls != 1 {
		t.Errorf("expected freeform=1, got %d", summ.freeformCalls)
	}
	body := msg.sent[0].Content
	if !strings.Contains(body, "# ") || !strings.Contains(body, "## 자율 본문") || !strings.Contains(body, "자유롭게 정리한 내용") {
		t.Errorf("expected freeform body wrapped with H1 header, got:\n%s", body)
	}
}

func Test_formatFromCustomID는_4개_매핑이_정확하다(t *testing.T) {
	cases := []struct {
		id   string
		want llm.NoteFormat
		ok   bool
	}{
		{customIDFinalizeDecisionStatus, llm.FormatDecisionStatus, true},
		{customIDFinalizeDiscussion, llm.FormatDiscussion, true},
		{customIDFinalizeRoleBased, llm.FormatRoleBased, true},
		{customIDFinalizeFreeform, llm.FormatFreeform, true},
		{"unknown_id", 0, false},
		{"", 0, false},
	}
	for _, c := range cases {
		got, ok := formatFromCustomID(c.id)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("formatFromCustomID(%q) = (%v, %v), want (%v, %v)", c.id, got, ok, c.want, c.ok)
		}
	}
}

func Test_finalizePromptComponents는_legacy_4포맷과_summarized_button을_2행으로_생성한다(t *testing.T) {
	// Phase 2 chunk 3b: legacy 4 button + 추가요청 (1행) + summarized 통합 button (2행)
	comps := finalizePromptComponents()
	if len(comps) != 2 {
		t.Fatalf("expected 2 ActionsRows (legacy + summarized), got %d", len(comps))
	}

	// Row 1: legacy 4 포맷 + 추가 요청 (5 button)
	row1, ok := comps[0].(discordgo.ActionsRow)
	if !ok {
		t.Fatalf("expected row[0] ActionsRow, got %T", comps[0])
	}
	if len(row1.Components) != 5 {
		t.Errorf("row 1 expected 5 buttons, got %d", len(row1.Components))
	}
	row1IDs := map[string]bool{
		customIDFinalizeDecisionStatus: false,
		customIDFinalizeDiscussion:     false,
		customIDFinalizeRoleBased:      false,
		customIDFinalizeFreeform:       false,
		customIDDirectiveBtn:           false,
	}
	for _, c := range row1.Components {
		btn, ok := c.(discordgo.Button)
		if !ok {
			t.Errorf("row 1: expected Button, got %T", c)
			continue
		}
		if _, exists := row1IDs[btn.CustomID]; !exists {
			t.Errorf("row 1: unexpected custom_id: %q", btn.CustomID)
		}
		row1IDs[btn.CustomID] = true
	}
	for id, seen := range row1IDs {
		if !seen {
			t.Errorf("row 1: missing button with custom_id %q", id)
		}
	}

	// Row 2: Phase 2 통합 button 1개
	row2, ok := comps[1].(discordgo.ActionsRow)
	if !ok {
		t.Fatalf("expected row[1] ActionsRow, got %T", comps[1])
	}
	if len(row2.Components) != 1 {
		t.Errorf("row 2 expected 1 button (summarized), got %d", len(row2.Components))
	}
	btn, ok := row2.Components[0].(discordgo.Button)
	if !ok {
		t.Fatalf("row 2: expected Button, got %T", row2.Components[0])
	}
	if btn.CustomID != customIDFinalizeSummarized {
		t.Errorf("row 2 button CustomID = %q, want %q", btn.CustomID, customIDFinalizeSummarized)
	}
}

func Test_finalizePromptWithDirectiveComponents는_directive_재입력_버튼을_포함한다(t *testing.T) {
	comps := finalizePromptWithDirectiveComponents()
	row, ok := comps[0].(discordgo.ActionsRow)
	if !ok {
		t.Fatalf("expected ActionsRow, got %T", comps[0])
	}
	if len(row.Components) != 5 {
		t.Errorf("expected 5 buttons, got %d", len(row.Components))
	}
	hasRetry := false
	for _, c := range row.Components {
		if btn, ok := c.(discordgo.Button); ok && btn.CustomID == customIDDirectiveRetryBtn {
			hasRetry = true
		}
	}
	if !hasRetry {
		t.Errorf("missing directive retry button")
	}
}

func Test_finalizeMeeting은_directive를_4포맷_호출에_그대로_전달한다(t *testing.T) {
	sess := newTestSession()
	sess.AddNote("hgkim", "노트 A")

	msg := &fakeMessenger{}
	summ := &fakeSummarizer{
		decisionStatusResp: &llm.DecisionStatusResponse{},
	}

	want := "프론트/백엔드 영역별 H3 섹션으로 묶어줘"
	finalizeMeeting(context.Background(), msg, summ, sess, time.Now(), llm.FormatDecisionStatus, want)

	if summ.lastDirective != want {
		t.Errorf("expected directive %q passed to summarizer, got %q", want, summ.lastDirective)
	}
}

// === splitByLines 테스트 ===

func Test_2000자_이하_메시지는_한_chunk로_반환한다(t *testing.T) {
	content := strings.Repeat("가", 1999)
	chunks := splitByLines(content, 2000)
	if len(chunks) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(chunks))
	}
	if chunks[0] != content {
		t.Error("chunk should be original content")
	}
}

func Test_2000자_초과_메시지는_줄단위로_분할한다(t *testing.T) {
	// 800자 줄 3개 = 총 2402 rune (800+\n+800+\n+800)
	line := strings.Repeat("가", 800)
	content := line + "\n" + line + "\n" + line
	chunks := splitByLines(content, 2000)
	if len(chunks) < 2 {
		t.Fatalf("expected at least 2 chunks, got %d", len(chunks))
	}
	for _, c := range chunks {
		if len([]rune(c)) > 2000 {
			t.Errorf("chunk exceeds 2000 runes: %d", len([]rune(c)))
		}
	}
	// 재결합하면 원본과 같아야 함
	joined := strings.Join(chunks, "\n")
	if joined != content {
		t.Error("joined chunks should equal original content")
	}
}

func Test_분할시_마크다운_줄_중간에서_끊기지_않는다(t *testing.T) {
	// 각 줄이 완전한 마크다운 bullet
	lines := make([]string, 30)
	for i := range lines {
		lines[i] = fmt.Sprintf("- **결정 %d** 이것은 중요한 내용입니다", i+1)
	}
	content := strings.Join(lines, "\n")
	chunks := splitByLines(content, 2000)
	for _, c := range chunks {
		// 각 chunk 안에서 bullet이 잘리지 않았는지 확인
		// chunk 시작이 "- " 또는 빈 문자열이어야 함 (줄 중간에서 시작 안 함)
		chunkLines := strings.Split(c, "\n")
		for _, l := range chunkLines {
			if l != "" && !strings.HasPrefix(l, "- ") {
				t.Errorf("line doesn't start with bullet: %q", l)
			}
		}
	}
}

func Test_sendLongMessage는_분할하여_모두_전송한다(t *testing.T) {
	line := strings.Repeat("나", 800)
	content := line + "\n" + line + "\n" + line
	msg := &fakeMessenger{}
	err := sendLongMessage(msg, "ch-1", content)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(msg.sent) < 2 {
		t.Fatalf("expected at least 2 messages, got %d", len(msg.sent))
	}
}

func Test_sendLongMessageWithComponents는_버튼을_마지막_chunk에만_첨부한다(t *testing.T) {
	line := strings.Repeat("다", 800)
	content := line + "\n" + line + "\n" + line
	msg := &fakeMessenger{}

	components := []discordgo.MessageComponent{
		discordgo.ActionsRow{
			Components: []discordgo.MessageComponent{
				discordgo.Button{Label: "테스트", CustomID: "test_btn"},
			},
		},
	}
	_, err := sendLongMessageWithComponents(msg, "ch-1", content, components)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// plain 메시지 1개+ + complex 메시지 1개(마지막)
	if len(msg.sentComplex) != 1 {
		t.Errorf("expected exactly 1 complex message (last chunk with button), got %d", len(msg.sentComplex))
	}
	totalMessages := len(msg.sent) + len(msg.sentComplex)
	if totalMessages < 2 {
		t.Errorf("expected at least 2 total messages, got %d", totalMessages)
	}
}

// === InterimInFlight 테스트 ===

func Test_TryStartManualInterim은_InFlight_true일때_false를_반환한다(t *testing.T) {
	sess := newTestSession()

	if !sess.TryStartManualInterim() {
		t.Fatal("first try should succeed")
	}
	if sess.TryStartManualInterim() {
		t.Error("second try should fail while in-flight")
	}
}

func Test_FinishInterim_호출_후_재시도_가능하다(t *testing.T) {
	sess := newTestSession()

	sess.TryStartManualInterim()
	sess.FinishInterim()

	if !sess.TryStartManualInterim() {
		t.Error("should be able to start again after FinishInterim")
	}
}
