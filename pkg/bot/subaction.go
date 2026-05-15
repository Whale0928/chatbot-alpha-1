package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync/atomic"
	"time"

	"chatbot-alpha-1/pkg/db"
)

// =====================================================================
// Phase 3 chunk 3B-2a — SubAction lifecycle helper
//
// 거시 디자인 결정 1 (세션이 부모) 핵심 구현:
//   - sub-action(주간/릴리즈/에이전트)을 같은 스레드 안에서 실행
//   - 시작 시 DB segments row 생성 → 결과를 NoteSource=WeeklyDump/ReleaseResult/AgentOutput으로
//     같은 corpus에 누적 → 종료 시 segment row update + artifact persist
//
// 결과는 finalize의 ContextNotes로 들어가 LLM에 참고 자료로 제공되되,
// action.origin 후보에서는 제외 (환각 방어 — 거시 결정 6).
// =====================================================================

// segmentIDCounter는 nano 단위 ID 충돌 방지용 (newSummarizedContentID와 같은 패턴).
var segmentIDCounter atomic.Uint64

// newSegmentID는 segments.id 생성 — sg_<unix_nano>_<counter>.
func newSegmentID() string {
	return fmt.Sprintf("sg_%d_%d", time.Now().UnixNano(), segmentIDCounter.Add(1))
}

// SubActionContext는 단일 sub-action의 lifecycle 컨텍스트.
//
// 사용:
//
//	sa, err := BeginSubAction(ctx, sess, db.SegmentWeeklySummary)
//	if err != nil { ... }
//	defer sa.End(ctx, artifact)
//	... 실제 sub-action 실행 ...
//	sa.AppendResult(ctx, "[tool]", source, content)
type SubActionContext struct {
	SessionDBID string         // sess.DBSessionID 사본 (lifecycle 안전)
	ThreadID    string
	Kind        db.SegmentKind
	SegmentID   string         // BeginSubAction이 채움 (DB persist 성공 시)
	StartedAt   time.Time
}

// BeginSubAction은 sub-action 시작 시점에 호출되어 DB segments row를 insert한다.
//
// dbConn nil 또는 sess.DBSessionID 비어있으면 best-effort fallback — SegmentID 빈 채로 SubActionContext 반환.
// (sub-action은 in-memory만으로도 동작 가능, DB는 audit/검색 용도)
//
// 호출자는 반드시 End()를 defer로 호출해야 한다 (segment lifecycle 정리).
func BeginSubAction(ctx context.Context, sess *Session, kind db.SegmentKind) *SubActionContext {
	sa := &SubActionContext{
		SessionDBID: sess.DBSessionID,
		ThreadID:    sess.ThreadID,
		Kind:        kind,
		StartedAt:   time.Now(),
	}
	if dbConn == nil || sa.SessionDBID == "" {
		log.Printf("[subaction] DB persist 스킵 (best-effort) thread=%s kind=%s", sa.ThreadID, kind)
		return sa
	}
	id := newSegmentID()
	row := db.Segment{
		ID:        id,
		SessionID: sa.SessionDBID,
		Kind:      kind,
		StartedAt: sa.StartedAt,
	}
	if err := dbConn.InsertSegment(ctx, row); err != nil {
		log.Printf("[subaction] WARN segment insert 실패 thread=%s kind=%s: %v (in-memory only)",
			sa.ThreadID, kind, err)
		return sa
	}
	sa.SegmentID = id
	log.Printf("[subaction] BEGIN thread=%s segment=%s kind=%s", sa.ThreadID, id, kind)
	return sa
}

// AppendResult는 sub-action의 결과(도구 출력)를 sess.Notes에 누적하고 DB note insert한다.
//
// 인자:
//   - sess: 누적할 세션 (in-memory Notes에 추가)
//   - author: 결과의 표시 author (보통 "[tool]" 또는 sub-action 식별 라벨)
//   - source: NoteSource (WeeklyDump / ReleaseResult / AgentOutput 중 하나)
//   - content: 결과 내용 (markdown 또는 plain text)
//
// best-effort — DB persist 실패해도 in-memory에는 반드시 추가.
//
// 거시 결정 6 핵심: 이 결과 노트는 attribution 후보 X (Source.IsAttributionCandidate() = false).
// finalize에서 PrepareContentExtractionInput이 ContextNotes로 분류해 LLM에 참고 자료로만 전달.
//
// 정책 enforcement: source가 attribution candidate(Human)이면 SegmentID를 비우고 진행 —
// DB에서도 일반 Human note로 분류되도록 (segment_id NULL)해서 잘못된 분류 차단.
// 호출자가 실수로 Human source를 넘기는 케이스 방어.
func (sa *SubActionContext) AppendResult(ctx context.Context, sess *Session, author string, source db.NoteSource, content string) {
	segmentRef := sa.SegmentID
	if source.IsAttributionCandidate() {
		log.Printf("[subaction] WARN AppendResult에 Human source 전달됨 (정책 위반 — SegmentID 비움): kind=%s author=%s",
			sa.Kind, author)
		segmentRef = "" // 정책 위반 source는 segment에 결합시키지 않음 (DB 데이터 오염 방지)
	}
	note := Note{
		Author:    author,
		Content:   content,
		Source:    source,
		SegmentID: segmentRef,
		Timestamp: time.Now(),
	}
	_, stored := sess.AddNoteWithMeta(note)
	persistNote(ctx, sess, stored)
	log.Printf("[subaction] AppendResult thread=%s segment=%s source=%s author=%s bytes=%d",
		sa.ThreadID, segmentRef, source, author, len(content))
}

// End는 sub-action 종료 시점에 호출되어 DB segments row를 ended_at + artifact로 갱신한다.
// artifact는 sub-action의 메타데이터 (예: weekly의 경우 {repo, commits, since, until}).
// nil/빈 artifact도 OK — segment 종료 timestamp만 기록.
//
// 멱등 (db.EndSegment가 ended_at IS NULL 조건이라 두 번 호출 안전).
func (sa *SubActionContext) End(ctx context.Context, artifact []byte) {
	if dbConn == nil || sa.SegmentID == "" {
		return
	}
	if err := dbConn.EndSegment(ctx, sa.SegmentID, time.Now(), artifact); err != nil {
		log.Printf("[subaction] WARN End 실패 segment=%s: %v", sa.SegmentID, err)
		return
	}
	log.Printf("[subaction] END thread=%s segment=%s kind=%s elapsed=%s",
		sa.ThreadID, sa.SegmentID, sa.Kind, time.Since(sa.StartedAt).Round(time.Second))
}

// EndWithArtifact는 artifact를 JSON으로 marshal해 End()에 위임하는 편의 함수.
// marshal 실패 시 빈 artifact로 End 진행 (segment timestamp만이라도 기록).
func (sa *SubActionContext) EndWithArtifact(ctx context.Context, artifact any) {
	if artifact == nil {
		sa.End(ctx, nil)
		return
	}
	raw, err := json.Marshal(artifact)
	if err != nil {
		log.Printf("[subaction] WARN artifact marshal 실패 segment=%s: %v", sa.SegmentID, err)
		sa.End(ctx, nil)
		return
	}
	sa.End(ctx, raw)
}

// =====================================================================
// Phase 3 chunk 3D — 명시 [세션 종료] 핸들러
//
// finalize와 분리된 lifecycle 종료:
//   - finalize는 정리본 추출 도구 (chunk 3B의 [노트 정리]) — 세션 lifecycle 영향 X (Phase 3 모델)
//   - 세션 종료는 명시 button — DB close + sticky 제거 + in-memory 세션 정리 + 안내
//
// 거시 디자인 결정 11 (명시 종료 + idle timeout) 의 명시 종료 부분.
// =====================================================================

// HandleSessionEnd는 [세션 종료] button 클릭 시 호출되는 핵심 로직.
//
// 흐름 (race-safe 순서 — 사용자 다른 button 동시 클릭 차단):
//  1. sessions map에서 즉시 제거 — 이후 같은 스레드의 button 클릭은 lookupSession nil
//  2. sticky 메시지 제거 (Discord에 button 노출 X)
//  3. DB sessions row CLOSED 전환 (best-effort, idempotent)
//  4. 사용자에게 세션 종료 안내 (참석자 수, 노트 수, 경과 시간)
//
// 1을 가장 먼저 두는 이유 (race 방어):
//   Discord 이벤트 핸들러는 별도 goroutine — sticky/DB 정리 중 다른 button 클릭이 같은 sess
//   포인터에 진입하는 race가 가능. map 삭제를 먼저 해야 후속 클릭이 lookupSession에서 즉시 nil.
//   sess 포인터는 이 함수 로컬 변수가 보유 → 후속 단계 안전.
//
// 호출 후 같은 스레드에 사용자가 다시 발화하면 messageCreate 핸들러는 lookupSession에서 nil을
// 받아 봇 침묵. 새 작업 시작은 메인 채널에서 슬래시.
func HandleSessionEnd(ctx context.Context, msg Messenger, sess *Session) {
	if sess == nil {
		return
	}

	// 1. in-memory 정리 — 가장 먼저 (race 방어)
	sessionsMu.Lock()
	delete(sessions, sess.ThreadID)
	sessionsMu.Unlock()

	// 2. sticky 제거 (button 더 이상 누를 수 없게)
	if oldID := sess.ClearSticky(); oldID != "" {
		if err := msg.ChannelMessageDelete(sess.ThreadID, oldID); err != nil {
			log.Printf("[세션/end] WARN sticky 삭제 실패 thread=%s msg=%s: %v", sess.ThreadID, oldID, err)
		}
	}

	// 3. DB 영속화
	persistSessionClose(ctx, sess)

	// 4. 사용자 안내 (요약 메타 — 참석자/노트 수/경과 시간)
	// elapsed는 StartedAt(세션 생성) 기준 — UpdatedAt(마지막 활동)이 방금 클릭 시각으로 갱신되어
	// 거의 0초로 표시되는 버그 방지.
	speakers := sess.SortedHumanSpeakers()
	noteCount := len(sess.SnapshotNotes())
	startedAt := sess.StartedAt
	if startedAt.IsZero() {
		startedAt = sess.UpdatedAt // legacy 세션 fallback (Phase 3 이전 생성된 세션)
	}
	elapsed := time.Since(startedAt).Round(time.Second)
	guide := fmt.Sprintf(
		"`super-session 종료` · 참석자 %d명 · 메모 **%d건** · 경과 %s\n"+
			"이 스레드의 추가 발화는 더 이상 corpus에 누적되지 않습니다. "+
			"새 작업은 메인 채널의 슬래시 명령으로 시작해주세요.",
		len(speakers), noteCount, elapsed,
	)
	if _, err := msg.ChannelMessageSend(sess.ThreadID, guide); err != nil {
		log.Printf("[세션/end] WARN 안내 메시지 전송 실패 thread=%s: %v", sess.ThreadID, err)
	}

	log.Printf("[세션/end] thread=%s db_session=%s notes=%d speakers=%d elapsed=%s",
		sess.ThreadID, sess.DBSessionID, noteCount, len(speakers), elapsed)
}
