package prompts

import (
	"strings"
	"testing"
)

// 정책 보존 regression tests — Copilot review #11 / PR #11 운영 사고 재발 방지.
//
// 이 테스트들은 LLM 호출 없이 prompt string 내용만 검증한다.
// prompt에서 핵심 정책 문구가 사라지면 즉시 fail하여 정책 회귀를 차단한다.

func TestFormatRenderCommon_금지표현_없음도배_사라짐(t *testing.T) {
	// "(없음)"이나 "이번 회의에서는 없음"을 출력 금지하는 규칙이 명시되어야 한다.
	mustContain(t, "common", formatRenderCommon, []string{
		`OMIT EMPTY SECTIONS ENTIRELY`,
		`"(없음)"`,
		`section's HEADER ALSO disappears`,
	})
}

func TestFormatRenderCommon_헤더정책_문장형(t *testing.T) {
	mustContain(t, "common header style", formatRenderCommon, []string{
		`HEADER STYLE`,
		`단어 한 개 헤더 금지`,
		`emoji 사용 금지`,
	})
}

func TestFormatRenderCommon_1뎁스불릿_강제(t *testing.T) {
	mustContain(t, "common bullet depth", formatRenderCommon, []string{
		`BULLET DEPTH`,
		`"- " 불릿으로 시작`,
		`최대 깊이 3`,
	})
}

func TestFormatRenderCommon_봇결과_흡수정책(t *testing.T) {
	// weekly/release highlight는 "이번 주에 완료한 작업"으로 흡수.
	mustContain(t, "common bot absorption", formatRenderCommon, []string{
		`weekly_reports[].highlights[]`,
		`release_results[].highlights[]`,
		`이번 주에 완료한 작업`,
		`(주간 리포트 기반`,
		`(릴리즈 PR #N`,
		`메타 line 형식`,
	})
}

func TestFormatRenderCommon_attribution_정합(t *testing.T) {
	// Decision에는 origin 필드 자체가 없다 — 추측 금지 명시.
	mustContain(t, "common attribution", formatRenderCommon, []string{
		`decisions에는 origin/author 필드 자체가 없다`,
		`target_user > target_roles`,
		`deadline이 빈 문자열이면`,
	})
}

func TestFormatRenderCommon_확인필요_중복방지(t *testing.T) {
	mustContain(t, "common 확인필요 중복", formatRenderCommon, []string{
		`확인 필요" 중복 방지`,
		`이미 "확인 필요"로 끝나면 그대로 출력`,
	})
}

func TestFormatRenderDecisionStatus_핵심헤더_표준라벨(t *testing.T) {
	mustContain(t, "decision_status headers", FormatRenderDecisionStatus, []string{
		`이번 회의에서 합의한 결정`,
		`앞으로 진행할 작업`,
		`이번 주에 완료한 작업`,
		`더 확인이 필요한 부분`,
		`회의에서 함께 참고한 자료`,
		`관련 키워드`,
	})
	mustNotContain(t, "decision_status no emoji header", FormatRenderDecisionStatus, []string{
		`## 📋`, `## ✅`, `## 🚀`, `## 🗣️`,
	})
}

func TestFormatRenderDiscussion_highlight흡수_섹션포함(t *testing.T) {
	// discussion에도 weekly highlight 흡수를 위해 "이번 주에 완료한 작업" 섹션이 명시되어야 한다.
	mustContain(t, "discussion absorption", FormatRenderDiscussion, []string{
		`이번 주에 완료한 작업`,
		`weekly_reports[].highlights + release_results[].highlights`,
	})
}

func TestFormatRenderRoleBased_봇결과_role매핑(t *testing.T) {
	mustContain(t, "role_based", FormatRenderRoleBased, []string{
		`이번 주에 마무리한 작업`,
		`차주 진행할 작업`,
		`repo / module 키워드 → role 매핑`,
		`agent_responses / external_refs`,
	})
	// 옛 sub-section 라벨(bold 형태)이 prompt에 잔존하면 안 됨.
	mustNotContain(t, "role_based no old policy", FormatRenderRoleBased, []string{
		`**본인이 맡은 작업**`,
		`**다른 역할에서 받은 요청**`,
		`**자기 발의 액션**`,
		`**받은 요청**`,
	})
}

// Codex re-review P2: role 매핑 실패 작업이 공통 fallback에 누락되지 않게 헤더 표준화.
func TestFormatRenderRoleBased_매핑실패_공통fallback헤더(t *testing.T) {
	mustContain(t, "role_based fallback headers", FormatRenderRoleBased, []string{
		`이번 주에 완료한 작업 (공통)`,
		`현재 진행 중인 작업 (공통)`,
		`곧 시작할 작업 (공통)`,
		`role 매핑 실패한 done[] 항목`,
		`매핑 실패 작업 항목은 어디에도 누락되지 않게`,
	})
}

// Codex re-review P2: external_refs highlights도 sub-bullet으로 출력 강제.
func TestFormatRenderCommon_external_refs_highlights_노출(t *testing.T) {
	mustContain(t, "external_refs highlights", formatRenderCommon, []string{
		`external_refs도 agent와 동일`,
		`highlights가 있으면 sub-bullet으로 모두 출력`,
		`누락 금지`,
	})
}

func TestFormatRenderFreeform_표준헤더_완전성(t *testing.T) {
	// freeform에도 done/in_progress/planned/shared 도착지가 있어야 한다 (Copilot #7).
	mustContain(t, "freeform standard headers", FormatRenderFreeform, []string{
		`회의 한 줄 정리`,
		`회의 핵심 결정`,
		`이번 주에 완료한 작업`,
		`현재 진행 중인 작업`,
		`곧 시작할 작업`,
		`후속으로 진행할 작업`,
		`이번 회의에서 다룬 주제`,
		`더 확인이 필요한 부분`,
		`팀에 함께 공유한 내용`,
		`회의에서 함께 참고한 자료`,
		`관련 키워드`,
	})
}

func TestSummarizedContent_STRICT분리(t *testing.T) {
	mustContain(t, "summarized strict separation", SummarizedContent, []string{
		`STRICT FIELD SEPARATION`,
		`HUMAN_NOTES`,
		`CONTEXT_NOTES`,
		`Putting a fact from CONTEXT_NOTES into ANY of the 10 human fields`,
	})
}

func TestSummarizedContent_봇author_라벨가이드(t *testing.T) {
	// NoteSource → author 라벨 강제 매핑이 SINGLE SOURCE OF TRUTH.
	mustContain(t, "summarized author label", SummarizedContent, []string{
		`NoteSource 기반으로 author 라벨을 강제 매핑`,
		`"[weekly]"`,
		`"[release]"`,
		`"[agent]"`,
		`SINGLE SOURCE OF TRUTH`,
		`content 휴리스틱 사용 금지`,
	})
	// 옛 휴리스틱 (content 기반 자동 분류) 잔존 금지.
	mustNotContain(t, "summarized no heuristic override", SummarizedContent, []string{
		`or clearly labeled GitHub weekly analysis`,
		`or clearly labeled release PR creation result`,
		`or AI question/answer output`,
	})
}

// ===== helpers =====

func mustContain(t *testing.T, name, src string, substrs []string) {
	t.Helper()
	for _, s := range substrs {
		if !strings.Contains(src, s) {
			t.Errorf("%s: prompt missing required substring %q", name, s)
		}
	}
}

func mustNotContain(t *testing.T, name, src string, substrs []string) {
	t.Helper()
	for _, s := range substrs {
		if strings.Contains(src, s) {
			t.Errorf("%s: prompt should not contain %q (policy regression)", name, s)
		}
	}
}
