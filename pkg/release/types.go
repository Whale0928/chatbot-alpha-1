// Package release는 릴리즈 흐름의 도메인 타입과 모듈 레지스트리를 제공한다.
//
// 봇/CLI 양쪽에서 공통으로 사용한다. 실제 GitHub 호출이나 LLM 호출은
// 이 패키지에서 하지 않고 호출자가 wiring 한다 (테스트성을 위해).
package release

import (
	"fmt"
	"strconv"
	"strings"
)

// Line은 릴리즈 라인 (백엔드/프론트엔드).
type Line int

const (
	LineUnknown Line = iota
	LineBackend
	LineFrontend
)

// String은 사용자 라벨용.
func (l Line) String() string {
	switch l {
	case LineBackend:
		return "백엔드"
	case LineFrontend:
		return "프론트엔드"
	default:
		return "unknown"
	}
}

// BumpType은 semver bump 방식.
type BumpType int

const (
	BumpUnknown BumpType = iota
	BumpMajor
	BumpMinor
	BumpPatch
)

// String은 사용자 라벨용.
func (b BumpType) String() string {
	switch b {
	case BumpMajor:
		return "메이저"
	case BumpMinor:
		return "마이너"
	case BumpPatch:
		return "패치"
	default:
		return "unknown"
	}
}

// ParseBumpType은 문자열을 BumpType 으로 변환한다. CLI 플래그용.
func ParseBumpType(s string) (BumpType, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "major", "메이저":
		return BumpMajor, true
	case "minor", "마이너":
		return BumpMinor, true
	case "patch", "패치":
		return BumpPatch, true
	default:
		return BumpUnknown, false
	}
}

// Module은 릴리즈 가능한 단일 모듈을 표현한다.
type Module struct {
	Key           string // CLI/식별자용: "product", "admin", "batch"
	Line          Line
	DisplayName   string // 한글 라벨: "프로덕트"
	VersionPath   string // 레포 루트 기준 상대 경로
	TagPrefix     string // 태그 prefix: "sandbox-product" → 태그는 "sandbox-product/v1.0.0"
	ReleaseBranch string // 릴리즈 PR 의 base 브랜치
	HasDeploy     bool   // 릴리즈 머지 시 prod 자동배포 워크플로우 존재 여부
}

// Version은 semver MAJOR.MINOR.PATCH.
type Version struct {
	Major, Minor, Patch int
}

// ParseVersion은 "1.2.3" 또는 "v1.2.3" 형식 문자열을 파싱한다.
// 공백/개행은 trim 된다. 4번째 컴포넌트나 pre-release 표기는 거부한다.
func ParseVersion(s string) (Version, error) {
	t := strings.TrimSpace(s)
	t = strings.TrimPrefix(t, "v")
	parts := strings.Split(t, ".")
	if len(parts) != 3 {
		return Version{}, fmt.Errorf("version %q: 3 components 필요 (MAJOR.MINOR.PATCH)", s)
	}
	out := Version{}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return Version{}, fmt.Errorf("version %q: 컴포넌트 %d 파싱 실패: %w", s, i, err)
		}
		if n < 0 {
			return Version{}, fmt.Errorf("version %q: 음수 거부", s)
		}
		switch i {
		case 0:
			out.Major = n
		case 1:
			out.Minor = n
		case 2:
			out.Patch = n
		}
	}
	return out, nil
}

// String은 "1.2.3" 형식.
func (v Version) String() string {
	return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
}

// Bump는 BumpType 에 따라 새 버전을 반환한다 (원본 불변).
// Major bump → minor/patch 리셋, Minor bump → patch 리셋.
func (v Version) Bump(b BumpType) (Version, error) {
	switch b {
	case BumpMajor:
		return Version{Major: v.Major + 1}, nil
	case BumpMinor:
		return Version{Major: v.Major, Minor: v.Minor + 1}, nil
	case BumpPatch:
		return Version{Major: v.Major, Minor: v.Minor, Patch: v.Patch + 1}, nil
	default:
		return Version{}, fmt.Errorf("알 수 없는 bump 타입: %v", b)
	}
}

// Tag는 모듈의 태그 컨벤션에 맞춰 태그명을 만든다 (예: "sandbox-product/v1.2.3").
func (v Version) Tag(m Module) string {
	return m.TagPrefix + "/v" + v.String()
}

// Compare는 -1/0/+1 을 반환한다 (semver 비교).
func (v Version) Compare(o Version) int {
	if v.Major != o.Major {
		if v.Major < o.Major {
			return -1
		}
		return 1
	}
	if v.Minor != o.Minor {
		if v.Minor < o.Minor {
			return -1
		}
		return 1
	}
	if v.Patch != o.Patch {
		if v.Patch < o.Patch {
			return -1
		}
		return 1
	}
	return 0
}

// SandboxModules 는 chatbot 자체 레포 안에서 굴리는 검증용 모듈 레지스트리.
// 실제 보틀노트 모듈과 분리하기 위해 TagPrefix 에 "sandbox-" 를 붙인다.
// chatbot 의 release 워크플로우는 "v*.*.*" 매칭이라 이 prefix 와 충돌하지 않는다.
var SandboxModules = []Module{
	{
		Key:           "product",
		Line:          LineBackend,
		DisplayName:   "프로덕트",
		VersionPath:   "testdata/release-sandbox/product/VERSION",
		TagPrefix:     "sandbox-product",
		ReleaseBranch: "release/sandbox-product",
		HasDeploy:     true,
	},
	{
		Key:           "admin",
		Line:          LineBackend,
		DisplayName:   "어드민",
		VersionPath:   "testdata/release-sandbox/admin/VERSION",
		TagPrefix:     "sandbox-admin",
		ReleaseBranch: "release/sandbox-admin",
		HasDeploy:     true,
	},
	{
		Key:           "batch",
		Line:          LineBackend,
		DisplayName:   "배치",
		VersionPath:   "testdata/release-sandbox/batch/VERSION",
		TagPrefix:     "sandbox-batch",
		ReleaseBranch: "release/sandbox-batch",
		HasDeploy:     false,
	},
}

// FindModule은 key 로 SandboxModules 에서 모듈을 찾는다.
func FindModule(key string) (Module, bool) {
	for _, m := range SandboxModules {
		if m.Key == key {
			return m, true
		}
	}
	return Module{}, false
}

// ModulesByLine은 라인 필터로 모듈 목록을 돌려준다.
func ModulesByLine(line Line) []Module {
	out := make([]Module, 0, len(SandboxModules))
	for _, m := range SandboxModules {
		if m.Line == line {
			out = append(out, m)
		}
	}
	return out
}
