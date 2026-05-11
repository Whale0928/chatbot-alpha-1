package release

import "testing"

func TestResolveLatestTag(t *testing.T) {
	module, _ := FindModule("product")

	cases := []struct {
		name string
		tags []string
		want string // 기대 tag name. "" 면 not found.
	}{
		{
			name: "기본 매칭 - 단일 태그",
			tags: []string{"sandbox-product/v1.0.0"},
			want: "sandbox-product/v1.0.0",
		},
		{
			name: "여러 태그 중 최신 선택",
			tags: []string{
				"sandbox-product/v1.0.0",
				"sandbox-product/v1.2.0",
				"sandbox-product/v1.1.5",
			},
			want: "sandbox-product/v1.2.0",
		},
		{
			name: "다른 모듈 태그 무시",
			tags: []string{
				"sandbox-admin/v9.9.9",
				"sandbox-product/v1.0.0",
				"v0.5.0", // chatbot 자체 태그
			},
			want: "sandbox-product/v1.0.0",
		},
		{
			name: "pre-release 형식 무시",
			tags: []string{
				"sandbox-product/v1.0.0-rc1",
				"sandbox-product/v0.9.0",
			},
			want: "sandbox-product/v0.9.0",
		},
		{
			name: "매칭 0개",
			tags: []string{"v1.0.0", "sandbox-admin/v1.0.0"},
			want: "",
		},
		{
			name: "major 우선",
			tags: []string{
				"sandbox-product/v1.99.99",
				"sandbox-product/v2.0.0",
			},
			want: "sandbox-product/v2.0.0",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := ResolveLatestTag(c.tags, module)
			if c.want == "" {
				if ok {
					t.Errorf("not found 기대했으나 %s 반환", got.TagName)
				}
				return
			}
			if !ok {
				t.Errorf("매칭 기대 (%s) — 못 찾음", c.want)
				return
			}
			if got.TagName != c.want {
				t.Errorf("TagName = %q, want %q", got.TagName, c.want)
			}
		})
	}
}
