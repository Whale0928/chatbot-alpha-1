package bot

import (
	"context"
	"fmt"
	"log"
	"sync/atomic"
	"time"

	"chatbot-alpha-1/pkg/db"

	"github.com/bwmarrin/discordgo"
)

// =====================================================================
// 영속화 헬퍼 — Phase 1 통합 레이어
//
// 정책:
//   - DB write는 best-effort. 실패는 log warn — 봇 동작은 in-memory로 계속.
//   - dbConn nil 가능 (테스트 환경 / DB 부팅 실패 후 fallback). 모든 함수가 nil-check.
//   - DB read는 봇 재시작 시 active session 복원에 사용 (Phase 1 후반에 추가).
// =====================================================================

// externalPasteThreshold는 외부 paste 자동 분류 임계 — 거시 디자인 결정 F (a) 500자.
//
// 이 글자 수 이상의 발화는 자동으로 db.SourceExternalPaste로 분류 → action attribution
// 후보에서 제외된다. 명시적 [외부 자료 첨부] 버튼은 별도 UI로 추가될 예정 (Phase 3).
const externalPasteThreshold = 500

// classifyMessageSource는 발화 길이로 NoteSource를 자동 결정한다 (pure).
//
// 룰:
//   - rune 길이 >= externalPasteThreshold → ExternalPaste
//   - 그 외 → Human
//
// 사용자가 명시적으로 외부 자료 버튼을 누른 케이스는 호출자가 직접 db.SourceExternalPaste를
// 사용한다 (이 함수는 자동 분류만 담당).
func classifyMessageSource(content string) db.NoteSource {
	if len([]rune(content)) >= externalPasteThreshold {
		return db.SourceExternalPaste
	}
	return db.SourceHuman
}

// idCounterPersist는 nano 충돌 방지용 atomic counter.
// summarized.go의 idCounter와 같은 패턴 — 같은 ns에 두 호출이 들어와도 counter suffix로 unique.
//
// 별도 atomic 변수를 두는 이유: summarized.go의 idCounter와 import 순환 회피.
// 단일 봇 인스턴스 가정 (replicas:1) — 다중 인스턴스 도입 시 instance prefix 추가.
var idCounterPersist atomic.Uint64

// newSessionID는 DB persist용 세션 ID를 생성한다.
// 형식: sess_<unix_nano>_<counter> — 같은 ns 호출도 unique.
func newSessionID() string {
	return fmt.Sprintf("sess_%d_%d", time.Now().UnixNano(), idCounterPersist.Add(1))
}

// newNoteID는 DB persist용 note ID 생성.
// 형식: note_<unix_nano>_<counter>.
//
// 매 Discord 메시지마다 호출되어 nano 단독 ID로는 messageCreate burst나 동시 goroutine
// (role-fetch path 등) 환경에서 PRIMARY KEY 충돌 위험. atomic counter로 차단.
func newNoteID() string {
	return fmt.Sprintf("note_%d_%d", time.Now().UnixNano(), idCounterPersist.Add(1))
}

// persistSessionStart는 새 세션을 DB에 기록한다 (best-effort).
// sess.DBSessionID를 채워서 후속 note insert가 참조 가능하게 한다.
//
// dbConn 미설정 / insert 실패 시 sess.DBSessionID는 "" 유지 (in-memory only).
// 호출자(openThread/startSlashSession)는 결과를 무시하고 Discord UX 계속 진행.
func persistSessionStart(ctx context.Context, sess *Session) {
	if dbConn == nil || sess == nil {
		return
	}
	id := newSessionID()
	rolesJSON, err := MarshalRoleSnapshot(sess.RolesSnapshot)
	if err != nil {
		log.Printf("[db] WARN session role snapshot marshal 실패 thread=%s: %v", sess.ThreadID, err)
		rolesJSON = []byte("{}")
	}
	row := db.Session{
		ID:            id,
		ThreadID:      sess.ThreadID,
		GuildID:       sess.GuildID,
		OwnerID:       sess.UserID,
		OpenedAt:      sess.UpdatedAt,
		Status:        db.SessionActive,
		RolesSnapshot: rolesJSON,
	}
	if err := dbConn.InsertSession(ctx, row); err != nil {
		log.Printf("[db] WARN session insert 실패 thread=%s: %v (in-memory only)", sess.ThreadID, err)
		return
	}
	sess.DBSessionID = id
	log.Printf("[db] 세션 영속화 thread=%s id=%s guild=%s owner=%s", sess.ThreadID, id, sess.GuildID, sess.UserID)
}

// persistNote는 누적된 노트 1건을 DB에 기록한다 (best-effort).
// sess.DBSessionID가 비면 noop (DB 부팅 실패 등으로 영속화 disabled 상태).
// 노트 삽입 실패는 log warn — in-memory에는 이미 들어 있으므로 finalize는 정상 동작.
func persistNote(ctx context.Context, sess *Session, n Note) {
	if dbConn == nil || sess == nil || sess.DBSessionID == "" {
		return
	}
	rolesJSON, err := MarshalAuthorRoles(n.AuthorRoles)
	if err != nil {
		log.Printf("[db] WARN author_roles marshal 실패 author=%s: %v", n.Author, err)
		rolesJSON = []byte("[]")
	}
	var segmentID *string
	if n.SegmentID != "" {
		s := n.SegmentID
		segmentID = &s
	}
	row := db.Note{
		ID:          newNoteID(),
		SessionID:   sess.DBSessionID,
		SegmentID:   segmentID,
		AuthorID:    n.AuthorID,
		AuthorName:  n.Author,
		AuthorRoles: rolesJSON,
		Content:     n.Content,
		Source:      n.Source,
		Timestamp:   n.Timestamp,
	}
	if err := dbConn.InsertNote(ctx, row); err != nil {
		log.Printf("[db] WARN note insert 실패 thread=%s author=%s: %v", sess.ThreadID, n.Author, err)
	}
}

// persistSessionClose는 세션을 CLOSED로 전환한다 (best-effort, idempotent).
func persistSessionClose(ctx context.Context, sess *Session) {
	if dbConn == nil || sess == nil || sess.DBSessionID == "" {
		return
	}
	if err := dbConn.CloseSession(ctx, sess.DBSessionID, time.Now()); err != nil {
		log.Printf("[db] WARN session close 실패 thread=%s id=%s: %v", sess.ThreadID, sess.DBSessionID, err)
	}
}

// =====================================================================
// Role 조회 — 발화 시점 lazy fetch + snapshot 캐시
// =====================================================================

// GetOrFetchRoles는 사용자의 role을 RolesSnapshot에서 먼저 lookup하고, 캐시 미스 시
// Discord API로 fetch한 뒤 snapshot에 추가한다 (NotesMu 보호).
//
// 동시성 정책:
//   - 캐시 hit는 lock 안에서 즉시 반환 (외부 API 호출 없음)
//   - 캐시 miss는 lock 풀고 fetch (외부 API는 lock 밖) → 결과를 다시 lock 잡고 등록
//   - 두 goroutine이 같은 미스 userID에 대해 동시 진입하면 둘 다 fetch 후 후행 호출이 덮어씀
//     (단일 봇 인스턴스 + Discord messageCreate는 사실상 직렬이라 실제 발생률 낮음).
//     "API 1회만"이 강한 요구가 되면 singleflight 도입 검토 — 현재는 캐시 hit 스킵만 보장.
//   - fetch 실패도 빈 slice로 캐시 — 같은 세션에서 같은 userID에 대해 매 메시지마다 재시도하지 않음
//
// guildID/userID가 비어 있으면 fetch 스킵 (DM 등 guild 없는 컨텍스트 대응).
func (sess *Session) GetOrFetchRoles(s *discordgo.Session, userID string) []string {
	if sess == nil || sess.GuildID == "" || userID == "" {
		return nil
	}
	sess.NotesMu.Lock()
	if sess.RolesSnapshot != nil {
		if roles, ok := sess.RolesSnapshot[userID]; ok {
			sess.NotesMu.Unlock()
			return roles
		}
	}
	sess.NotesMu.Unlock()

	roles, err := fetchUserRoles(s, sess.GuildID, userID)
	if err != nil {
		log.Printf("[roles] WARN fetch 실패 guild=%s user=%s: %v (빈 role로 캐시)", sess.GuildID, userID, err)
	}

	sess.NotesMu.Lock()
	if sess.RolesSnapshot == nil {
		sess.RolesSnapshot = make(map[string][]string)
	}
	sess.RolesSnapshot[userID] = roles
	sess.NotesMu.Unlock()
	return roles
}
