package github

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// FileContent는 /contents 응답 (file 케이스).
// SHA는 UpdateFile 호출 시 if-match 용으로 필수.
type FileContent struct {
	Path     string
	SHA      string
	Encoding string // 보통 "base64"
	Content  []byte // base64 decode된 raw bytes
}

// apiFileContent는 raw JSON 매핑용.
type apiFileContent struct {
	Type     string `json:"type"` // "file" 외에는 거부
	SHA      string `json:"sha"`
	Path     string `json:"path"`
	Encoding string `json:"encoding"`
	Content  string `json:"content"` // base64 — GitHub 응답은 76자 단위 개행 포함 가능
}

// GetFile은 ref 시점의 파일 내용을 읽는다.
// ref 비어있으면 default branch. ref는 branch/tag/sha 모두 허용.
//
// 디렉토리(type="dir")나 심볼릭링크는 거부.
func (c *Client) GetFile(ctx context.Context, owner, repo, path, ref string) (*FileContent, error) {
	if owner == "" || repo == "" || path == "" {
		return nil, fmt.Errorf("github: owner/repo/path is empty")
	}
	q := url.Values{}
	if ref != "" {
		q.Set("ref", ref)
	}
	qs := ""
	if len(q) > 0 {
		qs = "?" + q.Encode()
	}
	u := fmt.Sprintf("%s/repos/%s/%s/contents/%s%s", c.baseURL, owner, repo, escapeContentsPath(path), qs)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", userAgent)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("github: contents request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("github: %s %d: %s", u, resp.StatusCode, string(body))
	}
	var raw apiFileContent
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("github: decode contents: %w", err)
	}
	if raw.Type != "file" {
		return nil, fmt.Errorf("github: %s: type=%q (file 아님)", path, raw.Type)
	}
	if raw.Encoding != "base64" {
		return nil, fmt.Errorf("github: %s: encoding=%q (base64 외 미지원)", path, raw.Encoding)
	}
	clean := strings.ReplaceAll(raw.Content, "\n", "")
	bs, err := base64.StdEncoding.DecodeString(clean)
	if err != nil {
		return nil, fmt.Errorf("github: %s: base64 decode: %w", path, err)
	}
	return &FileContent{
		Path:     raw.Path,
		SHA:      raw.SHA,
		Encoding: raw.Encoding,
		Content:  bs,
	}, nil
}

// escapeContentsPath는 경로의 각 segment를 PathEscape 하되 slash는 보존한다.
// "testdata/release-sandbox/product/VERSION" → 그대로, 한글/공백 포함 segment만 인코딩.
func escapeContentsPath(p string) string {
	parts := strings.Split(p, "/")
	for i, s := range parts {
		parts[i] = url.PathEscape(s)
	}
	return strings.Join(parts, "/")
}

// UpdateFileInput 은 UpdateFile 의 입력 묶음.
type UpdateFileInput struct {
	Path    string // 레포 루트 기준 상대 경로
	Content []byte // raw bytes — 함수가 base64 인코딩
	SHA     string // 기존 파일의 blob sha (없으면 새 파일 생성)
	Message string // commit message
	Branch  string // 비어있으면 default branch
}

// UpdateFileResult 은 PUT /contents 응답에서 봇이 사용하는 필드만.
type UpdateFileResult struct {
	CommitSHA string // 새 commit sha — tag 생성에 사용
	FileSHA   string // 갱신된 file blob sha — 후속 갱신에 사용
}

// UpdateFile 은 ref 지정 브랜치에 파일을 생성/갱신한다 (Contents API).
//
// SHA 가 비어있으면 새 파일 생성, 있으면 갱신. GitHub 은 SHA 가 stale 하면 409 반환 —
// 호출자는 GetFile 로 최신 SHA 를 받아 즉시 UpdateFile 을 부르는 패턴을 유지해야 한다.
//
// commit author 는 토큰의 사용자 정보로 자동 채워진다 (별도 author/committer 미지정).
func (c *Client) UpdateFile(ctx context.Context, owner, repo string, in UpdateFileInput) (*UpdateFileResult, error) {
	if owner == "" || repo == "" || in.Path == "" {
		return nil, fmt.Errorf("github: updatefile: owner/repo/path is empty")
	}
	if in.Message == "" {
		return nil, fmt.Errorf("github: updatefile: commit message is empty")
	}

	body := map[string]any{
		"message": in.Message,
		"content": base64.StdEncoding.EncodeToString(in.Content),
	}
	if in.SHA != "" {
		body["sha"] = in.SHA
	}
	if in.Branch != "" {
		body["branch"] = in.Branch
	}
	bs, _ := json.Marshal(body)

	u := fmt.Sprintf("%s/repos/%s/%s/contents/%s", c.baseURL, owner, repo, escapeContentsPath(in.Path))
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, u, bytes.NewReader(bs))
	if err != nil {
		return nil, err
	}
	c.setCommonHeaders(req)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("github: updatefile request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("github: %s %d: %s", u, resp.StatusCode, string(respBody))
	}

	var raw struct {
		Content struct {
			SHA string `json:"sha"`
		} `json:"content"`
		Commit struct {
			SHA string `json:"sha"`
		} `json:"commit"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("github: decode updatefile: %w", err)
	}
	return &UpdateFileResult{
		CommitSHA: raw.Commit.SHA,
		FileSHA:   raw.Content.SHA,
	}, nil
}
