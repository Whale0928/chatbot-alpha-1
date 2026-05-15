package bot

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"chatbot-alpha-1/pkg/db"
	"chatbot-alpha-1/pkg/llm"

	"github.com/bwmarrin/discordgo"
)

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
