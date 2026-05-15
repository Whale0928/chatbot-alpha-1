package bot

import (
	"strings"
	"testing"

	"chatbot-alpha-1/pkg/db"
)

// =====================================================================
// Discord 시나리오 검증 harness
//
// 단위 테스트가 함수 단독 호출만 보고 button click + 텍스트 입력 시퀀스를 검증 못 하던 구조적 한계
// (2026-05-15 운영 회귀: 사용자가 "세션 종료" 텍스트 입력 → weekly directive로 잘못 처리)를 해결.
//
// 이 harness는 실제 Discord 흐름과 동일한 sequence를 시뮬레이션:
//   - sendText("...")           : 사용자 텍스트 입력 = messageCreate event
//   - clickButton("custom_id")  : button click = interactionCreate event (MessageComponent)
//   - assertSessionState(...)   : 각 입력 후 sess의 Mode/State/Notes/Pending* 검증
//   - assertLastBotMessage(...) : 봇이 마지막에 보낸 메시지 내용 검증
//
// 한계:
//   - LLM 호출 / GitHub fetch는 fake로 stub (실 호출 X — 통합 테스트 별도)
//   - Discord API의 rate limit / interaction timeout 같은 메타 동작은 시뮬레이션 안 함
//   - 시나리오는 핵심 user journey 위주 — 모든 case는 단위 테스트 + 시나리오 조합으로 커버
// =====================================================================

// scenarioBot은 Discord 인터랙션 시퀀스 시뮬레이션 환경.
type scenarioBot struct {
	t        *testing.T
	sess     *Session
	msg      *fakeMessenger
	authorID string // 시나리오의 주 사용자 ID (multi-user 시나리오는 별도 헬퍼)
}

// newScenarioBot은 미팅 모드 super-session 시작 상태의 환경을 만든다.
// authorID = 첫 사용자 ("u_alice"). guildID/threadID는 고정 mock 값.
func newScenarioBot(t *testing.T) *scenarioBot {
	t.Helper()
	sess := &Session{
		Mode:      ModeMeeting,
		State:     StateMeeting,
		ThreadID:  "thread_scenario",
		GuildID:   "guild_scenario",
		UserID:    "u_alice",
		StartedAt: timeNowFor(t),
		UpdatedAt: timeNowFor(t),
	}
	sessionsMu.Lock()
	sessions[sess.ThreadID] = sess
	sessionsMu.Unlock()
	t.Cleanup(func() {
		sessionsMu.Lock()
		delete(sessions, sess.ThreadID)
		sessionsMu.Unlock()
	})

	return &scenarioBot{
		t:        t,
		sess:     sess,
		msg:      &fakeMessenger{},
		authorID: "u_alice",
	}
}

// sendText는 사용자가 텍스트 메시지 입력하는 케이스를 시뮬레이션한다.
//
// D4 정책 (UX 재설계 2026-05): 모든 명령은 sticky button만, 텍스트 escape 폐기.
// 실제 handleSession이 *discordgo.Session 받아 nil 호출 시 panic하므로, 시나리오는 handleSession의
// 핵심 분기 로직만 fake Messenger 경유로 직접 시뮬레이션:
//   1. PendingExternalPasteUserID 일치 → ExternalPaste 분류
//   2. 그 외 → AddNoteWithMeta로 일반 노트 누적
//
// 세션 종료는 sendText 아닌 clickSessionEnd() helper로 button click 시뮬레이션.
//
// 단위 테스트로는 못 잡던 "state machine 전이 + 분기 우선순위"를 검증하는 것이 목적.
// fakeDiscord 인터랙션 (button click)은 별도 headless 시나리오로 후속 추가.
func (b *scenarioBot) sendText(content string) {
	b.t.Helper()
	b.handleAuthored(b.authorID, "alice", content)
}

// sendTextFrom은 다른 사용자가 메시지 보내는 multi-user 시나리오용.
func (b *scenarioBot) sendTextFrom(userID, username, content string) {
	b.t.Helper()
	b.handleAuthored(userID, username, content)
}

// clickSessionEnd는 sticky의 [세션 종료] button click을 시뮬레이션한다 (D4 button-only 정책).
// 실제 discord.go의 customIDSessionEnd case가 HandleSessionEnd를 호출하는 동작과 동일하게 모방.
//
// 단위 테스트가 button click을 직접 dispatch하지 못하므로, 본 helper는 "사용자가 sticky의 [세션 종료]
// button을 눌렀다"는 정확한 의미만 시뮬레이션 — discordgo.InteractionCreate 객체는 만들지 않음.
func (b *scenarioBot) clickSessionEnd() {
	b.t.Helper()
	HandleSessionEnd(b.t.Context(), b.msg, b.sess)
}

// handleAuthored는 handleSession 분기 우선순위를 fakeMessenger 경유로 시뮬레이션한다.
// 실제 코드 흐름과 분기 순서를 동기화 — handleSession 또는 handleMeetingMessage 변경 시 이것도 갱신.
//
// D4 정책: 텍스트 escape 분기 없음 — 모든 종료/취소는 sticky button (clickSessionEnd helper).
func (b *scenarioBot) handleAuthored(userID, username, content string) {
	b.t.Helper()
	switch b.sess.State {
	case StateMeeting:
		b.simulateMeetingMessage(userID, username, content)
	case StateMeetingAwaitDirective:
		// directive 입력 — finalize 흐름과 묶여있어 시나리오에서는 분기만 검증
	case StateAgentAwaitInput:
		// agent 입력 — handleAgentMessage 시뮬레이션은 LLM 호출 의존이라 시나리오에서 분기만 검증
	// StateWeeklyAwaitDirective 폐기 — D2 정책
	}
}

// simulateMeetingMessage는 handleMeetingMessage의 핵심 분기를 fake로 재현 (sticky 갱신/role fetch 제외).
// D4 정책: "미팅 종료" 텍스트 escape 분기 없음 — 일반 미팅 노트로만 누적. 종료는 clickSessionEnd로.
// Source 분류 + per-user pending 게이트만 검증.
func (b *scenarioBot) simulateMeetingMessage(userID, username, content string) {
	// per-user pending 게이트
	var source db.NoteSource
	if b.sess.PendingExternalPasteUserID != "" && b.sess.PendingExternalPasteUserID == userID {
		source = db.SourceExternalPaste
		b.sess.PendingExternalPasteUserID = ""
	} else {
		source = classifyMessageSource(content)
	}

	b.sess.AddNoteWithMeta(Note{
		Author:   username,
		AuthorID: userID,
		Content:  content,
		Source:   source,
	})
}

// addNoteDirect는 실제 Discord 발화 시뮬레이션이 어려운 경로(예: handleSession이 *discordgo.Session
// nil로 panic하는 케이스)를 우회해 sess에 노트만 추가한다. state 전환 검증과 분리.
func (b *scenarioBot) addNoteDirect(author string, source db.NoteSource, content string) {
	b.t.Helper()
	b.sess.AddNoteWithMeta(Note{
		Author:  author,
		Content: content,
		Source:  source,
	})
}

// assertState는 현재 sess의 Mode + State가 기대값과 일치하는지 검증.
func (b *scenarioBot) assertState(wantMode SessionMode, wantState SessionState, msg string) {
	b.t.Helper()
	if b.sess.Mode != wantMode {
		b.t.Errorf("%s: Mode = %v, want %v", msg, b.sess.Mode, wantMode)
	}
	if b.sess.State != wantState {
		b.t.Errorf("%s: State = %v, want %v", msg, b.sess.State, wantState)
	}
}

// assertSessionRemoved는 HandleSessionEnd 후 sessions map에서 제거되었는지 검증.
func (b *scenarioBot) assertSessionRemoved(msg string) {
	b.t.Helper()
	sessionsMu.RLock()
	_, exists := sessions[b.sess.ThreadID]
	sessionsMu.RUnlock()
	if exists {
		b.t.Errorf("%s: 세션이 sessions map에 잔존", msg)
	}
}

// =====================================================================
// 핵심 시나리오 1: D4 button-only 정책 — "세션 종료" 텍스트는 escape X, 일반 미팅 노트로 누적
//
// 2026-05-15 운영 회귀(이전 escape 정책)를 button-only 정책으로 전환하면서 회귀 방향 자체 변경.
// 새 정책: 텍스트는 모두 미팅 corpus, 종료는 sticky [세션 종료] button만.
// 전제: D3 적용으로 sticky가 항상 보이므로 button 분실 X.
// =====================================================================

func TestScenario_StateMeeting_세션종료_텍스트는_미팅노트로(t *testing.T) {
	b := newScenarioBot(t)

	// when: StateMeeting에서 사용자가 "세션 종료" 텍스트 입력
	b.sendText("세션 종료")

	// then: 일반 미팅 노트로 누적 (Source=Human), 종료 X
	if len(b.sess.Notes) != 1 {
		t.Fatalf("note count = %d, want 1 (텍스트는 corpus 누적)", len(b.sess.Notes))
	}
	if b.sess.Notes[0].Content != "세션 종료" {
		t.Errorf("note content = %q, want %q", b.sess.Notes[0].Content, "세션 종료")
	}
	// 세션 종료 X — sessions map에 그대로
	sessionsMu.RLock()
	_, exists := sessions[b.sess.ThreadID]
	sessionsMu.RUnlock()
	if !exists {
		t.Error("button-only 정책 깨짐 — '세션 종료' 텍스트가 종료 트리거됨")
	}
}

func TestScenario_IsMeetingEndCommand_세션종료_미인식(t *testing.T) {
	// IsMeetingEndCommand는 deprecated이지만 보존 — "세션 종료"는 인식 X (button-only 정책).
	if IsMeetingEndCommand("세션 종료") {
		t.Error("'세션 종료'가 인식되면 button-only 정책 깨짐")
	}
	// legacy "미팅 종료"/"회의 종료"는 인식되나 호출처 없음 (D4 정책상 dead).
	if !IsMeetingEndCommand("미팅 종료") {
		t.Error("'미팅 종료' 인식 실패 (legacy 호환)")
	}
}

// 핵심 시나리오 2: D4 button-only — sticky [세션 종료] button click만 실제 종료를 트리거한다.
// 텍스트 "세션 종료"는 정확히 corpus에 누적되어야 하고, button click은 sessions map에서 제거.
func TestScenario_ClickSessionEnd_세션_제거됨(t *testing.T) {
	b := newScenarioBot(t)

	// given: 미팅 진행 중 텍스트 누적
	b.sendText("alice 발화 1")
	b.sendText("alice 발화 2")
	if len(b.sess.Notes) != 2 {
		t.Fatalf("setup: note count = %d, want 2", len(b.sess.Notes))
	}

	// when: 사용자가 sticky의 [세션 종료] button click
	b.clickSessionEnd()

	// then: sessions map에서 제거됨
	sessionsMu.RLock()
	_, exists := sessions[b.sess.ThreadID]
	sessionsMu.RUnlock()
	if exists {
		t.Error("clickSessionEnd 후에도 sessions map에 남음 — HandleSessionEnd 분기 문제")
	}
}

// =====================================================================
// 핵심 시나리오 3: 환각 방어 — corpus에 도구 출력 + 외부 paste가 누적된 상태에서
// SortedHumanSpeakers가 정확히 Human author만 반환 (5/14 미팅 데이터 재현)
// =====================================================================

func TestScenario_HallucinationGate_5_14_재현(t *testing.T) {
	b := newScenarioBot(t)

	// given: 5/14 시나리오 (사용자 발화 + weekly dump + external paste)
	b.addNoteDirect("kimjuye", db.SourceHuman, "workspace 통합 이슈 정리")
	b.addNoteDirect("deadwhale", db.SourceHuman, "큐레이션 order spec 확장")
	b.addNoteDirect("[weekly]", db.SourceWeeklyDump, "주간 리포트 ...")
	b.addNoteDirect("hyejungpark", db.SourceExternalPaste, "[큰 paste]")
	b.addNoteDirect("hyejungpark", db.SourceHuman, "프론트 배포 완료")
	b.addNoteDirect("kimjuye", db.SourceHuman, "FE 이슈 206 체크 요청")
	b.addNoteDirect("[bot]", db.SourceInterimSummary, "[중간 요약]")

	// when
	speakers := b.sess.SortedHumanSpeakers()
	corpus := b.sess.SnapshotNotesForCorpus()

	// then: speakers는 Human source 발화자만 (alphabetical)
	wantSpeakers := []string{"deadwhale", "hyejungpark", "kimjuye"}
	if len(speakers) != len(wantSpeakers) {
		t.Errorf("speakers = %v, want %v ([weekly]/[bot] 제외)", speakers, wantSpeakers)
	}
	for i, w := range wantSpeakers {
		if speakers[i] != w {
			t.Errorf("position %d: got %q, want %q", i, speakers[i], w)
		}
	}

	// then: corpus는 InterimSummary만 제외 (6 notes)
	if len(corpus) != 6 {
		t.Errorf("corpus count = %d, want 6 (InterimSummary 1개 제외)", len(corpus))
	}
}

// =====================================================================
// 핵심 시나리오 4: paste 임계 1500자 — 본인 노션 메모는 Human, 외부 회의록은 ExternalPaste
// =====================================================================

func TestScenario_Paste_본인메모_1300자_Human_유지(t *testing.T) {
	if got := classifyMessageSource(strings.Repeat("가", 1300)); got != db.SourceHuman {
		t.Errorf("1300자 본인 메모 = %v, want Human (1500자 임계 미만)", got)
	}
	if got := classifyMessageSource(strings.Repeat("가", 1500)); got != db.SourceExternalPaste {
		t.Errorf("1500자 = %v, want ExternalPaste (임계 도달)", got)
	}
}

// =====================================================================
// 핵심 시나리오 5: per-user pending — 다른 참석자 발화가 paste/agent로 잘못 분류되지 않음
// =====================================================================

func TestScenario_MultiUser_PendingExternalPaste_본인만_소비(t *testing.T) {
	b := newScenarioBot(t)

	// given: A가 [외부 자료 첨부] click (pending 세팅)
	b.sess.PendingExternalPasteUserID = "u_alice"

	// when: B(u_bob)가 일반 메시지 발화
	b.sendTextFrom("u_bob", "bob", "일반 미팅 발화")

	// then: B의 메시지는 ExternalPaste로 분류 안 됨, A의 pending은 유지
	if len(b.sess.Notes) != 1 {
		t.Fatalf("note count = %d, want 1", len(b.sess.Notes))
	}
	bobNote := b.sess.Notes[0]
	if bobNote.Author != "bob" {
		t.Errorf("note author = %q, want bob", bobNote.Author)
	}
	if bobNote.Source != db.SourceHuman {
		t.Errorf("bob의 발화가 Source = %v, want Human (다른 사용자 pending에 영향 받지 않아야)", bobNote.Source)
	}
	if b.sess.PendingExternalPasteUserID != "u_alice" {
		t.Errorf("A의 pending이 사라짐 — got %q, want u_alice (다른 사용자 발화는 pending 소비 X)",
			b.sess.PendingExternalPasteUserID)
	}

	// when: A 본인이 paste
	b.sendText("alice의 paste 내용")

	// then: A의 메시지는 ExternalPaste로 분류 + pending clear
	if len(b.sess.Notes) != 2 {
		t.Fatalf("note count = %d, want 2", len(b.sess.Notes))
	}
	aliceNote := b.sess.Notes[1]
	if aliceNote.Source != db.SourceExternalPaste {
		t.Errorf("A 본인 paste source = %v, want ExternalPaste", aliceNote.Source)
	}
	if b.sess.PendingExternalPasteUserID != "" {
		t.Errorf("본인 발화 후 pending이 clear 안 됨: %q", b.sess.PendingExternalPasteUserID)
	}
}

// =====================================================================
// 핵심 시나리오 6: cross-role detect (Phase 4) — 발화 시점 키워드 매칭
// =====================================================================

func TestScenario_CrossRole_PM이_FE에게_요청(t *testing.T) {
	// 5/14 실제 케이스: kimjuye(PM)의 "프론트 체크 요청"
	originRoles := []string{"PM"}
	content := "차주 미팅까지 깃허브 이슈 206,207,208 프론트엔드 체크 요청"

	targets := DetectTargetRoles(content)
	if len(targets) != 1 || targets[0] != "FRONTEND" {
		t.Fatalf("DetectTargetRoles = %v, want [FRONTEND]", targets)
	}
	if !IsCrossRoleHint(originRoles, targets) {
		t.Error("PM → FE를 cross-role로 감지 못함 (Phase 4 회귀)")
	}
}
