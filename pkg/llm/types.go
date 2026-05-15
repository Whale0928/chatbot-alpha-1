package llm

// Note는 LLM 패키지가 입력으로 받는 최소한의 메모 구조.
// main 패키지의 Note와 별도로 정의하여 의존성 역방향을 방지한다.
type Note struct {
	Author  string
	Content string
}

// Decision은 결정 중심 템플릿(v1.4)의 1차 시민.
//
// 구조:
//   - Title: 결정 그 자체. 렌더링 시 **bold**로 강조되는 문구.
//   - Context: 배경/뉘앙스/회의 중 오간 관련 내용. 자식 bullet으로 렌더링.
//
// 왜 Context가 평평한 Discussion 필드보다 나은가:
//  1. 결정과 그 배경이 같은 묶음에 있어 6개월 후 "왜 이렇게 정했지?" 바로 답변 가능
//  2. 이전 템플릿의 "Discussion 섹션이 Decisions와 중복" 문제 해소
//  3. history 기록 역할을 Context가 자연스럽게 흡수 (같은 토픽에 대한 회의 흐름이
//     해당 결정 바로 아래에 모임)
//
// LLM 가이드: Context 는 대개 1~3개, 원문에 명시된 nuance가 있으면 추가. 없으면 비워둬도 OK.
type Decision struct {
	Title   string   `json:"title"   jsonschema_description:"결정 그 자체. 한 문장. 고유명사/기술용어 원문 보존."`
	Context []string `json:"context" jsonschema_description:"결정의 배경, 뉘앙스, 회의 중 함께 언급된 관련 내용. 자식 bullet으로 들어감. 0~3개 권장."`
}

// FinalNoteResponse는 미팅 종료 시 LLM이 생성하는 구조화된 최종 노트 데이터.
//
// v1.4 (결정 중심 템플릿):
//   - Decisions: 결정 1차 시민. 각 결정은 Title + Context 자식 구조
//   - OpenQuestions: 결정되지 않은 질문. 평평한 리스트 (자식 없음)
//   - NextSteps: 담당자/기한 명시된 액션 아이템만
//   - Tags: 짧은 키워드
//
// Discussion 필드는 제거되었다. 이전 템플릿에서 결정과 중복되던 history 기록은
// 이제 Decision.Context 자식 bullet이 담당한다.
//
// 참석자(Participants)는 LLM이 생성하지 않고 Go가 직접 주입한다.
type FinalNoteResponse struct {
	Decisions     []Decision `json:"decisions"      jsonschema_description:"회의에서 나온 결정. 각 Decision은 Title + Context 구조. 애매하면 결정사항으로 공격적 분류. 고유명사/기술용어 원문 보존."`
	OpenQuestions []string   `json:"open_questions" jsonschema_description:"언급만 되고 결론 안 난 질문. 사용자 '놓친 결정' 체크리스트. 각 항목 '~ 확인 필요'로 끝나도록."`
	NextSteps     []NextStep `json:"next_steps"     jsonschema_description:"확정된 액션 아이템 (담당자 또는 기한이 명시된 것)."`
	Tags          []string   `json:"tags"           jsonschema_description:"짧은 키워드. 공백 없는 단일 토큰. 선택."`
}

// InterimNoteResponse는 미팅 진행 중 주기적으로 생성하는 "현재까지 정리" 응답.
// FinalNoteResponse와 동일한 Decision 구조를 쓰되 NextSteps는 없다
// (미팅 중간에 LLM이 성급하게 TODO를 확정하면 안 되기 때문).
type InterimNoteResponse struct {
	Decisions     []Decision `json:"decisions"      jsonschema_description:"지금까지 나온 것으로 보이는 결정. Decision.Context는 배경/뉘앙스."`
	OpenQuestions []string   `json:"open_questions" jsonschema_description:"결정되어야 하는데 아직 결론 없는 질문. '~ 확인 필요' 접미사."`
	Tags          []string   `json:"tags"           jsonschema_description:"짧은 키워드. 공백 없는 단일 토큰. 선택."`
}

// NextStep은 단일 액션 아이템.
type NextStep struct {
	Who      string `json:"who"      jsonschema_description:"담당자 username. 불명확하면 빈 문자열."`
	Deadline string `json:"deadline" jsonschema_description:"YYYY-MM-DD 형식 또는 '이번주' 같은 한국어 기한. 불명확하면 빈 문자열."`
	What     string `json:"what"     jsonschema_description:"수행해야 할 작업. 반드시 채워야 한다."`
}

// =====================================================================
// v2.0 — 4 포맷 응답 타입 (docs/note-formats.md 참조)
// =====================================================================

// NoteFormat은 미팅 종료 시 생성할 노트 포맷을 식별한다.
type NoteFormat int

const (
	// FormatDecisionStatus: 결정 + 4분할 진행보고 통합형 (default).
	FormatDecisionStatus NoteFormat = iota
	// FormatDiscussion: 토픽별 논의 흐름 + 인사이트.
	FormatDiscussion
	// FormatRoleBased: 참석자별 결정/액션/공유.
	FormatRoleBased
	// FormatFreeform: LLM 자율 마크다운.
	FormatFreeform
)

// String은 로그/CLI 플래그 문자열 표현.
func (f NoteFormat) String() string {
	switch f {
	case FormatDecisionStatus:
		return "decision_status"
	case FormatDiscussion:
		return "discussion"
	case FormatRoleBased:
		return "role_based"
	case FormatFreeform:
		return "freeform"
	default:
		return "unknown"
	}
}

// ParseNoteFormat은 CLI 플래그 문자열을 NoteFormat으로 변환.
// 알 수 없는 값이면 ok=false.
func ParseNoteFormat(s string) (NoteFormat, bool) {
	switch s {
	case "decision_status", "1":
		return FormatDecisionStatus, true
	case "discussion", "2":
		return FormatDiscussion, true
	case "role_based", "3":
		return FormatRoleBased, true
	case "freeform", "4":
		return FormatFreeform, true
	default:
		return 0, false
	}
}

// DecisionStatusResponse는 포맷 1번. 결정 + 4분할 진행 + 미정 + 액션.
type DecisionStatusResponse struct {
	Decisions     []Decision `json:"decisions"      jsonschema_description:"결정 (Title + Context). v1.4 규칙 그대로."`
	Done          []string   `json:"done"           jsonschema_description:"완료된 작업/사실. '완료', '확인됨', '배포 완료' 등."`
	InProgress    []string   `json:"in_progress"    jsonschema_description:"진행 중. '진행 중', '체크 중', '작업 중'."`
	Planned       []string   `json:"planned"        jsonschema_description:"예정/대기. '예정', '할 것', 미래형."`
	Blockers      []string   `json:"blockers"       jsonschema_description:"막힘/이슈/리스크. '안 된다', '문제', '오류'."`
	OpenQuestions []string   `json:"open_questions" jsonschema_description:"결정되지 않은 질문. '<topic> - <구체 미정>. 확인 필요'."`
	NextSteps     []NextStep `json:"next_steps"     jsonschema_description:"담당자/기한 명시 액션."`
	Tags          []string   `json:"tags"           jsonschema_description:"공백 없는 단일 토큰 태그."`
}

// Topic은 DiscussionResponse의 단일 토픽.
type Topic struct {
	Title    string   `json:"title"    jsonschema_description:"토픽 한 줄 요약. 명사구."`
	Flow     []string `json:"flow"     jsonschema_description:"시간순 논의 흐름. 2~5개. 자연스러운 한국어 문장."`
	Insights []string `json:"insights" jsonschema_description:"도출된 관점/배움/합의된 방향. 0~3개. 단정형이 아닌 관점/제안 톤."`
}

// DiscussionResponse는 포맷 2번. 토픽별 논의 흐름.
type DiscussionResponse struct {
	Topics        []Topic  `json:"topics"         jsonschema_description:"논의 토픽들. 시간 순서로 클러스터링."`
	OpenQuestions []string `json:"open_questions" jsonschema_description:"결정되지 않은 질문. '확인 필요' 접미사."`
	Tags          []string `json:"tags"           jsonschema_description:"공백 없는 단일 토큰 태그."`
}

// RoleSection은 RoleBasedResponse의 단일 참석자 블록.
type RoleSection struct {
	Speaker   string     `json:"speaker"   jsonschema_description:"Discord username. 반드시 입력 Speakers 목록에 존재해야 한다 (strict 검증)."`
	Decisions []string   `json:"decisions" jsonschema_description:"이 사람이 내린/이 사람과 직접 관련된 결정."`
	Actions   []NextStep `json:"actions"   jsonschema_description:"이 사람의 액션 아이템."`
	Shared    []string   `json:"shared"    jsonschema_description:"이 사람이 공유한 진행/현황/정보."`
}

// RoleBasedResponse는 포맷 3번. 참석자별 정리.
type RoleBasedResponse struct {
	Roles         []RoleSection `json:"roles"          jsonschema_description:"참석자별 섹션. Speaker는 입력 Speakers 목록의 부분집합."`
	SharedItems   []string      `json:"shared_items"   jsonschema_description:"역할 비귀속 공동 합의/공유사항."`
	OpenQuestions []string      `json:"open_questions" jsonschema_description:"미정 질문."`
	Tags          []string      `json:"tags"           jsonschema_description:"공백 없는 단일 토큰 태그."`
}

// FreeformResponse는 포맷 4번. 단일 마크다운 필드.
// 단일 필드 JSON으로 강제해 파싱 안정성 확보 (response_format 미사용 대신
// 일반 JSON Schema strict로 markdown 한 필드만 받음).
type FreeformResponse struct {
	Markdown string `json:"markdown" jsonschema_description:"미팅 노트 본문 마크다운. H1 헤더와 참석자/태그 풋터는 포함하지 않는다 (Go가 주입). ## 헤딩부터 시작."`
}

// =====================================================================
// Phase 2 — SummarizedContent (정리본 원천, 1회 추출 → 4 포맷 렌더)
//
// 거시 디자인 결정 A: "콘텐츠는 1회 추출, 포맷은 N회 변환".
// 4 포맷 렌더러(decision_status / discussion / role_based / freeform)는
// 이 struct만 입력으로 받는 순수 함수 — LLM 재호출 없이 즉시 변환된다.
// 단, freeform은 사용자 directive별 다양성을 위해 별도 LLM 호출 가능.
//
// 호출 계약:
//   - 입력 notes는 Source.IsInCorpus()=true만 (InterimSummary 제외)
//   - 입력 speakers는 Source.IsAttributionCandidate()=true만 (Human source 발화자만)
//   - SummaryAction.Origin은 반드시 입력 speakers 안의 username
// =====================================================================

// SummarizedContent는 미팅 corpus에서 1회 추출한 모든 구조화 사실.
// JSON으로 직렬화되어 db.summarized_contents.content 컬럼에 저장된다.
//
// 4 포맷 렌더 매핑:
//   - decision_status → Decisions + Done + InProgress + Planned + Blockers + Actions(NextStep으로 변환)
//   - discussion      → Topics
//   - role_based      → Actions를 Origin/TargetRoles 기준으로 그룹핑
//   - freeform        → directive 적용한 LLM 재호출 (또는 Decisions+Topics+Actions 자유 합성)
type SummarizedContent struct {
	Decisions      []Decision             `json:"decisions"       jsonschema_description:"결정 (Title + Context). v1.4 규칙."`
	Done           []string               `json:"done"            jsonschema_description:"완료된 작업/사실."`
	InProgress     []string               `json:"in_progress"     jsonschema_description:"진행 중."`
	Planned        []string               `json:"planned"         jsonschema_description:"예정/대기."`
	Blockers       []string               `json:"blockers"        jsonschema_description:"막힘/이슈/리스크."`
	Topics         []Topic                `json:"topics"          jsonschema_description:"논의 토픽 (Discussion 포맷용). 시간순 클러스터링."`
	Actions        []SummaryAction        `json:"actions"         jsonschema_description:"확정된 액션 아이템. cross-role(Origin role ≠ Target role) 인식. role_based 렌더는 이 배열을 그룹핑한다."`
	WeeklyReports  []WeeklyReportSummary  `json:"weekly_reports"  jsonschema_description:"[GitHub 주간 분석] sub-action 결과 정리. ContextNotes 안의 [weekly] prefix 항목들에서 추출. 사람 발화의 결정/액션이 아니므로 Origin 부여 X."`
	ReleaseResults []ReleaseResultSummary `json:"release_results" jsonschema_description:"[릴리즈 PR 만들기] sub-action 결과 정리. ContextNotes 안의 [release] prefix 항목들에서 추출."`
	AgentResponses []AgentResponseSummary `json:"agent_responses" jsonschema_description:"[AI에게 질문] sub-action 결과 정리. ContextNotes 안의 [agent] prefix 항목들에서 추출."`
	ExternalRefs   []ExternalRefSummary   `json:"external_refs"   jsonschema_description:"[외부 문서 첨부] paste 핵심 추출. Source=ExternalPaste 발화 본문에서."`
	Shared         []string               `json:"shared"          jsonschema_description:"역할 비귀속 공동 합의/공유사항."`
	OpenQuestions  []string               `json:"open_questions"  jsonschema_description:"결정되지 않은 질문. '확인 필요' 접미사."`
	Tags           []string               `json:"tags"            jsonschema_description:"공백 없는 단일 토큰 태그."`
}

// WeeklyReportSummary는 [GitHub 주간 분석] 1건의 정리.
// Highlights는 정제된 3-5 bullet (raw markdown 본문 그대로 dump 금지).
type WeeklyReportSummary struct {
	Repo        string   `json:"repo"         jsonschema_description:"분석 대상 레포 (owner/name 또는 name)"`
	PeriodDays  int      `json:"period_days"  jsonschema_description:"분석 기간 (지난 N일). 모르면 0."`
	CommitCount int      `json:"commit_count" jsonschema_description:"분석된 commit 개수. 모르면 0."`
	Highlights  []string `json:"highlights"   jsonschema_description:"핵심 변경/이슈 3-5 bullet. 1줄 1 bullet."`
}

// ReleaseResultSummary는 [릴리즈 PR 만들기] 1건의 정리.
type ReleaseResultSummary struct {
	Module      string   `json:"module"       jsonschema_description:"모듈 키 (product/admin/frontend/dashboard 등)"`
	PrevVersion string   `json:"prev_version" jsonschema_description:"이전 버전 (예: 1.1.4)"`
	NewVersion  string   `json:"new_version"  jsonschema_description:"새 버전 (예: 1.1.5)"`
	BumpType    string   `json:"bump_type"    jsonschema_description:"메이저/마이너/패치 또는 major/minor/patch"`
	PRNumber    int      `json:"pr_number"    jsonschema_description:"PR 번호. 모르면 0."`
	PRURL       string   `json:"pr_url"       jsonschema_description:"PR URL. 모르면 빈 문자열."`
	Highlights  []string `json:"highlights"   jsonschema_description:"변경 핵심 3-5 bullet. PR body 그대로 dump 금지."`
}

// AgentResponseSummary는 [AI에게 질문] 1건의 정리.
type AgentResponseSummary struct {
	Question   string   `json:"question"   jsonschema_description:"사용자가 던진 질문 원문 (또는 요약)."`
	Highlights []string `json:"highlights" jsonschema_description:"AI 응답의 핵심 3-5 bullet."`
}

// ExternalRefSummary는 [외부 문서 첨부] paste 1건의 정리.
type ExternalRefSummary struct {
	Title      string   `json:"title"      jsonschema_description:"LLM이 부여한 라벨 (예: 'Vendor latency 보고서'). paste 첫 줄 또는 핵심 키워드."`
	Highlights []string `json:"highlights" jsonschema_description:"핵심 수치/관찰 3-5 bullet."`
}

// SummaryAction은 cross-role 인식을 포함한 단일 액션 아이템.
//
// 거시 디자인 결정 5/B: 발화자 role(Origin) ≠ 액션 대상 role(Target)인 경우를 명시 분리.
// 예: kimjuye(PM)의 "프론트 체크 요청" → Origin=kimjuye, OriginRoles=[PM], TargetRoles=[FRONTEND].
//
// 거시 디자인 결정 6 (환각 방어): Origin은 반드시 입력 speakers (Human source 발화자) 안의
// username이어야 한다. 도구 출력/외부 paste의 author는 Origin이 될 수 없다.
type SummaryAction struct {
	What        string   `json:"what"         jsonschema_description:"수행해야 할 작업. 반드시 채워야 한다."`
	Origin      string   `json:"origin"       jsonschema_description:"액션을 발의한 사람의 Discord username. 반드시 입력 Speakers 목록에 있어야 한다."`
	OriginRoles []string `json:"origin_roles" jsonschema_description:"발의자의 Discord guild role 라벨 (예: [\"BACKEND\", \"PM\"]). 입력 SpeakerRoles 매핑에서 가져온다."`
	TargetRoles []string `json:"target_roles" jsonschema_description:"액션 대상 role 그룹. 발의자 본인 작업이면 OriginRoles와 동일. 'FE 체크 요청' 같은 cross-role 발화면 다르다 (예: OriginRoles=[PM], TargetRoles=[FRONTEND]). 빈 배열도 허용 (대상 모호)."`
	TargetUser  string   `json:"target_user"  jsonschema_description:"특정 개인 지목 시 그 사람의 username. 일반 그룹 대상이면 빈 문자열."`
	Deadline    string   `json:"deadline"     jsonschema_description:"YYYY-MM-DD 형식 또는 '차주 미팅까지' 같은 한국어 기한. 불명확하면 빈 문자열."`
}

// WeeklyScope는 주간 정리에서 어떤 데이터를 LLM에 dump할지 식별한다.
//
//	WeeklyScopeIssues  — 현재 OPEN 이슈만 (커밋 fetch 생략)
//	WeeklyScopeCommits — 지난 N일 커밋만 (이슈 fetch 생략)
//	WeeklyScopeBoth    — 둘 다 (기존 동작, 디폴트)
//
// scope에 따라 fetch가 분기되고, summarize.Weekly가 시스템 프롬프트에 scope를 명시해
// LLM이 누락된 소스에 대한 추측이나 "(없음)" 같은 자리채움을 하지 않도록 한다.
type WeeklyScope int

const (
	WeeklyScopeBoth WeeklyScope = iota
	WeeklyScopeIssues
	WeeklyScopeCommits
)

// String은 로그/CLI/프롬프트에 들어가는 문자열 표현.
func (s WeeklyScope) String() string {
	switch s {
	case WeeklyScopeIssues:
		return "issues"
	case WeeklyScopeCommits:
		return "commits"
	case WeeklyScopeBoth:
		return "both"
	default:
		return "unknown"
	}
}

// ParseWeeklyScope는 CLI 플래그/customID 토큰을 WeeklyScope로 변환한다.
// 알 수 없는 값이면 ok=false.
func ParseWeeklyScope(s string) (WeeklyScope, bool) {
	switch s {
	case "issues":
		return WeeklyScopeIssues, true
	case "commits":
		return WeeklyScopeCommits, true
	case "both":
		return WeeklyScopeBoth, true
	default:
		return 0, false
	}
}

// IncludesIssues는 scope에 이슈 fetch가 포함되는지 반환한다.
func (s WeeklyScope) IncludesIssues() bool {
	return s == WeeklyScopeBoth || s == WeeklyScopeIssues
}

// IncludesCommits는 scope에 커밋 fetch가 포함되는지 반환한다.
func (s WeeklyScope) IncludesCommits() bool {
	return s == WeeklyScopeBoth || s == WeeklyScopeCommits
}

// ClosableIssue는 LLM이 "닫아도 될 것 같은 이슈"로 추천하는 단일 항목.
// 이 목록은 [닫아도 될 이슈 N건 닫기] 버튼이 GitHub close API를 호출할 때 ground truth로 사용된다.
//
// 보수적으로 채워져야 한다 — markdown 본문의 "## 닫아도 될 것 같은 이슈" 섹션과 정확히 동일한
// 항목만 들어가야 하며, 추측성 추천이면 빈 배열로 둬야 한다.
type ClosableIssue struct {
	Number int    `json:"number" jsonschema_description:"이슈 번호 (입력 dump의 #NNN과 정확히 일치)."`
	Title  string `json:"title"  jsonschema_description:"이슈 제목 원문 그대로."`
	Reason string `json:"reason" jsonschema_description:"닫기 추천 이유. 한 줄. 입력 dump의 사실에 기반."`
}

// WeeklyReportResponse는 주간 정리(레포 단위) LLM 분석 결과.
// markdown은 사람용 본문, closeable은 [닫기] 액션용 ground truth.
type WeeklyReportResponse struct {
	Markdown  string          `json:"markdown"  jsonschema_description:"주간 리포트 본문 마크다운. H1 헤더와 메타 풋터는 포함하지 않는다 (Go가 주입). ## 헤딩부터 시작."`
	Closeable []ClosableIssue `json:"closeable" jsonschema_description:"닫아도 될 것 같은 이슈 목록. markdown의 '## 닫아도 될 것 같은 이슈' 섹션과 정확히 동일해야 한다. 보수적으로 — 확신 없으면 빈 배열."`
}

// AgentResponse는 자유 자연어 지시(에이전트 모드) LLM 답변.
// 단일 markdown 필드 — 시스템 프롬프트가 형식 가이드를 강제한다.
type AgentResponse struct {
	Markdown string `json:"markdown" jsonschema_description:"사용자 지시에 대한 한국어 마크다운 답변. 데이터 dump에 근거하지 않는 추측 금지."`
}

// ReleaseNoteResponse는 릴리즈 PR 본문 LLM 생성 결과.
//
// 단일 markdown 필드. 시스템 프롬프트가 ### 헤더 컨벤션(신규/개선·버그 수정·내부·호환성 깨짐)을 강제한다.
// 커밋 메시지에 없는 정보를 만들어내면 안 된다 (환각 금지).
//
// H1 헤더와 메타 풋터(비교 base, 커밋 수 등)는 Go renderer가 주입하므로 LLM은 ### 섹션부터 시작.
type ReleaseNoteResponse struct {
	Markdown string `json:"markdown" jsonschema_description:"릴리즈 노트 본문 마크다운. ### 섹션 헤더부터 시작. H1/H2 사용 금지(Go가 주입). 빈 섹션은 생략."`
}
