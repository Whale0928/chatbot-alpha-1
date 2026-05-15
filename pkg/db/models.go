package db

import (
	"time"
)

// =====================================================================
// Domain models (DB row representation)
//
// Discord/봇 도메인 모델(pkg/bot.Session 등)과 분리한다 — DB 레이어는 직렬화/역직렬화만 책임.
// 변환은 호출자(pkg/bot)가 담당.
// =====================================================================

// SessionStatus는 super-session lifecycle.
type SessionStatus string

const (
	SessionActive SessionStatus = "ACTIVE"
	SessionClosed SessionStatus = "CLOSED"
)

// NoteSource는 Note의 출처 라벨. 환각 방어의 토대.
//   - SourceHuman:           발화자 본인 — 유일하게 액션 attribution 후보
//   - SourceWeeklyDump:      /weekly 도구 결과 — corpus 포함, attribution X
//   - SourceReleaseResult:   /release 도구 결과
//   - SourceAgentOutput:     /agent 도구 결과
//   - SourceInterimSummary:  중간 요약 결과 — corpus에서 제외 (자기 재흡수 방지)
//   - SourceExternalPaste:   사람이 붙여넣은 외부 텍스트 — corpus 포함, attribution X
type NoteSource string

const (
	SourceHuman          NoteSource = "Human"
	SourceWeeklyDump     NoteSource = "WeeklyDump"
	SourceReleaseResult  NoteSource = "ReleaseResult"
	SourceAgentOutput    NoteSource = "AgentOutput"
	SourceInterimSummary NoteSource = "InterimSummary"
	SourceExternalPaste  NoteSource = "ExternalPaste"
)

// IsAttributionCandidate는 이 source가 액션 attribution 후보 자격을 갖는지 반환한다.
// validate 단계에서 이 함수로 게이팅 — 거시 디자인 원칙 02 ("발화 vs 도구결과 라벨링") 코드화.
func (s NoteSource) IsAttributionCandidate() bool {
	return s == SourceHuman
}

// IsInCorpus는 이 source가 finalize corpus에 포함되어야 하는지 반환한다.
// InterimSummary만 제외 — 자기 자신 재흡수 방지.
func (s NoteSource) IsInCorpus() bool {
	return s != SourceInterimSummary
}

// SegmentKind는 sub-action 종류.
type SegmentKind string

const (
	SegmentWeeklySummary SegmentKind = "weekly_summary"
	SegmentRelease       SegmentKind = "release"
	SegmentAgent         SegmentKind = "agent"
)

// FormatKind는 정리본 렌더 포맷.
type FormatKind string

const (
	FormatDecisionStatus FormatKind = "decision_status"
	FormatDiscussion     FormatKind = "discussion"
	FormatRoleBased      FormatKind = "role_based"
	FormatFreeform       FormatKind = "freeform"
)

// =====================================================================
// Row structs — 1:1 with DB tables
// =====================================================================

// Session row.
// RolesSnapshot은 JSON ({"userID": ["BACKEND", ...]}) — pkg/bot이 marshal/unmarshal.
type Session struct {
	ID            string
	ThreadID      string
	GuildID       string
	OwnerID       string
	OpenedAt      time.Time
	ClosedAt      *time.Time
	Status        SessionStatus
	RolesSnapshot []byte // JSON raw bytes
}

// Segment row.
type Segment struct {
	ID        string
	SessionID string
	Kind      SegmentKind
	StartedAt time.Time
	EndedAt   *time.Time
	Artifact  []byte // JSON raw bytes (nil 가능)
}

// Note row.
// AuthorRoles는 JSON 배열 ([\"BACKEND\", \"FRONTEND\"]) raw bytes.
// SegmentID nil = 자유 발화 (sub-action 외부).
type Note struct {
	ID          string
	SessionID   string
	SegmentID   *string
	AuthorID    string
	AuthorName  string
	AuthorRoles []byte // JSON raw bytes
	Content     string
	Source      NoteSource
	Timestamp   time.Time
}

// SummarizedContent row.
// Content는 JSON (SummarizedContent struct 전체).
type SummarizedContent struct {
	ID          string
	SessionID   string
	Content     []byte
	ExtractedAt time.Time
}

// FinalizeRun row.
type FinalizeRun struct {
	ID                  string
	SummarizedContentID string
	Format              FormatKind
	Directive           string
	OutputMD            string
	CreatedAt           time.Time
}
