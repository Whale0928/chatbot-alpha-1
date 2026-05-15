package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ErrNotFound는 단일 row 조회 결과 없음.
// repository 메서드가 sql.ErrNoRows를 wrap해 반환한다.
var ErrNotFound = errors.New("db: not found")

// =====================================================================
// Sessions repository
// =====================================================================

// InsertSession은 새 세션 행을 삽입한다.
// id는 호출자가 생성 (예: nanoid, uuid). DB는 ID 중복 시 UNIQUE 위반 에러.
// ClosedAt은 호출 시점에 nil이어야 함 (세션 종료는 별도 CloseSession 호출).
func (d *DB) InsertSession(ctx context.Context, s Session) error {
	roles := s.RolesSnapshot
	if roles == nil {
		roles = []byte("{}")
	}
	_, err := d.ExecContext(ctx,
		`INSERT INTO sessions
		   (id, thread_id, guild_id, owner_id, opened_at, status, roles_snapshot)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		s.ID, s.ThreadID, s.GuildID, s.OwnerID,
		s.OpenedAt.Unix(), string(s.Status), string(roles),
	)
	if err != nil {
		return fmt.Errorf("db: insert session %q: %w", s.ID, err)
	}
	return nil
}

// GetSession은 id로 세션 행을 조회한다.
// 행이 없으면 ErrNotFound.
func (d *DB) GetSession(ctx context.Context, id string) (Session, error) {
	var (
		s        Session
		openedAt int64
		closedAt sql.NullInt64
		status   string
		roles    string
	)
	err := d.QueryRowContext(ctx,
		`SELECT id, thread_id, guild_id, owner_id, opened_at, closed_at, status, roles_snapshot
		 FROM sessions WHERE id = ?`, id,
	).Scan(&s.ID, &s.ThreadID, &s.GuildID, &s.OwnerID, &openedAt, &closedAt, &status, &roles)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, ErrNotFound
	}
	if err != nil {
		return Session{}, fmt.Errorf("db: get session %q: %w", id, err)
	}
	s.OpenedAt = time.Unix(openedAt, 0)
	if closedAt.Valid {
		t := time.Unix(closedAt.Int64, 0)
		s.ClosedAt = &t
	}
	s.Status = SessionStatus(status)
	s.RolesSnapshot = []byte(roles)
	return s, nil
}

// GetSessionByThread는 thread_id로 활성 세션을 조회한다 (Discord 메시지 핸들러에서 사용).
// thread_id는 UNIQUE이므로 0개 또는 1개.
func (d *DB) GetSessionByThread(ctx context.Context, threadID string) (Session, error) {
	var id string
	err := d.QueryRowContext(ctx,
		"SELECT id FROM sessions WHERE thread_id = ?", threadID,
	).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, ErrNotFound
	}
	if err != nil {
		return Session{}, fmt.Errorf("db: get session by thread %q: %w", threadID, err)
	}
	return d.GetSession(ctx, id)
}

// CloseSession은 세션을 CLOSED로 전환하고 closed_at을 기록한다.
// 이미 CLOSED인 세션을 다시 닫아도 noop (idempotent — Discord 메시지 재처리 안전).
func (d *DB) CloseSession(ctx context.Context, id string, closedAt time.Time) error {
	res, err := d.ExecContext(ctx,
		`UPDATE sessions SET status = ?, closed_at = ?
		 WHERE id = ? AND status = ?`,
		string(SessionClosed), closedAt.Unix(), id, string(SessionActive),
	)
	if err != nil {
		return fmt.Errorf("db: close session %q: %w", id, err)
	}
	// rows affected 0은 이미 CLOSED 상태이거나 id 없음 — 둘 다 idempotent OK.
	_ = res
	return nil
}

// ListActiveSessions는 봇 재시작 시 active 세션 복원용.
// 현재는 단일 봇 인스턴스라 0~수개 수준.
func (d *DB) ListActiveSessions(ctx context.Context) ([]Session, error) {
	rows, err := d.QueryContext(ctx,
		"SELECT id FROM sessions WHERE status = ? ORDER BY opened_at",
		string(SessionActive),
	)
	if err != nil {
		return nil, fmt.Errorf("db: list active sessions: %w", err)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("db: scan active session id: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: iterate active sessions: %w", err)
	}

	out := make([]Session, 0, len(ids))
	for _, id := range ids {
		s, err := d.GetSession(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("db: load active session %q: %w", id, err)
		}
		out = append(out, s)
	}
	return out, nil
}
