package validate

import (
	"log"
	"strings"
	"unicode"

	"chatbot-alpha-1/pkg/llm"
)

// AgainstSummarizedContent는 SummarizedContent의 actions가 환각인지 1차 검증한다.
// 실패해도 에러 반환 안 함 — log.Printf로 경고만 (호출자가 막을지 결정).
//
// 검증:
//   - actions[].origin: speakers 목록(Human source 발화자만)에 있는지
//     없으면 LLM이 ContextNotes author나 외부 인물을 잘못 attribute한 환각 의심
//   - actions[].what: 원본 corpus와 최소 1개 의미 토큰 겹침
//
// 호출 계약:
//   notes는 HumanNotes + ContextNotes 합집합 (corpus 검증용 — 모든 텍스트 포함)
//   speakers는 Human source 발화자만 (attribution 후보 — pkg/bot의 SortedHumanSpeakers)
func AgainstSummarizedContent(sc *llm.SummarizedContent, notes []llm.Note, speakers []string) {
	if sc == nil {
		return
	}
	corpus := buildNoteCorpus(notes)
	speakerSet := make(map[string]bool, len(speakers))
	for _, s := range speakers {
		speakerSet[strings.ToLower(s)] = true
	}
	for i, a := range sc.Actions {
		if a.Origin != "" && !speakerSet[strings.ToLower(a.Origin)] {
			log.Printf("[llm/validate] WARN summarized.actions[%d].origin=%q는 Human speakers 목록에 없습니다 (환각 의심 — ContextNotes author일 가능성)", i, a.Origin)
		}
		if a.What != "" && !hasAnyTokenOverlap(a.What, corpus) {
			log.Printf("[llm/validate] WARN summarized.actions[%d].what 토큰 겹침 없음: %q", i, a.What)
		}
		if a.TargetUser != "" && !speakerSet[strings.ToLower(a.TargetUser)] {
			log.Printf("[llm/validate] WARN summarized.actions[%d].target_user=%q는 Human speakers 목록에 없습니다", i, a.TargetUser)
		}
	}
}

// AgainstNotes는 LLM이 생성한 llm.FinalNoteResponse의 각 항목이
// 원본 notes에 근거하는지 substring 수준으로 검증한다.
//
// 1차 방어선이며, 실패해도 에러를 반환하지 않고 log.Printf로 경고만 남긴다.
// 검증 대상:
//   - Discussion 각 bullet: 본문이 notes 중 어느 하나와 최소 1개 의미 토큰 겹침
//   - llm.NextStep.What: 위와 동일
//   - llm.NextStep.Who: speakers 목록에 있는지 (있지 않아도 허용하되 경고)
//
// === 호출 계약 (Phase 1) ===
// 호출자(pkg/bot)는 다음을 보장해야 한다:
//   - notes:    Source.IsInCorpus()=true 항목만 (InterimSummary 제외)
//   - speakers: Source.IsAttributionCandidate()=true 발화자만 (Human source만)
// 이 좁힘으로 WeeklyDump/ExternalPaste paste가 발화자 액션으로 잘못 attribute되는
// 환각 케이스(거시 디자인 결정 6)를 SQL 단계에서 미리 차단한다. validate는 단순히
// "speakers 안에 있으면 OK"만 체크하므로 호출자의 좁힘이 정책의 실효를 보장한다.
func AgainstNotes(note *llm.FinalNoteResponse, notes []llm.Note, speakers []string) {
	if note == nil {
		return
	}

	corpus := buildNoteCorpus(notes)
	speakerSet := make(map[string]bool, len(speakers))
	for _, s := range speakers {
		speakerSet[strings.ToLower(s)] = true
	}

	for i, d := range note.Decisions {
		if !hasAnyTokenOverlap(d.Title, corpus) {
			log.Printf("[llm/validate] WARN decisions[%d].title는 원본 노트와 토큰 겹침이 없습니다: %q", i, d.Title)
		}
	}
	for i, ns := range note.NextSteps {
		if ns.What != "" && !hasAnyTokenOverlap(ns.What, corpus) {
			log.Printf("[llm/validate] WARN next_steps[%d].what는 원본 노트와 토큰 겹침이 없습니다: %q", i, ns.What)
		}
		if ns.Who != "" {
			if !speakerSet[strings.ToLower(ns.Who)] {
				log.Printf("[llm/validate] WARN next_steps[%d].who=%q는 관찰된 발화자 목록에 없습니다", i, ns.Who)
			}
		}
	}
}

// buildNoteCorpus는 notes의 content를 하나의 소문자 문자열로 합친다.
func buildNoteCorpus(notes []llm.Note) string {
	var b strings.Builder
	for _, n := range notes {
		b.WriteString(strings.ToLower(n.Content))
		b.WriteByte(' ')
	}
	return b.String()
}

// hasAnyTokenOverlap은 text에서 길이 2자 이상 의미 토큰을 뽑아
// 하나라도 corpus에 존재하는지 확인한다.
func hasAnyTokenOverlap(text, corpus string) bool {
	tokens := tokenize(text)
	for _, tok := range tokens {
		if strings.Contains(corpus, tok) {
			return true
		}
	}
	// 토큰이 하나도 추출되지 않았으면 검증 스킵 (false positive 방지).
	return len(tokens) == 0
}

// tokenize는 text에서 공백/구두점 기준으로 소문자 토큰을 뽑아낸다.
// 한글 2자 이상, 영숫자 2자 이상만 유지.
func tokenize(text string) []string {
	fields := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if len([]rune(f)) >= 2 {
			out = append(out, f)
		}
	}
	return out
}
