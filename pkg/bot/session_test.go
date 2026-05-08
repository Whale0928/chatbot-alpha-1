package bot

import (
	"sync"
	"testing"
)

func Test_메모를_여러번_입력할_때_Notes에_Author와_함께_누적된다(t *testing.T) {
	sess := &Session{}
	sess.AddNote("hgkim", "라이 필터 버번 노출 버그")
	sess.AddNote("Whale0928", "운영에서 발생 중")
	sess.AddNote("hgkim", "심각도 high")

	notes := sess.SnapshotNotes()
	if len(notes) != 3 {
		t.Fatalf("expected 3 notes, got %d", len(notes))
	}
	if notes[0].Author != "hgkim" || notes[0].Content != "라이 필터 버번 노출 버그" {
		t.Errorf("note[0] mismatch: %+v", notes[0])
	}
	if notes[1].Author != "Whale0928" {
		t.Errorf("note[1] author = %q", notes[1].Author)
	}
	if notes[2].Timestamp.IsZero() {
		t.Errorf("note[2] timestamp must be set")
	}
}

func Test_같은_사용자가_여러번_발화할_때_Speakers에_한번만_포함된다(t *testing.T) {
	sess := &Session{}
	sess.AddNote("hgkim", "a")
	sess.AddNote("hgkim", "b")
	sess.AddNote("hgkim", "c")
	sess.AddNote("Whale0928", "d")

	speakers := sess.SortedSpeakers()
	if len(speakers) != 2 {
		t.Fatalf("expected 2 unique speakers, got %d (%v)", len(speakers), speakers)
	}
	if speakers[0] != "Whale0928" || speakers[1] != "hgkim" {
		t.Errorf("sorted speakers mismatch: %v", speakers)
	}
}

func Test_미팅종료_시점이_언제더라_같은_문장을_입력할_때_종료로_오탐되지_않는다(t *testing.T) {
	cases := []string{
		"미팅 종료 시점이 언제더라",
		"미팅 종료할지 말지 얘기해보자",
		"회의 종료가 이상해",
		"아직 미팅 종료 안 했지?",
	}
	for _, c := range cases {
		if IsMeetingEndCommand(c) {
			t.Errorf("false positive: %q should not be end command", c)
		}
	}
}

func Test_정확히_미팅종료만_입력할_때_종료로_판별된다(t *testing.T) {
	cases := []string{
		"미팅 종료",
		"회의 종료",
		"  미팅 종료  ",
		"\n미팅 종료\n",
	}
	for _, c := range cases {
		if !IsMeetingEndCommand(c) {
			t.Errorf("false negative: %q should be end command", c)
		}
	}
}

func Test_세션이_존재하는_채널ID로_lookupSession_호출할_때_해당_세션을_반환한다(t *testing.T) {
	// 전역 맵 보호: 테스트 종료 후 복원
	sessionsMu.Lock()
	backup := sessions
	sessions = map[string]*Session{
		"thread-42": {ThreadID: "thread-42", Mode: ModeMeeting},
	}
	sessionsMu.Unlock()
	defer func() {
		sessionsMu.Lock()
		sessions = backup
		sessionsMu.Unlock()
	}()

	got := lookupSession("thread-42")
	if got == nil {
		t.Fatal("expected session, got nil")
	}
	if got.ThreadID != "thread-42" {
		t.Errorf("wrong session: %+v", got)
	}
}

func Test_존재하지_않는_채널ID로_lookupSession_호출할_때_nil을_반환한다(t *testing.T) {
	sessionsMu.Lock()
	backup := sessions
	sessions = map[string]*Session{}
	sessionsMu.Unlock()
	defer func() {
		sessionsMu.Lock()
		sessions = backup
		sessionsMu.Unlock()
	}()

	if got := lookupSession("nope"); got != nil {
		t.Errorf("expected nil, got %+v", got)
	}
}

func Test_truncate_한글_문자열을_rune_기준으로_자른다(t *testing.T) {
	in := "한글이포함된긴문장입니다"
	out := truncate(in, 5)
	if out != "한글이포함" {
		t.Errorf("expected '한글이포함', got %q", out)
	}
	// UTF-8 중간 절단 흔적이 없어야 함
	for _, r := range out {
		if r == '\uFFFD' {
			t.Errorf("found replacement rune in %q", out)
		}
	}
}

func Test_truncate_영문_문자열은_기존처럼_동작한다(t *testing.T) {
	if truncate("abcdef", 3) != "abc" {
		t.Errorf("ascii truncate broken")
	}
	if truncate("abc", 10) != "abc" {
		t.Errorf("short ascii should return as-is")
	}
}

func Test_truncate_정확히_경계길이일_때_원본을_반환한다(t *testing.T) {
	if truncate("한글글", 3) != "한글글" {
		t.Errorf("boundary case broken")
	}
}

func Test_동시에_여러_goroutine에서_메모를_추가할_때_경합없이_모두_기록된다(t *testing.T) {
	sess := &Session{}

	const authors = 4
	const perAuthor = 50
	var wg sync.WaitGroup
	wg.Add(authors)
	for i := 0; i < authors; i++ {
		author := []string{"a", "b", "c", "d"}[i]
		go func() {
			defer wg.Done()
			for j := 0; j < perAuthor; j++ {
				sess.AddNote(author, "msg")
			}
		}()
	}
	wg.Wait()

	notes := sess.SnapshotNotes()
	if len(notes) != authors*perAuthor {
		t.Fatalf("expected %d notes, got %d", authors*perAuthor, len(notes))
	}
	speakers := sess.SortedSpeakers()
	if len(speakers) != authors {
		t.Fatalf("expected %d speakers, got %d (%v)", authors, len(speakers), speakers)
	}
}
