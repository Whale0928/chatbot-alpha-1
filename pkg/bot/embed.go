package bot

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"chatbot-alpha-1/pkg/github"
	"chatbot-alpha-1/pkg/release"

	"github.com/bwmarrin/discordgo"
)

const (
	colorLineBackend  = 0x5865F2
	colorLineFrontend = 0xC44AFF
	colorOK           = 0x23A55A
	colorWarn         = 0xF0B232
	colorBad          = 0xF23F43
)

// releaseEmbed 는 모듈/PR 컨텍스트로 기본 embed 를 만든다.
func releaseEmbed(stripe int, author, title string) *discordgo.MessageEmbed {
	if stripe == 0 {
		stripe = colorLineBackend
	}
	embed := &discordgo.MessageEmbed{
		Title: title,
		Color: stripe,
	}
	if author != "" {
		embed.Author = &discordgo.MessageEmbedAuthor{Name: author}
	}
	return embed
}

// embedField 는 inline 옵션 있는 필드 빌더.
func embedField(name, value string, inline bool) *discordgo.MessageEmbedField {
	if value == "" {
		value = "없음"
	}
	return &discordgo.MessageEmbedField{Name: name, Value: value, Inline: inline}
}

// lineColor 는 모듈 라인에 대응하는 stripe 색을 반환한다.
func lineColor(line release.Line) int {
	switch line {
	case release.LineFrontend:
		return colorLineFrontend
	default:
		return colorLineBackend
	}
}

// bumpColor 는 bump 타입에 대응하는 위험도 stripe 색을 반환한다.
func bumpColor(b release.BumpType) int {
	switch b {
	case release.BumpMajor:
		return colorBad
	case release.BumpMinor:
		return colorWarn
	default:
		return colorOK
	}
}

// progressBar 는 10칸 진행바와 완료 개수를 반환한다.
func progressBar(done, total int) string {
	if total == 0 {
		return "—"
	}
	if done < 0 {
		done = 0
	}
	if done > total {
		done = total
	}
	pct := int(math.Round(float64(done) * 100 / float64(total)))
	filled := int(math.Round(float64(done) * 10 / float64(total)))
	if filled > 10 {
		filled = 10
	}
	bar := strings.Repeat("█", filled) + strings.Repeat("░", 10-filled)
	return fmt.Sprintf("%s %d / %d (%d%%)", bar, done, total, pct)
}

// groupedCheck 는 같은 이름 CI 체크를 묶은 결과다.
type groupedCheck struct {
	Name  string
	Count int
}

// groupChecks 는 check-run 을 통과/진행/실패 그룹으로 분류한다.
func groupChecks(checks []github.CheckRun) (passed, running, failed []groupedCheck) {
	var passNames, runNames, failNames []string
	for _, ch := range checks {
		switch {
		case ch.Status == "completed" && isPassedConclusion(ch.Conclusion):
			passNames = append(passNames, ch.Name)
		case ch.Status == "completed" && isFailedConclusion(ch.Conclusion):
			failNames = append(failNames, ch.Name)
		default:
			runNames = append(runNames, ch.Name)
		}
	}
	return groupCheckNames(passNames), groupCheckNames(runNames), groupCheckNames(failNames)
}

// isPassedConclusion 은 통과로 간주할 conclusion 을 판별한다.
func isPassedConclusion(conclusion string) bool {
	switch conclusion {
	case "success", "neutral", "skipped":
		return true
	default:
		return false
	}
}

// isFailedConclusion 은 실패로 간주할 conclusion 을 판별한다.
func isFailedConclusion(conclusion string) bool {
	switch conclusion {
	case "failure", "cancelled", "timed_out", "action_required":
		return true
	default:
		return false
	}
}

// groupCheckNames 는 같은 이름을 묶고 이름순으로 정렬한다.
func groupCheckNames(names []string) []groupedCheck {
	counts := make(map[string]int, len(names))
	for _, name := range names {
		counts[name]++
	}
	out := make([]groupedCheck, 0, len(counts))
	for name, count := range counts {
		out = append(out, groupedCheck{Name: name, Count: count})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Name < out[j].Name
	})
	return out
}
