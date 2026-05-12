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
	Owner         string // GitHub 레포 owner
	Repo          string // GitHub 레포 이름
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

// Modules 는 실제 보틀노트 운영 레포의 릴리즈 모듈 레지스트리.
//
// 1차 도입 범위는 **백엔드 라인 한정** — 백엔드 모듈들은 동일 모노레포(bottle-note-api-server)의
// 서브디렉토리이며 태그/브랜치 컨벤션이 일관된다 ("{prefix}/v1.2.3", release/{prefix}).
//
// 프론트엔드 라인(frontend, dashboard)은 운영 합의 후 등록한다. 아래 TODO 블록 참고.
//
// 주의: batch 는 아직 git tag 도, release/batch 브랜치도 존재하지 않는다.
// 첫 릴리즈 케이스를 봇 흐름이 지원하지 않으므로 일단 등록에서 제외했다.
var Modules = []Module{
	{
		Key:           "product",
		Line:          LineBackend,
		DisplayName:   "프로덕트",
		Owner:         "bottle-note",
		Repo:          "bottle-note-api-server",
		VersionPath:   "bottlenote-product-api/VERSION",
		TagPrefix:     "product",
		ReleaseBranch: "release/product",
		HasDeploy:     true,
	},
	{
		Key:           "admin",
		Line:          LineBackend,
		DisplayName:   "어드민",
		Owner:         "bottle-note",
		Repo:          "bottle-note-api-server",
		VersionPath:   "bottlenote-admin-api/VERSION",
		TagPrefix:     "admin",
		ReleaseBranch: "release/admin",
		HasDeploy:     true,
	},
	// TODO(batch): batch 모듈 측 정비 후 등록.
	// 필요한 사전 조건:
	//   1. 초기 git tag (예: batch/v0.0.0) — 현재 ListTags 결과 batch/* 없음
	//   2. release/batch 브랜치 생성 + protected 설정
	//   3. 배포 자동화 합의 (HasDeploy true/false)
	// 위 3개가 정해지면 아래 블록을 살리면 된다.
	// {
	//     Key:           "batch",
	//     Line:          LineBackend,
	//     DisplayName:   "배치",
	//     Owner:         "bottle-note",
	//     Repo:          "bottle-note-api-server",
	//     VersionPath:   "bottlenote-batch/VERSION",
	//     TagPrefix:     "batch",
	//     ReleaseBranch: "release/batch",
	//     HasDeploy:     false,
	// },
	// TODO(frontend): 프론트엔드 라인 도입 시 등록.
	// 별도 레포(bottle-note-frontend, admin-dashboard)라 owner/repo 가 백엔드와 다르다.
	// dashboard 는 아직 git tag 자체가 없어 TagPrefix 합의(기본안: "dashboard") 필요.
	// {
	//     Key:           "frontend",
	//     Line:          LineFrontend,
	//     DisplayName:   "프론트엔드",
	//     Owner:         "bottle-note",
	//     Repo:          "bottle-note-frontend",
	//     VersionPath:   "VERSION",
	//     TagPrefix:     "frontend",
	//     ReleaseBranch: "release",
	//     HasDeploy:     true,
	// },
	// {
	//     Key:           "dashboard",
	//     Line:          LineFrontend,
	//     DisplayName:   "어드민 대시보드",
	//     Owner:         "bottle-note",
	//     Repo:          "admin-dashboard",
	//     VersionPath:   "VERSION",
	//     TagPrefix:     "dashboard",
	//     ReleaseBranch: "release/dashboard",
	//     HasDeploy:     true,
	// },
}

// FindModule은 key 로 모듈을 찾는다.
func FindModule(key string) (Module, bool) {
	for _, m := range Modules {
		if m.Key == key {
			return m, true
		}
	}
	return Module{}, false
}

// ModulesByLine은 라인 필터로 모듈 목록을 돌려준다.
func ModulesByLine(line Line) []Module {
	out := make([]Module, 0, len(Modules))
	for _, m := range Modules {
		if m.Line == line {
			out = append(out, m)
		}
	}
	return out
}
