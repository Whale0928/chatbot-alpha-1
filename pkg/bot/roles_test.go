package bot

import (
	"reflect"
	"testing"

	"github.com/bwmarrin/discordgo"
)

func TestMapRoleIDsToNames(t *testing.T) {
	roleNames := map[string]string{
		"r1": "BACKEND",
		"r2": "FRONTEND",
		"r3": "PM",
	}

	tests := []struct {
		name string
		ids  []string
		want []string
	}{
		{"single role", []string{"r1"}, []string{"BACKEND"}},
		{"multiple roles, ordered", []string{"r3", "r1"}, []string{"PM", "BACKEND"}},
		{"unknown role id is dropped", []string{"r1", "rX", "r2"}, []string{"BACKEND", "FRONTEND"}},
		{"empty input", []string{}, []string{}},
		{"all unknown", []string{"rX", "rY"}, []string{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := mapRoleIDsToNames(roleNames, tc.ids)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestBuildRoleNameMap(t *testing.T) {
	roles := []*discordgo.Role{
		{ID: "r1", Name: "BACKEND"},
		nil, // 방어 — nil entry는 무시
		{ID: "r2", Name: "FRONTEND"},
	}
	got := buildRoleNameMap(roles)
	want := map[string]string{"r1": "BACKEND", "r2": "FRONTEND"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestMarshalUnmarshalRoleSnapshot_RoundTrip(t *testing.T) {
	tests := []struct {
		name string
		in   map[string][]string
	}{
		{"empty", map[string][]string{}},
		{"nil treated as empty", nil},
		{"single user single role", map[string][]string{"u1": {"BACKEND"}}},
		{"multi user multi role",
			map[string][]string{
				"u1": {"BACKEND", "PM"},
				"u2": {"FRONTEND"},
				"u3": {},
			}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b, err := MarshalRoleSnapshot(tc.in)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			got, err := UnmarshalRoleSnapshot(b)
			if err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			// nil 입력은 빈 map으로 정규화 — round-trip은 빈 map과 같음
			expected := tc.in
			if expected == nil {
				expected = map[string][]string{}
			}
			if !reflect.DeepEqual(got, expected) {
				t.Errorf("round-trip mismatch:\n got=%v\nwant=%v\n raw=%s", got, expected, string(b))
			}
		})
	}
}

func TestMarshalRoleSnapshot_EmptyReturnsEmptyJSON(t *testing.T) {
	b, err := MarshalRoleSnapshot(nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "{}" {
		t.Errorf("got %q, want %q", string(b), "{}")
	}
}

func TestUnmarshalRoleSnapshot_EmptyBytes(t *testing.T) {
	got, err := UnmarshalRoleSnapshot(nil)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Error("got nil map, want empty map")
	}
	if len(got) != 0 {
		t.Errorf("got %d entries, want 0", len(got))
	}
}

func TestMarshalAuthorRoles(t *testing.T) {
	tests := []struct {
		name string
		in   []string
		want string
	}{
		{"empty", nil, "[]"},
		{"single", []string{"BACKEND"}, `["BACKEND"]`},
		{"multiple", []string{"BACKEND", "PM"}, `["BACKEND","PM"]`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b, err := MarshalAuthorRoles(tc.in)
			if err != nil {
				t.Fatal(err)
			}
			if string(b) != tc.want {
				t.Errorf("got %q, want %q", string(b), tc.want)
			}
		})
	}
}
