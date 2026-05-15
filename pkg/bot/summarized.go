package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"sync/atomic"
	"time"

	"chatbot-alpha-1/pkg/db"
	"chatbot-alpha-1/pkg/llm"
	"chatbot-alpha-1/pkg/llm/render"
	"chatbot-alpha-1/pkg/llm/summarize"

	"github.com/bwmarrin/discordgo"
)

// =====================================================================
// Phase 2 — SummarizedContent 추출 + DB persist 어댑터
//
// 호출 흐름 (handler에서 [정리본] button 클릭 시):
//   1. PrepareContentExtractionInput(sess.Notes, sess.RolesSnapshot, time.Now())
//      → corpus 분리 (HumanNotes / ContextNotes), SpeakerRoles 매핑
//   2. summ.ExtractContent(ctx, input)
//      → LLM 1회 호출, *llm.SummarizedContent 반환
//   3. PersistSummarizedContent(ctx, sess, content)
//      → DB summarized_contents row insert (best-effort), ID 반환
//   4. 후속 토글에서 PersistFinalizeRun으로 렌더 결과 캐시
// =====================================================================

// PrepareContentExtractionInput는 세션의 in-memory Notes를 ContentExtractionInput으로 변환한다 (pure).
//
// 거시 디자인 결정 6 (Source 라벨 환각 방어) 핵심:
//   - Source.IsAttributionCandidate()=true (Human) → HumanNotes
//   - Source.IsInCorpus()=true 그 외 (WeeklyDump/ReleaseResult/AgentOutput/ExternalPaste) → ContextNotes
//   - Source.IsInCorpus()=false (InterimSummary) → 제외
//
// SpeakerRoles는 Source=Human 발화자만 포함 (도구 author/외부 paste author는 attribution 후보 X).
// note.AuthorRoles 우선, 비어있으면 sessionRoles[note.AuthorID] fallback.
//
// 반환 input의 Speakers는 정렬됨 (SortedHumanSpeakers와 같은 정책).
func PrepareContentExtractionInput(
	notes []Note,
	sessionRoles map[string][]string,
	date time.Time,
) summarize.ContentExtractionInput {
	var humanNotes, contextNotes []llm.Note
	speakerSet := make(map[string]bool)
	speakerRoles := make(map[string][]string)

	for _, n := range notes {
		ln := llm.Note{Author: n.Author, Content: n.Content}
		switch {
		case n.Source.IsAttributionCandidate():
			humanNotes = append(humanNotes, ln)
			if n.Author == "" {
				continue
			}
			speakerSet[n.Author] = true
			// 발화 시점 snapshot 우선, 없으면 세션 RolesSnapshot fallback
			if len(n.AuthorRoles) > 0 {
				speakerRoles[n.Author] = n.AuthorRoles
			} else if r, ok := sessionRoles[n.AuthorID]; ok && len(r) > 0 {
				speakerRoles[n.Author] = r
			}
		case n.Source.IsInCorpus():
			contextNotes = append(contextNotes, ln)
		}
	}

	speakers := make([]string, 0, len(speakerSet))
	for s := range speakerSet {
		speakers = append(speakers, s)
	}
	sort.Strings(speakers)

	return summarize.ContentExtractionInput{
		Date:         date,
		Speakers:     speakers,
		SpeakerRoles: speakerRoles,
		HumanNotes:   humanNotes,
		ContextNotes: contextNotes,
	}
}

// =====================================================================
// DB persist (best-effort)
// =====================================================================

// idCounter는 같은 ns에 두 ID 생성 호출이 들어와도 충돌하지 않도록 monotonic suffix를 보장.
// chunk 3c 토글 핸들러가 같은 SummarizedContent에 대해 finalize_run을 빠르게 연속 insert할 때
// nano 단독 ID는 PRIMARY KEY 위반 위험 → atomic counter suffix로 보강.
//
// 단일 봇 인스턴스 가정 (replicas:1). 다중 인스턴스 도입 시 instance prefix 추가 필요.
var idCounter atomic.Uint64

// newSummarizedContentID는 summarized_contents.id 생성.
// 형식: sc_<unix_nano>_<counter> — 같은 ns 호출도 unique.
func newSummarizedContentID() string {
	return fmt.Sprintf("sc_%d_%d", time.Now().UnixNano(), idCounter.Add(1))
}

// newFinalizeRunID는 finalize_runs.id 생성. 같은 패턴.
// 토글 핸들러가 빠르게 연속 호출되는 시나리오에서 충돌 차단.
func newFinalizeRunID() string {
	return fmt.Sprintf("fr_%d_%d", time.Now().UnixNano(), idCounter.Add(1))
}

// PersistSummarizedContent는 추출된 SummarizedContent를 DB에 저장하고 ID를 반환한다.
// best-effort — dbConn nil / sess.DBSessionID 비음 / marshal·insert 실패 시 빈 문자열.
//
// 후속 PersistFinalizeRun 호출이 이 ID를 FK로 참조한다.
func PersistSummarizedContent(ctx context.Context, sess *Session, content *llm.SummarizedContent) string {
	if dbConn == nil || sess == nil || sess.DBSessionID == "" || content == nil {
		return ""
	}
	raw, err := json.Marshal(content)
	if err != nil {
		log.Printf("[db] WARN summarized marshal 실패 thread=%s: %v", sess.ThreadID, err)
		return ""
	}
	id := newSummarizedContentID()
	row := db.SummarizedContent{
		ID:          id,
		SessionID:   sess.DBSessionID,
		Content:     raw,
		ExtractedAt: time.Now(),
	}
	if err := dbConn.InsertSummarizedContent(ctx, row); err != nil {
		log.Printf("[db] WARN summarized insert 실패 thread=%s: %v (in-memory only)", sess.ThreadID, err)
		return ""
	}
	log.Printf("[db] SummarizedContent 저장 thread=%s sc=%s decisions=%d actions=%d topics=%d",
		sess.ThreadID, id, len(content.Decisions), len(content.Actions), len(content.Topics))
	return id
}

// PersistFinalizeRun는 정리본 토글 결과(렌더된 markdown)를 DB에 저장한다.
// summarizedID가 "" 또는 dbConn nil이면 noop.
// best-effort — 실패 시 log warn, 호출자 흐름은 계속.
func PersistFinalizeRun(ctx context.Context, summarizedID string, format db.FormatKind, directive, outputMD string) {
	if dbConn == nil || summarizedID == "" {
		return
	}
	row := db.FinalizeRun{
		ID:                  newFinalizeRunID(),
		SummarizedContentID: summarizedID,
		Format:              format,
		Directive:           directive,
		OutputMD:            outputMD,
		CreatedAt:           time.Now(),
	}
	if err := dbConn.InsertFinalizeRun(ctx, row); err != nil {
		log.Printf("[db] WARN finalize_run insert 실패 sc=%s format=%s: %v", summarizedID, format, err)
	}
}

// =====================================================================
// finalizeSummarized — Phase 2 정리본 통합 button 핸들러 (legacy 4 button과 공존)
//
// 거시 디자인 결정 A: SummarizedContent 1회 추출 → 4 포맷 토글 메시지로 사용자에 노출.
//
// 흐름:
//   1. 빈 corpus 체크 — 발화 없으면 안내 후 종료
//   2. PrepareContentExtractionInput으로 corpus 분리
//   3. summ.ExtractContent (LLM 1회)
//   4. PersistSummarizedContent (DB best-effort)
//   5. default 포맷(decision_status)으로 렌더 + 메시지 전송
//      (chunk 3c에서 4 포맷 토글 button 첨부 추가 예정)
//
// keepSession은 finalizeMeeting과 동일한 의미 — true면 세션 보존(에러), false면 정리.
// =====================================================================

// FinalizeSummarized는 정리본 통합 button 클릭 시 호출되는 핵심 로직.
//
// 인자:
//   - msg: Discord 메시지 전송 인터페이스 (테스트 가능성)
//   - summ: MeetingSummarizer (ExtractContent 호출)
//   - sess: 현재 세션 (Notes / RolesSnapshot / DBSessionID)
//   - now: 미팅 날짜 (헤더용)
//
// 반환 keepSession=true면 세션 보존 (에러 재시도 가능), false면 정리.
func FinalizeSummarized(
	ctx context.Context,
	msg Messenger,
	summ MeetingSummarizer,
	sess *Session,
	now time.Time,
) (keepSession bool) {
	notes := sess.SnapshotNotesForCorpus()
	if len(notes) == 0 {
		log.Printf("[미팅/finalize_summarized] 빈 corpus thread=%s", sess.ThreadID)
		if _, err := msg.ChannelMessageSend(sess.ThreadID, "기록된 메모가 없어 정리본을 생성하지 않았습니다.\n조금 더 대화한 뒤 다시 [정리본 통합·토글] 버튼을 눌러주세요."); err != nil {
			log.Printf("[미팅/finalize_summarized] ERR 안내 전송 실패: %v", err)
		}
		// keepSession=true — Phase 2 정리본 button은 미팅 중간에도 클릭 가능한 도구.
		// 빈 corpus는 에러가 아닌 "노트 없음" 상태이므로 세션 보존 (legacy finalizeMeeting과 정책 차이).
		return true
	}

	// chunk 4 — progress 표시 시작 (LLM 호출 동안 ASCII 바 + 단계 + 경과 시간).
	// 5단계: corpus 분리 / LLM 호출+응답 대기 / 파싱·validate / DB persist / 메시지 전송
	// (LLM 호출과 응답 대기는 ticker 주기보다 짧은 간격으로 연속이라 단일 단계로 합침 — 사용자 가시성)
	progress := StartProgress(ctx, msg, sess.ThreadID, "정리본 추출", 5)
	defer progress.Finish()

	progress.SetStage(1, "corpus 분리")

	// corpus 분리 — Source 라벨 기반 (환각 방어 게이트).
	// RolesSnapshot은 NotesMu 보호. 다른 goroutine의 messageCreate가 GetOrFetchRoles로
	// snapshot을 mutate할 수 있으므로 사본을 만들어 lock 밖에서 안전하게 사용한다.
	sess.NotesMu.Lock()
	rolesCopy := make(map[string][]string, len(sess.RolesSnapshot))
	for k, v := range sess.RolesSnapshot {
		rolesCopy[k] = v
	}
	sess.NotesMu.Unlock()

	in := PrepareContentExtractionInput(notes, rolesCopy, now)

	log.Printf("[미팅/finalize_summarized] 시작 thread=%s human_notes=%d context_notes=%d speakers=%d",
		sess.ThreadID, len(in.HumanNotes), len(in.ContextNotes), len(in.Speakers))

	progress.SetStage(2, "LLM 호출 및 응답 대기")
	content, err := summ.ExtractContent(ctx, in)
	if err != nil {
		log.Printf("[미팅/finalize_summarized] ERR ExtractContent thread=%s: %v", sess.ThreadID, err)
		if _, sendErr := msg.ChannelMessageSend(sess.ThreadID, fmt.Sprintf("정리본 추출에 실패했습니다: %v\n다시 [정리본] 버튼을 눌러주세요.", err)); sendErr != nil {
			log.Printf("[미팅/finalize_summarized] ERR 에러 안내 전송 실패: %v", sendErr)
		}
		return true // 세션 보존 — 사용자가 재시도 가능
	}

	progress.SetStage(3, "응답 파싱·validate")

	// DB persist (best-effort)
	progress.SetStage(4, "DB 저장")
	scID := PersistSummarizedContent(ctx, sess, content)

	progress.SetStage(5, "메시지 전송")
	// chunk 3c: default 포맷으로 렌더 + 4 포맷 토글 button row 첨부.
	// 사용자가 클릭 시 메시지 edit으로 즉시 전환 — LLM 재호출 X.
	defaultFormat := llm.FormatDecisionStatus
	rendered := renderSummarizedByFormat(content, defaultFormat, in.Speakers, in.SpeakerRoles, now)

	// 정리본 메시지 전송 — content + 4 포맷 토글 button row 첨부.
	// Messenger.ChannelMessageSendComplex는 인터페이스에 정의돼 있고 fakeMessenger도 구현 중.
	if _, err := msg.ChannelMessageSendComplex(sess.ThreadID, &discordgo.MessageSend{
		Content:    rendered,
		Components: formatToggleComponents(defaultFormat),
	}); err != nil {
		log.Printf("[미팅/finalize_summarized] ERR 정리본+토글 전송 실패: %v", err)
		return true
	}

	// FinalizeRun persist (default 포맷)
	PersistFinalizeRun(ctx, scID, db.FormatDecisionStatus, "", rendered)

	log.Printf("[미팅/finalize_summarized] 완료 thread=%s sc=%s actions=%d topics=%d default_format=%s",
		sess.ThreadID, scID, len(content.Actions), len(content.Topics), defaultFormat)
	return false
}

// renderSummarizedByFormat은 SummarizedContent를 지정 포맷으로 렌더하는 dispatch.
// chunk 3c의 토글 핸들러도 같은 함수를 재사용하여 동일 콘텐츠 다른 포맷을 즉시 변환.
//
// 이 함수는 pkg/llm/render의 4 RenderSummarized* 순수 함수를 thin wrapper로 호출 —
// LLM 재호출 없음 (거시 디자인 결정 A).
func renderSummarizedByFormat(
	content *llm.SummarizedContent,
	format llm.NoteFormat,
	speakers []string,
	roles map[string][]string,
	date time.Time,
) string {
	in := render.SummarizedRenderInput{
		Content:       content,
		Date:          date,
		Speakers:      speakers,
		RolesSnapshot: roles,
	}
	switch format {
	case llm.FormatDecisionStatus:
		return render.RenderSummarizedDecisionStatus(in)
	case llm.FormatDiscussion:
		return render.RenderSummarizedDiscussion(in)
	case llm.FormatRoleBased:
		return render.RenderSummarizedRoleBased(in)
	case llm.FormatFreeform:
		return render.RenderSummarizedFreeform(in)
	default:
		return render.RenderSummarizedDecisionStatus(in)
	}
}

// =====================================================================
// chunk 3c — 정리본 메시지의 4 포맷 토글 button + 토글 핸들러
// =====================================================================

// formatToggleComponents는 정리본 메시지에 첨부되는 4 포맷 토글 button row를 생성한다.
// 모든 4 button을 한 row에 (Discord 5-button-per-row 제약 안에 적합).
//
// 활성 포맷 button은 SuccessButton (강조), 나머지는 SecondaryButton.
func formatToggleComponents(active llm.NoteFormat) []discordgo.MessageComponent {
	style := func(f llm.NoteFormat) discordgo.ButtonStyle {
		if f == active {
			return discordgo.SuccessButton
		}
		return discordgo.SecondaryButton
	}
	return []discordgo.MessageComponent{
		discordgo.ActionsRow{
			Components: []discordgo.MessageComponent{
				discordgo.Button{Label: "결정+진행", Style: style(llm.FormatDecisionStatus), CustomID: customIDFormatToggleDecisionStatus},
				discordgo.Button{Label: "논의", Style: style(llm.FormatDiscussion), CustomID: customIDFormatToggleDiscussion},
				discordgo.Button{Label: "역할별", Style: style(llm.FormatRoleBased), CustomID: customIDFormatToggleRoleBased},
				discordgo.Button{Label: "자율", Style: style(llm.FormatFreeform), CustomID: customIDFormatToggleFreeform},
			},
		},
	}
}

// formatFromToggleCustomID는 토글 button custom_id를 NoteFormat으로 변환한다.
// formatFromCustomID(legacy finalize button)와 별개 — toggle은 다른 customID 네임스페이스.
func formatFromToggleCustomID(id string) (llm.NoteFormat, bool) {
	switch id {
	case customIDFormatToggleDecisionStatus:
		return llm.FormatDecisionStatus, true
	case customIDFormatToggleDiscussion:
		return llm.FormatDiscussion, true
	case customIDFormatToggleRoleBased:
		return llm.FormatRoleBased, true
	case customIDFormatToggleFreeform:
		return llm.FormatFreeform, true
	default:
		return 0, false
	}
}

// formatToDBKind은 llm.NoteFormat을 db.FormatKind로 변환 (DB persist용).
func formatToDBKind(f llm.NoteFormat) db.FormatKind {
	switch f {
	case llm.FormatDecisionStatus:
		return db.FormatDecisionStatus
	case llm.FormatDiscussion:
		return db.FormatDiscussion
	case llm.FormatRoleBased:
		return db.FormatRoleBased
	case llm.FormatFreeform:
		return db.FormatFreeform
	default:
		return db.FormatDecisionStatus
	}
}

// HandleFormatToggle은 정리본 메시지의 4 포맷 토글 button 클릭 핸들러.
//
// 흐름:
//  1. 새 포맷 추출 (customID에서)
//  2. DB에서 sess의 latest SummarizedContent 조회 — LLM 재호출 X (거시 결정 A)
//  3. 새 포맷으로 렌더 (순수 함수)
//  4. 메시지 edit + 토글 button row 갱신 (active 강조)
//  5. FinalizeRun persist (이력 audit)
//
// 호출 계약: 호출자(discord.go interactionCreate)가 이미 InteractionResponseDeferredMessageUpdate로
// ACK를 완료한 상태. 따라서 사용자 피드백은 InteractionResponse* 대신 FollowupMessageCreate로
// 보내야 한다 (두 번째 InteractionRespond는 "interaction already acknowledged" 에러).
func HandleFormatToggle(
	ctx context.Context,
	s *discordgo.Session,
	i *discordgo.InteractionCreate,
	sess *Session,
	customID string,
) {
	// 에러 fallback — deferred ACK 이후이므로 ephemeral followup으로 안내.
	sendFollowup := func(msg string) {
		_, err := s.FollowupMessageCreate(i.Interaction, true, &discordgo.WebhookParams{
			Content: msg,
			Flags:   discordgo.MessageFlagsEphemeral,
		})
		if err != nil {
			log.Printf("[미팅/format_toggle] ERR followup 전송 실패 thread=%s: %v", sess.ThreadID, err)
		}
	}

	format, ok := formatFromToggleCustomID(customID)
	if !ok {
		sendFollowup("알 수 없는 포맷 토글입니다.")
		return
	}

	if dbConn == nil || sess.DBSessionID == "" {
		sendFollowup("정리본 데이터를 찾을 수 없습니다 (DB 미연결). 다시 [정리본 통합·토글] 버튼을 눌러주세요.")
		return
	}

	scRow, err := dbConn.GetLatestSummarizedContent(ctx, sess.DBSessionID)
	if err != nil {
		log.Printf("[미팅/format_toggle] ERR GetLatestSummarizedContent thread=%s: %v", sess.ThreadID, err)
		sendFollowup("정리본 데이터를 찾을 수 없습니다. 다시 [정리본 통합·토글] 버튼을 눌러주세요.")
		return
	}

	var content llm.SummarizedContent
	if err := json.Unmarshal(scRow.Content, &content); err != nil {
		log.Printf("[미팅/format_toggle] ERR unmarshal sc=%s: %v", scRow.ID, err)
		sendFollowup("정리본 파싱에 실패했습니다.")
		return
	}

	// 렌더 입력 — speakers/roles는 in-memory 세션 또는 DB에서. Phase 2에선 in-memory 우선.
	speakers := sess.SortedHumanSpeakers()
	sess.NotesMu.Lock()
	rolesCopy := make(map[string][]string, len(sess.RolesSnapshot))
	for k, v := range sess.RolesSnapshot {
		rolesCopy[k] = v
	}
	sess.NotesMu.Unlock()

	rendered := renderSummarizedByFormat(&content, format, speakers, rolesCopy, scRow.ExtractedAt)

	// 메시지 edit — interaction의 원본 메시지 갱신
	editMsg := &discordgo.WebhookEdit{
		Content:    &rendered,
		Components: ptrComponents(formatToggleComponents(format)),
	}
	if _, err := s.InteractionResponseEdit(i.Interaction, editMsg); err != nil {
		log.Printf("[미팅/format_toggle] ERR InteractionResponseEdit thread=%s: %v", sess.ThreadID, err)
		// fallback: ChannelMessageEdit (interaction이 deferred가 아닌 경우 대비)
	}

	// FinalizeRun persist (이력)
	PersistFinalizeRun(ctx, scRow.ID, formatToDBKind(format), "", rendered)

	log.Printf("[미팅/format_toggle] 완료 thread=%s sc=%s format=%s rendered_bytes=%d",
		sess.ThreadID, scRow.ID, format, len(rendered))
}
