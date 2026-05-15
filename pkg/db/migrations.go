package db

import (
	"context"
	"fmt"
)

// schemaSQL은 v1 스키마. 5테이블 + schema_version.
//
// CREATE TABLE/INDEX는 모두 IF NOT EXISTS — 단일 SQL 블록으로 멱등 적용 가능.
// 향후 v2 스키마 변경 시 schema_version 기반 분기 또는 별도 migration step 추가.
//
// 모든 timestamp는 INTEGER (Unix epoch sec) — sqlite의 INTEGER affinity가 가장 효율적.
// JSON 컬럼은 TEXT (sqlite3 JSON1 extension으로 쿼리 가능).
const schemaSQL = `
CREATE TABLE IF NOT EXISTS schema_version (
    version    INTEGER PRIMARY KEY,
    applied_at INTEGER NOT NULL
);

-- ===== sessions: super-session 컨테이너 =====
-- replicas:1 + Recreate 정책으로 동시 writer 1개 보장.
-- roles_snapshot: { userID: ["BACKEND", "FRONTEND"] } JSON. 세션 OPEN 시점 고정.
CREATE TABLE IF NOT EXISTS sessions (
    id              TEXT PRIMARY KEY,
    thread_id       TEXT NOT NULL UNIQUE,
    guild_id        TEXT NOT NULL,
    owner_id        TEXT NOT NULL,
    opened_at       INTEGER NOT NULL,
    closed_at       INTEGER,
    status          TEXT NOT NULL,
    roles_snapshot  TEXT NOT NULL DEFAULT '{}'
);
CREATE INDEX IF NOT EXISTS idx_sessions_status ON sessions(status);
CREATE INDEX IF NOT EXISTS idx_sessions_thread ON sessions(thread_id);

-- ===== segments: sub-action 1회분의 시간 구간 =====
-- kind = weekly_summary | release | agent
-- artifact = sub-action 결과물 JSON (예: 주간 리포트 metadata)
CREATE TABLE IF NOT EXISTS segments (
    id          TEXT PRIMARY KEY,
    session_id  TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    kind        TEXT NOT NULL,
    started_at  INTEGER NOT NULL,
    ended_at    INTEGER,
    artifact    TEXT
);
CREATE INDEX IF NOT EXISTS idx_segments_session ON segments(session_id, started_at);

-- ===== notes: 시간순 데이터 단위 =====
-- source: Human | WeeklyDump | ReleaseResult | AgentOutput | InterimSummary | ExternalPaste
-- segment_id: 자유 발화 시 NULL, sub-action 결과면 해당 segment id
-- author_roles: ["BE", "FE"] JSON, 발화 시점 snapshot
CREATE TABLE IF NOT EXISTS notes (
    id            TEXT PRIMARY KEY,
    session_id    TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    segment_id    TEXT REFERENCES segments(id) ON DELETE SET NULL,
    author_id     TEXT NOT NULL,
    author_name   TEXT NOT NULL,
    author_roles  TEXT NOT NULL DEFAULT '[]',
    content       TEXT NOT NULL,
    source        TEXT NOT NULL,
    timestamp     INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_notes_session ON notes(session_id, timestamp);
CREATE INDEX IF NOT EXISTS idx_notes_source ON notes(session_id, source);

-- ===== summarized_contents: 정리본 원천 (LLM 1회 추출 결과) =====
-- content: SummarizedContent struct 전체 JSON.
-- 한 세션에 N개 가능 (재추출 케이스), 최신만 active로 사용.
CREATE TABLE IF NOT EXISTS summarized_contents (
    id            TEXT PRIMARY KEY,
    session_id    TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    content       TEXT NOT NULL,
    extracted_at  INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_summarized_session ON summarized_contents(session_id, extracted_at);

-- ===== finalize_runs: 같은 SummarizedContent로 N회 포맷 변환 =====
-- format: decision_status | discussion | role_based | freeform
-- directive: freeform 자연어 추가 지시 (다른 포맷은 빈 문자열)
-- output_md: 렌더된 markdown
CREATE TABLE IF NOT EXISTS finalize_runs (
    id                       TEXT PRIMARY KEY,
    summarized_content_id    TEXT NOT NULL REFERENCES summarized_contents(id) ON DELETE CASCADE,
    format                   TEXT NOT NULL,
    directive                TEXT NOT NULL DEFAULT '',
    output_md                TEXT NOT NULL,
    created_at               INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_finalize_summarized ON finalize_runs(summarized_content_id);
`

// currentSchemaVersion은 schemaSQL이 정의하는 스키마 버전.
// 변경 시 v2 마이그레이션 step 추가 + Migrate에서 분기.
const currentSchemaVersion = 1

// Migrate는 멱등하게 스키마를 적용한다.
//
// 동작:
//  1. schemaSQL을 Exec — 모든 CREATE는 IF NOT EXISTS이므로 재실행 안전
//  2. schema_version 테이블에 currentSchemaVersion 행이 없으면 INSERT
//     (이미 있으면 noop — 마이그레이션 이력 중복 안 만듦)
//
// 호출 시점: 봇 부팅 시 db.Open 직후 1회.
func (d *DB) Migrate(ctx context.Context) error {
	if _, err := d.ExecContext(ctx, schemaSQL); err != nil {
		return fmt.Errorf("db: apply schema v%d: %w", currentSchemaVersion, err)
	}
	var existing int
	err := d.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM schema_version WHERE version = ?", currentSchemaVersion,
	).Scan(&existing)
	if err != nil {
		return fmt.Errorf("db: check schema_version: %w", err)
	}
	if existing == 0 {
		_, err = d.ExecContext(ctx,
			"INSERT INTO schema_version (version, applied_at) VALUES (?, ?)",
			currentSchemaVersion, nowUnix(),
		)
		if err != nil {
			return fmt.Errorf("db: record schema_version v%d: %w", currentSchemaVersion, err)
		}
	}
	return nil
}

// SchemaVersion은 현재 적용된 최신 스키마 버전을 반환한다.
// schema_version 테이블 자체가 없으면 0 반환 (마이그레이션 완전 미적용 상태) —
// Migrate 호출 전에 안전하게 호출 가능하도록 sqlite_master를 먼저 확인한다.
func (d *DB) SchemaVersion(ctx context.Context) (int, error) {
	var hasTable int
	err := d.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='schema_version'",
	).Scan(&hasTable)
	if err != nil {
		return 0, fmt.Errorf("db: check schema_version existence: %w", err)
	}
	if hasTable == 0 {
		return 0, nil
	}
	var v int
	err = d.QueryRowContext(ctx,
		"SELECT COALESCE(MAX(version), 0) FROM schema_version",
	).Scan(&v)
	if err != nil {
		return 0, fmt.Errorf("db: read schema_version: %w", err)
	}
	return v, nil
}
