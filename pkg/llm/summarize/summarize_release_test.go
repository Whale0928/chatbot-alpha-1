package summarize

import (
	"strings"
	"testing"
	"time"

	"chatbot-alpha-1/pkg/github"
)

func TestBuildReleaseUserMessage_Basic(t *testing.T) {
	in := ReleaseInput{
		ModuleKey:   "product",
		DisplayName: "프로덕트",
		PrevTag:     "sandbox-product/v1.0.0",
		PrevVersion: "1.0.0",
		NewVersion:  "1.0.1",
		BumpLabel:   "패치",
		Commits: []github.Commit{
			{SHA: "abc1234def", AuthorLogin: "alice", Date: time.Date(2026, 5, 8, 0, 0, 0, 0, time.UTC), Message: "feat: 새 기능 추가\n\n부연 설명은 두 번째 줄에"},
			{SHA: "deadbeef", AuthorName: "Bob", Date: time.Date(2026, 5, 9, 0, 0, 0, 0, time.UTC), Message: "fix: 버그 수정"},
		},
		Files: []github.ComparisonFile{
			{Filename: "pkg/a.go", Status: "modified", Additions: 10, Deletions: 2, Changes: 12},
			{Filename: "pkg/b.go", Status: "added", Additions: 50, Deletions: 0, Changes: 50},
		},
	}

	msg := buildReleaseUserMessage(in)

	mustContain := []string{
		"Module: 프로덕트 (product)",
		"Bump: 패치 — 1.0.0 → 1.0.1",
		"Compare base: sandbox-product/v1.0.0 ↔ main (커밋 2개)",
		"- abc1234 by alice (2026-05-08): feat: 새 기능 추가",
		"- deadbee by Bob (2026-05-09): fix: 버그 수정",
		"Changed files (2 shown of 2):",
		"- pkg/a.go [modified] +10/-2",
		"- pkg/b.go [added] +50/-0",
	}
	for _, s := range mustContain {
		if !strings.Contains(msg, s) {
			t.Errorf("user message에 %q 누락:\n%s", s, msg)
		}
	}

	// 커밋 메시지 두 번째 줄은 잘려 들어가지 않아야 함
	if strings.Contains(msg, "부연 설명은 두 번째 줄에") {
		t.Errorf("커밋 메시지 첫 줄만 들어가야 하는데 둘째 줄 포함됨:\n%s", msg)
	}
}

func TestBuildReleaseUserMessage_EmptyCommits(t *testing.T) {
	msg := buildReleaseUserMessage(ReleaseInput{
		ModuleKey:   "batch",
		DisplayName: "배치",
		PrevTag:     "sandbox-batch/v0.1.0",
		PrevVersion: "0.1.0",
		NewVersion:  "0.2.0",
		BumpLabel:   "마이너",
		Commits:     nil,
		Files:       nil,
	})
	if !strings.Contains(msg, "(no commits between previous tag and main)") {
		t.Errorf("빈 커밋 안내 누락:\n%s", msg)
	}
	if strings.Contains(msg, "Changed files") {
		t.Errorf("빈 Files면 섹션 자체 생략해야 하는데 노출됨:\n%s", msg)
	}
}

func TestBuildReleaseUserMessage_Directive(t *testing.T) {
	msg := buildReleaseUserMessage(ReleaseInput{
		ModuleKey:   "product",
		DisplayName: "프로덕트",
		PrevTag:     "sandbox-product/v1.0.0",
		PrevVersion: "1.0.0",
		NewVersion:  "1.1.0",
		BumpLabel:   "마이너",
		Directive:   "내부 섹션은 생략하고 신규/버그 수정만 강조",
	})
	if !strings.Contains(msg, "Reporting directive from the operator") {
		t.Errorf("directive 블록 누락:\n%s", msg)
	}
	if !strings.Contains(msg, "내부 섹션은 생략하고 신규/버그 수정만 강조") {
		t.Errorf("directive 본문 누락:\n%s", msg)
	}
}

func TestBuildReleaseUserMessage_FileListTruncation(t *testing.T) {
	var files []github.ComparisonFile
	for i := 0; i < releaseFileListLimit+10; i++ {
		files = append(files, github.ComparisonFile{
			Filename:  "f.go",
			Status:    "modified",
			Additions: 1, Deletions: 1, Changes: 2,
		})
	}
	msg := buildReleaseUserMessage(ReleaseInput{
		ModuleKey:   "product",
		DisplayName: "프로덕트",
		PrevTag:     "sandbox-product/v1.0.0",
		PrevVersion: "1.0.0",
		NewVersion:  "1.0.1",
		BumpLabel:   "패치",
		Files:       files,
	})
	if !strings.Contains(msg, "이하 생략") {
		t.Errorf("파일 리스트 truncation 안내 누락:\n%s", msg)
	}
	if !strings.Contains(msg, "60 shown of 70") {
		t.Errorf("파일 개수 표기 누락 (60 shown of 70):\n%s", msg)
	}
}

func TestReleaseCommitFirstLine(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"feat: A\nbody", "feat: A"},
		{"  trim me  ", "trim me"},
		{"", ""},
	}
	for _, c := range cases {
		got := releaseCommitFirstLine(c.in)
		if got != c.want {
			t.Errorf("releaseCommitFirstLine(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
