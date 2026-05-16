package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// =====================================================================
// SummarizedContent repository — 정리본 원천 (LLM 1회 추출 결과)
// =====================================================================

// InsertSummarizedContent는 새 정리본을 저장한다.
// 한 세션에 N개 가능 (재추출 케이스), 최신 1개를 active로 사용.
func (d *DB) InsertSummarizedContent(ctx context.Context, c SummarizedContent) error {
	_, err := d.ExecContext(ctx,
		`INSERT INTO summarized_contents (id, session_id, content, extracted_at)
		 VALUES (?, ?, ?, ?)`,
		c.ID, c.SessionID, string(c.Content), c.ExtractedAt.Unix(),
	)
	if err != nil {
		return fmt.Errorf("db: insert summarized_content %q: %w", c.ID, err)
	}
	return nil
}

// GetLatestSummarizedContent는 세션의 가장 최근 정리본을 반환한다.
// 정리본 토글에서 LLM 재호출 없이 이 행을 재사용한다 — 거시 디자인 원칙 03.
func (d *DB) GetLatestSummarizedContent(ctx context.Context, sessionID string) (SummarizedContent, error) {
	var (
		c           SummarizedContent
		content     string
		extractedAt int64
	)
	err := d.QueryRowContext(ctx,
		`SELECT id, session_id, content, extracted_at
		 FROM summarized_contents
		 WHERE session_id = ?
		 ORDER BY extracted_at DESC, id DESC
		 LIMIT 1`,
		sessionID,
	).Scan(&c.ID, &c.SessionID, &content, &extractedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return SummarizedContent{}, ErrNotFound
	}
	if err != nil {
		return SummarizedContent{}, fmt.Errorf("db: get latest summarized for %q: %w", sessionID, err)
	}
	c.Content = []byte(content)
	c.ExtractedAt = time.Unix(extractedAt, 0)
	return c, nil
}

// GetSummarizedContentByID는 정리본 ID로 정확한 행을 조회한다.
// HandleFormatCopy가 button customID에 박힌 sc_id로 옛 정리본 메시지의
// 정확한 markdown을 가져올 때 사용 — GetLatest는 다중 정리본 세션에서
// 옛 메시지를 클릭해도 최신 sc를 반환하는 회귀 차단용.
func (d *DB) GetSummarizedContentByID(ctx context.Context, id string) (SummarizedContent, error) {
	var (
		c           SummarizedContent
		content     string
		extractedAt int64
	)
	err := d.QueryRowContext(ctx,
		`SELECT id, session_id, content, extracted_at
		 FROM summarized_contents
		 WHERE id = ?`,
		id,
	).Scan(&c.ID, &c.SessionID, &content, &extractedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return SummarizedContent{}, ErrNotFound
	}
	if err != nil {
		return SummarizedContent{}, fmt.Errorf("db: get summarized by id %q: %w", id, err)
	}
	c.Content = []byte(content)
	c.ExtractedAt = time.Unix(extractedAt, 0)
	return c, nil
}

// =====================================================================
// FinalizeRun repository — 같은 SummarizedContent의 N회 포맷 변환 이력
// =====================================================================

// InsertFinalizeRun은 새 finalize 결과(렌더된 markdown)를 저장한다.
// 같은 (summarized_content_id, format) 조합도 재호출 시 새 행 — 이력 추적용.
func (d *DB) InsertFinalizeRun(ctx context.Context, r FinalizeRun) error {
	_, err := d.ExecContext(ctx,
		`INSERT INTO finalize_runs
		   (id, summarized_content_id, format, directive, output_md, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		r.ID, r.SummarizedContentID, string(r.Format), r.Directive, r.OutputMD, r.CreatedAt.Unix(),
	)
	if err != nil {
		return fmt.Errorf("db: insert finalize_run %q: %w", r.ID, err)
	}
	return nil
}

// GetLatestFinalizeRun은 (summarized_content_id, format) 조합의 가장 최근 결과를 반환한다.
// 같은 포맷 토글 시 캐시 히트 가능 (LLM 재호출 X — freeform은 directive별 다르므로 예외).
func (d *DB) GetLatestFinalizeRun(ctx context.Context, summarizedID string, format FormatKind) (FinalizeRun, error) {
	var (
		r         FinalizeRun
		createdAt int64
	)
	err := d.QueryRowContext(ctx,
		`SELECT id, summarized_content_id, format, directive, output_md, created_at
		 FROM finalize_runs
		 WHERE summarized_content_id = ? AND format = ?
		 ORDER BY created_at DESC, id DESC
		 LIMIT 1`,
		summarizedID, string(format),
	).Scan(&r.ID, &r.SummarizedContentID, (*string)(&r.Format), &r.Directive, &r.OutputMD, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return FinalizeRun{}, ErrNotFound
	}
	if err != nil {
		return FinalizeRun{}, fmt.Errorf("db: get latest finalize_run %s/%s: %w", summarizedID, format, err)
	}
	r.CreatedAt = time.Unix(createdAt, 0)
	return r, nil
}

// ListFinalizeRunsByContent는 한 SummarizedContent에서 만들어진 모든 포맷 이력을 반환한다.
// 운영 audit/디버깅 용.
func (d *DB) ListFinalizeRunsByContent(ctx context.Context, summarizedID string) ([]FinalizeRun, error) {
	rows, err := d.QueryContext(ctx,
		`SELECT id, summarized_content_id, format, directive, output_md, created_at
		 FROM finalize_runs
		 WHERE summarized_content_id = ?
		 ORDER BY created_at, id`,
		summarizedID,
	)
	if err != nil {
		return nil, fmt.Errorf("db: list finalize_runs for %q: %w", summarizedID, err)
	}
	defer rows.Close()

	var out []FinalizeRun
	for rows.Next() {
		var (
			r         FinalizeRun
			createdAt int64
		)
		err := rows.Scan(&r.ID, &r.SummarizedContentID, (*string)(&r.Format), &r.Directive, &r.OutputMD, &createdAt)
		if err != nil {
			return nil, fmt.Errorf("db: scan finalize_run: %w", err)
		}
		r.CreatedAt = time.Unix(createdAt, 0)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: iterate finalize_runs: %w", err)
	}
	return out, nil
}
