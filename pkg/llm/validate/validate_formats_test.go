package validate

import (
	"strings"
	"testing"

	"chatbot-alpha-1/pkg/llm"
)

func TestValidateRoleBased_RejectsUnknownSpeaker(t *testing.T) {
	notes := []llm.Note{{Author: "hgkim", Content: "발행일자 추출 마무리"}}
	speakers := []string{"hgkim"}
	resp := &llm.RoleBasedResponse{
		Roles: []llm.RoleSection{
			{Speaker: "ghost", Decisions: []string{"발행일자 추출 마무리"}},
		},
	}
	err := RoleBased(resp, notes, speakers)
	if err == nil {
		t.Fatal("발화자 목록 외 speaker는 ERROR 반환해야 함")
	}
	if !strings.Contains(err.Error(), "ghost") {
		t.Errorf("에러 메시지에 unknown speaker 이름 포함 기대: %v", err)
	}
}

func TestValidateRoleBased_AcceptsKnownSpeaker(t *testing.T) {
	notes := []llm.Note{{Author: "hgkim", Content: "발행일자 추출 마무리"}}
	speakers := []string{"hgkim", "whale"}
	resp := &llm.RoleBasedResponse{
		Roles: []llm.RoleSection{
			{Speaker: "hgkim", Decisions: []string{"발행일자 추출 마무리"}},
		},
	}
	if err := RoleBased(resp, notes, speakers); err != nil {
		t.Fatalf("known speaker는 통과해야 함: %v", err)
	}
}

func TestValidateRoleBased_RejectsActionWithUnknownWho(t *testing.T) {
	notes := []llm.Note{{Author: "hgkim", Content: "라이 위스키 #223"}}
	speakers := []string{"hgkim"}
	resp := &llm.RoleBasedResponse{
		Roles: []llm.RoleSection{
			{
				Speaker: "hgkim",
				Actions: []llm.NextStep{{Who: "ghost", What: "라이 위스키 #223 담당"}},
			},
		},
	}
	err := RoleBased(resp, notes, speakers)
	if err == nil {
		t.Fatal("Actions[].Who에 unknown 이름이면 ERROR 기대")
	}
}

func TestParseNoteFormat(t *testing.T) {
	cases := []struct {
		input    string
		expected llm.NoteFormat
		ok       bool
	}{
		{"decision_status", llm.FormatDecisionStatus, true},
		{"discussion", llm.FormatDiscussion, true},
		{"role_based", llm.FormatRoleBased, true},
		{"freeform", llm.FormatFreeform, true},
		{"1", llm.FormatDecisionStatus, true},
		{"4", llm.FormatFreeform, true},
		{"unknown", 0, false},
		{"", 0, false},
	}
	for _, c := range cases {
		got, ok := llm.ParseNoteFormat(c.input)
		if ok != c.ok || (ok && got != c.expected) {
			t.Errorf("ParseNoteFormat(%q) = (%v, %v), expected (%v, %v)", c.input, got, ok, c.expected, c.ok)
		}
	}
}
