package bot

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"chatbot-alpha-1/pkg/llm"
	"chatbot-alpha-1/pkg/llm/render"

	"github.com/bwmarrin/discordgo"
)

// dumpNotes는 finalize 직전 수집된 모든 메모를 로그에 박제한다.
// 각 노트는 개별 라인으로 찍혀서 나중에 grep/비교가 쉽다.
// 입력 손실 진단의 ground truth 역할.
func dumpNotes(threadID string, notes []Note, speakers []string) {
	log.Printf("[미팅/dump] === thread=%s notes=%d speakers=%v ===", threadID, len(notes), speakers)
	for i, n := range notes {
		// 전체 content를 찍되 개행은 \n 이스케이프해서 한 라인 유지
		safe := strings.ReplaceAll(n.Content, "\n", "\\n")
		log.Printf("[미팅/dump] %2d. [%s] %s", i+1, n.Author, safe)
	}
	log.Printf("[미팅/dump] === end thread=%s ===", threadID)
}

// previewRunes는 긴 문자열을 로그 프리뷰용으로 룬 기준 자른다.
// 개행은 \n 으로 이스케이프하여 한 라인 로그 유지.
func previewRunes(s string, max int) string {
	r := []rune(s)
	truncated := false
	if len(r) > max {
		r = r[:max]
		truncated = true
	}
	out := strings.ReplaceAll(string(r), "\n", "\\n")
	if truncated {
		out += "…"
	}
	return out
}

// Messenger는 finalizeMeeting/emitInterim/sendSticky가 디스코드에 메시지를 보낼 때 쓰는 인터페이스.
// *discordgo.Session이 이미 모두 만족하며, 테스트에서는 mock을 주입한다.
type Messenger interface {
	ChannelMessageSend(channelID, content string, opts ...discordgo.RequestOption) (*discordgo.Message, error)
	ChannelMessageSendComplex(channelID string, data *discordgo.MessageSend, opts ...discordgo.RequestOption) (*discordgo.Message, error)
	ChannelMessageDelete(channelID, messageID string, opts ...discordgo.RequestOption) error
}

// MeetingSummarizer는 미팅 종료/중간 요약을 4 포맷 + legacy + interim으로 제공한다.
// 실제 구현은 llmSummarizer (bot/llm_summarizer.go) — *llm.Client를 wrap해서
// summarize.X free function에 위임한다. 테스트는 fakeSummarizer mock 주입.
//
// 4 포맷 메서드의 directive 인자는 사용자가 [추가 요청] 흐름에서 입력한 정리 지시문이다.
// 빈 문자열이면 미적용. legacy/interim은 directive를 받지 않는다 (interim은 미팅 중
// 스냅샷이라 사용자 지시 의미가 약하고, legacy는 fallback 용도).
type MeetingSummarizer interface {
	// legacy v1.4 (FormatLegacy 또는 호환 fallback)
	SummarizeMeeting(ctx context.Context, notes []llm.Note, speakers []string, date time.Time) (*llm.FinalNoteResponse, error)

	// 4 포맷 (v2.0)
	SummarizeDecisionStatus(ctx context.Context, notes []llm.Note, speakers []string, date time.Time, directive string) (*llm.DecisionStatusResponse, error)
	SummarizeDiscussion(ctx context.Context, notes []llm.Note, speakers []string, date time.Time, directive string) (*llm.DiscussionResponse, error)
	SummarizeRoleBased(ctx context.Context, notes []llm.Note, speakers []string, date time.Time, directive string) (*llm.RoleBasedResponse, error)
	SummarizeFreeform(ctx context.Context, notes []llm.Note, speakers []string, date time.Time, directive string) (*llm.FreeformResponse, error)

	SummarizeInterim(ctx context.Context, notes []llm.Note, speakers []string, date time.Time) (*llm.InterimNoteResponse, error)
}

// finalizeMeeting은 "미팅 종료" 시 실행되는 핵심 로직.
//
// 기존 시그니처는 legacy 포맷(FinalNoteResponse) 고정이었으나 v2.0부터는
// `format` 인자로 4 포맷 중 하나를 명시한다. 호출자는 사용자가 선택한
// 버튼의 custom_id에서 NoteFormat을 결정하여 전달한다.
//
// 동작:
//  1. 세션 메모가 비어있으면 안내 메시지 후 세션 정리하고 종료
//  2. format에 맞는 LLM 메서드 호출 + 렌더
//  3. 실패 시: 에러 메시지 전송 + 세션 보존 (재시도 가능). keepSession=true 반환
//  4. 성공 시: 렌더링된 마크다운 전송 + 세션 정리. keepSession=false 반환
//
// 반환 keepSession은 호출자가 세션 정리 여부를 결정할 때 사용한다.
func finalizeMeeting(
	ctx context.Context,
	msg Messenger,
	summ MeetingSummarizer,
	sess *Session,
	now time.Time,
	format llm.NoteFormat,
	directive string,
) (keepSession bool) {
	notes := sess.SnapshotNotes()
	speakers := sess.SortedSpeakers()

	directiveRunes := len([]rune(directive))
	log.Printf("[미팅/finalize] 시작 thread=%s notes=%d speakers=%d format=%s directive_runes=%d",
		sess.ThreadID, len(notes), len(speakers), format, directiveRunes)
	if directiveRunes > 0 {
		log.Printf("[미팅/finalize] directive_preview=%q", previewRunes(directive, 200))
	}

	dumpNotes(sess.ThreadID, notes, speakers)

	if len(notes) == 0 {
		log.Printf("[미팅/finalize] 빈 노트 - 요약 건너뜀 thread=%s", sess.ThreadID)
		if _, err := msg.ChannelMessageSend(sess.ThreadID, "기록된 메모가 없어 요약을 생성하지 않았습니다."); err != nil {
			log.Printf("[미팅/finalize] ERR 안내 메시지 전송 실패 thread=%s: %v", sess.ThreadID, err)
		}
		return false
	}

	llmNotes := make([]llm.Note, len(notes))
	for i, n := range notes {
		llmNotes[i] = llm.Note{Author: n.Author, Content: n.Content}
	}

	start := time.Now()
	log.Printf("[미팅/llm] 요약 호출 시작 thread=%s notes=%d speakers=%v format=%s",
		sess.ThreadID, len(llmNotes), speakers, format)

	rendered, err := dispatchFinalize(ctx, summ, llmNotes, speakers, now, format, directive)
	dur := time.Since(start)
	if err != nil {
		log.Printf("[미팅/llm] ERR 요약 실패 thread=%s format=%s elapsed=%s err=%v notes=%d (세션 보존)",
			sess.ThreadID, format, dur, err, len(notes))
		if _, sendErr := msg.ChannelMessageSend(
			sess.ThreadID,
			fmt.Sprintf("요약 생성 중 오류가 발생했습니다. 메모 %d건은 그대로 유지됩니다. 잠시 후 다시 시도해주세요.", len(notes)),
		); sendErr != nil {
			log.Printf("[미팅/finalize] ERR 에러 메시지 전송 실패 thread=%s: %v", sess.ThreadID, sendErr)
		}
		return true
	}
	log.Printf("[미팅/llm] 요약 성공 thread=%s format=%s elapsed=%s rendered_runes=%d",
		sess.ThreadID, format, dur, len([]rune(rendered)))
	log.Printf("[미팅/finalize] rendered_preview=%q", previewRunes(rendered, 300))

	// 결과 마크다운은 sendLongMessage로 (분할 가능). [처음 메뉴] 버튼은 별도 메시지로 첨부 →
	// 사용자가 같은 스레드에서 다른 메뉴로 이어갈 수 있게 한다.
	if err := sendLongMessage(msg, sess.ThreadID, rendered); err != nil {
		log.Printf("[미팅/finalize] ERR 최종 노트 전송 실패 thread=%s: %v", sess.ThreadID, err)
	}
	if _, err := msg.ChannelMessageSendComplex(sess.ThreadID, &discordgo.MessageSend{
		Content: "이어서 다른 작업을 시작하려면 [처음 메뉴]를 눌러주세요.",
		Components: []discordgo.MessageComponent{
			discordgo.ActionsRow{Components: []discordgo.MessageComponent{
				discordgo.Button{Label: "처음 메뉴", Style: discordgo.SecondaryButton, CustomID: customIDHomeBtn},
			}},
		},
	}); err != nil {
		log.Printf("[미팅/finalize] ERR 처음 메뉴 버튼 전송 실패 thread=%s: %v", sess.ThreadID, err)
	}
	return false
}

// dispatchFinalize는 format에 따라 적절한 Summarize* 메서드를 호출하고
// 해당 Render* 결과를 마크다운 문자열로 반환한다.
func dispatchFinalize(
	ctx context.Context,
	summ MeetingSummarizer,
	notes []llm.Note,
	speakers []string,
	now time.Time,
	format llm.NoteFormat,
	directive string,
) (string, error) {
	switch format {
	case llm.FormatDecisionStatus:
		resp, err := summ.SummarizeDecisionStatus(ctx, notes, speakers, now, directive)
		if err != nil {
			return "", err
		}
		return render.RenderDecisionStatus(render.DecisionStatusRenderInput{Date: now, Participants: speakers, Response: resp}), nil
	case llm.FormatDiscussion:
		resp, err := summ.SummarizeDiscussion(ctx, notes, speakers, now, directive)
		if err != nil {
			return "", err
		}
		return render.RenderDiscussion(render.DiscussionRenderInput{Date: now, Participants: speakers, Response: resp}), nil
	case llm.FormatRoleBased:
		resp, err := summ.SummarizeRoleBased(ctx, notes, speakers, now, directive)
		if err != nil {
			return "", err
		}
		return render.RenderRoleBased(render.RoleBasedRenderInput{Date: now, Participants: speakers, Response: resp}), nil
	case llm.FormatFreeform:
		resp, err := summ.SummarizeFreeform(ctx, notes, speakers, now, directive)
		if err != nil {
			return "", err
		}
		return render.RenderFreeform(render.FreeformRenderInput{Date: now, Participants: speakers, Response: resp}), nil
	default:
		// 미정의 포맷이 들어와도 동작은 보장 (legacy로 fallback). legacy는 directive 미적용.
		resp, err := summ.SummarizeMeeting(ctx, notes, speakers, now)
		if err != nil {
			return "", err
		}
		return render.RenderFinalNote(render.RenderInput{Date: now, Participants: speakers, Response: resp}), nil
	}
}

// emitInterim은 사용자가 [중간 요약] 버튼을 눌렀을 때 실행되는 중간 정리 로직.
// InterimInFlight 가드 → SummarizeInterim 호출 → 렌더 → [중간 요약][미팅 종료]
// 버튼 포함 메시지 전송. 노트가 0건이면 LLM 호출 없이 안내 메시지만 보낸다.
//
// 호출자(interactionCreate)는 버튼 응답을 ack한 뒤 이 함수를 부른다.
// 발사 실패 시에도 회의 흐름을 깨지 않도록 사용자에게는 안내 없이 로그만 남긴다.
func emitInterim(
	ctx context.Context,
	msg Messenger,
	summ MeetingSummarizer,
	sess *Session,
	now time.Time,
) {
	// 빠른 중복 클릭 방어. 이미 진행 중이면 조용히 무시.
	if !sess.TryStartManualInterim() {
		log.Printf("[미팅/interim] skip thread=%s (이미 진행 중)", sess.ThreadID)
		return
	}
	defer sess.FinishInterim()

	notes := sess.SnapshotNotes()
	speakers := sess.SortedSpeakers()

	if len(notes) == 0 {
		log.Printf("[미팅/interim] 빈 노트 thread=%s", sess.ThreadID)
		if _, err := msg.ChannelMessageSend(sess.ThreadID, "아직 수집된 메모가 없습니다."); err != nil {
			log.Printf("[미팅/interim] ERR 안내 전송 실패 thread=%s: %v", sess.ThreadID, err)
		}
		return
	}

	llmNotes := make([]llm.Note, len(notes))
	for i, n := range notes {
		llmNotes[i] = llm.Note{Author: n.Author, Content: n.Content}
	}

	start := time.Now()
	log.Printf("[미팅/interim] 호출 시작 thread=%s notes=%d speakers=%v", sess.ThreadID, len(llmNotes), speakers)
	resp, err := summ.SummarizeInterim(ctx, llmNotes, speakers, now)
	dur := time.Since(start)
	if err != nil {
		log.Printf("[미팅/interim] ERR thread=%s elapsed=%s err=%v", sess.ThreadID, dur, err)
		return
	}
	log.Printf("[미팅/interim] ok thread=%s elapsed=%s decisions=%d open_questions=%d",
		sess.ThreadID, dur, len(resp.Decisions), len(resp.OpenQuestions))

	rendered := render.RenderInterimNote(render.InterimRenderInput{
		Date:         now,
		Participants: speakers,
		Response:     resp,
	})

	if _, sendErr := sendLongMessageWithComponents(msg, sess.ThreadID, rendered, interimControlComponents()); sendErr != nil {
		log.Printf("[미팅/interim] ERR 전송 실패 thread=%s: %v", sess.ThreadID, sendErr)
		return
	}

	log.Printf("[미팅/interim] 전송 완료 thread=%s notes=%d", sess.ThreadID, len(notes))
}

// customIDMeetingEndBtn은 미팅 컨트롤 메시지의 "미팅 종료" 버튼 custom_id.
// interactionCreate에서 이 값으로 라우팅하여 4 포맷 선택 prompt를 띄운다.
const customIDMeetingEndBtn = "meeting_end_btn"

// customIDInterimBtn은 미팅 컨트롤 메시지의 "중간 요약" 버튼 custom_id.
// 사용자가 직접 누를 때만 emitInterim을 호출한다 (자동 발사는 폐기됨).
const customIDInterimBtn = "interim_btn"

// 4 포맷 선택 버튼 custom_id. 미팅 종료 prompt에서 노출된다.
const (
	customIDFinalizeDecisionStatus = "finalize_decision_status"
	customIDFinalizeDiscussion     = "finalize_discussion"
	customIDFinalizeRoleBased      = "finalize_role_based"
	customIDFinalizeFreeform       = "finalize_freeform"
)

// customIDDirectiveBtn은 미팅 종료 prompt의 "추가 요청" 버튼 custom_id.
// 클릭 시 사용자가 다음 채팅 메시지로 정리 지시를 입력할 수 있는 상태로 전환한다.
const customIDDirectiveBtn = "directive_btn"

// customIDDirectiveRetryBtn은 directive 적용 후 prompt에서 노출되는 "지시 다시 입력" 버튼.
// 누르면 기존 directive를 비우고 다시 입력 받는 상태로 전환.
const customIDDirectiveRetryBtn = "directive_retry_btn"

// formatFromCustomID는 finalize 버튼 custom_id를 NoteFormat으로 변환한다.
// ok=false면 알 수 없는 ID.
func formatFromCustomID(id string) (llm.NoteFormat, bool) {
	switch id {
	case customIDFinalizeDecisionStatus:
		return llm.FormatDecisionStatus, true
	case customIDFinalizeDiscussion:
		return llm.FormatDiscussion, true
	case customIDFinalizeRoleBased:
		return llm.FormatRoleBased, true
	case customIDFinalizeFreeform:
		return llm.FormatFreeform, true
	default:
		return 0, false
	}
}

// finalizePromptComponents는 미팅 종료 시 노출되는 4 포맷 + 추가 요청 버튼 행을 생성한다.
// Discord 1 row max 5 버튼 제약을 정확히 채운다.
func finalizePromptComponents() []discordgo.MessageComponent {
	return []discordgo.MessageComponent{
		discordgo.ActionsRow{
			Components: []discordgo.MessageComponent{
				discordgo.Button{Label: "결정+진행", Style: discordgo.PrimaryButton, CustomID: customIDFinalizeDecisionStatus},
				discordgo.Button{Label: "논의", Style: discordgo.PrimaryButton, CustomID: customIDFinalizeDiscussion},
				discordgo.Button{Label: "역할별", Style: discordgo.PrimaryButton, CustomID: customIDFinalizeRoleBased},
				discordgo.Button{Label: "자율", Style: discordgo.SecondaryButton, CustomID: customIDFinalizeFreeform},
				discordgo.Button{Label: "추가 요청", Style: discordgo.SecondaryButton, CustomID: customIDDirectiveBtn},
			},
		},
	}
}

// finalizePromptWithDirectiveComponents는 directive 적용 후 노출되는 prompt 버튼 행이다.
// 4 포맷 버튼은 directive를 반영해 finalize되고, [지시 다시 입력]은 directive 재입력 흐름.
func finalizePromptWithDirectiveComponents() []discordgo.MessageComponent {
	return []discordgo.MessageComponent{
		discordgo.ActionsRow{
			Components: []discordgo.MessageComponent{
				discordgo.Button{Label: "결정+진행", Style: discordgo.PrimaryButton, CustomID: customIDFinalizeDecisionStatus},
				discordgo.Button{Label: "논의", Style: discordgo.PrimaryButton, CustomID: customIDFinalizeDiscussion},
				discordgo.Button{Label: "역할별", Style: discordgo.PrimaryButton, CustomID: customIDFinalizeRoleBased},
				discordgo.Button{Label: "자율", Style: discordgo.SecondaryButton, CustomID: customIDFinalizeFreeform},
				discordgo.Button{Label: "지시 다시 입력", Style: discordgo.SecondaryButton, CustomID: customIDDirectiveRetryBtn},
			},
		},
	}
}

// interimControlComponents는 sticky / interim 결과 메시지에 공통으로 첨부되는
// [중간 요약][미팅 종료] 버튼 행을 생성한다.
func interimControlComponents() []discordgo.MessageComponent {
	return []discordgo.MessageComponent{
		discordgo.ActionsRow{
			Components: []discordgo.MessageComponent{
				discordgo.Button{
					Label:    "중간 요약",
					Style:    discordgo.PrimaryButton,
					CustomID: customIDInterimBtn,
				},
				discordgo.Button{
					Label:    "미팅 종료",
					Style:    discordgo.DangerButton,
					CustomID: customIDMeetingEndBtn,
				},
			},
		},
	}
}

// stickyRefreshThreshold는 몇 개 노트가 쌓이면 sticky 컨트롤 메시지를 재전송할지.
// 3이면 "메모 3개마다 봇이 스크롤 끝에서 미팅 종료 버튼을 재생성".
// 이 값을 낮추면 사용성은 좋지만 API 호출 부담이 늘고, 높이면 반대.
const stickyRefreshThreshold = 3

// buildStickyMessageSend는 sticky 컨트롤 메시지 payload를 생성한다.
// "미팅 진행 중 · 메모 N건 수집됨" 본문 + [중간 요약][미팅 종료] 버튼.
// 정보형 컨텐츠로 사용자가 봇이 살아있고 몇 개 메모가 수집됐는지 확인 가능.
func buildStickyMessageSend(noteCount int) *discordgo.MessageSend {
	return &discordgo.MessageSend{
		Content:    fmt.Sprintf("`미팅 진행 중` · 메모 **%d건** 수집됨", noteCount),
		Components: interimControlComponents(),
	}
}

// sendSticky는 현재 sticky 메시지를 삭제하고 최신 위치에 새로 전송한다.
// 미팅 시작 시 초기 sticky 전송에도 사용된다 (이전 ID가 ""면 삭제 스킵).
// 실패는 로그만 남기고 return - sticky는 best-effort UI이므로 에러 전파 없음.
func sendSticky(msg Messenger, sess *Session) {
	if oldID := sess.CurrentStickyID(); oldID != "" {
		if err := msg.ChannelMessageDelete(sess.ThreadID, oldID); err != nil {
			log.Printf("[미팅/sticky] WARN delete 실패 thread=%s msg=%s: %v", sess.ThreadID, oldID, err)
		}
	}
	count := len(sess.SnapshotNotes())
	m, err := msg.ChannelMessageSendComplex(sess.ThreadID, buildStickyMessageSend(count))
	if err != nil {
		log.Printf("[미팅/sticky] ERR 전송 실패 thread=%s: %v", sess.ThreadID, err)
		return
	}
	sess.SetStickyMessageID(m.ID)
	log.Printf("[미팅/sticky] 갱신 완료 thread=%s msg=%s notes=%d", sess.ThreadID, m.ID, count)
}

// maybeRefreshSticky는 threshold 조건을 검사하고 만족 시 sendSticky를 호출한다.
// 노트가 추가될 때마다 handleMeetingMessage에서 호출된다.
// ReserveStickyRefresh가 원자적으로 예약해 동시 호출에도 한 번만 발사.
func maybeRefreshSticky(msg Messenger, sess *Session) {
	oldID, should := sess.ReserveStickyRefresh(stickyRefreshThreshold)
	if !should {
		return
	}
	// oldID는 이미 ReserveStickyRefresh가 반환한 값. 여기서 별도 CurrentStickyID 조회 없이
	// 바로 delete 처리 후 새로 보내야 한다. sendSticky는 CurrentStickyID를 다시 읽으므로
	// 우리는 직접 delete + send 흐름을 수행.
	if oldID != "" {
		if err := msg.ChannelMessageDelete(sess.ThreadID, oldID); err != nil {
			log.Printf("[미팅/sticky] WARN delete 실패 thread=%s msg=%s: %v", sess.ThreadID, oldID, err)
		}
	}
	count := len(sess.SnapshotNotes())
	m, err := msg.ChannelMessageSendComplex(sess.ThreadID, buildStickyMessageSend(count))
	if err != nil {
		log.Printf("[미팅/sticky] ERR 전송 실패 thread=%s: %v", sess.ThreadID, err)
		return
	}
	sess.SetStickyMessageID(m.ID)
	log.Printf("[미팅/sticky] threshold 도달 갱신 thread=%s msg=%s notes=%d", sess.ThreadID, m.ID, count)
}

// =====================================================================
// Discord 2000자 제한 대응 — 메시지 분할 전송
// =====================================================================
//
// Discord 일반 메시지 content는 2000자(Unicode code point 기준) 하드 리밋.
// LLM 출력이 긴 회의 노트를 생성하면 쉽게 초과한다.
//
// 전략: 줄(\n) 단위로 분할하여 2000자 이내 chunk를 만든다.
// 줄 중간에서 끊지 않아 마크다운 서식이 유지된다.
// 단일 줄이 2000자를 넘으면 (극히 드묾) rune 단위 강제 분할.

const discordMaxLen = 2000

// splitByLines는 content를 줄 단위로 분할하여 각 chunk가 maxLen rune 이하가 되도록 한다.
// 반환 slice는 항상 1개 이상.
func splitByLines(content string, maxLen int) []string {
	lines := strings.Split(content, "\n")
	var chunks []string
	var current strings.Builder

	for _, line := range lines {
		lineRunes := []rune(line)

		// 단일 줄이 maxLen 초과면 rune 단위 강제 분할
		if len(lineRunes) > maxLen {
			if current.Len() > 0 {
				chunks = append(chunks, current.String())
				current.Reset()
			}
			for len(lineRunes) > 0 {
				end := maxLen
				if end > len(lineRunes) {
					end = len(lineRunes)
				}
				chunks = append(chunks, string(lineRunes[:end]))
				lineRunes = lineRunes[end:]
			}
			continue
		}

		// current + "\n" + line이 maxLen을 넘으면 current를 flush
		addition := len([]rune(line)) + 1 // +1 for \n
		if current.Len() > 0 && len([]rune(current.String()))+addition > maxLen {
			chunks = append(chunks, current.String())
			current.Reset()
		}

		if current.Len() > 0 {
			current.WriteString("\n")
		}
		current.WriteString(line)
	}

	if current.Len() > 0 {
		chunks = append(chunks, current.String())
	}
	if len(chunks) == 0 {
		chunks = []string{""}
	}
	return chunks
}

// sendLongMessage는 content가 2000자를 넘으면 줄 단위로 분할하여 여러 메시지로 전송한다.
// 2000자 이하면 한 번에 전송.
func sendLongMessage(msg Messenger, channelID, content string) error {
	chunks := splitByLines(content, discordMaxLen)
	for _, chunk := range chunks {
		if _, err := msg.ChannelMessageSend(channelID, chunk); err != nil {
			return err
		}
	}
	return nil
}

// sendLongMessageWithComponents는 content가 2000자를 넘으면 분할하되,
// components(버튼 등)는 마지막 chunk에만 첨부한다.
func sendLongMessageWithComponents(
	msg Messenger,
	channelID, content string,
	components []discordgo.MessageComponent,
) (*discordgo.Message, error) {
	chunks := splitByLines(content, discordMaxLen)

	// 마지막 전까지는 plain 전송
	for i := 0; i < len(chunks)-1; i++ {
		if _, err := msg.ChannelMessageSend(channelID, chunks[i]); err != nil {
			return nil, err
		}
	}

	// 마지막 chunk에 components 첨부
	last := chunks[len(chunks)-1]
	return msg.ChannelMessageSendComplex(channelID, &discordgo.MessageSend{
		Content:    last,
		Components: components,
	})
}

// deleteStickyIfPresent는 미팅 종료 시 호출되어 마지막 sticky 메시지를 정리한다.
// 최종 노트가 스레드에 남는 동안 sticky가 같이 남아있으면 혼란 유발.
func deleteStickyIfPresent(msg Messenger, sess *Session) {
	id := sess.ClearSticky()
	if id == "" {
		return
	}
	if err := msg.ChannelMessageDelete(sess.ThreadID, id); err != nil {
		log.Printf("[미팅/sticky] WARN 종료 시 delete 실패 thread=%s msg=%s: %v", sess.ThreadID, id, err)
		return
	}
	log.Printf("[미팅/sticky] 종료 정리 thread=%s msg=%s", sess.ThreadID, id)
}
