package github

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newTestClient는 mock handler를 가진 httptest.Server와 그 URL을 바라보는 Client를 만든다.
// 모든 read API 테스트가 공통으로 쓴다.
func newTestClient(t *testing.T, handler http.HandlerFunc) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	c, err := NewClientForTest("dummy-token", srv.URL)
	if err != nil {
		t.Fatalf("NewClientForTest: %v", err)
	}
	return c, srv
}

func assertCommonHeaders(t *testing.T, r *http.Request) {
	t.Helper()
	if got := r.Header.Get("Authorization"); got != "Bearer dummy-token" {
		t.Errorf("Authorization = %q", got)
	}
	if got := r.Header.Get("Accept"); got != "application/vnd.github+json" {
		t.Errorf("Accept = %q", got)
	}
	if got := r.Header.Get("X-GitHub-Api-Version"); got != "2022-11-28" {
		t.Errorf("X-GitHub-Api-Version = %q", got)
	}
	if got := r.Header.Get("User-Agent"); got == "" {
		t.Error("User-Agent 비어있음")
	}
}

func TestListTags(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		assertCommonHeaders(t, r)
		if r.URL.Path != "/repos/owner/repo/tags" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if r.URL.Query().Get("per_page") != "100" {
			t.Errorf("per_page = %q", r.URL.Query().Get("per_page"))
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `[
			{"name":"sandbox-product/v1.0.0","commit":{"sha":"abc","url":"x"}},
			{"name":"v0.5.0","commit":{"sha":"def","url":"y"}}
		]`)
	})

	tags, err := c.ListTags(context.Background(), "owner", "repo")
	if err != nil {
		t.Fatalf("ListTags: %v", err)
	}
	if len(tags) != 2 {
		t.Fatalf("tags 길이 = %d, want 2", len(tags))
	}
	if tags[0].Name != "sandbox-product/v1.0.0" || tags[0].Commit.SHA != "abc" {
		t.Errorf("tags[0] = %+v", tags[0])
	}
}

func TestListTagsPagination(t *testing.T) {
	page := 0
	c, srv := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		page++
		if page == 1 {
			// Link 헤더로 page 2 안내
			next := fmt.Sprintf(`<%s/repos/owner/repo/tags?page=2&per_page=100>; rel="next"`, "$BASE$")
			w.Header().Set("Link", next)
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `[{"name":"t1","commit":{"sha":"s1"}}]`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `[{"name":"t2","commit":{"sha":"s2"}}]`)
	})
	// Link 헤더 URL에 실제 server URL을 박는다.
	c, srv = newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_ = srv
		if r.URL.Query().Get("page") == "2" {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `[{"name":"t2","commit":{"sha":"s2"}}]`)
			return
		}
		w.Header().Set("Link", fmt.Sprintf(`<%s/repos/owner/repo/tags?page=2&per_page=100>; rel="next"`, srv.URL))
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `[{"name":"t1","commit":{"sha":"s1"}}]`)
	})

	tags, err := c.ListTags(context.Background(), "owner", "repo")
	if err != nil {
		t.Fatalf("ListTags: %v", err)
	}
	if len(tags) != 2 {
		t.Fatalf("tags 길이 = %d (want 2)", len(tags))
	}
}

func TestListTagsError(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	_, err := c.ListTags(context.Background(), "owner", "repo")
	if err == nil {
		t.Fatal("error 기대")
	}
}

func TestCompareCommits(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		assertCommonHeaders(t, r)
		// 태그가 slash 포함이라 PathEscape 결과: "sandbox-product%2Fv1.0.0...main"
		// URL.Path는 디코딩되므로 RawPath로 인코딩 보존 여부 검증.
		if !strings.HasPrefix(r.URL.Path, "/repos/owner/repo/compare/") {
			t.Errorf("path = %q", r.URL.Path)
		}
		if !strings.Contains(r.URL.RawPath, "sandbox-product%2Fv1.0.0...main") {
			t.Errorf("base...head 인코딩 누락 (RawPath): %q", r.URL.RawPath)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{
			"status":"ahead",
			"ahead_by":3,
			"behind_by":0,
			"total_commits":3,
			"commits":[
				{"sha":"c1","commit":{"message":"feat: a","author":{"name":"A","date":"2026-05-01T00:00:00Z"}}},
				{"sha":"c2","commit":{"message":"fix: b","author":{"name":"B","date":"2026-05-02T00:00:00Z"}}}
			],
			"files":[
				{"filename":"a.go","status":"modified","additions":2,"deletions":1,"changes":3}
			]
		}`)
	})

	cmp, err := c.CompareCommits(context.Background(), "owner", "repo", "sandbox-product/v1.0.0", "main")
	if err != nil {
		t.Fatalf("CompareCommits: %v", err)
	}
	if cmp.Status != "ahead" || cmp.AheadBy != 3 || cmp.TotalCommits != 3 {
		t.Errorf("status/ahead 잘못: %+v", cmp)
	}
	if len(cmp.Commits) != 2 || cmp.Commits[0].SHA != "c1" || cmp.Commits[0].Message != "feat: a" {
		t.Errorf("commits 잘못: %+v", cmp.Commits)
	}
	if len(cmp.Files) != 1 || cmp.Files[0].Filename != "a.go" {
		t.Errorf("files 잘못: %+v", cmp.Files)
	}
}

func TestGetFile(t *testing.T) {
	content := "1.0.0\n"
	encoded := base64.StdEncoding.EncodeToString([]byte(content))
	// 76자 단위 개행 시뮬레이션
	if len(encoded) > 4 {
		encoded = encoded[:4] + "\n" + encoded[4:]
	}

	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		assertCommonHeaders(t, r)
		if r.URL.Path != "/repos/owner/repo/contents/testdata/release-sandbox/product/VERSION" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if r.URL.Query().Get("ref") != "main" {
			t.Errorf("ref = %q", r.URL.Query().Get("ref"))
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{
			"type":"file",
			"sha":"file-sha-abc",
			"path":"testdata/release-sandbox/product/VERSION",
			"encoding":"base64",
			"content":%q
		}`, encoded)
	})

	got, err := c.GetFile(context.Background(), "owner", "repo", "testdata/release-sandbox/product/VERSION", "main")
	if err != nil {
		t.Fatalf("GetFile: %v", err)
	}
	if got.SHA != "file-sha-abc" {
		t.Errorf("SHA = %q", got.SHA)
	}
	if string(got.Content) != content {
		t.Errorf("Content = %q, want %q", got.Content, content)
	}
}

func TestGetFileRejectsDir(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"type":"dir","sha":"x","path":"x","encoding":"","content":""}`)
	})
	_, err := c.GetFile(context.Background(), "owner", "repo", "somedir", "")
	if err == nil {
		t.Fatal("dir 거부 기대")
	}
	if !strings.Contains(err.Error(), "type=") {
		t.Errorf("에러 메시지 부적절: %v", err)
	}
}
