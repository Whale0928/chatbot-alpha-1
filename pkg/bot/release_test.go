package bot

import (
	"sync"
	"sync/atomic"
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

// =====================================================================
// Race condition 검증 (codex 3차 리뷰 fix)
//
// 운영 환경에서 discordgo가 InteractionCreate를 goroutine마다 dispatch하므로
// 같은 thread에서 여러 사용자가 동시에 button/SelectMenu 클릭 시 BatchReleaseContext와
// ReleaseContext가 cross-goroutine read/write됨. 평범한 bool/map은 -race로 검출되며
// map의 경우 Go runtime이 "fatal error: concurrent map writes"로 프로세스 panic.
//
// 본 테스트는 -race 플래그로 수행 시 fix가 빠지면 실패하도록 설계.
// =====================================================================

// Test_BatchReleaseContext_SetSelection_동시_쓰기는 panic-level race를 reproduce 검증한다.
//
// 시나리오: 두 사용자가 같은 thread의 batch UI에서 동시에 다른 모듈 SelectMenu 클릭.
// 평범한 map[string]release.BumpType 직접 mutate라면 Go runtime이 "concurrent map writes"로
// 프로세스를 panic 시킨다 (kubectl logs에 fatal error + pod crash). mu.Mutex 보호로 방지.
func Test_BatchReleaseContext_SetSelection_동시_쓰기(t *testing.T) {
	bc := &BatchReleaseContext{
		Modules: release.Modules,
	}
	var wg sync.WaitGroup
	const N = 50 // 과부하로 race window 확장
	for i := 0; i < N; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); bc.SetSelection("product", release.BumpPatch) }()
		go func() { defer wg.Done(); bc.SetSelection("admin", release.BumpMinor) }()
	}
	wg.Wait()

	// 결과: 두 키 모두 set 되어야 함 (마지막 write가 winner이지만 panic 없이 완료가 핵심).
	if got := bc.GetSelection("product"); got != release.BumpPatch {
		t.Errorf("product selection = %v, want BumpPatch", got)
	}
	if got := bc.GetSelection("admin"); got != release.BumpMinor {
		t.Errorf("admin selection = %v, want BumpMinor", got)
	}
}

// Test_BatchReleaseContext_Selections_읽기_쓰기_동시는 -race로 mu 보호 검증.
// SnapshotSelections (read) ↔ SetSelection (write) 동시 호출.
func Test_BatchReleaseContext_Selections_읽기_쓰기_동시(t *testing.T) {
	bc := &BatchReleaseContext{Modules: release.Modules}
	bc.SetSelection("product", release.BumpPatch)

	var wg sync.WaitGroup
	const N = 100
	for i := 0; i < N; i++ {
		wg.Add(3)
		go func() { defer wg.Done(); bc.SetSelection("admin", release.BumpMinor) }()
		go func() { defer wg.Done(); _ = bc.SnapshotSelections() }()
		go func() { defer wg.Done(); _ = bc.SelectedCount() }()
	}
	wg.Wait()
	// panic 없이 완료되면 통과 (-race로 데이터 race 감지 안 되어야 함).
}

// Test_ReleaseContext_InProgress_동시는 atomic.Bool race 보호 검증.
// runReleaseFlow goroutine의 Store(true/false) ↔ interaction handler의 Load() 동시.
func Test_ReleaseContext_InProgress_동시(t *testing.T) {
	rc := &ReleaseContext{}
	var wg sync.WaitGroup
	var loadCount atomic.Int64
	const N = 200
	for i := 0; i < N; i++ {
		wg.Add(3)
		go func() { defer wg.Done(); rc.InProgress.Store(true) }()
		go func() { defer wg.Done(); rc.InProgress.Store(false) }()
		go func() {
			defer wg.Done()
			if rc.InProgress.Load() {
				loadCount.Add(1)
			}
		}()
	}
	wg.Wait()
	// 본 테스트의 1차 목적은 -race 검출 — Store/Load가 atomic.Bool 보호로 데이터 race 없음을 검증.
	// 부차적 invariant: loadCount는 0 ~ N (각 Load는 0 또는 1 추가). 값 자체는 비결정적이지만 한도는 명확.
	if got := loadCount.Load(); got < 0 || got > int64(N) {
		t.Errorf("loadCount = %d, want 0 <= count <= %d (각 goroutine이 0/1만 추가)", got, N)
	}
}

// Test_BatchReleaseContext_InProgress_CompareAndSwap_단일_진입은
// 동시 [모두 진행] click 중 정확히 1번만 진입 성공함을 검증.
func Test_BatchReleaseContext_InProgress_CompareAndSwap_단일_진입(t *testing.T) {
	bc := &BatchReleaseContext{Modules: release.Modules}
	var wg sync.WaitGroup
	var winnerCount atomic.Int64
	const N = 100
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if bc.InProgress.CompareAndSwap(false, true) {
				winnerCount.Add(1)
			}
		}()
	}
	wg.Wait()
	if got := winnerCount.Load(); got != 1 {
		t.Errorf("CompareAndSwap winner count = %d, want 1 (race 시에도 단일 진입 보장)", got)
	}
	if !bc.InProgress.Load() {
		t.Error("CompareAndSwap 후 InProgress가 true가 아님")
	}
}

// Test_ReleaseContext_Failed_lifecycle은 Failed atomic.Bool이 한 번 set되면 false로 안 돌아오는지 검증.
// (codex 4차 P2 fix — 실패한 ctx의 stale 재실행 방어).
func Test_ReleaseContext_Failed_lifecycle(t *testing.T) {
	rc := &ReleaseContext{}
	if rc.Failed.Load() {
		t.Error("초기 Failed는 false여야 함")
	}
	rc.Failed.Store(true)
	if !rc.Failed.Load() {
		t.Error("Store(true) 후 Failed=true여야 함")
	}
	// 의도적 단방향 — 외부에서 false로 reset하지 않음을 컨벤션으로. atomic.Bool 자체는 reset 가능하나
	// updateProgressError만 set하고 그 외엔 set/reset하지 않음.
}

// Test_ReleaseContext_Failed_동시_set_load는 Failed의 cross-goroutine atomic 안전성 검증 (-race).
func Test_ReleaseContext_Failed_동시_set_load(t *testing.T) {
	rc := &ReleaseContext{}
	var wg sync.WaitGroup
	const N = 200
	for i := 0; i < N; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); rc.Failed.Store(true) }()
		go func() { defer wg.Done(); _ = rc.Failed.Load() }()
	}
	wg.Wait()
	if !rc.Failed.Load() {
		t.Error("최종 Failed=true여야 함 (모든 Store가 true)")
	}
}
