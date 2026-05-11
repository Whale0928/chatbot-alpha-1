package github

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestGetRef(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		assertCommonHeaders(t, r)
		if r.URL.Path != "/repos/owner/repo/git/ref/heads/main" {
			t.Errorf("path = %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ref":"refs/heads/main","node_id":"x","url":"y","object":{"sha":"abc","type":"commit","url":"z"}}`)
	})
	ref, err := c.GetRef(context.Background(), "owner", "repo", "heads/main")
	if err != nil {
		t.Fatalf("GetRef: %v", err)
	}
	if ref.Object.SHA != "abc" {
		t.Errorf("SHA = %q", ref.Object.SHA)
	}
}

func TestGetRefNotFound(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
	})
	_, err := c.GetRef(context.Background(), "owner", "repo", "heads/missing")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("ErrNotFound 기대, got %v", err)
	}
}

func TestCreateRef(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		assertCommonHeaders(t, r)
		if r.Method != http.MethodPost {
			t.Errorf("method = %s", r.Method)
		}
		if r.URL.Path != "/repos/owner/repo/git/refs" {
			t.Errorf("path = %q", r.URL.Path)
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if body["ref"] != "refs/tags/sandbox-product/v1.0.1" || body["sha"] != "abc" {
			t.Errorf("body = %+v", body)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ref":"refs/tags/sandbox-product/v1.0.1","object":{"sha":"abc","type":"commit"}}`)
	})
	ref, err := c.CreateRef(context.Background(), "owner", "repo", "refs/tags/sandbox-product/v1.0.1", "abc")
	if err != nil {
		t.Fatalf("CreateRef: %v", err)
	}
	if ref.Ref != "refs/tags/sandbox-product/v1.0.1" {
		t.Errorf("Ref = %q", ref.Ref)
	}
}

func TestCreateRefAlreadyExists(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"Reference already exists"}`, http.StatusUnprocessableEntity)
	})
	_, err := c.CreateRef(context.Background(), "owner", "repo", "refs/heads/x", "sha1")
	if !errors.Is(err, ErrAlreadyExists) {
		t.Errorf("ErrAlreadyExists 기대, got %v", err)
	}
}

func TestUpdateFile(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		assertCommonHeaders(t, r)
		if r.Method != http.MethodPut {
			t.Errorf("method = %s", r.Method)
		}
		if r.URL.Path != "/repos/owner/repo/contents/path/to/VERSION" {
			t.Errorf("path = %q", r.URL.Path)
		}
		var body map[string]any
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if body["message"] != "bump" || body["sha"] != "old-sha" || body["branch"] != "main" {
			t.Errorf("body = %+v", body)
		}
		// content 는 base64
		got, _ := base64.StdEncoding.DecodeString(body["content"].(string))
		if string(got) != "1.0.1\n" {
			t.Errorf("content decoded = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"content":{"sha":"new-file-sha"},"commit":{"sha":"new-commit-sha"}}`)
	})
	out, err := c.UpdateFile(context.Background(), "owner", "repo", UpdateFileInput{
		Path: "path/to/VERSION", Content: []byte("1.0.1\n"), SHA: "old-sha", Message: "bump", Branch: "main",
	})
	if err != nil {
		t.Fatalf("UpdateFile: %v", err)
	}
	if out.CommitSHA != "new-commit-sha" || out.FileSHA != "new-file-sha" {
		t.Errorf("out = %+v", out)
	}
}

func TestCreatePullRequest(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		assertCommonHeaders(t, r)
		if r.Method != http.MethodPost {
			t.Errorf("method = %s", r.Method)
		}
		if r.URL.Path != "/repos/owner/repo/pulls" {
			t.Errorf("path = %q", r.URL.Path)
		}
		var body map[string]any
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if body["title"] != "[deploy] product-v1.0.1" {
			t.Errorf("title = %v", body["title"])
		}
		if body["head"] != "main" || body["base"] != "release/sandbox-product" {
			t.Errorf("head/base = %v / %v", body["head"], body["base"])
		}
		if !strings.Contains(body["body"].(string), "## Release") {
			t.Errorf("body 누락: %v", body["body"])
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"number":42,"state":"open","title":"[deploy] product-v1.0.1","html_url":"https://gh/pr/42","body":"...","head":{"sha":"h1","ref":"main"},"base":{"ref":"release/sandbox-product"}}`)
	})
	pr, err := c.CreatePullRequest(context.Background(), "owner", "repo", CreatePullRequestInput{
		Title: "[deploy] product-v1.0.1",
		Body:  "## Release product v1.0.1\n\n…",
		Head:  "main",
		Base:  "release/sandbox-product",
	})
	if err != nil {
		t.Fatalf("CreatePullRequest: %v", err)
	}
	if pr.Number != 42 || pr.HTMLURL != "https://gh/pr/42" {
		t.Errorf("pr = %+v", pr)
	}
}

func TestGetPullRequest(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/owner/repo/pulls/42" {
			t.Errorf("path = %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		mergeable := true
		_ = mergeable
		fmt.Fprint(w, `{"number":42,"state":"open","mergeable":true,"mergeable_state":"clean","merged":false,"head":{"sha":"h1"},"base":{"ref":"release/sandbox-product"}}`)
	})
	pr, err := c.GetPullRequest(context.Background(), "owner", "repo", 42)
	if err != nil {
		t.Fatalf("GetPullRequest: %v", err)
	}
	if pr.Mergeable == nil || *pr.Mergeable != true {
		t.Errorf("mergeable = %v", pr.Mergeable)
	}
	if pr.MergeableState != "clean" {
		t.Errorf("mergeable_state = %q", pr.MergeableState)
	}
}
