package bot

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
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

type formatToggleSummarizer struct {
	fakeSummarizer
	renderFormatCalls int
	outputs           map[llm.NoteFormat]string
	errs              map[llm.NoteFormat]error
}

func (f *formatToggleSummarizer) RenderFormat(_ context.Context, in summarize.FormatRenderInput) (string, error) {
	f.renderFormatCalls++
	if err, ok := f.errs[in.Format]; ok {
		return "", err
	}
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

// Codex 3차 P2: NoteSource → author 라벨 강제 매핑 회귀 가드.
// AppendResult가 [tool] 같은 임의 author로 박아도 LLM payload에는 [weekly]/[release]/[agent]가 박혀야 함.
func TestPrepareContentExtractionInput_NoteSource_Author라벨_강제매핑(t *testing.T) {
	notes := []Note{
		{Author: "[tool]", Content: "주간 dump", Source: db.SourceWeeklyDump},
		{Author: "anything", Content: "릴리즈 PR", Source: db.SourceReleaseResult},
		{Author: "x", Content: "AI 답변", Source: db.SourceAgentOutput},
		{Author: "alice", Content: "외부 paste 본문", Source: db.SourceExternalPaste},
	}
	in := PrepareContentExtractionInput(notes, nil, time.Now())

	if len(in.ContextNotes) != 4 {
		t.Fatalf("ContextNotes count = %d, want 4", len(in.ContextNotes))
	}
	wantAuthors := []string{"[weekly]", "[release]", "[agent]", "alice"}
	for i, w := range wantAuthors {
		if in.ContextNotes[i].Author != w {
			t.Errorf("ContextNotes[%d].Author = %q, want %q (Source=%s)",
				i, in.ContextNotes[i].Author, w, notes[i].Source)
		}
	}
}

func TestLabelForContextSource(t *testing.T) {
	cases := []struct {
		src        db.NoteSource
		origAuthor string
		want       string
	}{
		{db.SourceWeeklyDump, "[tool]", "[weekly]"},
		{db.SourceReleaseResult, "anything", "[release]"},
		{db.SourceAgentOutput, "bot", "[agent]"},
		{db.SourceExternalPaste, "alice", "alice"},
		{db.SourceExternalPaste, "", ""},
	}
	for _, c := range cases {
		got := labelForContextSource(c.src, c.origAuthor)
		if got != c.want {
			t.Errorf("labelForContextSource(%s, %q) = %q, want %q",
				c.src, c.origAuthor, got, c.want)
		}
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

func TestFinalizeSummarized_InitialSendUsesRenderFormatAndPersistsDefaultCache(t *testing.T) {
	ctx := context.Background()
	d := newBotTestDB(t)

	oldDBConn := dbConn
	t.Cleanup(func() { dbConn = oldDBConn })
	dbConn = d

	if err := d.InsertSession(ctx, db.Session{
		ID:       "sess_finalize",
		ThreadID: "thread-finalize",
		GuildID:  "guild-finalize",
		OwnerID:  "owner-finalize",
		OpenedAt: time.Unix(1700000000, 0),
		Status:   db.SessionActive,
	}); err != nil {
		t.Fatalf("InsertSession: %v", err)
	}

	content := &llm.SummarizedContent{
		Decisions: []llm.Decision{{Title: "초기 결정"}},
		Actions: []llm.SummaryAction{{
			What: "초기 액션", Origin: "alice",
			OriginRoles: []string{"BACKEND"}, TargetRoles: []string{"BACKEND"},
		}},
		Topics: []llm.Topic{{Title: "초기 토픽", Flow: []string{"흐름"}}},
	}
	summ := &fakeSummarizer{
		extractResp:      content,
		renderFormatResp: "initial decision render from LLM",
	}
	msg := &fakeMessenger{}
	sess := &Session{
		ThreadID:      "thread-finalize",
		DBSessionID:   "sess_finalize",
		RolesSnapshot: map[string][]string{"u_alice": {"BACKEND"}},
	}
	sess.AddNoteWithMeta(Note{
		Author:      "alice",
		AuthorID:    "u_alice",
		AuthorRoles: []string{"BACKEND"},
		Content:     "초기 발화",
		Source:      db.SourceHuman,
	})
	now := time.Date(2026, 5, 14, 9, 0, 0, 0, time.UTC)

	keep := FinalizeSummarized(ctx, msg, summ, sess, now)

	if keep {
		t.Fatalf("keepSession = true, want false")
	}
	if summ.extractCalls != 1 {
		t.Fatalf("ExtractContent calls = %d, want 1", summ.extractCalls)
	}
	if summ.renderFormatCalls != 1 {
		t.Fatalf("RenderFormat calls = %d, want 1", summ.renderFormatCalls)
	}
	if summ.lastRenderFormat.Format != llm.FormatDecisionStatus {
		t.Fatalf("RenderFormat format = %s, want %s", summ.lastRenderFormat.Format, llm.FormatDecisionStatus)
	}
	if summ.lastRenderFormat.Content != content {
		t.Fatalf("RenderFormat content pointer mismatch")
	}
	if got := summ.lastRenderFormat.Speakers; !reflect.DeepEqual(got, []string{"alice"}) {
		t.Fatalf("RenderFormat speakers = %v, want [alice]", got)
	}
	if got := summ.lastRenderFormat.SpeakerRoles["alice"]; !reflect.DeepEqual(got, []string{"BACKEND"}) {
		t.Fatalf("RenderFormat speaker roles = %v, want [BACKEND]", got)
	}
	if len(msg.complexPayloads) != 1 {
		t.Fatalf("complex sends = %d, want 1", len(msg.complexPayloads))
	}
	payload := msg.complexPayloads[0]
	if len(payload.Embeds) != 1 || payload.Embeds[0].Description != "initial decision render from LLM" {
		t.Fatalf("embed description = %#v, want LLM render", payload.Embeds)
	}
	if len(payload.Components) != 1 {
		t.Fatalf("components = %d, want 1 row", len(payload.Components))
	}

	var count int
	if err := d.QueryRowContext(ctx, "SELECT COUNT(*) FROM finalize_runs").Scan(&count); err != nil {
		t.Fatalf("count finalize_runs: %v", err)
	}
	var format, directive, output string
	if err := d.QueryRowContext(ctx,
		"SELECT format, directive, output_md FROM finalize_runs",
	).Scan(&format, &directive, &output); err != nil {
		t.Fatalf("query finalize_runs: %v", err)
	}
	if count != 1 || format != string(db.FormatDecisionStatus) || directive != "" || output != "initial decision render from LLM" {
		t.Fatalf("finalize_run = count:%d format:%q directive:%q output:%q", count, format, directive, output)
	}
}

func TestFinalizeSummarized_RenderFormatFailureFallsBackToPureRender(t *testing.T) {
	ctx := context.Background()
	content := &llm.SummarizedContent{
		Decisions: []llm.Decision{{Title: "fallback decision"}},
	}
	summ := &fakeSummarizer{
		extractResp:     content,
		renderFormatErr: errors.New("llm down"),
	}
	msg := &fakeMessenger{}
	sess := &Session{ThreadID: "thread-fallback"}
	sess.AddNoteWithMeta(Note{Author: "alice", Content: "fallback input", Source: db.SourceHuman})

	keep := FinalizeSummarized(ctx, msg, summ, sess, time.Date(2026, 5, 14, 9, 0, 0, 0, time.UTC))

	if keep {
		t.Fatalf("keepSession = true, want false")
	}
	if summ.renderFormatCalls != 1 {
		t.Fatalf("RenderFormat calls = %d, want 1", summ.renderFormatCalls)
	}
	if len(msg.complexPayloads) != 1 || len(msg.complexPayloads[0].Embeds) != 1 {
		t.Fatalf("complex embed send missing: %#v", msg.complexPayloads)
	}
	got := msg.complexPayloads[0].Embeds[0].Description
	if !strings.Contains(got, "# 2026-05-14") || !strings.Contains(got, "fallback decision") {
		t.Fatalf("fallback render missing expected content:\n%s", got)
	}
}

func TestFinalizeSummarized_RenderFormatFailureDoesNotPersistFallbackRun(t *testing.T) {
	ctx := context.Background()
	d := newBotTestDB(t)

	oldDBConn := dbConn
	t.Cleanup(func() { dbConn = oldDBConn })
	dbConn = d

	if err := d.InsertSession(ctx, db.Session{
		ID:       "sess_finalize_fallback_no_cache",
		ThreadID: "thread-finalize-fallback-no-cache",
		GuildID:  "guild-finalize-fallback-no-cache",
		OwnerID:  "owner-finalize-fallback-no-cache",
		OpenedAt: time.Unix(1700000000, 0),
		Status:   db.SessionActive,
	}); err != nil {
		t.Fatalf("InsertSession: %v", err)
	}

	content := &llm.SummarizedContent{
		Decisions: []llm.Decision{{Title: "fallback decision no cache"}},
	}
	summ := &fakeSummarizer{
		extractResp:     content,
		renderFormatErr: errors.New("llm down"),
	}
	msg := &fakeMessenger{}
	sess := &Session{
		ThreadID:    "thread-finalize-fallback-no-cache",
		DBSessionID: "sess_finalize_fallback_no_cache",
	}
	sess.AddNoteWithMeta(Note{Author: "alice", Content: "fallback input", Source: db.SourceHuman})

	keep := FinalizeSummarized(ctx, msg, summ, sess, time.Date(2026, 5, 14, 9, 0, 0, 0, time.UTC))

	if keep {
		t.Fatalf("keepSession = true, want false")
	}
	var count int
	if err := d.QueryRowContext(ctx, "SELECT COUNT(*) FROM finalize_runs").Scan(&count); err != nil {
		t.Fatalf("count finalize_runs: %v", err)
	}
	if count != 0 {
		t.Fatalf("finalize_runs count = %d, want 0 for fallback render", count)
	}
}

// =====================================================================
// chunk 3c — 토글 button + 포맷 변환 테스트
// =====================================================================

func TestFormatToggleComponents_ActiveHighlighted(t *testing.T) {
	comps := formatToggleComponents(llm.FormatRoleBased, "")
	row, ok := comps[0].(discordgo.ActionsRow)
	if !ok {
		t.Fatalf("expected ActionsRow, got %T", comps[0])
	}
	// 토글 4 + 복사 1 = 5 (Discord ActionsRow 최대 5 button)
	if len(row.Components) != 5 {
		t.Errorf("expected 5 buttons (4 toggle + 1 copy), got %d", len(row.Components))
	}
	// 활성(role_based)은 SuccessButton, 나머지 토글은 SecondaryButton, 복사는 PrimaryButton.
	for _, c := range row.Components {
		btn := c.(discordgo.Button)
		switch btn.CustomID {
		case customIDFormatToggleRoleBased:
			if btn.Style != discordgo.SuccessButton {
				t.Errorf("active(role_based) style = %v, want SuccessButton", btn.Style)
			}
		case customIDFormatToggleDecisionStatus, customIDFormatToggleDiscussion, customIDFormatToggleFreeform:
			if btn.Style != discordgo.SecondaryButton {
				t.Errorf("inactive %s style = %v, want SecondaryButton", btn.CustomID, btn.Style)
			}
		case customIDFormatCopy:
			if btn.Style != discordgo.PrimaryButton {
				t.Errorf("copy button style = %v, want PrimaryButton", btn.Style)
			}
			if btn.Label != "복사" {
				t.Errorf("copy button label = %q, want %q", btn.Label, "복사")
			}
		default:
			t.Errorf("unexpected button customID = %q", btn.CustomID)
		}
	}
}

// labelForFormat (discord.go에 정의)은 finalize 흐름과 toggle placeholder 모두에서 사용.
// 두 곳에서 라벨이 어긋나지 않게 회귀 가드.
func TestLabelForFormat(t *testing.T) {
	cases := []struct {
		in   llm.NoteFormat
		want string
	}{
		{llm.FormatDecisionStatus, "결정+진행"},
		{llm.FormatDiscussion, "논의"},
		{llm.FormatRoleBased, "역할별"},
		{llm.FormatFreeform, "자율"},
	}
	for _, c := range cases {
		got := labelForFormat(c.in)
		if got != c.want {
			t.Errorf("labelForFormat(%v) = %q, want %q", c.in, got, c.want)
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

func TestFormatToggleDiscordTimeoutIs90Seconds(t *testing.T) {
	raw, err := os.ReadFile("discord.go")
	if err != nil {
		t.Fatalf("ReadFile discord.go: %v", err)
	}
	src := string(raw)
	// PR #14: switch case 폐기, prefix 매칭 if 블록으로 이동.
	start := strings.Index(src, "base == customIDFormatToggleDecisionStatus ||")
	if start < 0 {
		t.Fatal("format toggle prefix dispatch not found")
	}
	rest := src[start:]
	end := strings.Index(rest, "HandleFormatToggle(ctx, s, i, sess, data.CustomID)")
	if end < 0 {
		t.Fatal("HandleFormatToggle call not found after toggle dispatch")
	}
	block := rest[:end]
	if !strings.Contains(block, "context.WithTimeout(context.Background(), 90*time.Second)") {
		t.Fatalf("format toggle timeout must be 90s, block:\n%s", block)
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
			llm.FormatRoleBased:      "role render from LLM",
			llm.FormatFreeform:       "freeform render from LLM",
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

	// wantHTTPCalls: cache miss는 placeholder edit + final edit = 2 HTTP, cache hit은 final edit만 = 1.
	toggle := func(customID, wantBody string, wantRenderCalls int, wantFinalizeRuns int, wantHTTPCalls int) {
		t.Helper()
		beforeHTTP := len(rt.calls)

		HandleFormatToggle(ctx, discordSession, interaction, sess, customID)

		if summ.renderFormatCalls != wantRenderCalls {
			t.Fatalf("RenderFormat calls = %d, want %d", summ.renderFormatCalls, wantRenderCalls)
		}
		if delta := len(rt.calls) - beforeHTTP; delta != wantHTTPCalls {
			t.Fatalf("HTTP calls delta = %d, want %d", delta, wantHTTPCalls)
		}
		// 마지막 호출이 final edit이어야 하고 wantBody를 포함해야 함.
		call := rt.calls[len(rt.calls)-1]
		if call.method != http.MethodPatch {
			t.Fatalf("HTTP method = %s, want PATCH", call.method)
		}
		if !strings.Contains(call.body, wantBody) {
			t.Fatalf("final edit body missing %q:\n%s", wantBody, call.body)
		}
		// 2 HTTP calls (cache miss)인 경우, 첫 호출은 placeholder ("다시 만드는 중") 이어야 함.
		if wantHTTPCalls == 2 {
			placeholderCall := rt.calls[len(rt.calls)-2]
			if !strings.Contains(placeholderCall.body, "다시 만드는 중") {
				t.Fatalf("placeholder edit body missing '다시 만드는 중':\n%s", placeholderCall.body)
			}
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

	cases := []struct {
		customID string
		format   llm.NoteFormat
		body     string
	}{
		{customIDFormatToggleDecisionStatus, llm.FormatDecisionStatus, "decision render from LLM"},
		{customIDFormatToggleDiscussion, llm.FormatDiscussion, "discussion render from LLM"},
		{customIDFormatToggleRoleBased, llm.FormatRoleBased, "role render from LLM"},
		{customIDFormatToggleFreeform, llm.FormatFreeform, "freeform render from LLM"},
	}
	for idx, tc := range cases {
		wantCalls := idx + 1
		wantRuns := idx + 1
		// 첫 클릭: cache miss → placeholder + final = 2 HTTP
		toggle(tc.customID, tc.body, wantCalls, wantRuns, 2)

		summ.outputs[tc.format] = "rerender should not be used"
		// 두 번째 클릭: cache hit → final만 = 1 HTTP
		toggle(tc.customID, tc.body, wantCalls, wantRuns, 1)
	}
}

func TestHandleFormatToggle_RenderFormatFailureFallsBackWithoutCachingPureRender(t *testing.T) {
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
		ID:       "sess_toggle_fallback",
		ThreadID: "thread-toggle-fallback",
		GuildID:  "guild-toggle-fallback",
		OwnerID:  "owner-toggle-fallback",
		OpenedAt: time.Unix(1700000000, 0),
		Status:   db.SessionActive,
	}); err != nil {
		t.Fatalf("InsertSession: %v", err)
	}

	content := &llm.SummarizedContent{
		Decisions: []llm.Decision{{Title: "fallback cached decision"}},
	}
	raw, err := json.Marshal(content)
	if err != nil {
		t.Fatalf("Marshal summarized content: %v", err)
	}
	if err := d.InsertSummarizedContent(ctx, db.SummarizedContent{
		ID:          "sc_toggle_fallback",
		SessionID:   "sess_toggle_fallback",
		Content:     raw,
		ExtractedAt: time.Date(2026, 5, 14, 9, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("InsertSummarizedContent: %v", err)
	}

	summ := &formatToggleSummarizer{
		errs: map[llm.NoteFormat]error{
			llm.FormatDecisionStatus: errors.New("llm down"),
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
			AppID: "app-toggle-fallback",
			Token: "token-toggle-fallback",
		},
	}
	sess := &Session{
		ThreadID:    "thread-toggle-fallback",
		DBSessionID: "sess_toggle_fallback",
	}
	sess.AddNoteWithMeta(Note{Author: "alice", Content: "toggle fallback", Source: db.SourceHuman})

	HandleFormatToggle(ctx, discordSession, interaction, sess, customIDFormatToggleDecisionStatus)

	if summ.renderFormatCalls != 1 {
		t.Fatalf("RenderFormat calls = %d, want 1", summ.renderFormatCalls)
	}
	// cache miss → placeholder edit + final edit = 2 HTTP calls.
	if len(rt.calls) != 2 {
		t.Fatalf("HTTP calls = %d, want 2 (placeholder + final)", len(rt.calls))
	}
	if !strings.Contains(rt.calls[0].body, "다시 만드는 중") {
		t.Fatalf("first call body missing placeholder '다시 만드는 중':\n%s", rt.calls[0].body)
	}
	if !strings.Contains(rt.calls[1].body, "# 2026-05-14") || !strings.Contains(rt.calls[1].body, "fallback cached decision") {
		t.Fatalf("final edit body missing fallback render:\n%s", rt.calls[1].body)
	}
	var count int
	if err := d.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM finalize_runs WHERE summarized_content_id = ? AND format = ?",
		"sc_toggle_fallback", string(db.FormatDecisionStatus),
	).Scan(&count); err != nil {
		t.Fatalf("count fallback finalize_run: %v", err)
	}
	if count != 0 {
		t.Fatalf("fallback finalize_runs count = %d, want 0", count)
	}
}

// Codex review PR #13 P2: parse 실패 시 placeholder edit가 메시지를 변경하면 안 됨.
// 옛 순서로는 placeholder edit이 먼저 실행돼 채널 메시지가 "다시 만드는 중" 상태로 영구 stuck됐다.
// parse를 placeholder edit 앞으로 옮긴 후 회귀 가드.
func TestHandleFormatToggle_ParseFailure_NoPlaceholderEdit(t *testing.T) {
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
		ID:       "sess_parse_fail",
		ThreadID: "thread-parse-fail",
		GuildID:  "guild-parse-fail",
		OwnerID:  "owner-parse-fail",
		OpenedAt: time.Unix(1700000000, 0),
		Status:   db.SessionActive,
	}); err != nil {
		t.Fatalf("InsertSession: %v", err)
	}

	// malformed JSON content (legacy / corrupt row 시뮬레이션)
	if err := d.InsertSummarizedContent(ctx, db.SummarizedContent{
		ID:          "sc_malformed",
		SessionID:   "sess_parse_fail",
		Content:     []byte(`{"this is not valid": true, "decisions": <broken>`),
		ExtractedAt: time.Date(2026, 5, 14, 9, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("InsertSummarizedContent: %v", err)
	}

	// summarizer는 호출되면 안 됨 (parse 실패로 더 일찍 return).
	summarizer = &formatToggleSummarizer{outputs: map[llm.NoteFormat]string{}}

	rt := &recordingRoundTripper{}
	discordSession, err := discordgo.New("Bot test-token")
	if err != nil {
		t.Fatalf("discordgo.New: %v", err)
	}
	discordSession.Client = &http.Client{Transport: rt}

	interaction := &discordgo.InteractionCreate{
		Interaction: &discordgo.Interaction{
			AppID: "app-parse-fail",
			Token: "token-parse-fail",
		},
	}
	sess := &Session{
		ThreadID:    "thread-parse-fail",
		DBSessionID: "sess_parse_fail",
	}

	HandleFormatToggle(ctx, discordSession, interaction, sess, customIDFormatToggleDiscussion)

	// 핵심 가드: placeholder edit 발생 X — 메시지 원본 유지.
	for idx, c := range rt.calls {
		if strings.Contains(c.body, "다시 만드는 중") {
			t.Fatalf("call[%d] contains placeholder '다시 만드는 중' — parse 실패 시 메시지 stuck 회귀:\n%s",
				idx, c.body)
		}
	}
	// followup만 (ephemeral 에러 안내) — InteractionResponseEdit 없음.
	// FollowupMessageCreate는 POST /webhooks, InteractionResponseEdit은 PATCH /webhooks/.../messages/@original.
	for _, c := range rt.calls {
		if c.method == http.MethodPatch {
			t.Fatalf("PATCH (message edit) 발생 — parse 실패 시 메시지 안 건드려야 함:\n%s", c.body)
		}
	}
}

// ===== Copilot review PR #12 #3: HandleFormatCopy 단위 테스트 =====

func TestHandleFormatCopy_NoEmbed_SendsErrorFollowup(t *testing.T) {
	rt := &recordingRoundTripper{}
	discordSession, err := discordgo.New("Bot test-token")
	if err != nil {
		t.Fatalf("discordgo.New: %v", err)
	}
	discordSession.Client = &http.Client{Transport: rt}

	interaction := &discordgo.InteractionCreate{
		Interaction: &discordgo.Interaction{
			AppID:   "app-copy-no-embed",
			Token:   "token-copy-no-embed",
			Message: &discordgo.Message{}, // embed 없음
		},
	}
	sess := &Session{ThreadID: "thread-copy-no-embed"}

	HandleFormatCopy(context.Background(), discordSession, interaction, sess, customIDFormatCopy)

	// followup 1번만 (에러 안내).
	if len(rt.calls) != 1 {
		t.Fatalf("HTTP calls = %d, want 1 (error followup)", len(rt.calls))
	}
	if !strings.Contains(rt.calls[0].body, "찾을 수 없습니다") {
		t.Fatalf("error followup missing message:\n%s", rt.calls[0].body)
	}
}

func TestHandleFormatCopy_EmptyDescription_SendsErrorFollowup(t *testing.T) {
	rt := &recordingRoundTripper{}
	discordSession, err := discordgo.New("Bot test-token")
	if err != nil {
		t.Fatalf("discordgo.New: %v", err)
	}
	discordSession.Client = &http.Client{Transport: rt}

	interaction := &discordgo.InteractionCreate{
		Interaction: &discordgo.Interaction{
			AppID: "app-copy-empty-desc",
			Token: "token-copy-empty-desc",
			Message: &discordgo.Message{
				Embeds: []*discordgo.MessageEmbed{{Description: ""}},
			},
		},
	}
	sess := &Session{ThreadID: "thread-copy-empty-desc"}

	HandleFormatCopy(context.Background(), discordSession, interaction, sess, customIDFormatCopy)

	if len(rt.calls) != 1 {
		t.Fatalf("HTTP calls = %d, want 1", len(rt.calls))
	}
	if !strings.Contains(rt.calls[0].body, "비어 있습니다") {
		t.Fatalf("empty-desc followup missing message:\n%s", rt.calls[0].body)
	}
}

func TestHandleFormatCopy_AttachesMarkdownFile(t *testing.T) {
	rt := &recordingRoundTripper{}
	discordSession, err := discordgo.New("Bot test-token")
	if err != nil {
		t.Fatalf("discordgo.New: %v", err)
	}
	discordSession.Client = &http.Client{Transport: rt}

	md := "## 이번 회의에서 합의한 결정\n- 첫 결정\n- 두 번째 결정\n\n## 후속 작업\n- 코드 ```snippet``` 포함도 안전\n"
	interaction := &discordgo.InteractionCreate{
		Interaction: &discordgo.Interaction{
			AppID: "app-copy-ok",
			Token: "token-copy-ok",
			Message: &discordgo.Message{
				Embeds: []*discordgo.MessageEmbed{{Description: md}},
			},
		},
	}
	sess := &Session{ThreadID: "thread-copy-ok"}

	HandleFormatCopy(context.Background(), discordSession, interaction, sess, customIDFormatCopy)

	if len(rt.calls) != 1 {
		t.Fatalf("HTTP calls = %d, want 1 (file followup)", len(rt.calls))
	}
	body := rt.calls[0].body
	// 안내 메시지 + 파일 첨부 헤더 검증.
	if !strings.Contains(body, "정리본 markdown 파일을 첨부했습니다") {
		t.Errorf("notice text missing:\n%s", body)
	}
	// multipart/form-data 안에 .md 파일 + content-type 박혀야 함.
	if !strings.Contains(body, "meeting-summary-thread-copy-ok") || !strings.Contains(body, ".md") {
		t.Errorf("file attachment filename missing:\n%s", body)
	}
	if !strings.Contains(body, "text/markdown") {
		t.Errorf("file content-type missing:\n%s", body)
	}
	// 원본 markdown 그대로 (fence 충돌 무관) 박혀 있어야 함.
	if !strings.Contains(body, "코드 ```snippet``` 포함도 안전") {
		t.Errorf("file body missing original markdown (fence preserved):\n%s", body)
	}
	// 인라인 fenced code block (```markdown) 폐기 검증.
	if strings.Contains(body, "```markdown\n") {
		t.Errorf("inline ```markdown fence shouldn't appear (file attachment only):\n%s", body)
	}
}

// ===== Copilot review PR #13 #4: placeholder edit 실패 fallback 테스트 =====

func TestHandleFormatToggle_PlaceholderEditFailure_StillProceedsToFinalEdit(t *testing.T) {
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
		ID:       "sess_placeholder_fail",
		ThreadID: "thread-placeholder-fail",
		GuildID:  "guild-placeholder-fail",
		OwnerID:  "owner-placeholder-fail",
		OpenedAt: time.Unix(1700000000, 0),
		Status:   db.SessionActive,
	}); err != nil {
		t.Fatalf("InsertSession: %v", err)
	}
	content := &llm.SummarizedContent{Decisions: []llm.Decision{{Title: "테스트 결정"}}}
	raw, err := json.Marshal(content)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if err := d.InsertSummarizedContent(ctx, db.SummarizedContent{
		ID:          "sc_placeholder_fail",
		SessionID:   "sess_placeholder_fail",
		Content:     raw,
		ExtractedAt: time.Date(2026, 5, 14, 9, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("InsertSummarizedContent: %v", err)
	}

	summarizer = &formatToggleSummarizer{
		outputs: map[llm.NoteFormat]string{llm.FormatDiscussion: "discussion final render"},
	}

	// 첫 PATCH (placeholder)는 500 에러, 두 번째 PATCH (final)는 200.
	failFirstPATCH := &recordingRoundTripper{}
	patchCount := 0
	httpClient := &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		failFirstPATCH.calls = append(failFirstPATCH.calls, recordedHTTPCall{method: r.Method, path: r.URL.Path})
		if r.Method == http.MethodPatch {
			patchCount++
			if patchCount == 1 {
				return &http.Response{
					StatusCode: 500,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(strings.NewReader(`{"message": "internal error"}`)),
				}, nil
			}
		}
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{}`)),
		}, nil
	})}
	discordSession, err := discordgo.New("Bot test-token")
	if err != nil {
		t.Fatalf("discordgo.New: %v", err)
	}
	discordSession.Client = httpClient

	interaction := &discordgo.InteractionCreate{
		Interaction: &discordgo.Interaction{
			AppID: "app-placeholder-fail",
			Token: "token-placeholder-fail",
		},
	}
	sess := &Session{
		ThreadID:    "thread-placeholder-fail",
		DBSessionID: "sess_placeholder_fail",
	}

	HandleFormatToggle(ctx, discordSession, interaction, sess, customIDFormatToggleDiscussion)

	// placeholder PATCH 실패에도 LLM 호출 + final PATCH 진행.
	if patchCount < 2 {
		t.Fatalf("PATCH count = %d, want >= 2 (placeholder failed but final must proceed)", patchCount)
	}
	// finalize_runs 캐시 저장됐는지 (LLM 호출 성공 후 persist) 검증.
	var runs int
	if err := d.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM finalize_runs WHERE summarized_content_id = ? AND format = ?",
		"sc_placeholder_fail", string(db.FormatDiscussion),
	).Scan(&runs); err != nil {
		t.Fatalf("count finalize_runs: %v", err)
	}
	if runs != 1 {
		t.Errorf("finalize_runs count = %d, want 1 (placeholder failure shouldn't block persist)", runs)
	}
}

// ===== Copilot review PR #13 #3: 인플라이트 가드 회귀 테스트 =====

func TestHandleFormatToggle_InFlightGuard_BlocksConcurrentCacheMiss(t *testing.T) {
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
		ID:       "sess_inflight",
		ThreadID: "thread-inflight",
		GuildID:  "guild-inflight",
		OwnerID:  "owner-inflight",
		OpenedAt: time.Unix(1700000000, 0),
		Status:   db.SessionActive,
	}); err != nil {
		t.Fatalf("InsertSession: %v", err)
	}
	content := &llm.SummarizedContent{Decisions: []llm.Decision{{Title: "테스트"}}}
	raw, _ := json.Marshal(content)
	if err := d.InsertSummarizedContent(ctx, db.SummarizedContent{
		ID:          "sc_inflight",
		SessionID:   "sess_inflight",
		Content:     raw,
		ExtractedAt: time.Date(2026, 5, 14, 9, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("InsertSummarizedContent: %v", err)
	}

	summ := &formatToggleSummarizer{
		outputs: map[llm.NoteFormat]string{llm.FormatDiscussion: "discussion render"},
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
			AppID: "app-inflight",
			Token: "token-inflight",
		},
	}
	sess := &Session{
		ThreadID:             "thread-inflight",
		DBSessionID:          "sess_inflight",
		FormatToggleInFlight: true, // 이미 다른 호출 진행 중 시뮬레이션
	}

	HandleFormatToggle(ctx, discordSession, interaction, sess, customIDFormatToggleDiscussion)

	// LLM 호출 발생 X (인플라이트로 거부).
	if summ.renderFormatCalls != 0 {
		t.Errorf("RenderFormat calls = %d, want 0 (in-flight 거부)", summ.renderFormatCalls)
	}
	// followup ephemeral 1건 (안내 메시지) — 메시지 edit (PATCH) X.
	for _, c := range rt.calls {
		if c.method == http.MethodPatch {
			t.Errorf("PATCH 발생 — in-flight 거부 시 메시지 안 건드려야 함:\n%s", c.body)
		}
	}
	if len(rt.calls) != 1 {
		t.Errorf("HTTP calls = %d, want 1 (ephemeral followup만)", len(rt.calls))
	}
	if len(rt.calls) > 0 && !strings.Contains(rt.calls[0].body, "이전 포맷 변환이 아직 진행 중") {
		t.Errorf("in-flight followup message missing:\n%s", rt.calls[0].body)
	}
}

// roundTripperFunc는 http.RoundTripper 인터페이스를 함수 리터럴로 만족하는 헬퍼.
type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// ===== Codex review PR #14 P2: 복사 시 DB 캐시 full markdown 우선 사용 =====

func TestHandleFormatCopy_UsesDBCachedMarkdown_NotTruncatedEmbed(t *testing.T) {
	ctx := context.Background()
	d := newBotTestDB(t)
	oldDBConn := dbConn
	t.Cleanup(func() { dbConn = oldDBConn })
	dbConn = d

	if err := d.InsertSession(ctx, db.Session{
		ID:       "sess_copy_db",
		ThreadID: "thread-copy-db",
		GuildID:  "guild-copy-db",
		OwnerID:  "owner-copy-db",
		OpenedAt: time.Unix(1700000000, 0),
		Status:   db.SessionActive,
	}); err != nil {
		t.Fatalf("InsertSession: %v", err)
	}
	if err := d.InsertSummarizedContent(ctx, db.SummarizedContent{
		ID:          "sc_copy_db",
		SessionID:   "sess_copy_db",
		Content:     []byte(`{}`),
		ExtractedAt: time.Date(2026, 5, 14, 9, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("InsertSummarizedContent: %v", err)
	}

	// DB에는 full markdown 5000자, embed에는 잘린 4090자.
	fullMD := strings.Repeat("가", 5000) + "\n끝"
	truncatedEmbed := string([]rune(fullMD)[:4090]) + "…"
	if err := d.InsertFinalizeRun(ctx, db.FinalizeRun{
		ID:                  "fr_copy_db",
		SummarizedContentID: "sc_copy_db",
		Format:              db.FormatDecisionStatus,
		OutputMD:            fullMD,
		CreatedAt:           time.Now(),
	}); err != nil {
		t.Fatalf("InsertFinalizeRun: %v", err)
	}

	rt := &recordingRoundTripper{}
	discordSession, err := discordgo.New("Bot test-token")
	if err != nil {
		t.Fatalf("discordgo.New: %v", err)
	}
	discordSession.Client = &http.Client{Transport: rt}

	// 메시지 button row: decision_status SuccessButton (active) + 나머지 Secondary + Primary 복사.
	msg := &discordgo.Message{
		Embeds: []*discordgo.MessageEmbed{{Description: truncatedEmbed}},
		Components: []discordgo.MessageComponent{
			&discordgo.ActionsRow{
				Components: []discordgo.MessageComponent{
					&discordgo.Button{Label: "결정+진행", Style: discordgo.SuccessButton, CustomID: customIDFormatToggleDecisionStatus},
					&discordgo.Button{Label: "논의", Style: discordgo.SecondaryButton, CustomID: customIDFormatToggleDiscussion},
					&discordgo.Button{Label: "역할별", Style: discordgo.SecondaryButton, CustomID: customIDFormatToggleRoleBased},
					&discordgo.Button{Label: "자율", Style: discordgo.SecondaryButton, CustomID: customIDFormatToggleFreeform},
					&discordgo.Button{Label: "복사", Style: discordgo.PrimaryButton, CustomID: customIDFormatCopy},
				},
			},
		},
	}
	interaction := &discordgo.InteractionCreate{
		Interaction: &discordgo.Interaction{
			AppID:   "app-copy-db",
			Token:   "token-copy-db",
			Message: msg,
		},
	}
	sess := &Session{ThreadID: "thread-copy-db", DBSessionID: "sess_copy_db"}

	HandleFormatCopy(ctx, discordSession, interaction, sess, customIDFormatCopy)

	if len(rt.calls) != 1 {
		t.Fatalf("HTTP calls = %d, want 1", len(rt.calls))
	}
	body := rt.calls[0].body
	// 파일 본문에 full markdown 끝(잘리지 않은 부분)이 포함돼야 함.
	if !strings.Contains(body, "끝") {
		t.Errorf("file body missing tail of full markdown (DB cached SSOT 미사용):\n%s", body[:min(500, len(body))])
	}
	// 안내 메시지는 5002자 (full: "가"×5000 + "\n끝") 표시 — truncated 4090 아님.
	if !strings.Contains(body, "5002") {
		t.Errorf("notice text missing full-length count 5002:\n%s", body[:min(500, len(body))])
	}
	// embed fallback 안내는 없어야 함.
	if strings.Contains(body, "잘렸을 수 있음") {
		t.Errorf("DB hit인데 'embed 잘림' 안내가 들어감:\n%s", body[:min(500, len(body))])
	}
}

func TestHandleFormatCopy_FallsBackToEmbed_WhenDBMiss(t *testing.T) {
	ctx := context.Background()
	d := newBotTestDB(t)
	oldDBConn := dbConn
	t.Cleanup(func() { dbConn = oldDBConn })
	dbConn = d

	if err := d.InsertSession(ctx, db.Session{
		ID:       "sess_copy_dbmiss",
		ThreadID: "thread-copy-dbmiss",
		GuildID:  "g",
		OwnerID:  "o",
		OpenedAt: time.Unix(1700000000, 0),
		Status:   db.SessionActive,
	}); err != nil {
		t.Fatalf("InsertSession: %v", err)
	}
	// finalize_runs 비어 있음 (cache miss). summarized_contents 도 없음.

	rt := &recordingRoundTripper{}
	discordSession, err := discordgo.New("Bot test-token")
	if err != nil {
		t.Fatalf("discordgo.New: %v", err)
	}
	discordSession.Client = &http.Client{Transport: rt}

	embedMD := "## 결정\n- short markdown"
	msg := &discordgo.Message{
		Embeds: []*discordgo.MessageEmbed{{Description: embedMD}},
		Components: []discordgo.MessageComponent{
			&discordgo.ActionsRow{
				Components: []discordgo.MessageComponent{
					&discordgo.Button{Label: "결정+진행", Style: discordgo.SuccessButton, CustomID: customIDFormatToggleDecisionStatus},
				},
			},
		},
	}
	interaction := &discordgo.InteractionCreate{
		Interaction: &discordgo.Interaction{
			AppID:   "app-copy-dbmiss",
			Token:   "token-copy-dbmiss",
			Message: msg,
		},
	}
	sess := &Session{ThreadID: "thread-copy-dbmiss", DBSessionID: "sess_copy_dbmiss"}

	HandleFormatCopy(ctx, discordSession, interaction, sess, customIDFormatCopy)

	if len(rt.calls) != 1 {
		t.Fatalf("HTTP calls = %d, want 1", len(rt.calls))
	}
	body := rt.calls[0].body
	// embed fallback 안내가 있어야 함.
	if !strings.Contains(body, "잘렸을 수 있음") {
		t.Errorf("embed fallback notice 누락:\n%s", body[:min(500, len(body))])
	}
	// 파일 본문은 embed markdown 그대로.
	if !strings.Contains(body, "short markdown") {
		t.Errorf("file body missing embed markdown:\n%s", body[:min(500, len(body))])
	}
}

func TestActiveFormatFromMessage(t *testing.T) {
	cases := []struct {
		name      string
		msg       *discordgo.Message
		wantFmt   llm.NoteFormat
		wantFound bool
	}{
		{
			name:      "nil message",
			msg:       nil,
			wantFound: false,
		},
		{
			name: "no components",
			msg:  &discordgo.Message{},
		},
		{
			name: "discussion active",
			msg: &discordgo.Message{Components: []discordgo.MessageComponent{
				&discordgo.ActionsRow{Components: []discordgo.MessageComponent{
					&discordgo.Button{Style: discordgo.SecondaryButton, CustomID: customIDFormatToggleDecisionStatus},
					&discordgo.Button{Style: discordgo.SuccessButton, CustomID: customIDFormatToggleDiscussion},
					&discordgo.Button{Style: discordgo.PrimaryButton, CustomID: customIDFormatCopy}, // 복사는 매치 X
				}},
			}},
			wantFmt:   llm.FormatDiscussion,
			wantFound: true,
		},
		{
			name: "no success button",
			msg: &discordgo.Message{Components: []discordgo.MessageComponent{
				&discordgo.ActionsRow{Components: []discordgo.MessageComponent{
					&discordgo.Button{Style: discordgo.SecondaryButton, CustomID: customIDFormatToggleDecisionStatus},
					&discordgo.Button{Style: discordgo.PrimaryButton, CustomID: customIDFormatCopy},
				}},
			}},
			wantFound: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := activeFormatFromMessage(c.msg)
			if ok != c.wantFound {
				t.Errorf("ok = %v, want %v", ok, c.wantFound)
			}
			if c.wantFound && got != c.wantFmt {
				t.Errorf("format = %v, want %v", got, c.wantFmt)
			}
		})
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// Codex review PR #14 2차 P2: placeholder 로딩 중 [복사] 클릭 시
// "다시 만드는 중" 텍스트가 파일로 첨부되는 회귀 차단.
func TestHandleFormatCopy_InFlight_RejectsCopy(t *testing.T) {
	rt := &recordingRoundTripper{}
	discordSession, err := discordgo.New("Bot test-token")
	if err != nil {
		t.Fatalf("discordgo.New: %v", err)
	}
	discordSession.Client = &http.Client{Transport: rt}

	// embed 자체는 정상 (placeholder 시뮬레이션 — "다시 만드는 중..." 텍스트).
	msg := &discordgo.Message{
		Embeds: []*discordgo.MessageEmbed{{
			Description: "**논의** 포맷으로 정리본을 다시 만드는 중입니다…",
		}},
	}
	interaction := &discordgo.InteractionCreate{
		Interaction: &discordgo.Interaction{
			AppID:   "app-copy-inflight",
			Token:   "token-copy-inflight",
			Message: msg,
		},
	}
	sess := &Session{
		ThreadID:             "thread-copy-inflight",
		FormatToggleInFlight: true, // 다른 cache miss LLM 호출 진행 중
	}

	HandleFormatCopy(context.Background(), discordSession, interaction, sess, customIDFormatCopy)

	if len(rt.calls) != 1 {
		t.Fatalf("HTTP calls = %d, want 1 (ephemeral reject followup만)", len(rt.calls))
	}
	body := rt.calls[0].body
	if !strings.Contains(body, "포맷 변환이 진행 중") {
		t.Errorf("in-flight reject message missing:\n%s", body[:min(500, len(body))])
	}
	// "다시 만드는 중" placeholder가 파일로 첨부되면 안 됨.
	if strings.Contains(body, `filename="meeting-summary`) {
		t.Errorf("placeholder text가 파일로 첨부됨 (회귀):\n%s", body[:min(500, len(body))])
	}
}

// Codex review PR #14 3차 P2: button customID의 sc_id suffix로 옛 메시지 정확 lookup.
func TestParseToggleCustomID(t *testing.T) {
	cases := []struct {
		in       string
		wantBase string
		wantSCID string
	}{
		{"format_copy", "format_copy", ""},
		{"format_copy:sc_123_4", "format_copy", "sc_123_4"},
		{"format_toggle_decision_status", "format_toggle_decision_status", ""},
		{"format_toggle_discussion:sc_abc", "format_toggle_discussion", "sc_abc"},
		{"", "", ""},
	}
	for _, c := range cases {
		base, scID := parseToggleCustomID(c.in)
		if base != c.wantBase || scID != c.wantSCID {
			t.Errorf("parseToggleCustomID(%q) = (%q, %q), want (%q, %q)",
				c.in, base, scID, c.wantBase, c.wantSCID)
		}
	}
}

func TestFormatToggleComponents_WithSCID_BindsSuffix(t *testing.T) {
	comps := formatToggleComponents(llm.FormatDiscussion, "sc_xyz_7")
	row := comps[0].(discordgo.ActionsRow)
	if len(row.Components) != 5 {
		t.Fatalf("want 5 buttons, got %d", len(row.Components))
	}
	wantSuffix := ":sc_xyz_7"
	for _, c := range row.Components {
		btn := c.(discordgo.Button)
		if !strings.HasSuffix(btn.CustomID, wantSuffix) {
			t.Errorf("button %q missing suffix %q", btn.CustomID, wantSuffix)
		}
	}
}

func TestFormatToggleComponents_WithoutSCID_NoSuffix(t *testing.T) {
	comps := formatToggleComponents(llm.FormatDiscussion, "")
	row := comps[0].(discordgo.ActionsRow)
	for _, c := range row.Components {
		btn := c.(discordgo.Button)
		if strings.Contains(btn.CustomID, ":") {
			t.Errorf("empty scID인데 suffix가 박힘: %q", btn.CustomID)
		}
	}
}

// 옛 메시지(sc_id suffix 없음) 토글 시 GetLatestSummarizedContent fallback 동작.
func TestHandleFormatToggle_LegacyCustomID_FallsBackToLatest(t *testing.T) {
	ctx := context.Background()
	d := newBotTestDB(t)
	oldDBConn := dbConn
	oldSummarizer := summarizer
	t.Cleanup(func() { dbConn = oldDBConn; summarizer = oldSummarizer })
	dbConn = d

	if err := d.InsertSession(ctx, db.Session{
		ID: "sess_legacy", ThreadID: "thread-legacy", GuildID: "g", OwnerID: "o",
		OpenedAt: time.Unix(1700000000, 0), Status: db.SessionActive,
	}); err != nil {
		t.Fatalf("InsertSession: %v", err)
	}
	if err := d.InsertSummarizedContent(ctx, db.SummarizedContent{
		ID: "sc_latest", SessionID: "sess_legacy", Content: []byte(`{}`),
		ExtractedAt: time.Date(2026, 5, 14, 9, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("InsertSummarizedContent: %v", err)
	}
	if err := d.InsertFinalizeRun(ctx, db.FinalizeRun{
		ID: "fr_legacy", SummarizedContentID: "sc_latest",
		Format: db.FormatDiscussion, OutputMD: "## latest", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("InsertFinalizeRun: %v", err)
	}

	summarizer = &formatToggleSummarizer{}
	rt := &recordingRoundTripper{}
	discordSession, err := discordgo.New("Bot test-token")
	if err != nil {
		t.Fatalf("discordgo.New: %v", err)
	}
	discordSession.Client = &http.Client{Transport: rt}

	interaction := &discordgo.InteractionCreate{
		Interaction: &discordgo.Interaction{AppID: "a", Token: "t"},
	}
	sess := &Session{ThreadID: "thread-legacy", DBSessionID: "sess_legacy"}

	// 옛 customID (suffix 없음) → GetLatest fallback.
	HandleFormatToggle(ctx, discordSession, interaction, sess, customIDFormatToggleDiscussion)

	if len(rt.calls) != 1 {
		t.Fatalf("HTTP calls = %d, want 1", len(rt.calls))
	}
	if !strings.Contains(rt.calls[0].body, "## latest") {
		t.Errorf("final edit body missing latest cache:\n%s", rt.calls[0].body[:min(500, len(rt.calls[0].body))])
	}
}

// sc_id suffix가 있으면 그 sc의 markdown 사용 (GetLatest와 다른 sc 케이스).
func TestHandleFormatToggle_SCIDSuffix_UsesExactRow(t *testing.T) {
	ctx := context.Background()
	d := newBotTestDB(t)
	oldDBConn := dbConn
	oldSummarizer := summarizer
	t.Cleanup(func() { dbConn = oldDBConn; summarizer = oldSummarizer })
	dbConn = d

	if err := d.InsertSession(ctx, db.Session{
		ID: "sess_multi", ThreadID: "thread-multi", GuildID: "g", OwnerID: "o",
		OpenedAt: time.Unix(1700000000, 0), Status: db.SessionActive,
	}); err != nil {
		t.Fatalf("InsertSession: %v", err)
	}
	// 옛 sc + 새 sc 둘 다 같은 세션에.
	if err := d.InsertSummarizedContent(ctx, db.SummarizedContent{
		ID: "sc_old", SessionID: "sess_multi", Content: []byte(`{}`),
		ExtractedAt: time.Date(2026, 5, 14, 9, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("InsertSummarizedContent old: %v", err)
	}
	if err := d.InsertSummarizedContent(ctx, db.SummarizedContent{
		ID: "sc_new", SessionID: "sess_multi", Content: []byte(`{}`),
		ExtractedAt: time.Date(2026, 5, 14, 10, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("InsertSummarizedContent new: %v", err)
	}
	if err := d.InsertFinalizeRun(ctx, db.FinalizeRun{
		ID: "fr_old", SummarizedContentID: "sc_old",
		Format: db.FormatDiscussion, OutputMD: "## old summary", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("InsertFinalizeRun old: %v", err)
	}
	if err := d.InsertFinalizeRun(ctx, db.FinalizeRun{
		ID: "fr_new", SummarizedContentID: "sc_new",
		Format: db.FormatDiscussion, OutputMD: "## new summary", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("InsertFinalizeRun new: %v", err)
	}

	summarizer = &formatToggleSummarizer{}
	rt := &recordingRoundTripper{}
	discordSession, err := discordgo.New("Bot test-token")
	if err != nil {
		t.Fatalf("discordgo.New: %v", err)
	}
	discordSession.Client = &http.Client{Transport: rt}

	interaction := &discordgo.InteractionCreate{
		Interaction: &discordgo.Interaction{AppID: "a", Token: "t"},
	}
	sess := &Session{ThreadID: "thread-multi", DBSessionID: "sess_multi"}

	// 옛 메시지 토글 (sc_id=sc_old suffix) → "## old summary" 나와야 함 (GetLatest는 sc_new인데 무시).
	HandleFormatToggle(ctx, discordSession, interaction, sess, customIDFormatToggleDiscussion+":sc_old")

	if len(rt.calls) != 1 {
		t.Fatalf("HTTP calls = %d, want 1", len(rt.calls))
	}
	if !strings.Contains(rt.calls[0].body, "old summary") {
		t.Errorf("sc_old의 markdown 미사용 — sc_id binding 회귀:\n%s", rt.calls[0].body[:min(500, len(rt.calls[0].body))])
	}
	if strings.Contains(rt.calls[0].body, "new summary") {
		t.Errorf("sc_new의 markdown이 잘못 사용됨:\n%s", rt.calls[0].body[:min(500, len(rt.calls[0].body))])
	}
}

// HandleFormatCopy도 sc_id suffix로 정확한 sc 조회.
func TestHandleFormatCopy_SCIDSuffix_UsesExactRow(t *testing.T) {
	ctx := context.Background()
	d := newBotTestDB(t)
	oldDBConn := dbConn
	t.Cleanup(func() { dbConn = oldDBConn })
	dbConn = d

	if err := d.InsertSession(ctx, db.Session{
		ID: "sess_copy_multi", ThreadID: "thread-copy-multi", GuildID: "g", OwnerID: "o",
		OpenedAt: time.Unix(1700000000, 0), Status: db.SessionActive,
	}); err != nil {
		t.Fatalf("InsertSession: %v", err)
	}
	if err := d.InsertSummarizedContent(ctx, db.SummarizedContent{
		ID: "sc_old_copy", SessionID: "sess_copy_multi", Content: []byte(`{}`),
		ExtractedAt: time.Date(2026, 5, 14, 9, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("InsertSummarizedContent: %v", err)
	}
	if err := d.InsertSummarizedContent(ctx, db.SummarizedContent{
		ID: "sc_new_copy", SessionID: "sess_copy_multi", Content: []byte(`{}`),
		ExtractedAt: time.Date(2026, 5, 14, 10, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("InsertSummarizedContent new: %v", err)
	}
	if err := d.InsertFinalizeRun(ctx, db.FinalizeRun{
		ID: "fr_old_copy", SummarizedContentID: "sc_old_copy",
		Format: db.FormatDecisionStatus, OutputMD: "OLD MARKDOWN", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("InsertFinalizeRun: %v", err)
	}
	if err := d.InsertFinalizeRun(ctx, db.FinalizeRun{
		ID: "fr_new_copy", SummarizedContentID: "sc_new_copy",
		Format: db.FormatDecisionStatus, OutputMD: "NEW MARKDOWN", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("InsertFinalizeRun new: %v", err)
	}

	rt := &recordingRoundTripper{}
	discordSession, err := discordgo.New("Bot test-token")
	if err != nil {
		t.Fatalf("discordgo.New: %v", err)
	}
	discordSession.Client = &http.Client{Transport: rt}

	// 옛 메시지의 [복사] 시뮬레이션 — embed는 "OLD..." 보이고, customID에는 sc_old_copy.
	msg := &discordgo.Message{
		Embeds: []*discordgo.MessageEmbed{{Description: "OLD MARKDOWN (embed)"}},
		Components: []discordgo.MessageComponent{
			&discordgo.ActionsRow{Components: []discordgo.MessageComponent{
				&discordgo.Button{Style: discordgo.SuccessButton, CustomID: customIDFormatToggleDecisionStatus + ":sc_old_copy"},
			}},
		},
	}
	interaction := &discordgo.InteractionCreate{
		Interaction: &discordgo.Interaction{AppID: "a", Token: "t", Message: msg},
	}
	sess := &Session{ThreadID: "thread-copy-multi", DBSessionID: "sess_copy_multi"}

	// customID에 sc_old_copy suffix → "OLD MARKDOWN" 사용해야 함.
	HandleFormatCopy(ctx, discordSession, interaction, sess, customIDFormatCopy+":sc_old_copy")

	if len(rt.calls) != 1 {
		t.Fatalf("HTTP calls = %d, want 1", len(rt.calls))
	}
	body := rt.calls[0].body
	if !strings.Contains(body, "OLD MARKDOWN") {
		t.Errorf("sc_old_copy markdown 미사용 — sc_id binding 회귀:\n%s", body[:min(500, len(body))])
	}
	if strings.Contains(body, "NEW MARKDOWN") {
		t.Errorf("최신 sc_new_copy markdown 잘못 사용:\n%s", body[:min(500, len(body))])
	}
}
