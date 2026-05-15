package bot

import (
	"strings"
	"testing"
	"time"

	"chatbot-alpha-1/pkg/db"
)

func TestClassifyMessageSource(t *testing.T) {
	tests := []struct {
		name string
		body string
		want db.NoteSource
	}{
		{"empty", "", db.SourceHuman},
		{"short normal", "큐레이션 화면은 제작 후 현기님께 전달완료", db.SourceHuman},
		{"medium normal", strings.Repeat("가", 100), db.SourceHuman},
		// 임계 1500자 (2026-05-15 운영 피드백 반영 — 본인 메모 1000자 범위 자동 분류 회귀 차단)
		{"just below threshold (1499 runes)", strings.Repeat("가", 1499), db.SourceHuman},
		{"at threshold (1500 runes) → ExternalPaste", strings.Repeat("가", 1500), db.SourceExternalPaste},
		{"본인 노션 메모 1300자 (Human 유지)", strings.Repeat("가", 1300), db.SourceHuman},
		{"long paste (3000 runes)", strings.Repeat("가", 3000), db.SourceExternalPaste},
		{"latin chars at threshold", strings.Repeat("a", 1500), db.SourceExternalPaste},
		{"mixed multi-byte just under (750 한 + 749 라)", strings.Repeat("가", 750) + strings.Repeat("a", 749), db.SourceHuman},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyMessageSource(tc.body)
			if got != tc.want {
				t.Errorf("got %q, want %q (rune len = %d)", got, tc.want, len([]rune(tc.body)))
			}
		})
	}
}

func TestAddNoteWithMeta_NormalizesAndReturnsCopy(t *testing.T) {
	sess := &Session{}

	// when: Timestamp/Source 미설정 + meta 채워진 노트 추가
	idx, stored := sess.AddNoteWithMeta(Note{
		Author:      "alice",
		Content:     "test",
		AuthorID:    "u_alice",
		AuthorRoles: []string{"BACKEND"},
		// Source / Timestamp 의도적으로 zero
	})

	// then: idx == 1
	if idx != 1 {
		t.Errorf("idx = %d, want 1", idx)
	}
	// then: Source default Human으로 정규화
	if stored.Source != db.SourceHuman {
		t.Errorf("stored.Source = %q, want %q", stored.Source, db.SourceHuman)
	}
	// then: Timestamp 채워짐
	if stored.Timestamp.IsZero() {
		t.Error("stored.Timestamp이 zero — 정규화 누락")
	}
	// then: meta 보존
	if stored.AuthorID != "u_alice" {
		t.Errorf("AuthorID = %q, want %q", stored.AuthorID, "u_alice")
	}
	if len(stored.AuthorRoles) != 1 || stored.AuthorRoles[0] != "BACKEND" {
		t.Errorf("AuthorRoles = %v, want [BACKEND]", stored.AuthorRoles)
	}
	// then: in-memory에도 같은 내용으로 추가됨
	if len(sess.Notes) != 1 {
		t.Fatalf("Notes count = %d, want 1", len(sess.Notes))
	}
	if sess.Notes[0].Source != db.SourceHuman || sess.Notes[0].Author != "alice" {
		t.Errorf("Notes[0] mismatch: %+v", sess.Notes[0])
	}
	// then: Speakers 등록
	if !sess.Speakers["alice"] {
		t.Error("Speakers에 alice 미등록")
	}
}

func TestAddNoteWithMeta_PreservesExplicitTimestampAndSource(t *testing.T) {
	sess := &Session{}
	ts := time.Date(2026, 5, 14, 19, 58, 14, 0, time.UTC)

	_, stored := sess.AddNoteWithMeta(Note{
		Author:    "[tool]",
		Content:   "weekly dump",
		Source:    db.SourceWeeklyDump,
		Timestamp: ts,
		SegmentID: "seg_w1",
	})

	if !stored.Timestamp.Equal(ts) {
		t.Errorf("Timestamp = %v, want %v (덮어씀)", stored.Timestamp, ts)
	}
	if stored.Source != db.SourceWeeklyDump {
		t.Errorf("Source = %q, want %q (default 덮어씀)", stored.Source, db.SourceWeeklyDump)
	}
	if stored.SegmentID != "seg_w1" {
		t.Errorf("SegmentID = %q, want %q", stored.SegmentID, "seg_w1")
	}
}

func TestAddNote_BackwardsCompatible(t *testing.T) {
	sess := &Session{}

	// 기존 호출 시그니처 — handler.go의 다른 호출자가 깨지지 않는지 회귀 검증
	idx := sess.AddNote("bob", "old style call")

	if idx != 1 {
		t.Errorf("idx = %d, want 1", idx)
	}
	if len(sess.Notes) != 1 {
		t.Fatalf("Notes count = %d, want 1", len(sess.Notes))
	}
	n := sess.Notes[0]
	if n.Author != "bob" || n.Content != "old style call" {
		t.Errorf("note mismatch: %+v", n)
	}
	if n.Source != db.SourceHuman {
		t.Errorf("Source = %q, want %q (default Human)", n.Source, db.SourceHuman)
	}
	if n.Timestamp.IsZero() {
		t.Error("Timestamp zero — 정규화 누락")
	}
}

// =====================================================================
// 핵심 환각 시나리오 회귀 테스트 — 거시 디자인 결정 6 ("Source 라벨로 환각 방어") 검증
//
// 5/14 미팅의 실제 케이스: deadwhale(BE)이 git-bot weekly dump를 paste했고,
// 이전 봇은 그 내용을 deadwhale 본인 액션으로 잘못 attribute했다.
// 새 모델에서는 SortedHumanSpeakers + SnapshotNotesForCorpus가 LLM 입력 단계에서 게이트해
// 같은 환각이 재발하지 않아야 한다.
// =====================================================================

// TestSortedHumanSpeakers_ExcludesToolOnlyAuthors — 도구 출력만 한 author는 attribution 후보 X.
func TestSortedHumanSpeakers_ExcludesToolOnlyAuthors(t *testing.T) {
	sess := &Session{}
	// deadwhale은 사람 발화 + 외부 paste 둘 다
	sess.AddNoteWithMeta(Note{Author: "deadwhale", Content: "큐레이션 order spec", Source: db.SourceHuman})
	sess.AddNoteWithMeta(Note{Author: "deadwhale", Content: "[큰 paste]", Source: db.SourceExternalPaste})
	// [tool]은 weekly dump만
	sess.AddNoteWithMeta(Note{Author: "[tool]", Content: "주간 리포트 ...", Source: db.SourceWeeklyDump})
	// kimjuye는 사람 발화만
	sess.AddNoteWithMeta(Note{Author: "kimjuye", Content: "차주 미팅까지 체크 요청", Source: db.SourceHuman})

	got := sess.SortedHumanSpeakers()

	// then: deadwhale + kimjuye만 (Human 발화가 1번이라도 있으면 포함). [tool]은 제외
	want := []string{"deadwhale", "kimjuye"}
	if len(got) != len(want) {
		t.Fatalf("got %v (count %d), want %v (count %d) — [tool]이 attribution 후보로 잡힘 = 환각 방어 깨짐",
			got, len(got), want, len(want))
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("position %d: got %q, want %q", i, got[i], w)
		}
	}
}

// TestSnapshotNotesForCorpus_ExcludesInterimSummary — 자기 재흡수 방지.
func TestSnapshotNotesForCorpus_ExcludesInterimSummary(t *testing.T) {
	sess := &Session{}
	sess.AddNoteWithMeta(Note{Author: "alice", Content: "human msg", Source: db.SourceHuman})
	sess.AddNoteWithMeta(Note{Author: "[bot]", Content: "이전 interim 요약", Source: db.SourceInterimSummary})
	sess.AddNoteWithMeta(Note{Author: "[tool]", Content: "weekly dump", Source: db.SourceWeeklyDump})

	got := sess.SnapshotNotesForCorpus()

	if len(got) != 2 {
		t.Fatalf("corpus count = %d, want 2 (InterimSummary 제외)", len(got))
	}
	for _, n := range got {
		if n.Source == db.SourceInterimSummary {
			t.Errorf("InterimSummary가 corpus에 포함됨 (자기 재흡수 위험): %+v", n)
		}
	}
}

// TestHallucinationGate_5_14_Scenario — 5/14 실제 케이스 재현.
//
// 시나리오:
//   - kimjuye(PM), deadwhale(BE), hyejungpark(FE) 사람 발화 누적
//   - deadwhale이 git-bot weekly dump를 paste (자동 분류로 Source=ExternalPaste 또는 [tool] author로 WeeklyDump)
//   - 봇이 중간 요약 생성 (Source=InterimSummary)
//
// 검증:
//   - corpus = Human + WeeklyDump + ExternalPaste (InterimSummary 제외)
//   - speakers = Human 발화한 사람만 (3명, [tool] 제외)
func TestHallucinationGate_5_14_Scenario(t *testing.T) {
	sess := &Session{}

	// === 누적 ===
	sess.AddNoteWithMeta(Note{Author: "kimjuye", Source: db.SourceHuman, Content: "workspace 이슈 통합"})
	sess.AddNoteWithMeta(Note{Author: "kimjuye", Source: db.SourceHuman, Content: "위스키 캐스크 90%"})
	sess.AddNoteWithMeta(Note{Author: "deadwhale", Source: db.SourceHuman, Content: "큐레이션 order 확장"})
	// 봇 도구 출력 (예: weekly dump를 사용자 [주간 추가] 클릭으로 받았다고 가정)
	sess.AddNoteWithMeta(Note{Author: "[tool]", Source: db.SourceWeeklyDump, Content: "주간 리포트 ..."})
	// 사용자가 외부 회의록 등을 직접 paste (500자 초과 → ExternalPaste 자동 분류)
	sess.AddNoteWithMeta(Note{Author: "hyejungpark", Source: db.SourceExternalPaste, Content: "[큰 paste]"})
	// 그 후 사람 발화도 함
	sess.AddNoteWithMeta(Note{Author: "hyejungpark", Source: db.SourceHuman, Content: "프론트 배포 완료"})
	sess.AddNoteWithMeta(Note{Author: "kimjuye", Source: db.SourceHuman, Content: "FE에 이슈 206 체크 요청"})
	// 봇이 중간 요약
	sess.AddNoteWithMeta(Note{Author: "[bot]", Source: db.SourceInterimSummary, Content: "[중간 요약 결과]"})

	// === 검증 ===
	corpus := sess.SnapshotNotesForCorpus()
	if len(corpus) != 7 {
		t.Errorf("corpus count = %d, want 7 (InterimSummary 1개만 제외)", len(corpus))
	}
	for _, n := range corpus {
		if n.Source == db.SourceInterimSummary {
			t.Errorf("corpus에 InterimSummary 잔존: %+v", n)
		}
	}

	speakers := sess.SortedHumanSpeakers()
	want := []string{"deadwhale", "hyejungpark", "kimjuye"}
	if len(speakers) != len(want) {
		t.Fatalf("speakers = %v, want %v — [tool]이나 [bot]이 attribution 후보로 잡힘 = 환각 방어 깨짐",
			speakers, want)
	}
	for i, w := range want {
		if speakers[i] != w {
			t.Errorf("position %d: %q, want %q", i, speakers[i], w)
		}
	}
}
