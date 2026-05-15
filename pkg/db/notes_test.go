package db

import (
	"context"
	"errors"
	"testing"
	"time"
)

// seedSession은 노트 테스트용 세션 1개를 만든다.
func seedSession(t *testing.T, d *DB, sessionID, threadID string) {
	t.Helper()
	err := d.InsertSession(context.Background(), Session{
		ID: sessionID, ThreadID: threadID, GuildID: "g", OwnerID: "o",
		OpenedAt: time.Unix(1700000000, 0), Status: SessionActive,
	})
	if err != nil {
		t.Fatalf("seedSession: %v", err)
	}
}

func TestNotes_InsertAndList(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()
	seedSession(t, d, "sess_n", "thread_n")

	// given: 3 notes (시간 역순 삽입 → list는 정순으로 와야 함)
	notes := []Note{
		{ID: "n3", SessionID: "sess_n", AuthorID: "u1", AuthorName: "alice",
			AuthorRoles: []byte(`["BACKEND"]`), Content: "third", Source: SourceHuman,
			Timestamp: time.Unix(1700000300, 0)},
		{ID: "n1", SessionID: "sess_n", AuthorID: "u2", AuthorName: "bob",
			AuthorRoles: []byte(`["FRONTEND"]`), Content: "first", Source: SourceHuman,
			Timestamp: time.Unix(1700000100, 0)},
		{ID: "n2", SessionID: "sess_n", AuthorID: "u1", AuthorName: "alice",
			AuthorRoles: []byte(`["BACKEND"]`), Content: "second", Source: SourceHuman,
			Timestamp: time.Unix(1700000200, 0)},
	}
	for _, n := range notes {
		if err := d.InsertNote(ctx, n); err != nil {
			t.Fatalf("InsertNote %q: %v", n.ID, err)
		}
	}

	// when
	got, err := d.ListNotesBySession(ctx, "sess_n")
	if err != nil {
		t.Fatalf("ListNotesBySession: %v", err)
	}

	// then: 시간순 [n1, n2, n3]
	wantOrder := []string{"n1", "n2", "n3"}
	if len(got) != len(wantOrder) {
		t.Fatalf("count = %d, want %d", len(got), len(wantOrder))
	}
	for i, w := range wantOrder {
		if got[i].ID != w {
			t.Errorf("position %d: id = %q, want %q", i, got[i].ID, w)
		}
	}
}

func TestNotes_ListForCorpusExcludesInterimSummary(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()
	seedSession(t, d, "sess_corp", "thread_corp")

	// given: Human + WeeklyDump + InterimSummary + ExternalPaste 섞여 있음
	mix := []Note{
		{ID: "h1", SessionID: "sess_corp", AuthorID: "u1", AuthorName: "alice",
			Content: "human", Source: SourceHuman, Timestamp: time.Unix(1700000100, 0)},
		{ID: "w1", SessionID: "sess_corp", AuthorID: "[tool]", AuthorName: "[tool]",
			Content: "weekly dump", Source: SourceWeeklyDump, Timestamp: time.Unix(1700000200, 0)},
		{ID: "i1", SessionID: "sess_corp", AuthorID: "[bot]", AuthorName: "[bot]",
			Content: "interim summary", Source: SourceInterimSummary, Timestamp: time.Unix(1700000300, 0)},
		{ID: "e1", SessionID: "sess_corp", AuthorID: "u2", AuthorName: "bob",
			Content: "external paste", Source: SourceExternalPaste, Timestamp: time.Unix(1700000400, 0)},
	}
	for _, n := range mix {
		if err := d.InsertNote(ctx, n); err != nil {
			t.Fatalf("InsertNote %q: %v", n.ID, err)
		}
	}

	// when
	corpus, err := d.ListNotesForCorpus(ctx, "sess_corp")
	if err != nil {
		t.Fatalf("ListNotesForCorpus: %v", err)
	}

	// then: i1 (InterimSummary)만 빠진 3개
	wantIDs := map[string]bool{"h1": true, "w1": true, "e1": true}
	if len(corpus) != 3 {
		t.Errorf("corpus count = %d, want 3 (InterimSummary 제외)", len(corpus))
	}
	for _, n := range corpus {
		if !wantIDs[n.ID] {
			t.Errorf("unexpected note in corpus: %q (Source=%q)", n.ID, n.Source)
		}
		if n.Source == SourceInterimSummary {
			t.Errorf("InterimSummary가 corpus에 포함됨: %q (필터 깨짐)", n.ID)
		}
	}
}

func TestNoteSource_Predicates(t *testing.T) {
	tests := []struct {
		src             NoteSource
		wantAttribution bool
		wantInCorpus    bool
	}{
		{SourceHuman, true, true},
		{SourceWeeklyDump, false, true},
		{SourceReleaseResult, false, true},
		{SourceAgentOutput, false, true},
		{SourceInterimSummary, false, false},
		{SourceExternalPaste, false, true},
	}
	for _, tc := range tests {
		t.Run(string(tc.src), func(t *testing.T) {
			if got := tc.src.IsAttributionCandidate(); got != tc.wantAttribution {
				t.Errorf("IsAttributionCandidate = %v, want %v", got, tc.wantAttribution)
			}
			if got := tc.src.IsInCorpus(); got != tc.wantInCorpus {
				t.Errorf("IsInCorpus = %v, want %v", got, tc.wantInCorpus)
			}
		})
	}
}

func TestNotes_GetNotFound(t *testing.T) {
	d := newTestDB(t)
	_, err := d.GetNote(context.Background(), "missing")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestSegments_InsertEndList(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()
	seedSession(t, d, "sess_seg", "thread_seg")

	startedAt := time.Unix(1700000100, 0)
	endedAt := time.Unix(1700000200, 0)

	// given: segment 시작
	if err := d.InsertSegment(ctx, Segment{
		ID: "seg_w1", SessionID: "sess_seg", Kind: SegmentWeeklySummary,
		StartedAt: startedAt,
	}); err != nil {
		t.Fatalf("InsertSegment: %v", err)
	}

	// when: segment 종료 + artifact 갱신
	artifact := []byte(`{"repo":"bottle-note/api","commits":37}`)
	if err := d.EndSegment(ctx, "seg_w1", endedAt, artifact); err != nil {
		t.Fatalf("EndSegment: %v", err)
	}

	// then: 목록에서 ended_at + artifact 모두 보임
	got, err := d.ListSegmentsBySession(ctx, "sess_seg")
	if err != nil {
		t.Fatalf("ListSegmentsBySession: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("segment count = %d, want 1", len(got))
	}
	s := got[0]
	if s.Kind != SegmentWeeklySummary {
		t.Errorf("Kind = %q, want %q", s.Kind, SegmentWeeklySummary)
	}
	if s.EndedAt == nil || s.EndedAt.Unix() != endedAt.Unix() {
		t.Errorf("EndedAt = %v, want %v", s.EndedAt, endedAt)
	}
	if string(s.Artifact) != string(artifact) {
		t.Errorf("Artifact = %s, want %s", s.Artifact, artifact)
	}
}

func TestSegments_EndIsIdempotent(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()
	seedSession(t, d, "sess_idem", "thread_idem")

	startedAt := time.Unix(1700000100, 0)
	firstEnd := time.Unix(1700000200, 0)
	if err := d.InsertSegment(ctx, Segment{
		ID: "seg_x", SessionID: "sess_idem", Kind: SegmentRelease, StartedAt: startedAt,
	}); err != nil {
		t.Fatalf("InsertSegment: %v", err)
	}
	if err := d.EndSegment(ctx, "seg_x", firstEnd, []byte(`"first"`)); err != nil {
		t.Fatalf("first EndSegment: %v", err)
	}

	// when: 두 번째 end (다른 timestamp + artifact)
	if err := d.EndSegment(ctx, "seg_x", firstEnd.Add(1*time.Hour), []byte(`"second"`)); err != nil {
		t.Fatalf("second EndSegment: %v", err)
	}

	// then: 첫 값 유지 (UPDATE 조건이 ended_at IS NULL이므로)
	got, _ := d.ListSegmentsBySession(ctx, "sess_idem")
	if len(got) != 1 {
		t.Fatalf("segment count = %d, want 1", len(got))
	}
	if got[0].EndedAt.Unix() != firstEnd.Unix() {
		t.Errorf("EndedAt = %v, want %v (멱등 깨짐)", got[0].EndedAt, firstEnd)
	}
	if string(got[0].Artifact) != `"first"` {
		t.Errorf("Artifact = %s, want %q (덮어씀)", got[0].Artifact, `"first"`)
	}
}

func TestNotes_SegmentForeignKeyOnDelete(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()
	seedSession(t, d, "sess_fk", "thread_fk")

	// given: segment + 그 segment에 속한 note
	if err := d.InsertSegment(ctx, Segment{
		ID: "seg_d", SessionID: "sess_fk", Kind: SegmentAgent, StartedAt: time.Unix(1700000100, 0),
	}); err != nil {
		t.Fatalf("InsertSegment: %v", err)
	}
	segID := "seg_d"
	if err := d.InsertNote(ctx, Note{
		ID: "n_in_seg", SessionID: "sess_fk", SegmentID: &segID,
		AuthorID: "u1", AuthorName: "alice", Content: "agent output",
		Source: SourceAgentOutput, Timestamp: time.Unix(1700000150, 0),
	}); err != nil {
		t.Fatalf("InsertNote: %v", err)
	}

	// when: segment 삭제
	if _, err := d.ExecContext(ctx, "DELETE FROM segments WHERE id = ?", segID); err != nil {
		t.Fatalf("delete segment: %v", err)
	}

	// then: note의 segment_id가 NULL로 풀림 (ON DELETE SET NULL)
	got, err := d.GetNote(ctx, "n_in_seg")
	if err != nil {
		t.Fatalf("GetNote after segment delete: %v", err)
	}
	if got.SegmentID != nil {
		t.Errorf("SegmentID = %v, want nil (ON DELETE SET NULL 깨짐)", *got.SegmentID)
	}
}
