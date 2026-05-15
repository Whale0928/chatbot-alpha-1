package bot

import (
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"
)

// =====================================================================
// 통일 로깅 컨벤션 — kubectl logs 운영 디버깅 entry point
// =====================================================================
//
// 목적: pod의 stdout 로그만으로 사용자 보고("button이 안 됨", "session이 막힘",
// "release가 잘못 떴음")를 재구성할 수 있을 만큼 충분한 컨텍스트를 일관된 형식으로 남긴다.
//
// 모든 로그는 다음 패턴을 따른다:
//
//   [<area>/<event>] [LEVEL] <human-readable msg> key1=value1 key2=value2 ...
//
// 예:
//   [meeting/start] super-session 진입 thread=t_123 uid=u_456 mode=meeting
//   [release/guard_reject] reason="batch in progress" thread=t_123 user="alice"
//   [release/batch] err="CreatePullRequest 실패: ..." module=frontend step=5
//
// area 분류 (grep "[area/" 으로 도메인 추출):
//   meeting    — 미팅 모드 (StateMeeting/Meeting* 흐름)
//   weekly     — GitHub 주간 분석
//   release    — 단일 release PR 생성
//   release/batch — [전체] batch release
//   agent      — AI 에이전트 (자유 자연어 지시)
//   sticky     — sticky control 메시지 lifecycle
//   discord    — Discord interaction/message 진입 + 라우팅
//   db         — SQLite persistence
//   roles      — Discord guild role 조회/캐시
//   env        — bootstrap 환경변수 로딩
//   세션/sub   — DB SubAction (segments) lifecycle
//
// event 분류 (grep "/event]"):
//   start / end                        흐름 시작/종료
//   click                              사용자 button click 진입
//   guard_reject                       race/state 가드가 진입을 거절
//   state_change                       sess.Mode 또는 sess.State 변경
//   pending_set / pending_consume / pending_clear
//                                      Pending* per-user 게이트 lifecycle
//   ok / err                           외부 호출 (LLM/GitHub/Discord) 결과
//   skip                               의도된 분기 스킵 (e.g. 이미 진행 중)
//
// 공통 필드 (있으면 항상 포함):
//   thread=<sess.ThreadID>            Discord 스레드 ID — 세션 추적의 1차 키
//   user=<username>                   호출자 username
//   uid=<userID>                      호출자 Discord user ID — race 추적의 1차 키
//   mode=<normal|meeting>             SessionMode
//   state=<state name>                SessionState
//   custom_id=<string>                button click custom_id
//   reason=<string>                   guard reject 사유
//
// 도메인 필드:
//   source=<NoteSource>               Note source 분류 (Human/WeeklyDump/...)
//   runes=<int>                       룬 길이 (preview 컨텍스트)
//   elapsed=<duration>                LLM/GitHub call 소요
//   step=<int>                        runReleaseSteps step 1-5
//   module=<release.Module.Key>       release 모듈
//   bump=<release.BumpType>           메이저/마이너/패치
//   selected=<int>                    batch release 선택 모듈 수
//
// 진단 시나리오 → grep 패턴:
//   "버튼이 안 됨"        →  grep '\[discord/click\]' + custom_id 확인
//   "세션이 안 끝남"      →  grep '\[meeting/end\]\|\[discord/guard_reject\]'
//   "release 실패"        →  grep '\[release/' + module + step
//   "race condition"      →  grep '/pending_\|/race\|/guard_reject'
//   "특정 사용자 흐름"     →  grep 'uid=u_<id>'
//   "특정 스레드 흐름"     →  grep 'thread=t_<id>'
//
// 새로운 로그 추가 시 본 컨벤션 준수 — area/event 신규 추가는 위 분류표에도 반영.

// logField는 구조화 필드. lf("key", value)로 생성. value는 형식별 자동 인코딩.
type logField struct {
	Key   string
	Value any
}

// lf는 logField 생성 shorthand — 호출처 가독성용 (logEvent("...", lf("k", v)) ).
func lf(key string, value any) logField {
	return logField{Key: key, Value: value}
}

// logEvent는 일반 정보성 이벤트를 로그한다.
// area/event는 lowercase, msg는 사람이 읽는 한 줄 요약 (생략 가능 — "" 전달).
//
// 사용:
//
//	logEvent("meeting", "start", "super-session 즉시 진입", lf("thread", sess.ThreadID), lf("uid", sess.UserID))
func logEvent(area, event, msg string, fields ...logField) {
	log.Print(buildLogLine(area, event, "", msg, fields))
}

// logError는 에러를 ERR 레벨로 로그한다 — kubectl logs에서 "ERR" grep으로 모든 에러를 한 번에 추출 가능.
// err 필드는 자동으로 첫 번째 필드로 prepend. nil err 호출도 허용 (err=<nil>로 표기).
//
// 사용:
//
//	logError("release", "create_pr_failed", "PR 생성 실패", err, lf("module", rc.Module.Key), lf("step", 5))
func logError(area, event, msg string, err error, fields ...logField) {
	allFields := make([]logField, 0, len(fields)+1)
	allFields = append(allFields, lf("err", err))
	allFields = append(allFields, fields...)
	log.Print(buildLogLine(area, event, "ERR", msg, allFields))
}

// logGuard는 race/state 가드가 진입을 거절한 케이스를 로그한다.
// 사용자가 button을 눌렀는데 "이미 진행 중" 같은 안내만 받고 종료된 케이스를 grep으로 한 번에 추출.
//
// 사용:
//
//	logGuard("release", "release_in_progress", "단일 release 진행 중 — 새 진입 reject", lf("thread", ...), lf("user", ...))
func logGuard(area, reason, msg string, fields ...logField) {
	allFields := make([]logField, 0, len(fields)+1)
	allFields = append(allFields, lf("reason", reason))
	allFields = append(allFields, fields...)
	log.Print(buildLogLine(area, "guard_reject", "", msg, allFields))
}

// logState는 sess.Mode 또는 sess.State 전이를 로그한다 — bug 재현 시 state machine trace에 필수.
//
// 사용:
//
//	logState("session", "agent → meeting (long-running 보호)", "agent_await_input", "meeting", lf("thread", ...))
func logState(area, msg, from, to string, fields ...logField) {
	allFields := make([]logField, 0, len(fields)+2)
	allFields = append(allFields, lf("from", from), lf("to", to))
	allFields = append(allFields, fields...)
	log.Print(buildLogLine(area, "state_change", "", msg, allFields))
}

// buildLogLine은 [area/event] [LEVEL] msg key=value... 형식으로 한 줄을 만든다.
// log.Printf에 그대로 넘길 수 있도록 마지막 개행 없음.
func buildLogLine(area, event, level, msg string, fields []logField) string {
	var b strings.Builder
	b.WriteString("[")
	b.WriteString(area)
	b.WriteString("/")
	b.WriteString(event)
	b.WriteString("]")
	if level != "" {
		b.WriteString(" ")
		b.WriteString(level)
	}
	if msg != "" {
		b.WriteString(" ")
		b.WriteString(msg)
	}
	for _, f := range fields {
		b.WriteString(" ")
		b.WriteString(f.Key)
		b.WriteString("=")
		b.WriteString(formatLogValue(f.Value))
	}
	return b.String()
}

// formatLogValue는 logField.Value를 grep-friendly 형식으로 인코딩한다.
//
// 규칙:
//   - nil          → "<nil>"
//   - string       → strconv.Quote (특수 문자 escape, 공백 포함 안전)
//   - error        → strconv.Quote(err.Error())
//   - time.Duration → Round(ms).String() (예: "1.523s")
//   - time.Time    → UTC RFC3339
//   - fmt.Stringer → strconv.Quote(s.String())
//   - 그 외        → fmt.Sprintf("%v", x)
//
// quote 정책: 사람이 읽기 좋은 short value(int/bool/duration)는 quote 없음, 임의 문자열은 quote.
// 이래야 grep으로 'thread=t_123' 매칭이 동작 (quoted면 'thread="..."' 매칭이 더 복잡).
// 단 string/error는 공백/특수문자 escape 안전을 우선해 quote.
func formatLogValue(v any) string {
	switch x := v.(type) {
	case nil:
		return "<nil>"
	case string:
		// short identifier(thread/uid/etc)는 quote 없이 보내도 안전하지만, 안전 default로 quote.
		// 단 전형적인 short ID 형식(영숫자/_/-만)은 quote 생략해 grep 친화 — heuristic.
		if isShortIdent(x) {
			return x
		}
		return strconv.Quote(x)
	case error:
		if x == nil {
			return "<nil>"
		}
		return strconv.Quote(x.Error())
	case time.Duration:
		return x.Round(time.Millisecond).String()
	case time.Time:
		return x.UTC().Format(time.RFC3339)
	case fmt.Stringer:
		return strconv.Quote(x.String())
	default:
		return fmt.Sprintf("%v", v)
	}
}

// isShortIdent는 영숫자/_/-/:만으로 구성된 짧은 식별자인지 판단 — quote 생략해도 grep 안전.
// thread ID, custom_id, module key 등에 매칭. 한 글자라도 공백/quote/제어문자 있으면 false.
func isShortIdent(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' || r == ':' || r == '/' || r == '.' {
			continue
		}
		return false
	}
	return true
}
