package bot

import (
	"sort"
	"strings"
	"time"

	"chatbot-alpha-1/pkg/db"
)

// Note는 미팅 중 수집되는 단일 메모. Author는 Discord 발화자 username.
//
// Phase 1 추가 필드 (모두 optional, default 시 Source=Human):
//   - AuthorID:    Discord user snowflake (DB persist 시 author_id 컬럼)
//   - AuthorRoles: 발화 시점 guild role snapshot (예: ["BACKEND", "PM"])
//   - Source:      Human / WeeklyDump / ReleaseResult / AgentOutput / InterimSummary / ExternalPaste
//   - SegmentID:   sub-action 결과물이면 해당 segment id, 자유 발화면 ""
//
// 기존 호출자(AddNote(author, content))는 자동으로 Source=Human, AuthorRoles=nil로 처리된다.
type Note struct {
	Author      string
	Content     string
	Timestamp   time.Time
	AuthorID    string
	AuthorRoles []string
	Source      db.NoteSource
	SegmentID   string
}

// AddNote는 사람 발화 기본 케이스 (Source=Human, AuthorRoles 미설정).
// Discord 메시지 핸들러에서 가장 흔하게 호출된다.
// 세션 내부 mutex로 보호되므로 여러 goroutine에서 동시 호출해도 안전하다.
// 반환값은 추가 후 전체 노트 개수 (1-based로 쓰기 편하도록 새 노트의 idx).
func (s *Session) AddNote(author, content string) int {
	idx, _ := s.AddNoteWithMeta(Note{
		Author:  author,
		Content: content,
		Source:  db.SourceHuman,
	})
	return idx
}

// AddNoteWithMeta는 명시적 Source/AuthorID/AuthorRoles/SegmentID와 함께 노트를 추가한다.
// 외부 paste 자동 분류, 도구 결과(WeeklyDump 등) persist, role snapshot 적용 시 사용.
//
// 동작:
//   - Timestamp가 zero면 time.Now()로 설정
//   - Source가 비어 있으면 db.SourceHuman으로 default
//   - Speakers 집합에 Author 추가 (모든 source — Human이 아니더라도 발화자 집합 추적)
//
// 반환:
//   - int: 추가 후 전체 노트 개수 (1-based 새 노트 idx)
//   - Note: 정규화 적용된 노트 사본. 호출자가 후속 persist에 그대로 전달 가능
//     (락 밖에서 sess.Notes[len-1]을 다시 읽으면 다른 goroutine이 추가한 노트를
//     집을 수 있으므로, 호출자는 반드시 이 반환값을 사용해야 함).
func (s *Session) AddNoteWithMeta(n Note) (int, Note) {
	s.NotesMu.Lock()
	defer s.NotesMu.Unlock()
	if n.Timestamp.IsZero() {
		n.Timestamp = time.Now()
	}
	if n.Source == "" {
		n.Source = db.SourceHuman
	}
	s.Notes = append(s.Notes, n)
	if s.Speakers == nil {
		s.Speakers = make(map[string]bool)
	}
	if n.Author != "" {
		s.Speakers[n.Author] = true
	}
	return len(s.Notes), n
}

// SnapshotNotes는 현재까지 수집된 메모의 복사본을 반환한다.
func (s *Session) SnapshotNotes() []Note {
	s.NotesMu.Lock()
	defer s.NotesMu.Unlock()
	out := make([]Note, len(s.Notes))
	copy(out, s.Notes)
	return out
}

// SortedSpeakers는 발화자 username을 정렬된 slice로 반환한다.
// 모든 source 발화자 포함 — 발화자 집합 자체는 도구 출력/외부 paste author까지 추적한다.
// finalize/interim 입력으로는 SortedHumanSpeakers 사용 권장 (attribution 게이트).
func (s *Session) SortedSpeakers() []string {
	s.NotesMu.Lock()
	defer s.NotesMu.Unlock()
	out := make([]string, 0, len(s.Speakers))
	for k := range s.Speakers {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// SortedHumanSpeakers는 Source=Human으로 발화한 사람만 정렬해 반환한다.
// validate의 attribution 후보 게이트 — 거시 디자인 원칙 02:
// "발화·도구결과·외부dump가 corpus에 같이 살되, attribution은 라벨로 차별".
//
// 한 사람이 Human 발화 + 외부 paste 둘 다 했으면 등장 (Human이 1번이라도 있으면).
// WeeklyDump/ReleaseResult/AgentOutput/InterimSummary/ExternalPaste만 한 author는 제외.
func (s *Session) SortedHumanSpeakers() []string {
	s.NotesMu.Lock()
	defer s.NotesMu.Unlock()
	seen := make(map[string]bool, len(s.Speakers))
	for _, n := range s.Notes {
		if n.Source.IsAttributionCandidate() && n.Author != "" {
			seen[n.Author] = true
		}
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// SnapshotNotesForCorpus는 finalize/interim 입력용 노트 사본을 반환한다.
// Source.IsInCorpus()=false인 항목(현재는 InterimSummary)을 제외 — 자기 재흡수 방지.
//
// 일반 SnapshotNotes는 모든 source 포함 (UI 표시용·로깅용).
func (s *Session) SnapshotNotesForCorpus() []Note {
	s.NotesMu.Lock()
	defer s.NotesMu.Unlock()
	out := make([]Note, 0, len(s.Notes))
	for _, n := range s.Notes {
		if n.Source.IsInCorpus() {
			out = append(out, n)
		}
	}
	return out
}

// IsMeetingEndCommand는 문자열이 정확히 미팅 종료 명령인지 판별한다.
// 공백만 trim 후 정확 일치. "미팅 종료 시점" 같은 문장은 false.
func IsMeetingEndCommand(content string) bool {
	trimmed := strings.TrimSpace(content)
	return trimmed == "미팅 종료" || trimmed == "회의 종료"
}

// =====================================================================
// Interim 보고 (수동 트리거 기반 중간 요약) 관련 세션 메서드
// =====================================================================
//
// 중간 요약은 사용자가 sticky 또는 이전 interim 메시지의 [중간 요약] 버튼을
// 직접 눌렀을 때만 발사된다. 이전의 5초 유휴 자동 발사는 폐기.
//
// 가드는 InterimInFlight 하나뿐 — "이미 진행 중인 interim 응답이 도착하기 전에
// 사용자가 또 누르는" 중복 클릭만 막는다. 동일 노트 상태에서 다시 누르는 것은
// 사용자 의도이므로 막지 않는다.

// TryStartManualInterim은 사용자가 [중간 요약] 버튼을 눌렀을 때 호출되어
// InterimInFlight를 원자적으로 검사하고 true 설정 후 true를 반환한다.
// 이미 다른 interim이 진행 중이면 false (호출자가 ephemeral 안내).
func (s *Session) TryStartManualInterim() bool {
	s.NotesMu.Lock()
	defer s.NotesMu.Unlock()
	if s.InterimInFlight {
		return false
	}
	s.InterimInFlight = true
	return true
}

// FinishInterim은 emitInterim 종료 시 (성공/실패 모두) 호출되어
// InterimInFlight 플래그를 false로 복원한다.
func (s *Session) FinishInterim() {
	s.NotesMu.Lock()
	defer s.NotesMu.Unlock()
	s.InterimInFlight = false
}

// =====================================================================
// Sticky 컨트롤 메시지 관련 헬퍼
// =====================================================================
//
// 스티키 패턴: Discord는 진짜 "pinned floating button" 같은 UI가 없으므로
// "이전 봇 메시지 삭제 → 맨 아래에 새 봇 메시지 전송"으로 항상 하단 노출을 흉내낸다.
// threshold 간격(예: 3개 노트)마다 갱신하여 API 호출 부담을 낮춘다.

// ReserveStickyRefresh는 sticky 갱신 조건을 검사하고 "예약"한다.
// 조건 (len(Notes) - NotesAtLastSticky >= threshold)을 만족하면
// NotesAtLastSticky를 미리 갱신해 중복 발사를 막고, 현재 sticky 메시지 ID를
// 반환한다 (호출자가 delete 대상으로 사용).
//
// 반환값:
//   - oldID: 삭제 대상 기존 sticky 메시지 ID (없으면 "")
//   - should: true면 호출자가 refresh를 진행해야 함
//
// "pre-bump" 설계인 이유: 두 goroutine이 동시에 threshold를 통과했을 때
// 하나만 발사하도록 하기 위해 락 안에서 카운터를 즉시 올린다. 발사가 실패해도
// 카운터는 롤백하지 않는다 (best-effort UI, 다음 threshold에서 재시도).
func (s *Session) ReserveStickyRefresh(threshold int) (oldID string, should bool) {
	s.NotesMu.Lock()
	defer s.NotesMu.Unlock()
	if len(s.Notes)-s.NotesAtLastSticky < threshold {
		return "", false
	}
	s.NotesAtLastSticky = len(s.Notes)
	return s.StickyMessageID, true
}

// SetStickyMessageID는 새로 전송한 sticky 메시지의 ID를 저장한다.
func (s *Session) SetStickyMessageID(id string) {
	s.NotesMu.Lock()
	defer s.NotesMu.Unlock()
	s.StickyMessageID = id
}

// CurrentStickyID는 현재 저장된 sticky 메시지 ID를 반환한다.
// 초기 sticky 전송 시 이전 ID 체크용.
func (s *Session) CurrentStickyID() string {
	s.NotesMu.Lock()
	defer s.NotesMu.Unlock()
	return s.StickyMessageID
}

// ClearSticky는 sticky 상태를 초기화하고 기존 ID를 반환한다.
// 미팅 종료 시 "마지막으로 이 ID를 삭제해달라"는 용도로 사용.
func (s *Session) ClearSticky() string {
	s.NotesMu.Lock()
	defer s.NotesMu.Unlock()
	id := s.StickyMessageID
	s.StickyMessageID = ""
	s.NotesAtLastSticky = 0
	return id
}
