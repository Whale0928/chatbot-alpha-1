package db

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestSummarizedContent_InsertAndGetLatest(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()
	seedSession(t, d, "sess_sc", "thread_sc")

	// given: 같은 세션에 정리본 2번 추출 (재추출 케이스)
	older := SummarizedContent{
		ID: "sc_1", SessionID: "sess_sc",
		Content:     []byte(`{"decisions":["v1"]}`),
		ExtractedAt: time.Unix(1700000100, 0),
	}
	newer := SummarizedContent{
		ID: "sc_2", SessionID: "sess_sc",
		Content:     []byte(`{"decisions":["v2"]}`),
		ExtractedAt: time.Unix(1700000300, 0),
	}
	if err := d.InsertSummarizedContent(ctx, older); err != nil {
		t.Fatalf("Insert older: %v", err)
	}
	if err := d.InsertSummarizedContent(ctx, newer); err != nil {
		t.Fatalf("Insert newer: %v", err)
	}

	// when
	got, err := d.GetLatestSummarizedContent(ctx, "sess_sc")
	if err != nil {
		t.Fatalf("GetLatestSummarizedContent: %v", err)
	}

	// then: 가장 최근(newer) 반환
	if got.ID != newer.ID {
		t.Errorf("got id = %q, want %q (latest)", got.ID, newer.ID)
	}
	if string(got.Content) != string(newer.Content) {
		t.Errorf("got content = %s, want %s", got.Content, newer.Content)
	}
}

func TestSummarizedContent_GetLatestNotFound(t *testing.T) {
	d := newTestDB(t)
	seedSession(t, d, "sess_empty", "thread_empty")
	_, err := d.GetLatestSummarizedContent(context.Background(), "sess_empty")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestFinalizeRun_GetLatestPerFormat(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()
	seedSession(t, d, "sess_fr", "thread_fr")

	// given: 정리본 1개 + 같은 정리본에 4 포맷 + role_based만 2회 (포맷 토글 재호출)
	sc := SummarizedContent{
		ID: "sc_fr", SessionID: "sess_fr",
		Content:     []byte(`{}`),
		ExtractedAt: time.Unix(1700000100, 0),
	}
	if err := d.InsertSummarizedContent(ctx, sc); err != nil {
		t.Fatalf("Insert sc: %v", err)
	}

	runs := []FinalizeRun{
		{ID: "fr_ds", SummarizedContentID: "sc_fr", Format: FormatDecisionStatus,
			OutputMD: "## 결정\n", CreatedAt: time.Unix(1700000200, 0)},
		{ID: "fr_dc", SummarizedContentID: "sc_fr", Format: FormatDiscussion,
			OutputMD: "## 논의\n", CreatedAt: time.Unix(1700000210, 0)},
		{ID: "fr_rb1", SummarizedContentID: "sc_fr", Format: FormatRoleBased,
			OutputMD: "## 역할별 v1\n", CreatedAt: time.Unix(1700000220, 0)},
		{ID: "fr_ff", SummarizedContentID: "sc_fr", Format: FormatFreeform, Directive: "한 줄로",
			OutputMD: "## 자율\n", CreatedAt: time.Unix(1700000230, 0)},
		{ID: "fr_rb2", SummarizedContentID: "sc_fr", Format: FormatRoleBased,
			OutputMD: "## 역할별 v2\n", CreatedAt: time.Unix(1700000300, 0)},
	}
	for _, r := range runs {
		if err := d.InsertFinalizeRun(ctx, r); err != nil {
			t.Fatalf("InsertFinalizeRun %q: %v", r.ID, err)
		}
	}

	// when: role_based 최신 조회
	got, err := d.GetLatestFinalizeRun(ctx, "sc_fr", FormatRoleBased)
	if err != nil {
		t.Fatalf("GetLatestFinalizeRun: %v", err)
	}

	// then
	if got.ID != "fr_rb2" {
		t.Errorf("latest role_based id = %q, want %q", got.ID, "fr_rb2")
	}

	// when: 전체 이력
	all, err := d.ListFinalizeRunsByContent(ctx, "sc_fr")
	if err != nil {
		t.Fatalf("ListFinalizeRunsByContent: %v", err)
	}
	if len(all) != len(runs) {
		t.Errorf("history count = %d, want %d", len(all), len(runs))
	}
}

func TestFinalizeRun_DirectivePreservedForFreeform(t *testing.T) {
	d := newTestDB(t)
	ctx := context.Background()
	seedSession(t, d, "sess_dr", "thread_dr")
	_ = d.InsertSummarizedContent(ctx, SummarizedContent{
		ID: "sc_dr", SessionID: "sess_dr", Content: []byte(`{}`),
		ExtractedAt: time.Unix(1700000100, 0),
	})

	directive := "프론트/백엔드/기획 H3 섹션으로 묶어줘"
	if err := d.InsertFinalizeRun(ctx, FinalizeRun{
		ID: "fr_dr", SummarizedContentID: "sc_dr",
		Format: FormatFreeform, Directive: directive,
		OutputMD: "## section\n", CreatedAt: time.Unix(1700000200, 0),
	}); err != nil {
		t.Fatalf("InsertFinalizeRun: %v", err)
	}

	got, err := d.GetLatestFinalizeRun(ctx, "sc_dr", FormatFreeform)
	if err != nil {
		t.Fatalf("GetLatestFinalizeRun: %v", err)
	}
	if got.Directive != directive {
		t.Errorf("Directive = %q, want %q", got.Directive, directive)
	}
}
