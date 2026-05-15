package bot

import (
	"strings"
	"testing"
)

// 운영 사고 회귀 차단:
// 2026-05-15 _deadwhale의 thread에서 finalize_summarized가 정리본 출력 길이로 인해
// HTTP 400 BAD REQUEST (Must be 2000 or fewer in length) 받고 stuck → 봇 무응답 UX.
// embed.Description은 4096자 한도라 plain content 2000자보다 4x 큼.

func Test_buildSummarizedEmbed_짧은_입력은_그대로(t *testing.T) {
	in := "## 한 줄 요약\n간단한 정리본 본문."
	embed, truncated := buildSummarizedEmbed(in)
	if truncated {
		t.Errorf("짧은 입력은 truncate되면 안 됨")
	}
	if embed.Description != in {
		t.Errorf("Description 변형됨\n got=%q\nwant=%q", embed.Description, in)
	}
	if embed.Footer != nil {
		t.Errorf("truncate 안 됐는데 footer 추가됨: %v", embed.Footer)
	}
}

func Test_buildSummarizedEmbed_2000_초과_4096_미만은_그대로(t *testing.T) {
	// Discord plain content 2000 한도는 초과하지만 embed.Description 4090 한도 안 — 본문 손실 X여야 함.
	in := strings.Repeat("가", 3000)
	embed, truncated := buildSummarizedEmbed(in)
	if truncated {
		t.Errorf("4090 이하는 truncate되면 안 됨 (입력 %d자)", len([]rune(in)))
	}
	if embed.Description != in {
		t.Errorf("Description 길이 = %d, want %d", len([]rune(embed.Description)), len([]rune(in)))
	}
}

func Test_buildSummarizedEmbed_4090_초과는_truncate(t *testing.T) {
	in := strings.Repeat("가", 5000)
	embed, truncated := buildSummarizedEmbed(in)
	if !truncated {
		t.Errorf("5000자 입력인데 truncate=false")
	}
	// 4090자 + "…" = 4091자
	if got := len([]rune(embed.Description)); got != 4091 {
		t.Errorf("Description 길이 = %d, want 4091 (4090 + ellipsis)", got)
	}
	if !strings.HasSuffix(embed.Description, "…") {
		t.Errorf("truncate된 Description은 …로 끝나야 함")
	}
	if embed.Footer == nil || !strings.Contains(embed.Footer.Text, "5000") {
		t.Errorf("truncate 시 footer에 원본 길이 안내 필요: %v", embed.Footer)
	}
}

func Test_buildSummarizedEmbed_정확히_4090(t *testing.T) {
	in := strings.Repeat("가", 4090)
	embed, truncated := buildSummarizedEmbed(in)
	if truncated {
		t.Errorf("정확히 4090자는 truncate=false여야 함")
	}
	if got := len([]rune(embed.Description)); got != 4090 {
		t.Errorf("Description 길이 = %d, want 4090", got)
	}
}
