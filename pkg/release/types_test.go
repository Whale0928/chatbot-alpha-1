package release

import "testing"

func TestParseVersion(t *testing.T) {
	cases := []struct {
		in   string
		want Version
		err  bool
	}{
		{"1.2.3", Version{1, 2, 3}, false},
		{"v1.2.3", Version{1, 2, 3}, false},
		{"0.0.0", Version{0, 0, 0}, false},
		{" 1.2.3\n", Version{1, 2, 3}, false},
		{"1.2", Version{}, true},
		{"1.2.3.4", Version{}, true},
		{"1.2.a", Version{}, true},
		{"-1.0.0", Version{}, true},
		{"", Version{}, true},
	}
	for _, c := range cases {
		got, err := ParseVersion(c.in)
		if c.err {
			if err == nil {
				t.Errorf("ParseVersion(%q) 에러 기대했으나 성공 %+v", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseVersion(%q) 에러: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParseVersion(%q) = %+v, want %+v", c.in, got, c.want)
		}
	}
}

func TestVersionString(t *testing.T) {
	v := Version{1, 2, 3}
	if got := v.String(); got != "1.2.3" {
		t.Errorf("String = %q, want %q", got, "1.2.3")
	}
}

func TestVersionBump(t *testing.T) {
	base := Version{1, 2, 3}
	cases := []struct {
		bump BumpType
		want Version
		err  bool
	}{
		{BumpMajor, Version{2, 0, 0}, false},
		{BumpMinor, Version{1, 3, 0}, false},
		{BumpPatch, Version{1, 2, 4}, false},
		{BumpUnknown, Version{}, true},
	}
	for _, c := range cases {
		got, err := base.Bump(c.bump)
		if c.err {
			if err == nil {
				t.Errorf("Bump(%v) 에러 기대", c.bump)
			}
			continue
		}
		if err != nil {
			t.Errorf("Bump(%v) 에러: %v", c.bump, err)
			continue
		}
		if got != c.want {
			t.Errorf("Bump(%v) = %+v, want %+v", c.bump, got, c.want)
		}
	}
}

func TestVersionTag(t *testing.T) {
	v := Version{1, 0, 0}
	m, ok := FindModule("product")
	if !ok {
		t.Fatal("product 모듈 찾기 실패")
	}
	got := v.Tag(m)
	want := "sandbox-product/v1.0.0"
	if got != want {
		t.Errorf("Tag = %q, want %q", got, want)
	}
}

func TestVersionCompare(t *testing.T) {
	cases := []struct {
		a, b Version
		want int
	}{
		{Version{1, 0, 0}, Version{1, 0, 0}, 0},
		{Version{1, 0, 0}, Version{2, 0, 0}, -1},
		{Version{2, 0, 0}, Version{1, 0, 0}, 1},
		{Version{1, 1, 0}, Version{1, 2, 0}, -1},
		{Version{1, 2, 3}, Version{1, 2, 4}, -1},
	}
	for _, c := range cases {
		got := c.a.Compare(c.b)
		if got != c.want {
			t.Errorf("Compare(%v, %v) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestParseBumpType(t *testing.T) {
	cases := []struct {
		in   string
		want BumpType
		ok   bool
	}{
		{"major", BumpMajor, true},
		{"MAJOR", BumpMajor, true},
		{"메이저", BumpMajor, true},
		{"minor", BumpMinor, true},
		{"마이너", BumpMinor, true},
		{"patch", BumpPatch, true},
		{"패치", BumpPatch, true},
		{"  patch  ", BumpPatch, true},
		{"hotfix", BumpUnknown, false},
		{"", BumpUnknown, false},
	}
	for _, c := range cases {
		got, ok := ParseBumpType(c.in)
		if ok != c.ok || got != c.want {
			t.Errorf("ParseBumpType(%q) = (%v, %v), want (%v, %v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestSandboxModulesIntegrity(t *testing.T) {
	if len(SandboxModules) == 0 {
		t.Fatal("SandboxModules 비어있음")
	}
	seen := make(map[string]bool)
	for _, m := range SandboxModules {
		if m.Key == "" {
			t.Errorf("module %+v: Key 비어있음", m)
		}
		if seen[m.Key] {
			t.Errorf("duplicate key: %s", m.Key)
		}
		seen[m.Key] = true
		if m.VersionPath == "" {
			t.Errorf("module %s: VersionPath 비어있음", m.Key)
		}
		if m.TagPrefix == "" {
			t.Errorf("module %s: TagPrefix 비어있음", m.Key)
		}
		if m.ReleaseBranch == "" {
			t.Errorf("module %s: ReleaseBranch 비어있음", m.Key)
		}
	}
}

func TestModulesByLine(t *testing.T) {
	be := ModulesByLine(LineBackend)
	if len(be) == 0 {
		t.Error("backend 모듈 0개")
	}
	for _, m := range be {
		if m.Line != LineBackend {
			t.Errorf("ModulesByLine(Backend)에 %s 라인 모듈 포함: %s", m.Line, m.Key)
		}
	}
}
