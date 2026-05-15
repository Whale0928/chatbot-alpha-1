package bot

import (
	"strings"
	"testing"

	"chatbot-alpha-1/pkg/db"
)

// strings import는 HandleSessionEnd 안내 검증용

func TestNewSegmentID_FormatAndUniqueness(t *testing.T) {
	a := newSegmentID()
	b := newSegmentID()
	if !strings.HasPrefix(a, "sg_") {
		t.Errorf("ID prefix = %q, want sg_", a[:3])
	}
	if a == b {
		t.Errorf("연속 호출 충돌: %q == %q (atomic counter 깨짐)", a, b)
	}
}

func TestBeginSubAction_BestEffortWhenDBUnavailable(t *testing.T) {
	// dbConn nil 상태(테스트 default) — SegmentID는 빈 채로 SubActionContext 반환,
	// in-memory 동작은 그대로 가능
	sess := &Session{ThreadID: "t1", DBSessionID: "sess_x"}
	sa := BeginSubAction(t.Context(), sess, db.SegmentWeeklySummary)

	if sa == nil {
		t.Fatal("BeginSubAction이 nil 반환")
	}
	if sa.SegmentID != "" {
		t.Errorf("dbConn nil 상태에서 SegmentID = %q, want empty (best-effort)", sa.SegmentID)
	}
	if sa.Kind != db.SegmentWeeklySummary {
		t.Errorf("Kind = %q, want %q", sa.Kind, db.SegmentWeeklySummary)
	}
	if sa.ThreadID != "t1" {
		t.Errorf("ThreadID = %q, want %q", sa.ThreadID, "t1")
	}
}

func TestBeginSubAction_NoOpWhenSessionDBIDEmpty(t *testing.T) {
	// DBSessionID 비어있으면 (DB persist 실패 후 fallback 상태) segment ID도 빈 채로
	sess := &Session{ThreadID: "t1"}
	sa := BeginSubAction(t.Context(), sess, db.SegmentRelease)
	if sa.SegmentID != "" {
		t.Errorf("SegmentID = %q, want empty", sa.SegmentID)
	}
}

func TestSubActionContext_AppendResult_AddsToInMemoryWithCorrectSource(t *testing.T) {
	sess := &Session{ThreadID: "t1"}
	sa := BeginSubAction(t.Context(), sess, db.SegmentWeeklySummary)

	sa.AppendResult(t.Context(), sess, "[tool]", db.SourceWeeklyDump, "주간 리포트 본문")

	if len(sess.Notes) != 1 {
		t.Fatalf("Notes count = %d, want 1", len(sess.Notes))
	}
	n := sess.Notes[0]
	if n.Author != "[tool]" {
		t.Errorf("Author = %q, want %q", n.Author, "[tool]")
	}
	if n.Source != db.SourceWeeklyDump {
		t.Errorf("Source = %q, want %q", n.Source, db.SourceWeeklyDump)
	}
	if n.Content != "주간 리포트 본문" {
		t.Errorf("Content mismatch: %q", n.Content)
	}
	if n.Timestamp.IsZero() {
		t.Error("Timestamp 미설정")
	}
}

func TestSubActionContext_AppendResult_RejectsHumanSourceWithWarning(t *testing.T) {
	// Human source는 정책 위반 — 그래도 in-memory에는 들어가야 함 (방어 코드는 log warn만).
	// 사용자 코드가 실수로 Human source를 sub-action 결과로 넘기는 케이스 안전성 보장.
	sess := &Session{ThreadID: "t1"}
	sa := BeginSubAction(t.Context(), sess, db.SegmentWeeklySummary)

	sa.AppendResult(t.Context(), sess, "alice", db.SourceHuman, "잘못된 source")

	if len(sess.Notes) != 1 {
		t.Errorf("Human source여도 in-memory 추가는 진행되어야 함, count=%d", len(sess.Notes))
	}
	if sess.Notes[0].Source != db.SourceHuman {
		t.Errorf("정책 위반 source가 변경됨 (정책 enforcement는 log warn만): %q", sess.Notes[0].Source)
	}
}

func TestSubActionContext_End_NoOpWithoutSegmentID(t *testing.T) {
	// SegmentID 빈 SubActionContext (DB persist 실패 케이스)에 End 호출 시 panic 없음
	sa := &SubActionContext{ThreadID: "t1", Kind: db.SegmentWeeklySummary}
	sa.End(t.Context(), []byte(`{"x":1}`))
	// no panic = pass
}

func TestSubActionContext_EndWithArtifact_HandlesNilAndUnmarshalable(t *testing.T) {
	sa := &SubActionContext{ThreadID: "t1", Kind: db.SegmentRelease}

	// nil artifact — 그냥 End(nil)로 위임
	sa.EndWithArtifact(t.Context(), nil)

	// marshalable artifact — End가 호출됨 (SegmentID 빈 상태라 noop지만 panic 없음)
	sa.EndWithArtifact(t.Context(), map[string]int{"commits": 37})

	// non-marshalable (chan 같은 type) — log warn 후 nil로 fallback
	ch := make(chan int)
	sa.EndWithArtifact(t.Context(), ch)
	// no panic = pass
}

// =====================================================================
// Phase 3 chunk 3D — HandleSessionEnd 테스트
// =====================================================================

func TestHandleSessionEnd_RemovesStickyAndSession(t *testing.T) {
	// given: in-memory 세션 + sticky 메시지 ID 세팅
	sess := &Session{
		ThreadID:        "thread_end",
		UserID:          "u1",
		StickyMessageID: "sticky_msg_xyz",
	}
	sess.AddNote("alice", "hi")

	sessionsMu.Lock()
	sessions[sess.ThreadID] = sess
	sessionsMu.Unlock()
	t.Cleanup(func() {
		sessionsMu.Lock()
		delete(sessions, sess.ThreadID)
		sessionsMu.Unlock()
	})

	msg := &fakeMessenger{}

	// when
	HandleSessionEnd(t.Context(), msg, sess)

	// then: sticky 삭제됨
	if len(msg.deleted) != 1 {
		t.Errorf("sticky 삭제 1회 기대, got %d", len(msg.deleted))
	} else if msg.deleted[0] != "thread_end:sticky_msg_xyz" {
		t.Errorf("삭제된 메시지 ID = %q, want %q", msg.deleted[0], "thread_end:sticky_msg_xyz")
	}

	// then: 안내 메시지 1건 전송 (참석자/노트 수/경과 포함)
	if len(msg.sent) != 1 {
		t.Fatalf("안내 메시지 1건 기대, got %d", len(msg.sent))
	}
	guide := msg.sent[0].Content
	mustContain := []string{"super-session 종료", "참석자", "메모", "**1건**", "더 이상 corpus에 누적되지 않습니다"}
	for _, sub := range mustContain {
		if !strings.Contains(guide, sub) {
			t.Errorf("안내 메시지에 %q 누락:\n%s", sub, guide)
		}
	}

	// then: in-memory sessions에서 제거됨
	sessionsMu.RLock()
	_, exists := sessions[sess.ThreadID]
	sessionsMu.RUnlock()
	if exists {
		t.Error("HandleSessionEnd 후 sessions map에서 제거 안 됨")
	}
}

func TestHandleSessionEnd_NilSessionNoOp(t *testing.T) {
	// nil session에 대해 panic 없이 noop
	msg := &fakeMessenger{}
	HandleSessionEnd(t.Context(), msg, nil)
	if len(msg.sent) != 0 || len(msg.deleted) != 0 {
		t.Errorf("nil session에 unexpected I/O — sent=%d deleted=%d", len(msg.sent), len(msg.deleted))
	}
}

func TestHandleSessionEnd_NoStickyNoOp(t *testing.T) {
	// StickyMessageID 비어있으면 ChannelMessageDelete 호출 안 함
	sess := &Session{ThreadID: "t_no_sticky"}
	sessionsMu.Lock()
	sessions[sess.ThreadID] = sess
	sessionsMu.Unlock()
	t.Cleanup(func() {
		sessionsMu.Lock()
		delete(sessions, sess.ThreadID)
		sessionsMu.Unlock()
	})

	msg := &fakeMessenger{}
	HandleSessionEnd(t.Context(), msg, sess)

	if len(msg.deleted) != 0 {
		t.Errorf("sticky 없는데 delete 호출됨: %v", msg.deleted)
	}
	if len(msg.sent) != 1 {
		t.Errorf("안내 메시지는 보내져야 함 (sticky 유무와 무관), got %d", len(msg.sent))
	}
}
