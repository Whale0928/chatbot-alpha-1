package bot

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"chatbot-alpha-1/pkg/db"
	"chatbot-alpha-1/pkg/llm"
	"chatbot-alpha-1/pkg/llm/summarize"

	"github.com/bwmarrin/discordgo"
)

type recordedHTTPCall struct {
	method string
	path   string
	body   string
}

type recordingRoundTripper struct {
	calls []recordedHTTPCall
}

func (rt *recordingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	rt.calls = append(rt.calls, recordedHTTPCall{
		method: req.Method,
		path:   req.URL.Path,
		body:   string(body),
	})
	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(`{"id":"edited-message"}`)),
		Request:    req,
	}, nil
}

func newBotTestDB(t *testing.T) *db.DB {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "bot-test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if err := d.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return d
}

func (f *fakeSummarizer) RenderFormat(_ context.Context, _ summarize.FormatRenderInput) (string, error) {
	return "", nil
}

type formatToggleSummarizer struct {
	fakeSummarizer
	renderFormatCalls int
	outputs           map[llm.NoteFormat]string
}

func (f *formatToggleSummarizer) RenderFormat(_ context.Context, in summarize.FormatRenderInput) (string, error) {
	f.renderFormatCalls++
	if out, ok := f.outputs[in.Format]; ok {
		return out, nil
	}
	return "rendered " + in.Format.String(), nil
}

func TestPrepareContentExtractionInput_SeparatesHumanFromContext(t *testing.T) {
	// given: 5/14 미팅 시나리오 — Human/WeeklyDump/ExternalPaste/InterimSummary 섞임
	notes := []Note{
		{Author: "kimjuye", AuthorID: "u1", AuthorRoles: []string{"PM"}, Content: "workspace 통합", Source: db.SourceHuman},
		{Author: "deadwhale", AuthorID: "u2", AuthorRoles: []string{"BACKEND"}, Content: "큐레이션 order", Source: db.SourceHuman},
		{Author: "[tool]", Content: "weekly dump", Source: db.SourceWeeklyDump},
		{Author: "hyejungpark", AuthorID: "u3", AuthorRoles: []string{"FRONTEND"}, Content: "[큰 paste]", Source: db.SourceExternalPaste},
		{Author: "hyejungpark", AuthorID: "u3", AuthorRoles: []string{"FRONTEND"}, Content: "FE 배포", Source: db.SourceHuman},
		{Author: "[bot]", Content: "interim", Source: db.SourceInterimSummary},
	}

	in := PrepareContentExtractionInput(notes, nil, time.Date(2026, 5, 14, 0, 0, 0, 0, time.UTC))

	// then: HumanNotes 3개 (kimjuye + deadwhale + hyejungpark Human 발화)
	if len(in.HumanNotes) != 3 {
		t.Errorf("HumanNotes count = %d, want 3", len(in.HumanNotes))
	}
	// then: ContextNotes 2개 (WeeklyDump + ExternalPaste). InterimSummary는 corpus 제외.
	if len(in.ContextNotes) != 2 {
		t.Errorf("ContextNotes count = %d, want 2 (InterimSummary 제외)", len(in.ContextNotes))
	}
	// then: Speakers는 Human author만 (정렬)
	wantSpeakers := []string{"deadwhale", "hyejungpark", "kimjuye"}
	if !reflect.DeepEqual(in.Speakers, wantSpeakers) {
		t.Errorf("Speakers = %v, want %v ([tool]/[bot] 제외 + 정렬)", in.Speakers, wantSpeakers)
	}
	// then: SpeakerRoles는 Human 발화자만
	wantRoles := map[string][]string{
		"kimjuye":     {"PM"},
		"deadwhale":   {"BACKEND"},
		"hyejungpark": {"FRONTEND"},
	}
	if !reflect.DeepEqual(in.SpeakerRoles, wantRoles) {
		t.Errorf("SpeakerRoles = %v, want %v", in.SpeakerRoles, wantRoles)
	}
}

func TestPrepareContentExtractionInput_RolesFallbackFromSession(t *testing.T) {
	// note에 AuthorRoles 비어있을 때 sessionRoles[AuthorID]에서 fallback
	notes := []Note{
		{Author: "alice", AuthorID: "u_alice", Content: "hi", Source: db.SourceHuman},
	}
	sessionRoles := map[string][]string{"u_alice": {"BACKEND", "PM"}}

	in := PrepareContentExtractionInput(notes, sessionRoles, time.Now())

	if got := in.SpeakerRoles["alice"]; !reflect.DeepEqual(got, []string{"BACKEND", "PM"}) {
		t.Errorf("alice roles = %v, want [BACKEND PM] (session fallback 깨짐)", got)
	}
}

func TestPrepareContentExtractionInput_NotePerSourceRolesPreferred(t *testing.T) {
	// note.AuthorRoles와 sessionRoles 둘 다 있으면 발화 시점 snapshot(note.AuthorRoles) 우선
	notes := []Note{
		{Author: "alice", AuthorID: "u1", AuthorRoles: []string{"PM"}, Content: "hi", Source: db.SourceHuman},
	}
	sessionRoles := map[string][]string{"u1": {"BACKEND"}} // 다른 값

	in := PrepareContentExtractionInput(notes, sessionRoles, time.Now())

	if got := in.SpeakerRoles["alice"]; !reflect.DeepEqual(got, []string{"PM"}) {
		t.Errorf("alice roles = %v, want [PM] (발화 시점 snapshot 우선 깨짐)", got)
	}
}

func TestPrepareContentExtractionInput_ToolAuthorsExcludedFromSpeakers(t *testing.T) {
	// 환각 방어 핵심 회귀 — [tool] author는 Speakers/SpeakerRoles에 절대 등장 X
	notes := []Note{
		{Author: "[tool]", Content: "weekly dump", Source: db.SourceWeeklyDump},
		{Author: "[bot]", Content: "interim", Source: db.SourceInterimSummary},
	}
	in := PrepareContentExtractionInput(notes, nil, time.Now())

	if len(in.Speakers) != 0 {
		t.Errorf("Speakers = %v, want empty (도구 author는 attribution 후보 X)", in.Speakers)
	}
	if len(in.SpeakerRoles) != 0 {
		t.Errorf("SpeakerRoles = %v, want empty", in.SpeakerRoles)
	}
}

func TestPrepareContentExtractionInput_EmptyNotes(t *testing.T) {
	in := PrepareContentExtractionInput(nil, nil, time.Now())
	if len(in.HumanNotes) != 0 || len(in.ContextNotes) != 0 || len(in.Speakers) != 0 {
		t.Errorf("empty input → 모든 필드 비어야 함, got %+v", in)
	}
}

func TestPersistSummarizedContent_NoOpWhenDBUnavailable(t *testing.T) {
	// dbConn nil인 상태(테스트 default)에선 빈 문자열 반환 + panic 없음.
	sess := &Session{ThreadID: "t1", DBSessionID: "sess_x"}
	content := &llm.SummarizedContent{}
	got := PersistSummarizedContent(t.Context(), sess, content)
	if got != "" {
		t.Errorf("dbConn nil → want empty id, got %q", got)
	}
}

func TestPersistSummarizedContent_NoOpWhenSessionMissing(t *testing.T) {
	got := PersistSummarizedContent(t.Context(), nil, &llm.SummarizedContent{})
	if got != "" {
		t.Errorf("nil session → want empty id, got %q", got)
	}
}

func TestPersistSummarizedContent_NoOpWhenDBSessionIDEmpty(t *testing.T) {
	sess := &Session{ThreadID: "t1"} // DBSessionID 비어있음 (DB persist 실패 후 fallback 상태)
	got := PersistSummarizedContent(t.Context(), sess, &llm.SummarizedContent{})
	if got != "" {
		t.Errorf("DBSessionID empty → want empty id, got %q", got)
	}
}

// =====================================================================
// chunk 3c — 토글 button + 포맷 변환 테스트
// =====================================================================

func TestFormatToggleComponents_ActiveHighlighted(t *testing.T) {
	comps := formatToggleComponents(llm.FormatRoleBased)
	row, ok := comps[0].(discordgo.ActionsRow)
	if !ok {
		t.Fatalf("expected ActionsRow, got %T", comps[0])
	}
	if len(row.Components) != 4 {
		t.Errorf("expected 4 toggle buttons, got %d", len(row.Components))
	}
	// 활성(role_based)은 SuccessButton, 나머지는 SecondaryButton
	for _, c := range row.Components {
		btn := c.(discordgo.Button)
		if btn.CustomID == customIDFormatToggleRoleBased {
			if btn.Style != discordgo.SuccessButton {
				t.Errorf("active(role_based) style = %v, want SuccessButton", btn.Style)
			}
		} else if btn.Style != discordgo.SecondaryButton {
			t.Errorf("inactive %s style = %v, want SecondaryButton", btn.CustomID, btn.Style)
		}
	}
}

func TestFormatFromToggleCustomID(t *testing.T) {
	tests := []struct {
		id   string
		want llm.NoteFormat
		ok   bool
	}{
		{customIDFormatToggleDecisionStatus, llm.FormatDecisionStatus, true},
		{customIDFormatToggleDiscussion, llm.FormatDiscussion, true},
		{customIDFormatToggleRoleBased, llm.FormatRoleBased, true},
		{customIDFormatToggleFreeform, llm.FormatFreeform, true},
		{"unknown", 0, false},
		{customIDFinalizeSummarized, 0, false}, // legacy customID 혼동 방지
	}
	for _, tc := range tests {
		t.Run(tc.id, func(t *testing.T) {
			got, ok := formatFromToggleCustomID(tc.id)
			if ok != tc.ok || got != tc.want {
				t.Errorf("got (%v, %v), want (%v, %v)", got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestFormatToDBKind(t *testing.T) {
	tests := []struct {
		in   llm.NoteFormat
		want db.FormatKind
	}{
		{llm.FormatDecisionStatus, db.FormatDecisionStatus},
		{llm.FormatDiscussion, db.FormatDiscussion},
		{llm.FormatRoleBased, db.FormatRoleBased},
		{llm.FormatFreeform, db.FormatFreeform},
	}
	for _, tc := range tests {
		t.Run(tc.in.String(), func(t *testing.T) {
			if got := formatToDBKind(tc.in); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRenderSummarizedByFormat_AllFourFormats(t *testing.T) {
	c := &llm.SummarizedContent{
		Decisions: []llm.Decision{{Title: "테스트 결정"}},
		Topics:    []llm.Topic{{Title: "테스트 토픽", Flow: []string{"흐름1"}}},
		Actions: []llm.SummaryAction{{
			What: "테스트 액션", Origin: "alice",
			OriginRoles: []string{"BACKEND"}, TargetRoles: []string{"BACKEND"},
		}},
	}
	speakers := []string{"alice"}
	roles := map[string][]string{"alice": {"BACKEND"}}
	date := time.Date(2026, 5, 14, 0, 0, 0, 0, time.UTC)

	for _, f := range []llm.NoteFormat{llm.FormatDecisionStatus, llm.FormatDiscussion, llm.FormatRoleBased, llm.FormatFreeform} {
		t.Run(f.String(), func(t *testing.T) {
			got := renderSummarizedByFormat(c, f, speakers, roles, date)
			if got == "" {
				t.Errorf("format %s rendered empty markdown", f)
			}
			// 모든 포맷이 H1 헤더 포함
			if !strings.Contains(got, "# 2026-05-14") {
				t.Errorf("format %s missing date header:\n%s", f, got)
			}
		})
	}
}

func TestHandleFormatToggle_UsesFinalizeRunCache(t *testing.T) {
	ctx := context.Background()
	d := newBotTestDB(t)

	oldDBConn := dbConn
	oldSummarizer := summarizer
	t.Cleanup(func() {
		dbConn = oldDBConn
		summarizer = oldSummarizer
	})
	dbConn = d

	if err := d.InsertSession(ctx, db.Session{
		ID:       "sess_cache",
		ThreadID: "thread-cache",
		GuildID:  "guild-cache",
		OwnerID:  "owner-cache",
		OpenedAt: time.Unix(1700000000, 0),
		Status:   db.SessionActive,
	}); err != nil {
		t.Fatalf("InsertSession: %v", err)
	}

	content := &llm.SummarizedContent{
		Decisions: []llm.Decision{{Title: "캐시 결정"}},
		Topics:    []llm.Topic{{Title: "캐시 토픽", Flow: []string{"논의 흐름"}}},
		Actions: []llm.SummaryAction{{
			What: "캐시 액션", Origin: "alice",
			OriginRoles: []string{"BACKEND"}, TargetRoles: []string{"BACKEND"},
		}},
	}
	raw, err := json.Marshal(content)
	if err != nil {
		t.Fatalf("Marshal summarized content: %v", err)
	}
	if err := d.InsertSummarizedContent(ctx, db.SummarizedContent{
		ID:          "sc_cache",
		SessionID:   "sess_cache",
		Content:     raw,
		ExtractedAt: time.Date(2026, 5, 14, 9, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("InsertSummarizedContent: %v", err)
	}

	summ := &formatToggleSummarizer{
		outputs: map[llm.NoteFormat]string{
			llm.FormatDecisionStatus: "decision render from LLM",
			llm.FormatDiscussion:     "discussion render from LLM",
		},
	}
	summarizer = summ

	rt := &recordingRoundTripper{}
	discordSession, err := discordgo.New("Bot test-token")
	if err != nil {
		t.Fatalf("discordgo.New: %v", err)
	}
	discordSession.Client = &http.Client{Transport: rt}

	interaction := &discordgo.InteractionCreate{
		Interaction: &discordgo.Interaction{
			AppID: "app-cache",
			Token: "token-cache",
		},
	}
	sess := &Session{
		ThreadID:      "thread-cache",
		DBSessionID:   "sess_cache",
		RolesSnapshot: map[string][]string{"u_alice": {"BACKEND"}},
	}
	sess.AddNoteWithMeta(Note{
		Author:      "alice",
		AuthorID:    "u_alice",
		AuthorRoles: []string{"BACKEND"},
		Content:     "캐시 테스트",
		Source:      db.SourceHuman,
	})

	toggle := func(customID, wantBody string, wantRenderCalls int, wantFinalizeRuns int) {
		t.Helper()
		beforeHTTP := len(rt.calls)

		HandleFormatToggle(ctx, discordSession, interaction, sess, customID)

		if summ.renderFormatCalls != wantRenderCalls {
			t.Fatalf("RenderFormat calls = %d, want %d", summ.renderFormatCalls, wantRenderCalls)
		}
		if len(rt.calls) != beforeHTTP+1 {
			t.Fatalf("HTTP calls delta = %d, want 1", len(rt.calls)-beforeHTTP)
		}
		call := rt.calls[len(rt.calls)-1]
		if call.method != http.MethodPatch {
			t.Fatalf("HTTP method = %s, want PATCH", call.method)
		}
		if !strings.Contains(call.body, wantBody) {
			t.Fatalf("edit body missing %q:\n%s", wantBody, call.body)
		}

		var count int
		if err := d.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM finalize_runs WHERE summarized_content_id = ?",
			"sc_cache",
		).Scan(&count); err != nil {
			t.Fatalf("count finalize_runs: %v", err)
		}
		if count != wantFinalizeRuns {
			t.Fatalf("finalize_runs count = %d, want %d", count, wantFinalizeRuns)
		}
	}

	toggle(customIDFormatToggleDecisionStatus, "decision render from LLM", 1, 1)

	summ.outputs[llm.FormatDecisionStatus] = "decision rerender should not be used"
	toggle(customIDFormatToggleDecisionStatus, "decision render from LLM", 1, 1)

	toggle(customIDFormatToggleDiscussion, "discussion render from LLM", 2, 2)

	summ.outputs[llm.FormatDecisionStatus] = "decision rerender should still not be used"
	toggle(customIDFormatToggleDecisionStatus, "decision render from LLM", 2, 2)
}
