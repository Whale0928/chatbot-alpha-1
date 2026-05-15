package bot

import "strings"

// =====================================================================
// Phase 4 — cross-role 정밀화 (키워드 매칭 + LLM 최종 확정)
//
// 거시 디자인 결정 5/B 핵심 — 발화자 role(Origin)과 액션 대상 role(Target)을 분리해 인식.
// 채팅마다 LLM 호출은 안 함 (결정 7) — 발화 시점은 빠른 키워드 매칭으로 임시 라벨,
// 정리본 추출(ExtractContent) 시 LLM이 cross-role 가이드 prompt 기반으로 최종 확정.
//
// 매칭 정밀도는 80% 정도 목표 (true positive 위주, false negative는 LLM이 보강).
// =====================================================================

// crossRoleKeywordMap은 한국어/영어 키워드 → role 라벨 매핑.
// pkg/llm/prompts/summarized_content.go의 attribution rule과 일관 유지 (DESIGN 별도 분리).
var crossRoleKeywordMap = map[string]string{
	// FRONTEND
	"프론트":   "FRONTEND",
	"프론트엔드":  "FRONTEND",
	"프엔":     "FRONTEND",
	"fe":     "FRONTEND",
	"frontend": "FRONTEND",

	// BACKEND
	"백엔드":  "BACKEND",
	"백엔":   "BACKEND",
	"서버":   "BACKEND",
	"be":   "BACKEND",
	"backend": "BACKEND",

	// PM
	"기획":  "PM",
	"피엠":  "PM",
	"pm":  "PM",
	"planning": "PM",

	// DESIGN
	"디자인":  "DESIGN",
	"디자이너": "DESIGN",
	"design":   "DESIGN",
	"designer": "DESIGN",
}

// DetectTargetRoles는 발화 본문에서 cross-role 대상 키워드를 찾아 role 라벨 set으로 반환한다 (pure).
//
// 정렬된 결과 (결정성 — 같은 입력 → 같은 출력 순서).
// 빈 결과는 "대상 모호" 의미 (발화자 본인 발의로 간주 가능).
//
// 동작:
//   - lowercase + word boundary 단순 substring 매칭
//   - "프론트엔드", "프론트" 둘 다 매칭되면 FRONTEND로 dedupe
//   - "프론트와 백엔드 둘 다 체크" → ["BACKEND", "FRONTEND"] (정렬 순서)
//
// 한계 (LLM이 보강할 부분):
//   - "프론트는 잘 되는데" 같은 대상 아닌 언급도 detect됨 (false positive)
//   - "두 번째 화면 검토" 같은 키워드 없는 cross-role 요청은 못 잡음 (false negative)
func DetectTargetRoles(content string) []string {
	if content == "" {
		return nil
	}
	lower := strings.ToLower(content)
	seen := make(map[string]bool)
	for keyword, role := range crossRoleKeywordMap {
		if strings.Contains(lower, keyword) {
			seen[role] = true
		}
	}
	if len(seen) == 0 {
		return nil
	}
	out := make([]string, 0, len(seen))
	for r := range seen {
		out = append(out, r)
	}
	// 결정성 보장 — 알파벳 정렬
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

// IsCrossRoleHint는 발화자의 role과 detected target role이 다른지 빠르게 판단.
// 발화자 role 중 하나라도 target에 포함되면 self/partial로 간주 (cross-role 아님).
// 둘 다 비면 false (정보 부족).
func IsCrossRoleHint(originRoles, targetRoles []string) bool {
	if len(originRoles) == 0 || len(targetRoles) == 0 {
		return false
	}
	originSet := make(map[string]bool, len(originRoles))
	for _, r := range originRoles {
		originSet[r] = true
	}
	for _, t := range targetRoles {
		if !originSet[t] {
			return true
		}
	}
	return false
}
