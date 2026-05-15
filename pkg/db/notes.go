package db

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// =====================================================================
// Notes repository
// =====================================================================

// InsertNote는 새 노트를 삽입한다.
// SegmentID가 nil이면 자유 발화로 기록 (segment 외부).
// AuthorRoles가 nil이면 빈 배열 ([])로 저장.
func (d *DB) InsertNote(ctx context.Context, n Note) error {
	roles := n.AuthorRoles
	if roles == nil {
		roles = []byte("[]")
	}
	var segmentID sql.NullString
	if n.SegmentID != nil {
		segmentID = sql.NullString{String: *n.SegmentID, Valid: true}
	}
	_, err := d.ExecContext(ctx,
		`INSERT INTO notes
		   (id, session_id, segment_id, author_id, author_name, author_roles, content, source, timestamp)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		n.ID, n.SessionID, segmentID,
		n.AuthorID, n.AuthorName, string(roles),
		n.Content, string(n.Source), n.Timestamp.Unix(),
	)
	if err != nil {
		return fmt.Errorf("db: insert note %q: %w", n.ID, err)
	}
	return nil
}

// ListNotesBySession은 세션의 모든 노트를 시간순 반환한다.
// 정리본 추출 시 호출 — corpus 빌드용.
func (d *DB) ListNotesBySession(ctx context.Context, sessionID string) ([]Note, error) {
	return d.queryNotes(ctx,
		`SELECT id, session_id, segment_id, author_id, author_name, author_roles, content, source, timestamp
		 FROM notes WHERE session_id = ? ORDER BY timestamp, id`,
		sessionID,
	)
}

// ListNotesForCorpus는 finalize/interim corpus 입력용 노트만 반환한다.
// Source.IsInCorpus()=false인 항목 (예: InterimSummary)을 SQL 단계에서 제외한다.
func (d *DB) ListNotesForCorpus(ctx context.Context, sessionID string) ([]Note, error) {
	return d.queryNotes(ctx,
		`SELECT id, session_id, segment_id, author_id, author_name, author_roles, content, source, timestamp
		 FROM notes WHERE session_id = ? AND source != ? ORDER BY timestamp, id`,
		sessionID, string(SourceInterimSummary),
	)
}

// GetNote는 id 단건 조회.
func (d *DB) GetNote(ctx context.Context, id string) (Note, error) {
	notes, err := d.queryNotes(ctx,
		`SELECT id, session_id, segment_id, author_id, author_name, author_roles, content, source, timestamp
		 FROM notes WHERE id = ?`,
		id,
	)
	if err != nil {
		return Note{}, err
	}
	if len(notes) == 0 {
		return Note{}, ErrNotFound
	}
	return notes[0], nil
}

// queryNotes는 공통 row scanner.
func (d *DB) queryNotes(ctx context.Context, query string, args ...any) ([]Note, error) {
	rows, err := d.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("db: query notes: %w", err)
	}
	defer rows.Close()

	var out []Note
	for rows.Next() {
		var (
			n         Note
			segmentID sql.NullString
			roles     string
			source    string
			ts        int64
		)
		err := rows.Scan(
			&n.ID, &n.SessionID, &segmentID,
			&n.AuthorID, &n.AuthorName, &roles,
			&n.Content, &source, &ts,
		)
		if err != nil {
			return nil, fmt.Errorf("db: scan note: %w", err)
		}
		if segmentID.Valid {
			s := segmentID.String
			n.SegmentID = &s
		}
		n.AuthorRoles = []byte(roles)
		n.Source = NoteSource(source)
		n.Timestamp = time.Unix(ts, 0)
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: iterate notes: %w", err)
	}
	return out, nil
}

// =====================================================================
// Segments repository
// =====================================================================

// InsertSegment는 새 segment를 삽입한다 (sub-action 시작 시점).
// EndedAt은 호출 시점에 nil이어야 함 (종료는 EndSegment).
// Artifact가 nil이면 NULL 저장.
func (d *DB) InsertSegment(ctx context.Context, s Segment) error {
	var artifact sql.NullString
	if s.Artifact != nil {
		artifact = sql.NullString{String: string(s.Artifact), Valid: true}
	}
	_, err := d.ExecContext(ctx,
		`INSERT INTO segments (id, session_id, kind, started_at, ended_at, artifact)
		 VALUES (?, ?, ?, ?, NULL, ?)`,
		s.ID, s.SessionID, string(s.Kind), s.StartedAt.Unix(), artifact,
	)
	if err != nil {
		return fmt.Errorf("db: insert segment %q: %w", s.ID, err)
	}
	return nil
}

// EndSegment는 segment를 종료한다 (ended_at + 최종 artifact 갱신).
// idempotent — 이미 종료된 segment 재호출 시 noop.
func (d *DB) EndSegment(ctx context.Context, id string, endedAt time.Time, artifact []byte) error {
	var art sql.NullString
	if artifact != nil {
		art = sql.NullString{String: string(artifact), Valid: true}
	}
	_, err := d.ExecContext(ctx,
		`UPDATE segments SET ended_at = ?, artifact = COALESCE(?, artifact)
		 WHERE id = ? AND ended_at IS NULL`,
		endedAt.Unix(), art, id,
	)
	if err != nil {
		return fmt.Errorf("db: end segment %q: %w", id, err)
	}
	return nil
}

// ListSegmentsBySession은 세션의 모든 segment를 시간순 반환한다.
func (d *DB) ListSegmentsBySession(ctx context.Context, sessionID string) ([]Segment, error) {
	rows, err := d.QueryContext(ctx,
		`SELECT id, session_id, kind, started_at, ended_at, artifact
		 FROM segments WHERE session_id = ? ORDER BY started_at, id`,
		sessionID,
	)
	if err != nil {
		return nil, fmt.Errorf("db: list segments for %q: %w", sessionID, err)
	}
	defer rows.Close()

	var out []Segment
	for rows.Next() {
		var (
			s         Segment
			startedAt int64
			endedAt   sql.NullInt64
			artifact  sql.NullString
			kind      string
		)
		err := rows.Scan(&s.ID, &s.SessionID, &kind, &startedAt, &endedAt, &artifact)
		if err != nil {
			return nil, fmt.Errorf("db: scan segment: %w", err)
		}
		s.Kind = SegmentKind(kind)
		s.StartedAt = time.Unix(startedAt, 0)
		if endedAt.Valid {
			t := time.Unix(endedAt.Int64, 0)
			s.EndedAt = &t
		}
		if artifact.Valid {
			s.Artifact = []byte(artifact.String)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: iterate segments: %w", err)
	}
	return out, nil
}

