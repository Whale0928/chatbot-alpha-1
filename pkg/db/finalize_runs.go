package db

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// GetFinalizeRunByFormat는 (summarized_content_id, format) 조합으로 finalize_run 조회.
// 같은 SC + 같은 포맷 재토글 시 LLM 호출 skip하도록 cache로 사용.
//
// 반환:
//   - 행 있음 -> *FinalizeRun (rendered markdown은 OutputMD에)
//   - 행 없음 -> nil, sql.ErrNoRows
//   - 그 외 에러 -> nil, err
func (d *DB) GetFinalizeRunByFormat(ctx context.Context, summarizedContentID string, format FormatKind) (*FinalizeRun, error) {
	var (
		r         FinalizeRun
		createdAt int64
	)
	err := d.QueryRowContext(ctx,
		`SELECT id, summarized_content_id, format, directive, output_md, created_at
		 FROM finalize_runs
		 WHERE summarized_content_id = ? AND format = ?
		 ORDER BY created_at DESC
		 LIMIT 1`,
		summarizedContentID, string(format),
	).Scan(&r.ID, &r.SummarizedContentID, (*string)(&r.Format), &r.Directive, &r.OutputMD, &createdAt)
	if err == sql.ErrNoRows {
		return nil, sql.ErrNoRows
	}
	if err != nil {
		return nil, fmt.Errorf("db: get finalize_run by format %s/%s: %w", summarizedContentID, format, err)
	}
	r.CreatedAt = time.Unix(createdAt, 0)
	return &r, nil
}
