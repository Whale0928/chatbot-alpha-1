package validate

import (
	"fmt"
	"log"
	"strings"

	"chatbot-alpha-1/pkg/llm"
)

// DecisionStatus는 포맷 1번 응답을 검증한다.
// 모든 항목 WARN 수준 (검증 실패해도 에러 반환 안 함).
func DecisionStatus(r *llm.DecisionStatusResponse, notes []llm.Note, speakers []string) {
	if r == nil {
		return
	}
	corpus := buildNoteCorpus(notes)
	speakerSet := lowerSet(speakers)

	for i, d := range r.Decisions {
		if !hasAnyTokenOverlap(d.Title, corpus) {
			log.Printf("[llm/validate] WARN decision_status.decisions[%d].title 토큰 겹침 없음: %q", i, d.Title)
		}
	}
	warnIfNoOverlap(r.Done, corpus, "decision_status.done")
	warnIfNoOverlap(r.InProgress, corpus, "decision_status.in_progress")
	warnIfNoOverlap(r.Planned, corpus, "decision_status.planned")
	warnIfNoOverlap(r.Blockers, corpus, "decision_status.blockers")

	for i, ns := range r.NextSteps {
		if ns.What != "" && !hasAnyTokenOverlap(ns.What, corpus) {
			log.Printf("[llm/validate] WARN decision_status.next_steps[%d].what 토큰 겹침 없음: %q", i, ns.What)
		}
		if ns.Who != "" && !speakerSet[strings.ToLower(ns.Who)] {
			log.Printf("[llm/validate] WARN decision_status.next_steps[%d].who=%q는 발화자 목록에 없음", i, ns.Who)
		}
	}
}

// Discussion은 포맷 2번 응답을 검증한다. 모두 WARN.
func Discussion(r *llm.DiscussionResponse, notes []llm.Note, speakers []string) {
	if r == nil {
		return
	}
	corpus := buildNoteCorpus(notes)
	for i, t := range r.Topics {
		if !hasAnyTokenOverlap(t.Title, corpus) {
			log.Printf("[llm/validate] WARN discussion.topics[%d].title 토큰 겹침 없음: %q", i, t.Title)
		}
		for j, f := range t.Flow {
			if !hasAnyTokenOverlap(f, corpus) {
				log.Printf("[llm/validate] WARN discussion.topics[%d].flow[%d] 토큰 겹침 없음: %q", i, j, f)
			}
		}
	}
	_ = speakers // discussion 포맷은 speaker 검증 안 함
}

// RoleBased는 포맷 3번 응답을 검증한다.
// Speaker만 strict — 발화자 목록 외 이름이면 ERROR 반환 (호출자가 막을 수 있도록).
// 그 외는 WARN.
func RoleBased(r *llm.RoleBasedResponse, notes []llm.Note, speakers []string) error {
	if r == nil {
		return nil
	}
	corpus := buildNoteCorpus(notes)
	speakerSet := lowerSet(speakers)

	var unknown []string
	for i, role := range r.Roles {
		if role.Speaker == "" {
			unknown = append(unknown, fmt.Sprintf("roles[%d]: empty speaker", i))
			continue
		}
		if !speakerSet[strings.ToLower(role.Speaker)] {
			unknown = append(unknown, fmt.Sprintf("roles[%d].speaker=%q", i, role.Speaker))
		}
		warnIfNoOverlap(role.Decisions, corpus, fmt.Sprintf("role_based.roles[%d].decisions", i))
		warnIfNoOverlap(role.Shared, corpus, fmt.Sprintf("role_based.roles[%d].shared", i))
		for j, a := range role.Actions {
			if a.What != "" && !hasAnyTokenOverlap(a.What, corpus) {
				log.Printf("[llm/validate] WARN role_based.roles[%d].actions[%d].what 토큰 겹침 없음: %q", i, j, a.What)
			}
			if a.Who != "" && !speakerSet[strings.ToLower(a.Who)] {
				unknown = append(unknown, fmt.Sprintf("roles[%d].actions[%d].who=%q", i, j, a.Who))
			}
		}
	}
	if len(unknown) > 0 {
		return fmt.Errorf("llm/validate: role_based에 발화자 목록 외 이름 발견: %s", strings.Join(unknown, ", "))
	}
	warnIfNoOverlap(r.SharedItems, corpus, "role_based.shared_items")
	return nil
}

// Freeform은 포맷 4번 응답을 검증한다.
// 토큰 겹침 비율이 임계 미만이면 WARN. 막진 않음.
func Freeform(r *llm.FreeformResponse, notes []llm.Note) {
	if r == nil || strings.TrimSpace(r.Markdown) == "" {
		return
	}
	corpus := buildNoteCorpus(notes)
	tokens := tokenize(r.Markdown)
	if len(tokens) == 0 {
		return
	}
	overlap := 0
	for _, tok := range tokens {
		if strings.Contains(corpus, tok) {
			overlap++
		}
	}
	ratio := float64(overlap) / float64(len(tokens))
	if ratio < 0.3 {
		log.Printf("[llm/validate] WARN freeform: 노트와 토큰 겹침 비율 낮음 ratio=%.2f tokens=%d overlap=%d", ratio, len(tokens), overlap)
	}
}

// warnIfNoOverlap은 string 슬라이스 각 항목의 토큰 겹침을 확인하고 없으면 WARN.
func warnIfNoOverlap(items []string, corpus, label string) {
	for i, s := range items {
		if !hasAnyTokenOverlap(s, corpus) {
			log.Printf("[llm/validate] WARN %s[%d] 토큰 겹침 없음: %q", label, i, s)
		}
	}
}

// lowerSet은 문자열 슬라이스를 소문자 set으로 변환.
func lowerSet(ss []string) map[string]bool {
	out := make(map[string]bool, len(ss))
	for _, s := range ss {
		out[strings.ToLower(s)] = true
	}
	return out
}
