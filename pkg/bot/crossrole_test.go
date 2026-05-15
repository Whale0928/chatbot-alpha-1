package bot

import (
	"reflect"
	"testing"
)

func TestDetectTargetRoles(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    []string
	}{
		{"empty", "", nil},
		{"no keyword", "위스키 캐스크 정보 업데이트", nil},

		// FRONTEND 단일
		{"프론트", "프론트 이슈 206 체크 요청", []string{"FRONTEND"}},
		{"프론트엔드", "프론트엔드 영역 확인", []string{"FRONTEND"}},
		{"프엔 단축", "프엔 진행 부탁", []string{"FRONTEND"}},
		{"FE 영문", "FE 검수 필요", []string{"FRONTEND"}},
		{"frontend lowercase", "frontend integration", []string{"FRONTEND"}},

		// BACKEND 단일
		{"백엔드", "백엔드 spec 확장", []string{"BACKEND"}},
		{"서버", "서버에서 처리해줘", []string{"BACKEND"}},
		{"BE 대문자", "BE 작업 시작", []string{"BACKEND"}},

		// PM 단일
		{"기획", "기획 이슈 정리 필요", []string{"PM"}},
		{"PM", "PM 검토 부탁", []string{"PM"}},

		// DESIGN 단일
		{"디자인", "디자인 시안 검토", []string{"DESIGN"}},
		{"디자이너", "디자이너 컨펌 필요", []string{"DESIGN"}},

		// 복수 role — 정렬 순서로 반환
		{"FE+BE", "프론트와 백엔드 둘 다 체크", []string{"BACKEND", "FRONTEND"}},
		{"FE+BE+PM", "프론트, 백엔드, 기획 모두", []string{"BACKEND", "FRONTEND", "PM"}},
		{"PM+DESIGN", "기획+디자인 협의", []string{"DESIGN", "PM"}},

		// dedupe — 같은 role 매칭 키워드 복수
		{"프론트 + 프론트엔드 같은 role", "프론트와 프론트엔드 검토", []string{"FRONTEND"}},

		// cross-role 실제 미팅 케이스
		{"5/14 PM→FE 케이스", "차주 미팅까지 깃허브 이슈 206,207,208 프론트엔드 체크 요청", []string{"FRONTEND"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := DetectTargetRoles(tc.content)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestIsCrossRoleHint(t *testing.T) {
	tests := []struct {
		name        string
		originRoles []string
		targetRoles []string
		want        bool
	}{
		{"BE→FE cross", []string{"BACKEND"}, []string{"FRONTEND"}, true},
		{"PM→FE cross (5/14)", []string{"PM"}, []string{"FRONTEND"}, true},
		{"BE→BE self", []string{"BACKEND"}, []string{"BACKEND"}, false},
		{"BE→[BE,FE] partial cross", []string{"BACKEND"}, []string{"BACKEND", "FRONTEND"}, true},
		{"empty origin", nil, []string{"FRONTEND"}, false},
		{"empty target", []string{"BACKEND"}, nil, false},
		{"both empty", nil, nil, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := IsCrossRoleHint(tc.originRoles, tc.targetRoles)
			if got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestDetectTargetRoles_DeterministicOrder(t *testing.T) {
	// 같은 입력에 대해 매번 같은 순서 보장 (map iteration 비결정성 차단)
	content := "프론트, 백엔드, 기획, 디자인 모두"
	first := DetectTargetRoles(content)
	for i := 0; i < 10; i++ {
		got := DetectTargetRoles(content)
		if !reflect.DeepEqual(got, first) {
			t.Fatalf("run %d: 비결정적 — first=%v got=%v", i, first, got)
		}
	}
	want := []string{"BACKEND", "DESIGN", "FRONTEND", "PM"}
	if !reflect.DeepEqual(first, want) {
		t.Errorf("got %v, want %v (알파벳 정렬)", first, want)
	}
}
