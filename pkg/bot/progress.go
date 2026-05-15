package bot

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
)

// =====================================================================
// Phase 2 chunk 4 — Progress 표시 (ASCII 바 + 단계 + 경과 시간)
//
// 거시 디자인 결정 10 (Progress: ASCII 바 + 단계 + 경과 시간) 코드화.
// 거시 디자인 결정 (다) — Phase 2에 묶어 finalize flow 개선과 함께 도입.
//
// 동작:
//   1. 사용자 트리거 (예: [정리본 통합·토글] button) 직후 progress 메시지 전송
//   2. 별도 goroutine이 1.5초 간격으로 메시지 edit (단계 진행 + 경과 시간 갱신)
//   3. LLM 호출 완료 시 Finish() 호출 → goroutine 종료 + 메시지 삭제
//
// Discord rate limit: 5 req/5sec per channel. 1.5초 간격 edit는 안전 (≈3.3 req/5sec).
// =====================================================================

// ProgressStage는 LLM 호출 도구의 단계 라벨 + 진행률 표현용.
//
// 도구마다 단계 정의가 다르지만 공통 패턴:
//   1. corpus 수집
//   2. LLM 호출 시작
//   3. LLM 응답 대기 (가장 긴 단계)
//   4. 응답 파싱
//   5. validate
//   6. 렌더 + 메시지 전송
type ProgressStage struct {
	Index int    // 1-based 현재 단계
	Total int    // 전체 단계 수
	Label string // 사람이 읽는 단계 설명 (예: "응답 대기 중")
}

// Progress는 단일 진행 표시 lifecycle.
//
// 사용:
//
//	p := StartProgress(ctx, msg, channelID, "정리본 추출", 6)
//	defer p.Finish()
//	p.SetStage(1, "corpus 수집")
//	... 작업 ...
//	p.SetStage(2, "LLM 호출 시작")
//	... LLM 호출 ...
//
// Finish() 호출 시 progress 메시지 삭제 (호출자가 별도 결과 메시지 전송).
type Progress struct {
	msg       Messenger
	channelID string
	label     string

	startedAt  time.Time
	totalSteps int

	mu          sync.Mutex
	currentStep int
	currentMsg  string
	messageID   string

	stop     chan struct{}
	stopOnce sync.Once // close(stop) panic 방지 — Finish 동시/중복 호출 안전
	done     chan struct{}
}

// StartProgress는 progress 메시지를 즉시 전송하고 주기적 edit goroutine을 시작한다.
//
// 인자:
//   - msg: 메시지 전송/삭제 인터페이스 (Messenger)
//   - channelID: Discord 스레드 ID
//   - label: 작업 이름 (예: "정리본 추출")
//   - totalSteps: 전체 단계 수 (예: 6)
//
// 반환된 Progress의 Finish()를 반드시 호출 (defer 권장) — goroutine leak 방지.
//
// 메시지 전송 실패해도 nil 반환 안 함 — 빈 messageID로 진행. SetStage/Finish는 noop fallback.
func StartProgress(ctx context.Context, msg Messenger, channelID, label string, totalSteps int) *Progress {
	p := &Progress{
		msg:        msg,
		channelID:  channelID,
		label:      label,
		startedAt:  time.Now(),
		totalSteps: totalSteps,
		stop:       make(chan struct{}),
		done:       make(chan struct{}),
	}
	p.SetStage(1, "시작")

	// 초기 메시지 전송 — formatLine은 mu lock 안에서 호출
	p.mu.Lock()
	initial := p.formatLineLocked()
	p.mu.Unlock()
	sent, err := msg.ChannelMessageSend(channelID, initial)
	if err != nil {
		log.Printf("[progress] WARN 초기 메시지 전송 실패 channel=%s: %v (silent fallback)", channelID, err)
		close(p.done)
		return p
	}
	p.mu.Lock()
	p.messageID = sent.ID
	p.mu.Unlock()

	go p.runTicker(ctx)
	return p
}

// SetStage는 현재 단계 + 라벨을 갱신한다 (다음 ticker tick에서 메시지 edit으로 반영).
// 즉시 edit하지 않는 이유: rate limit 안전 + tick 주기와 통일된 시각 갱신.
func (p *Progress) SetStage(idx int, label string) {
	p.mu.Lock()
	p.currentStep = idx
	p.currentMsg = label
	p.mu.Unlock()
}

// Finish는 progress goroutine을 멈추고 메시지를 삭제한다.
// 멱등 — 두 번 호출해도 안전, 동시 호출도 안전 (sync.Once + deleteMessage 분리).
//
// ctx 취소로 goroutine이 먼저 종료된 경로에서도 메시지 삭제는 보장됨
// (이전 구조는 done close 시 즉시 return해서 메시지가 채널에 영구 잔존하는 버그가 있었음).
func (p *Progress) Finish() {
	// goroutine이 아직 살아있으면 stop 신호 + 종료 대기
	select {
	case <-p.done:
		// 이미 종료됨 (Finish 중복 호출 또는 ctx Done) — 메시지 삭제로 직행
	default:
		p.stopOnce.Do(func() { close(p.stop) })
		<-p.done
	}
	p.deleteMessage()
}

// deleteMessage는 progress 메시지를 삭제한다 (멱등 — 빈 messageID나 중복 삭제 안전).
// stopOnce와 같은 멱등 보호는 ChannelMessageDelete 자체가 이미 멱등 (이미 삭제된 메시지에 대한
// 호출은 Discord 404 에러로 log warn만 남고 에러 없이 진행)이라 별도 once 불필요.
func (p *Progress) deleteMessage() {
	p.mu.Lock()
	mid := p.messageID
	p.messageID = "" // 다음 deleteMessage 호출 시 noop 유도
	p.mu.Unlock()
	if mid == "" {
		return
	}
	if err := p.msg.ChannelMessageDelete(p.channelID, mid); err != nil {
		log.Printf("[progress] WARN 메시지 삭제 실패 channel=%s msg=%s: %v", p.channelID, mid, err)
	}
}

// runTicker는 1.5초 간격으로 메시지 edit을 수행하는 goroutine.
// stop 채널 close 또는 ctx Done 시 종료.
func (p *Progress) runTicker(ctx context.Context) {
	defer close(p.done)
	ticker := time.NewTicker(1500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-p.stop:
			return
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.mu.Lock()
			mid := p.messageID
			line := p.formatLineLocked()
			p.mu.Unlock()
			if mid == "" {
				continue
			}
			if _, err := p.msg.ChannelMessageEdit(p.channelID, mid, line); err != nil {
				log.Printf("[progress] WARN edit 실패 channel=%s msg=%s: %v (계속 진행)", p.channelID, mid, err)
			}
		}
	}
}

// formatLineLocked는 현재 progress 한 줄 markdown을 생성한다.
// 호출자가 p.mu를 보유한 상태에서 호출해야 함 — 이름의 "Locked" 접미사로 계약 명시.
//
// 형식: `[●●●○○○] 정리본 추출 · 단계 3/6 (LLM 응답 대기) · 14초`
func (p *Progress) formatLineLocked() string {
	step := p.currentStep
	total := p.totalSteps
	if total <= 0 {
		total = 1
	}
	if step < 1 {
		step = 1
	}
	if step > total {
		step = total
	}
	bar := buildProgressBar(step, total)
	elapsed := time.Since(p.startedAt).Round(time.Second)
	return fmt.Sprintf("`%s` %s · 단계 %d/%d (%s) · %s 경과", bar, p.label, step, total, p.currentMsg, elapsed)
}

// buildProgressBar는 step/total 비율로 ●○ ASCII 바를 만든다 (pure).
//
// 예: step=3 total=6 → [●●●○○○]
//     step=6 total=6 → [●●●●●●]
func buildProgressBar(step, total int) string {
	var b strings.Builder
	b.WriteByte('[')
	for i := 1; i <= total; i++ {
		if i <= step {
			b.WriteString("●")
		} else {
			b.WriteString("○")
		}
	}
	b.WriteByte(']')
	return b.String()
}
