// Package db는 chatbot-alpha-1의 영속화 레이어다.
// SQLite 단일 파일을 사용하며 modernc.org/sqlite (pure Go) 드라이버를 통해
// CGO_ENABLED=0 distroless 이미지에서도 동작하도록 한다.
//
// 책임 범위:
//   - SQLite 연결 + 안정 PRAGMA 적용 (WAL/foreign_keys/busy_timeout)
//   - 스키마 마이그레이션 (멱등 적용)
//
// 모델·repository 함수는 별도 파일로 분리 (sessions.go, notes.go 등 — Phase 1 진행 중 추가).
package db

import (
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

// driverName은 modernc.org/sqlite가 등록하는 driver name.
const driverName = "sqlite"

// DB는 *sql.DB wrapper. 향후 prepared statement 캐시 등 확장 여지를 둔다.
type DB struct {
	*sql.DB
	path string
}

// Open은 path의 SQLite 파일을 열고 안정 PRAGMA를 적용한다.
//
// path가 ":memory:"이면 in-memory DB (테스트용). 단 in-memory는 connection별 별도 DB가 되므로
// 호출자가 SetMaxOpenConns(1)로 강제하거나 file::memory:?cache=shared를 직접 지정해야 한다.
func Open(path string) (*DB, error) {
	sqldb, err := sql.Open(driverName, path)
	if err != nil {
		return nil, fmt.Errorf("db: sql.Open(%q): %w", path, err)
	}
	// SetMaxOpenConns(1) — 단일 connection 강제.
	//
	// 이유: PRAGMA(foreign_keys/journal_mode/busy_timeout)는 SQLite connection 단위로 적용된다.
	// database/sql의 기본 connection pool이 새 connection을 열면 PRAGMA가 미적용 상태로 시작되어
	// foreign key cascade가 침묵하게 깨질 수 있다 (TestForeignKeyCascade가 검증하는 정책).
	//
	// 운영 정합: Discord 봇은 replicas:1 + Recreate (deployment.yaml) — 동시 writer 1개가 보장된
	// 환경이므로 connection 1개로 충분하고, busy_timeout이 5초 재시도 안전망을 제공한다.
	sqldb.SetMaxOpenConns(1)

	if err := sqldb.Ping(); err != nil {
		_ = sqldb.Close()
		return nil, fmt.Errorf("db: ping(%q): %w", path, err)
	}
	// 안정 PRAGMA — 위 SetMaxOpenConns(1) 덕분에 이 한 번의 적용이 영구히 유효하다.
	// WAL: reader가 writer를 안 막음. 향후 read-only 분석 도구 확장 대비.
	// foreign_keys: REFERENCES + ON DELETE CASCADE 동작 보장.
	// busy_timeout: writer 락 충돌 시 5초까지 자동 재시도.
	pragmas := []string{
		"PRAGMA journal_mode = WAL",
		"PRAGMA foreign_keys = ON",
		"PRAGMA synchronous = NORMAL",
		"PRAGMA busy_timeout = 5000",
	}
	for _, p := range pragmas {
		if _, err := sqldb.Exec(p); err != nil {
			_ = sqldb.Close()
			return nil, fmt.Errorf("db: %s: %w", p, err)
		}
	}
	return &DB{DB: sqldb, path: path}, nil
}

// Path는 connection이 가리키는 파일 경로를 반환한다 (로그/디버그 용).
func (d *DB) Path() string { return d.path }

// nowUnix는 마이그레이션 timestamp 기록용.
// migration·repository 양쪽에서 공유되므로 패키지 헬퍼로 둔다.
func nowUnix() int64 { return time.Now().Unix() }
