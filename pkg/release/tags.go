package release

import (
	"sort"
	"strings"
)

// LatestTagResult는 ResolveLatestTag 의 반환값.
type LatestTagResult struct {
	TagName string  // "sandbox-product/v1.2.3"
	Version Version // 1.2.3
}

// ResolveLatestTag는 태그 이름 목록에서 module.TagPrefix 로 시작하는 태그 중
// semver 기준 가장 높은 버전을 찾는다.
//
// 매칭 패턴: <TagPrefix>/v<MAJOR>.<MINOR>.<PATCH>
// pre-release/build metadata 가 붙은 태그는 무시한다 (ParseVersion 거부).
//
// 매칭 0개면 ok=false. 호출자는 "첫 릴리즈 — base 없음" 시나리오로 분기한다.
func ResolveLatestTag(tagNames []string, module Module) (LatestTagResult, bool) {
	prefix := module.TagPrefix + "/v"
	var matched []LatestTagResult
	for _, name := range tagNames {
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		suffix := strings.TrimPrefix(name, prefix)
		v, err := ParseVersion(suffix)
		if err != nil {
			continue
		}
		matched = append(matched, LatestTagResult{TagName: name, Version: v})
	}
	if len(matched) == 0 {
		return LatestTagResult{}, false
	}
	sort.Slice(matched, func(i, j int) bool {
		// 내림차순 — 가장 큰 버전이 앞으로.
		return matched[i].Version.Compare(matched[j].Version) > 0
	})
	return matched[0], true
}
