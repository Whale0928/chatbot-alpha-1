package bot

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
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

// labelForContextSource는 NoteSource를 Stage 3 prompt가 기대하는 author 라벨로 매핑.
// ExternalPaste는 사람 username을 유지 (외부 paste는 author 정보가 의미 있음).
func labelForContextSource(src db.NoteSource, origAuthor string) string {
	switch src {
	case db.SourceWeeklyDump:
		return "[weekly]"
	case db.SourceReleaseResult:
		return "[release]"
	case db.SourceAgentOutput:
		return "[agent]"
	default:
		return origAuthor
	}
}

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
			// NoteSource → author prefix 강제 매핑 (Stage 3 prompt가 라벨 기반 분류).
			// 호출자가 어떤 author로 박았든 LLM payload는 결정적.
			// ExternalPaste만 원본 author 유지 (사람이 붙여넣은 것이라 username 의미 있음).
			ln.Author = labelForContextSource(n.Source, n.Author)
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
//   3. summ.ExtractContent (Stage 3 LLM)
//   4. PersistSummarizedContent (DB best-effort)
//   5. default 포맷(decision_status)으로 summ.RenderFormat (Stage 4 LLM)
//   6. embed 메시지 전송 + default 포맷 finalize_run 캐시 저장
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

	progress.SetStage(2, "LLM 호출 — 1차 정리 (ExtractContent)")
	content, err := summ.ExtractContent(ctx, in)
	if err != nil {
		log.Printf("[미팅/finalize_summarized] ERR ExtractContent thread=%s: %v", sess.ThreadID, err)
		if _, sendErr := msg.ChannelMessageSend(sess.ThreadID, fmt.Sprintf("정리본 추출에 실패했습니다: %v\n다시 [정리본] 버튼을 눌러주세요.", err)); sendErr != nil {
			log.Printf("[미팅/finalize_summarized] ERR 에러 안내 전송 실패: %v", sendErr)
		}
		return true // 세션 보존 — 사용자가 재시도 가능
	}

	progress.SetStage(3, "응답 파싱 / DB 저장")
	scID := PersistSummarizedContent(ctx, sess, content)

	defaultFormat := llm.FormatDecisionStatus
	progress.SetStage(4, "LLM 호출 — 포맷 렌더 (RenderFormat)")
	rendered, usedFallback := renderFormatWithPureFallback(ctx, summ, summarize.FormatRenderInput{
		Content:      content,
		Format:       defaultFormat,
		Date:         now,
		Speakers:     in.Speakers,
		SpeakerRoles: in.SpeakerRoles,
		Directive:    "",
	}, "미팅/finalize_summarized", sess.ThreadID, scID)
	if !usedFallback {
		log.Printf("[미팅/finalize_summarized] llm_render_ok thread=%s format=%s sc=%s rendered_bytes=%d",
			sess.ThreadID, defaultFormat, scID, len(rendered))
	}

	progress.SetStage(5, "메시지 전송")
	// 정리본 메시지 전송 — embed로 보내 Discord 4096자 한도 활용 (plain content는 2000자 한도).
	// 운영 사고(2026-05-15): plain content로 보내다 LLM 출력이 2000자 초과 시 HTTP 400 →
	// "정리본을 추출하는 중..." stuck. embed.Description은 4096자라 대부분 케이스 커버.
	embed, truncated := buildSummarizedEmbed(rendered)
	if truncated {
		log.Printf("[미팅/finalize_summarized] WARN rendered가 4096자 초과 — truncate (원본 %d자)", len([]rune(rendered)))
	}
	if _, err := msg.ChannelMessageSendComplex(sess.ThreadID, &discordgo.MessageSend{
		Embeds:     []*discordgo.MessageEmbed{embed},
		Components: formatToggleComponents(defaultFormat),
	}); err != nil {
		log.Printf("[미팅/finalize_summarized] ERR 정리본+토글 전송 실패: %v", err)
		return true
	}

	// FinalizeRun persist (default 포맷). Fallback은 LLM 재시도 기회를 보존하기 위해 캐시하지 않는다.
	if !usedFallback {
		PersistFinalizeRun(ctx, scID, db.FormatDecisionStatus, "", rendered)
	}

	log.Printf("[미팅/finalize_summarized] 완료 thread=%s sc=%s actions=%d topics=%d default_format=%s",
		sess.ThreadID, scID, len(content.Actions), len(content.Topics), defaultFormat)
	return false
}

func renderFormatWithPureFallback(
	ctx context.Context,
	summ MeetingSummarizer,
	in summarize.FormatRenderInput,
	scope string,
	threadID string,
	summarizedID string,
) (string, bool) {
	if summ == nil {
		log.Printf("[%s] WARN LLM 실패 — pure render fallback thread=%s format=%s sc=%s err=summarizer nil",
			scope, threadID, in.Format, summarizedID)
		return renderSummarizedByFormat(in.Content, in.Format, in.Speakers, in.SpeakerRoles, in.Date), true
	}
	rendered, err := summ.RenderFormat(ctx, in)
	if err != nil {
		log.Printf("[%s] WARN LLM 실패 — pure render fallback thread=%s format=%s sc=%s err=%v",
			scope, threadID, in.Format, summarizedID, err)
		return renderSummarizedByFormat(in.Content, in.Format, in.Speakers, in.SpeakerRoles, in.Date), true
	}
	return rendered, false
}

// renderSummarizedByFormat은 SummarizedContent를 지정 포맷으로 pure render하는 dispatch.
//
// Deprecated: Stage 4 LLM (summarize.RenderFormat)으로 대체. fallback 용도로만 호출 가능 (LLM 장애 시).
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

// formatToggleComponents는 정리본 메시지에 첨부되는 4 포맷 토글 + [복사] button row를 생성한다.
// Discord는 한 ActionsRow에 button 5개까지 — 토글 4 + 복사 1 = 5, 한 row에 모두 적합.
//
// 활성 포맷 button은 SuccessButton (강조), 나머지는 SecondaryButton.
// [복사] button은 PrimaryButton (시각 구분, 항상 active 무관).
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
				discordgo.Button{Label: "복사", Style: discordgo.PrimaryButton, CustomID: customIDFormatCopy},
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

// activeFormatFromMessage는 정리본 메시지 button row에서 SuccessButton(active 토글) 의
// customID를 찾아 현재 활성 포맷을 식별한다. 식별 실패 시 (false) 반환.
//
// 사용: HandleFormatCopy가 embed.Description(잘릴 수 있음) 대신 DB의 full markdown을
// 조회하기 위해 활성 포맷을 알아야 한다.
//
// 메시지 구조: ActionsRow [Button×4(토글) + Button×1(복사)]. SuccessButton 1개는 active 토글.
// 복사 button은 PrimaryButton이라 매치 X. 토글 customID에 매칭되지 않는 button은 무시.
func activeFormatFromMessage(msg *discordgo.Message) (llm.NoteFormat, bool) {
	if msg == nil {
		return 0, false
	}
	for _, comp := range msg.Components {
		row, ok := comp.(*discordgo.ActionsRow)
		if !ok {
			continue
		}
		for _, sub := range row.Components {
			btn, ok := sub.(*discordgo.Button)
			if !ok {
				continue
			}
			if btn.Style != discordgo.SuccessButton {
				continue
			}
			if f, ok := formatFromToggleCustomID(btn.CustomID); ok {
				return f, true
			}
		}
	}
	return 0, false
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
//  2. DB에서 sess의 latest SummarizedContent 조회
//  3. finalize_runs cache 조회
//  4. cache miss면 SummarizedContent를 새 포맷으로 LLM 재렌더
//  5. 메시지 edit + 토글 button row 갱신 (active 강조)
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
		logGuard("meeting/format_toggle", "unknown_custom_id", "포맷 토글 customID 매칭 실패",
			lf("thread", sess.ThreadID), lf("custom_id", customID))
		sendFollowup("알 수 없는 포맷 토글입니다.")
		return
	}

	if dbConn == nil || sess.DBSessionID == "" {
		// 운영 사고 fix (2026-05-15): 이전엔 silent return으로 디버깅 불가능 — 사용자 ephemeral followup만
		// 받고 봇은 응답 안 하는 것처럼 보였음. logGuard로 진단 컨텍스트 박제.
		reason := "db_nil"
		if dbConn != nil {
			reason = "db_session_id_empty"
		}
		logGuard("meeting/format_toggle", reason,
			"DB 조회 불가 — 사용자에게 ephemeral 안내 + skip",
			lf("thread", sess.ThreadID), lf("custom_id", customID),
			lf("db_session_id", sess.DBSessionID))
		sendFollowup("정리본 데이터를 찾을 수 없습니다 (DB 미연결 또는 세션 만료). 다시 [회의록 정리] 버튼을 눌러주세요.")
		return
	}

	scRow, err := dbConn.GetLatestSummarizedContent(ctx, sess.DBSessionID)
	if err != nil {
		log.Printf("[미팅/format_toggle] ERR GetLatestSummarizedContent thread=%s: %v", sess.ThreadID, err)
		sendFollowup("정리본 데이터를 찾을 수 없습니다. 다시 [정리본 통합·토글] 버튼을 눌러주세요.")
		return
	}

	formatKind := formatToDBKind(format)
	existing, err := dbConn.GetFinalizeRunByFormat(ctx, scRow.ID, formatKind)
	var rendered string
	switch {
	case err == nil && existing != nil:
		rendered = existing.OutputMD
		log.Printf("[meeting/format_toggle] cache_hit thread=%s format=%s sc=%s",
			sess.ThreadID, format, scRow.ID)
	case errors.Is(err, sql.ErrNoRows):
		log.Printf("[meeting/format_toggle] cache_miss thread=%s format=%s sc=%s — LLM call",
			sess.ThreadID, format, scRow.ID)

		// Codex review (PR #13) P2: parse 먼저 — placeholder edit 전에 검증.
		// 옛 순서로는 parse 실패 시 메시지가 "다시 만드는 중"으로 영구 stuck됐다.
		var content llm.SummarizedContent
		if err := json.Unmarshal(scRow.Content, &content); err != nil {
			log.Printf("[미팅/format_toggle] ERR unmarshal sc=%s: %v", scRow.ID, err)
			sendFollowup("정리본 파싱에 실패했습니다.")
			return
		}

		// Copilot review (PR #13 #3) P2: 인플라이트 가드.
		// LLM 호출 3-10초 동안 사용자가 다른 토글 button 연속 클릭 시 중복 LLM 호출 가능.
		// 첫 cache miss 호출이 끝날 때까지 다른 토글은 거부 (ephemeral followup).
		// cache hit은 InFlight 무관 (이미 위 case에서 return).
		sess.NotesMu.Lock()
		if sess.FormatToggleInFlight {
			sess.NotesMu.Unlock()
			logGuard("meeting/format_toggle", "in_flight",
				"이미 다른 포맷 cache miss LLM 호출 진행 중 — 중복 클릭 거부",
				lf("thread", sess.ThreadID), lf("custom_id", customID))
			sendFollowup("이전 포맷 변환이 아직 진행 중입니다. 잠시 기다린 뒤 다시 눌러주세요.")
			return
		}
		sess.FormatToggleInFlight = true
		sess.NotesMu.Unlock()
		// 어떤 분기로 끝나든 InFlight 해제 — panic/return 무관.
		defer func() {
			sess.NotesMu.Lock()
			sess.FormatToggleInFlight = false
			sess.NotesMu.Unlock()
		}()

		// Option 2 UX — cache miss는 LLM 호출 3-10초 걸려서 사용자가 "버튼 눌렀는데 반응 없음"으로 체감.
		// parse 성공 후 placeholder embed로 edit해서 시각 피드백 제공.
		// active 토글 button은 새 포맷으로 미리 강조 → 어떤 포맷 로딩 중인지 명확.
		placeholderEmbed := &discordgo.MessageEmbed{
			Description: fmt.Sprintf("**%s** 포맷으로 정리본을 다시 만드는 중입니다…\n\n잠시만 기다려주세요. (보통 3~10초 소요)",
				labelForFormat(format)),
		}
		emptyContent := ""
		placeholderEdit := &discordgo.WebhookEdit{
			Content:    &emptyContent,
			Embeds:     &[]*discordgo.MessageEmbed{placeholderEmbed},
			Components: ptrComponents(formatToggleComponents(format)),
		}
		if _, err := s.InteractionResponseEdit(i.Interaction, placeholderEdit); err != nil {
			// placeholder 실패는 치명 X — 로그만 남기고 LLM 호출 진행. 최종 edit에서 다시 시도.
			log.Printf("[미팅/format_toggle] WARN placeholder edit 실패 (LLM 호출은 계속) thread=%s: %v",
				sess.ThreadID, err)
		}

		// 렌더 입력 — speakers/roles는 in-memory 세션 또는 DB에서. Phase 2에선 in-memory 우선.
		speakers := sess.SortedHumanSpeakers()
		sess.NotesMu.Lock()
		rolesCopy := make(map[string][]string, len(sess.RolesSnapshot))
		for k, v := range sess.RolesSnapshot {
			rolesCopy[k] = v
		}
		sess.NotesMu.Unlock()

		start := time.Now()
		var usedFallback bool
		rendered, usedFallback = renderFormatWithPureFallback(ctx, summarizer, summarize.FormatRenderInput{
			Content:      &content,
			Format:       format,
			Date:         scRow.ExtractedAt,
			Speakers:     speakers,
			SpeakerRoles: rolesCopy,
			Directive:    "",
		}, "meeting/format_toggle", sess.ThreadID, scRow.ID)
		elapsed := time.Since(start)
		if !usedFallback {
			log.Printf("[meeting/format_toggle] llm_render_ok thread=%s format=%s sc=%s elapsed=%s rendered_bytes=%d",
				sess.ThreadID, format, scRow.ID, elapsed, len(rendered))
		} else {
			log.Printf("[meeting/format_toggle] pure_render_fallback_ok thread=%s format=%s sc=%s elapsed=%s rendered_bytes=%d",
				sess.ThreadID, format, scRow.ID, elapsed, len(rendered))
		}
		if !usedFallback {
			PersistFinalizeRun(ctx, scRow.ID, formatKind, "", rendered)
		}
	default:
		log.Printf("[미팅/format_toggle] ERR GetFinalizeRunByFormat thread=%s sc=%s format=%s: %v",
			sess.ThreadID, scRow.ID, format, err)
		sendFollowup("정리본 캐시 조회에 실패했습니다. 다시 토글 button을 눌러주세요.")
		return
	}

	// 메시지 edit — interaction의 원본 메시지를 embed로 갱신 (FinalizeSummarized 초기 send와 동일 형식).
	// content 필드 비우고 embeds로 교체 — Discord는 둘 중 하나만 있어도 OK.
	embed, truncated := buildSummarizedEmbed(rendered)
	if truncated {
		log.Printf("[미팅/format_toggle] WARN rendered가 4096자 초과 — truncate (원본 %d자)", len([]rune(rendered)))
	}
	emptyContent := ""
	editMsg := &discordgo.WebhookEdit{
		Content:    &emptyContent,
		Embeds:     &[]*discordgo.MessageEmbed{embed},
		Components: ptrComponents(formatToggleComponents(format)),
	}
	if _, err := s.InteractionResponseEdit(i.Interaction, editMsg); err != nil {
		log.Printf("[미팅/format_toggle] ERR InteractionResponseEdit thread=%s: %v", sess.ThreadID, err)
		// 사용자에게 보이지 않게 끝나면 안 됨 — ephemeral followup으로 명시 안내 + persist 스킵.
		sendFollowup("정리본 메시지 갱신에 실패했습니다. 다시 토글 button을 눌러주세요.")
		return
	}

	log.Printf("[미팅/format_toggle] 완료 thread=%s sc=%s format=%s rendered_bytes=%d",
		sess.ThreadID, scRow.ID, format, len(rendered))
}

// HandleFormatCopy는 정리본 메시지의 [복사] button 클릭 핸들러.
//
// 동작 (Codex review #12 P2 2건 + #14 P2 1건 반영):
//   - 메시지 button row의 SuccessButton(active 토글)에서 현재 포맷 식별.
//   - DB의 finalize_runs에서 (sc_id, format)으로 원본 full markdown 조회 — embed.Description은
//     4090자에서 잘려 있을 수 있으므로 SSOT로 부적합.
//   - 조회 성공 시 그 OutputMD를 .md 파일 첨부.
//   - 조회 실패/없음 시 embed.Description fallback + 잘림 가능성 명시 안내.
//   - 파일 첨부 방식 채택 이유:
//     · Discord ephemeral content 한도 2000자 vs embed 한도 4090자 → 인라인 fenced code block은
//       1984자에서 잘렸음. 파일 첨부는 10MB 한도라 사실상 무제한.
//     · 정리본 markdown 안에 fenced code block(```) 이 있으면 outer ```markdown fence가 닫혀버려
//       복사 깨졌음. 파일 첨부는 원본 그대로 보존.
//
// 호출 계약: 호출자(discord.go)가 이미 deferred ack 완료.
func HandleFormatCopy(
	ctx context.Context,
	s *discordgo.Session,
	i *discordgo.InteractionCreate,
	sess *Session,
) {
	sendError := func(msg string) {
		_, err := s.FollowupMessageCreate(i.Interaction, true, &discordgo.WebhookParams{
			Content: msg,
			Flags:   discordgo.MessageFlagsEphemeral,
		})
		if err != nil {
			log.Printf("[미팅/format_copy] ERR followup 전송 실패 thread=%s: %v", sess.ThreadID, err)
		}
	}

	if i.Message == nil || len(i.Message.Embeds) == 0 || i.Message.Embeds[0] == nil {
		logGuard("meeting/format_copy", "no_embed",
			"메시지에 embed가 없어 복사 대상 없음",
			lf("thread", sess.ThreadID))
		sendError("복사할 정리본 내용을 찾을 수 없습니다. 메시지를 다시 생성해주세요.")
		return
	}

	// active 포맷 식별 — button row의 SuccessButton에서 추출.
	// 식별 실패 시 embed.Description fallback (truncated 가능성 명시).
	activeFormat, hasActiveFormat := activeFormatFromMessage(i.Message)
	embedDesc := i.Message.Embeds[0].Description
	if embedDesc == "" {
		sendError("복사할 정리본 내용이 비어 있습니다.")
		return
	}

	// DB 캐시에서 원본 full markdown 조회 시도.
	var md string
	var sourceLabel string
	if hasActiveFormat && dbConn != nil && sess.DBSessionID != "" {
		scRow, err := dbConn.GetLatestSummarizedContent(ctx, sess.DBSessionID)
		if err == nil {
			run, qErr := dbConn.GetFinalizeRunByFormat(ctx, scRow.ID, formatToDBKind(activeFormat))
			if qErr == nil && run != nil && run.OutputMD != "" {
				md = run.OutputMD
				sourceLabel = "DB 원본"
				log.Printf("[미팅/format_copy] DB 원본 사용 thread=%s sc=%s format=%s runes=%d",
					sess.ThreadID, scRow.ID, activeFormat, len([]rune(md)))
			}
		}
	}
	// DB 조회 실패 시 embed.Description fallback.
	if md == "" {
		md = embedDesc
		sourceLabel = "embed (잘림 가능)"
		log.Printf("[미팅/format_copy] embed fallback 사용 thread=%s runes=%d (DB 조회 실패 또는 active 식별 실패)",
			sess.ThreadID, len([]rune(md)))
	}

	// .md 파일 첨부 — 파일명 thread + timestamp로 고유.
	totalRunes := len([]rune(md))
	filename := fmt.Sprintf("meeting-summary-%s-%s.md",
		sess.ThreadID, time.Now().Format("20060102-150405"))

	noticePrefix := "정리본 markdown 파일을 첨부했습니다"
	if sourceLabel == "embed (잘림 가능)" {
		noticePrefix = "정리본 markdown 파일을 첨부했습니다 (embed 표시본 기준 — 4090자에서 잘렸을 수 있음)"
	}
	notice := fmt.Sprintf("%s (%d자).\n파일을 다운로드해 원본 그대로 복사하거나 다른 곳에 붙여넣으세요.",
		noticePrefix, totalRunes)

	_, err := s.FollowupMessageCreate(i.Interaction, true, &discordgo.WebhookParams{
		Content: notice,
		Files: []*discordgo.File{
			{
				Name:        filename,
				ContentType: "text/markdown; charset=utf-8",
				Reader:      strings.NewReader(md),
			},
		},
		Flags: discordgo.MessageFlagsEphemeral,
	})
	if err != nil {
		log.Printf("[미팅/format_copy] ERR file followup 전송 실패 thread=%s file=%s: %v",
			sess.ThreadID, filename, err)
		// 파일 첨부 실패 시 inline fallback — 안전한 plain text로 (fence 충돌 회피).
		// 첫 1900자 잘라서 보냄. 사용자가 다시 button 누르도록 안내.
		const maxFallback = 1900
		runes := []rune(md)
		body := md
		if len(runes) > maxFallback {
			body = string(runes[:maxFallback]) + "\n…(이하 잘림 — 파일 첨부 실패로 인라인 표시)"
		}
		sendError(fmt.Sprintf("파일 첨부에 실패해 inline으로 보냅니다 (%d자 중 일부).\n\n%s",
			totalRunes, body))
		return
	}

	log.Printf("[미팅/format_copy] 완료 thread=%s file=%s runes=%d",
		sess.ThreadID, filename, totalRunes)
}

// buildSummarizedEmbed는 정리본 markdown을 Discord embed로 wrapping한다.
//
// Discord 한도:
//   - plain content: 2000자 (기존 사용 — LLM 출력 길어지면 HTTP 400 reject로 stuck UX)
//   - embed.Description: 4096자 (4x 큼) — 대부분 정리본 케이스 커버
//
// 4096자 초과 시 4090자에서 truncate + footer로 명시 안내 (sendLongMessage로 split하면 toggle UI가
// 깨져서 — toggle은 InteractionResponseEdit가 단일 메시지 edit이라 multi-message split 호환 X).
//
// 반환 second value `truncated`는 호출자가 로그/메트릭에 사용.
func buildSummarizedEmbed(rendered string) (*discordgo.MessageEmbed, bool) {
	const maxDesc = 4090 // 안전 여유 6자 (footer/내부 메타에 한도 정확히 닿지 않게)
	runes := []rune(rendered)
	truncated := false
	desc := rendered
	if len(runes) > maxDesc {
		desc = string(runes[:maxDesc]) + "…"
		truncated = true
	}
	embed := &discordgo.MessageEmbed{
		Description: desc,
	}
	if truncated {
		embed.Footer = &discordgo.MessageEmbedFooter{
			Text: fmt.Sprintf("정리본이 %d자 → 4090자에서 잘림 (Discord embed 한도). 전체는 DB summarized_contents에 보존.", len(runes)),
		}
	}
	return embed, truncated
}
