package summarize

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"chatbot-alpha-1/pkg/github"
	"chatbot-alpha-1/pkg/llm"
	"chatbot-alpha-1/pkg/llm/prompts"
)

// releaseSchema는 ReleaseNoteResponse용 strict JSON Schema.
var releaseSchema = llm.GenerateSchema[llm.ReleaseNoteResponse]()

// releaseCommitMessageRunes는 커밋 메시지 첫 줄 한도 (LLM 토큰 절약).
// weekly와 동일한 값 사용.
const releaseCommitMessageRunes = 200

// releaseFileListLimit은 LLM에 노출하는 변경 파일 통계 최대 개수.
// 큰 PR(수백 파일)에서 토큰을 잡아먹지 않도록 가장 변경량 많은 N개만 dump.
// 호출자가 미리 정렬/필터해서 넘기면 더 좋지만 default 안전망.
const releaseFileListLimit = 60

// ReleaseInput은 LLM에 릴리즈 노트 생성 시 넘기는 입력 묶음.
//
// pkg/release 의 BumpType은 의도적으로 의존하지 않고 BumpLabel(한국어 라벨)만 받는다.
// pkg/llm 이 pkg/release 를 import 하면 향후 release 패키지가 llm 컨텍스트를 import 할 때 cycle 가능.
type ReleaseInput struct {
	ModuleKey   string // "product"
	DisplayName string // "프로덕트"
	PrevTag     string // "sandbox-product/v1.0.0" — 비교 base
	PrevVersion string // "1.0.0"
	NewVersion  string // "1.0.1"
	BumpLabel   string // "메이저" / "마이너" / "패치"
	Commits     []github.Commit
	Files       []github.ComparisonFile
	Directive   string // optional. 사용자 [추가 요청] 입력
}

// Release는 입력 dump를 LLM에 보내 PR 본문(### 섹션 시작)을 생성한다.
// 결과의 Markdown은 ### 헤더부터 시작하며, H1/H2 와 메타 풋터는 호출 렌더러가 주입한다.
func Release(ctx context.Context, c *llm.Client, in ReleaseInput) (*llm.ReleaseNoteResponse, error) {
	userMsg := buildReleaseUserMessage(in)
	raw, err := callMeetingFormat(ctx, c, "release", prompts.Release, userMsg, "release_note", releaseSchema)
	if err != nil {
		return nil, err
	}
	var out llm.ReleaseNoteResponse
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, fmt.Errorf("llm: unmarshal release: %w (raw=%q)", err, raw)
	}
	return &out, nil
}

// buildReleaseUserMessage는 LLM user message 본문을 구성한다.
//
// 형식은 weekly 패턴 따라 사람이 읽기 좋은 평문. JSON dump 대비 LLM 인지도 더 높음.
// directive는 헤더 직후, 커밋/파일 dump 앞에 prepend.
func buildReleaseUserMessage(in ReleaseInput) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Module: %s (%s)\n", in.DisplayName, in.ModuleKey)
	fmt.Fprintf(&b, "Bump: %s — %s → %s\n", in.BumpLabel, in.PrevVersion, in.NewVersion)
	fmt.Fprintf(&b, "Compare base: %s ↔ main (커밋 %d개)\n", in.PrevTag, len(in.Commits))

	if d := strings.TrimSpace(in.Directive); d != "" {
		b.WriteString("\nReporting directive from the operator (priority over default style, but must not violate the schema):\n")
		b.WriteString(d)
		b.WriteString("\n")
	}

	b.WriteString("\nCommits (oldest first):\n\n")
	if len(in.Commits) == 0 {
		b.WriteString("(no commits between previous tag and main)\n")
	} else {
		for _, c := range in.Commits {
			writeReleaseCommit(&b, c)
		}
	}

	if len(in.Files) > 0 {
		shown := in.Files
		truncated := false
		if len(shown) > releaseFileListLimit {
			shown = shown[:releaseFileListLimit]
			truncated = true
		}
		fmt.Fprintf(&b, "\nChanged files (%d shown of %d):\n\n", len(shown), len(in.Files))
		for _, f := range shown {
			fmt.Fprintf(&b, "- %s [%s] +%d/-%d\n", f.Filename, f.Status, f.Additions, f.Deletions)
		}
		if truncated {
			b.WriteString("- (... 이하 생략, 상위 변경량 파일만 노출)\n")
		}
	}

	return b.String()
}

// writeReleaseCommit는 단일 커밋을 dump한다.
// 형식: "- abc1234 by login (2026-05-08): feat: 어드민 ... (첫 줄 잘림)"
func writeReleaseCommit(b *strings.Builder, c github.Commit) {
	sha := c.SHA
	if len(sha) > 7 {
		sha = sha[:7]
	}
	author := c.AuthorLogin
	if author == "" {
		author = c.AuthorName
	}
	if author == "" {
		author = "(unknown)"
	}
	first := releaseCommitFirstLine(c.Message)
	fmt.Fprintf(b, "- %s by %s (%s): %s\n",
		sha, author, c.Date.UTC().Format("2006-01-02"), first)
}

// releaseCommitFirstLine은 커밋 메시지 첫 줄만 추출 + 룬 한도로 자른다.
func releaseCommitFirstLine(msg string) string {
	msg = strings.TrimSpace(msg)
	if i := strings.IndexByte(msg, '\n'); i >= 0 {
		msg = msg[:i]
	}
	r := []rune(msg)
	if len(r) > releaseCommitMessageRunes {
		return string(r[:releaseCommitMessageRunes]) + "…"
	}
	return msg
}
