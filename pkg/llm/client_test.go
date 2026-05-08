package llm

import (
	"errors"
	"testing"
)

func Test_빈_API키로_NewClient_호출할_때_에러를_반환한다(t *testing.T) {
	_, err := NewClient("")
	if !errors.Is(err, ErrMissingAPIKey) {
		t.Fatalf("expected ErrMissingAPIKey, got %v", err)
	}
}

func Test_유효한_API키로_NewClient_호출할_때_클라이언트를_반환한다(t *testing.T) {
	c, err := NewClient("sk-test-dummy")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c == nil || c.API() == nil {
		t.Fatalf("expected non-nil client")
	}
}
