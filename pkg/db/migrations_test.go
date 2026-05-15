package db

import (
	"context"
	"path/filepath"
	"testing"
)

// TestMigrate_AppliesAllTables는 새 DB 파일에 Migrate 호출 시
// 5테이블 + schema_version 테이블이 모두 생성되는지 검증한다.
func TestMigrate_AppliesAllTables(t *testing.T) {
	// given
	path := filepath.Join(t.TempDir(), "test.db")
	d, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })

	// when
	if err := d.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	// then
	expected := []string{
		"schema_version",
		"sessions",
		"segments",
		"notes",
		"summarized_contents",
		"finalize_runs",
	}
	for _, name := range expected {
		var got string
		err := d.QueryRow(
			"SELECT name FROM sqlite_master WHERE type='table' AND name=?", name,
		).Scan(&got)
		if err != nil {
			t.Errorf("table %q not created: %v", name, err)
			continue
		}
		if got != name {
			t.Errorf("table name = %q, want %q", got, name)
		}
	}
}

// TestMigrate_IsIdempotent는 Migrate를 두 번 호출해도
// 에러 없고 schema_version에 중복 행이 생기지 않는지 검증한다.
func TestMigrate_IsIdempotent(t *testing.T) {
	// given
	path := filepath.Join(t.TempDir(), "test.db")
	d, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })

	ctx := context.Background()

	// when
	if err := d.Migrate(ctx); err != nil {
		t.Fatalf("first Migrate: %v", err)
	}
	if err := d.Migrate(ctx); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}

	// then
	var count int
	err = d.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM schema_version WHERE version = ?", currentSchemaVersion,
	).Scan(&count)
	if err != nil {
		t.Fatalf("count schema_version: %v", err)
	}
	if count != 1 {
		t.Errorf("schema_version row count = %d, want 1 (멱등 깨짐)", count)
	}
}

// TestMigrate_RecordsSchemaVersion은 Migrate 후 SchemaVersion()이
// currentSchemaVersion을 반환하는지 검증한다.
func TestMigrate_RecordsSchemaVersion(t *testing.T) {
	// given
	path := filepath.Join(t.TempDir(), "test.db")
	d, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })

	ctx := context.Background()

	// when: 마이그레이션 전
	v0, err := d.SchemaVersion(ctx)
	if err != nil {
		t.Fatalf("SchemaVersion before Migrate: %v", err)
	}

	// then: 0 (미적용 상태)
	if v0 != 0 {
		t.Errorf("SchemaVersion before Migrate = %d, want 0", v0)
	}

	// when: 마이그레이션 후
	if err := d.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	v1, err := d.SchemaVersion(ctx)
	if err != nil {
		t.Fatalf("SchemaVersion after Migrate: %v", err)
	}

	// then
	if v1 != currentSchemaVersion {
		t.Errorf("SchemaVersion after Migrate = %d, want %d", v1, currentSchemaVersion)
	}
}

// TestOpen_AppliesPragmas는 Open이 PRAGMA foreign_keys/journal_mode를
// 정상 적용하는지 검증한다 (테이블 외 정책의 ground truth).
func TestOpen_AppliesPragmas(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	d, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })

	var fk int
	if err := d.QueryRow("PRAGMA foreign_keys").Scan(&fk); err != nil {
		t.Fatalf("PRAGMA foreign_keys: %v", err)
	}
	if fk != 1 {
		t.Errorf("foreign_keys = %d, want 1 (ON)", fk)
	}

	var mode string
	if err := d.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatalf("PRAGMA journal_mode: %v", err)
	}
	if mode != "wal" {
		t.Errorf("journal_mode = %q, want %q", mode, "wal")
	}
}

// TestForeignKeyCascade는 sessions DELETE 시 notes/segments가
// CASCADE로 함께 정리되는지 검증한다 (PRAGMA foreign_keys=ON 동작 확인).
func TestForeignKeyCascade(t *testing.T) {
	// given
	path := filepath.Join(t.TempDir(), "test.db")
	d, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })

	ctx := context.Background()
	if err := d.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	_, err = d.ExecContext(ctx,
		`INSERT INTO sessions (id, thread_id, guild_id, owner_id, opened_at, status)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		"sess_1", "thread_1", "guild_1", "owner_1", 1700000000, "ACTIVE",
	)
	if err != nil {
		t.Fatalf("insert session: %v", err)
	}
	_, err = d.ExecContext(ctx,
		`INSERT INTO notes (id, session_id, author_id, author_name, content, source, timestamp)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"note_1", "sess_1", "u1", "alice", "hi", "Human", 1700000001,
	)
	if err != nil {
		t.Fatalf("insert note: %v", err)
	}

	// when: 세션 삭제
	if _, err := d.ExecContext(ctx, "DELETE FROM sessions WHERE id = ?", "sess_1"); err != nil {
		t.Fatalf("delete session: %v", err)
	}

	// then: note도 함께 사라져야 함
	var remaining int
	if err := d.QueryRowContext(ctx, "SELECT COUNT(*) FROM notes WHERE session_id = ?", "sess_1").Scan(&remaining); err != nil {
		t.Fatalf("count notes: %v", err)
	}
	if remaining != 0 {
		t.Errorf("CASCADE 동작 실패 — notes 남음: %d (foreign_keys OFF 가능성)", remaining)
	}
}
