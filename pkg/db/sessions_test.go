package db

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// newTestDB는 임시 디렉터리에 새 SQLite를 열고 마이그레이션까지 실행한 *DB를 반환한다.
// 모든 repository 테스트가 공유하는 헬퍼.
func newTestDB(t *testing.T) *DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	d, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if err := d.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return d
}

func TestSessions_InsertAndGet(t *testing.T) {
	// given
	d := newTestDB(t)
	ctx := context.Background()
	now := time.Unix(1700000000, 0)
	s := Session{
		ID:            "sess_1",
		ThreadID:      "thread_1",
		GuildID:       "guild_1",
		OwnerID:       "owner_1",
		OpenedAt:      now,
		Status:        SessionActive,
		RolesSnapshot: []byte(`{"u1":["BACKEND"],"u2":["FRONTEND","PM"]}`),
	}

	// when
	if err := d.InsertSession(ctx, s); err != nil {
		t.Fatalf("InsertSession: %v", err)
	}
	got, err := d.GetSession(ctx, "sess_1")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}

	// then
	if got.ID != s.ID || got.ThreadID != s.ThreadID || got.GuildID != s.GuildID {
		t.Errorf("session round-trip mismatch: %+v vs %+v", got, s)
	}
	if got.Status != SessionActive {
		t.Errorf("status = %q, want %q", got.Status, SessionActive)
	}
	if string(got.RolesSnapshot) != string(s.RolesSnapshot) {
		t.Errorf("roles_snapshot mismatch:\n got=%s\nwant=%s", got.RolesSnapshot, s.RolesSnapshot)
	}
	if got.ClosedAt != nil {
		t.Errorf("ClosedAt should be nil for active session, got %v", got.ClosedAt)
	}
}

func TestSessions_GetByThread(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()
	now := time.Unix(1700000000, 0)
	s := Session{ID: "sess_a", ThreadID: "thread_a", GuildID: "g", OwnerID: "o", OpenedAt: now, Status: SessionActive}
	if err := d.InsertSession(ctx, s); err != nil {
		t.Fatalf("InsertSession: %v", err)
	}

	got, err := d.GetSessionByThread(ctx, "thread_a")
	if err != nil {
		t.Fatalf("GetSessionByThread: %v", err)
	}
	if got.ID != "sess_a" {
		t.Errorf("got id = %q, want %q", got.ID, "sess_a")
	}
}

func TestSessions_GetNotFound(t *testing.T) {
	d := newTestDB(t)
	_, err := d.GetSession(context.Background(), "missing")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestSessions_CloseIsIdempotent(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()
	openedAt := time.Unix(1700000000, 0)
	closedAt := time.Unix(1700000600, 0)
	if err := d.InsertSession(ctx, Session{
		ID: "sess_c", ThreadID: "t_c", GuildID: "g", OwnerID: "o",
		OpenedAt: openedAt, Status: SessionActive,
	}); err != nil {
		t.Fatalf("InsertSession: %v", err)
	}

	// when: close 두 번
	if err := d.CloseSession(ctx, "sess_c", closedAt); err != nil {
		t.Fatalf("first CloseSession: %v", err)
	}
	if err := d.CloseSession(ctx, "sess_c", closedAt.Add(1*time.Hour)); err != nil {
		t.Fatalf("second CloseSession: %v", err)
	}

	// then: status=CLOSED, closed_at은 첫 호출 값 유지 (UPDATE는 status=ACTIVE인 행만 건드림)
	got, err := d.GetSession(ctx, "sess_c")
	if err != nil {
		t.Fatalf("GetSession after close: %v", err)
	}
	if got.Status != SessionClosed {
		t.Errorf("status = %q, want %q", got.Status, SessionClosed)
	}
	if got.ClosedAt == nil || got.ClosedAt.Unix() != closedAt.Unix() {
		t.Errorf("ClosedAt = %v, want %v (멱등 깨짐 — 두 번째 close가 덮어씀)", got.ClosedAt, closedAt)
	}
}

func TestSessions_ListActive(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()
	base := time.Unix(1700000000, 0)

	// given: ACTIVE 2, CLOSED 1
	for i, st := range []SessionStatus{SessionActive, SessionClosed, SessionActive} {
		s := Session{
			ID: "s_" + string(rune('a'+i)), ThreadID: "th_" + string(rune('a'+i)),
			GuildID: "g", OwnerID: "o",
			OpenedAt: base.Add(time.Duration(i) * time.Minute),
			Status:   SessionActive,
		}
		if err := d.InsertSession(ctx, s); err != nil {
			t.Fatalf("InsertSession[%d]: %v", i, err)
		}
		if st == SessionClosed {
			if err := d.CloseSession(ctx, s.ID, base.Add(time.Hour)); err != nil {
				t.Fatalf("CloseSession[%d]: %v", i, err)
			}
		}
	}

	got, err := d.ListActiveSessions(ctx)
	if err != nil {
		t.Fatalf("ListActiveSessions: %v", err)
	}

	if len(got) != 2 {
		t.Errorf("active count = %d, want 2", len(got))
	}
	for _, s := range got {
		if s.Status != SessionActive {
			t.Errorf("session %q status = %q, want ACTIVE", s.ID, s.Status)
		}
	}
}
