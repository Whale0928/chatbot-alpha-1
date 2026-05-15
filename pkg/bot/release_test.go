package bot

import (
	"testing"

	"chatbot-alpha-1/pkg/release"
)

// Test_batchReleaseMaxModules_invariant는 등록된 release 모듈 수가 batch UI 한도 안에 있는지 검증.
//
// 한도(batchReleaseMaxModules=4)는 Discord 메시지 ActionsRow 5개 제약에서 유래 — batchReleaseModuleComponents가
// 모듈마다 1 row + [모두 진행] button row 1개 사용. 등록 모듈이 한도를 초과하면 [전체] UI가 silent 깨짐.
//
// 새 모듈 추가가 이 한도를 초과하면 본 테스트가 실패해서 운영 사고를 사전 차단.
// (한도 자체를 늘려야 하는 시점이면 batchReleaseModuleComponents의 row 구조 자체를 재설계 — 페이징/멀티 메시지)
func Test_batchReleaseMaxModules_invariant(t *testing.T) {
	if got := len(release.Modules); got > batchReleaseMaxModules {
		t.Fatalf("release.Modules = %d개, batchReleaseMaxModules = %d 초과 — [전체] UI Discord row 5 제약 위반.\n"+
			"수정 옵션:\n"+
			"  1. batchReleaseModuleComponents를 페이징/멀티 메시지로 재설계\n"+
			"  2. 일부 모듈을 release.Modules에서 제외\n"+
			"  3. batchReleaseMaxModules 자체를 키우는 건 위 1번 재설계 후에만 안전",
			got, batchReleaseMaxModules)
	}
}
