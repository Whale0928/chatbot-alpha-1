package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"chatbot-alpha-1/pkg/github"
	"chatbot-alpha-1/pkg/llm"
	"chatbot-alpha-1/pkg/llm/render"
	"chatbot-alpha-1/pkg/llm/summarize"
	"chatbot-alpha-1/pkg/release"

	"github.com/joho/godotenv"
	"github.com/spf13/cobra"
)

// newReleaseBotCmd는 디스코드 없이 릴리즈 흐름을 stdout에서 검증하는 서브커맨드.
//
// 흐름 (Slice 4 — dry-run 전용):
//  1. 모듈 결정 (--module)
//  2. 이전 tag 자동 결정 (ListTags + semver 정렬) 또는 --base-tag override
//  3. GetFile 로 현재 VERSION 확인 (sanity)
//  4. CompareCommits (base ↔ main) 로 커밋/파일 통계 수집
//  5. summarize.Release → render.RenderReleasePRBody
//  6. PR 본문 마크다운 stdout
//
// Slice 4 시점에는 --apply 불가 (write API 미구현). --apply 플래그는 거부.
func newReleaseBotCmd(envFileRef *string) *cobra.Command {
	var (
		moduleKey string
		bumpStr   string
		owner     string
		repo      string
		baseTag   string
		directive string
		head      string
		apply     bool
	)

	cmd := &cobra.Command{
		Use:   "release-bot",
		Short: "Discord 없이 릴리즈 PR 본문 생성 파이프라인을 검증한다 (dry-run)",
		Long: `Discord 봇을 띄우지 않고 릴리즈 흐름을 stdout에서 시뮬레이션 (기본 dry-run).

흐름:
  1. --module/--bump 로 새 버전 계산
  2. 직전 tag 자동 결정 (또는 --base-tag override)
  3. base ↔ head diff/커밋 수집
  4. LLM 으로 릴리즈 노트 본문 생성
  5. PR 본문 마크다운을 stdout 출력
  6. --apply 시 실제 호출:
     - main 의 VERSION 파일 갱신 (commit)
     - lightweight git tag 생성 + push
     - release/* 브랜치 없으면 base tag 의 sha 에서 생성
     - PR 생성 (base=release/sandbox-{module}, head=main)
     - 머지는 사람이 직접 (auto-merge 사용 안 함)

예시:
  go run . release-bot --module=product --bump=patch                   # dry-run
  go run . release-bot --module=admin   --bump=minor --directive "내부 섹션 생략"
  go run . release-bot --module=product --bump=patch --apply           # 실제 PR/tag 생성`,
		RunE: func(cmd *cobra.Command, args []string) error {
			bump, ok := release.ParseBumpType(bumpStr)
			if !ok {
				return fmt.Errorf("알 수 없는 --bump=%q (major|minor|patch)", bumpStr)
			}
			return runReleaseBot(*envFileRef, moduleKey, bump, owner, repo, baseTag, head, directive, apply)
		},
		SilenceUsage: true,
	}

	cmd.Flags().StringVar(&moduleKey, "module", "", "릴리즈 대상 모듈 키 (sandbox: product|admin|batch / prod: product|admin|frontend|dashboard, 필수)")
	cmd.Flags().StringVar(&bumpStr, "bump", "", "버전 bump 방식 (major|minor|patch, 필수)")
	cmd.Flags().StringVar(&owner, "owner", "", "GitHub 레포 owner (비우면 모듈 레지스트리의 값 사용)")
	cmd.Flags().StringVar(&repo, "repo", "", "GitHub 레포 이름 (비우면 모듈 레지스트리의 값 사용)")
	cmd.Flags().StringVar(&baseTag, "base-tag", "", "비교 base tag (비우면 ListTags 로 자동 결정)")
	cmd.Flags().StringVar(&head, "head", "main", "비교 head ref (기본 main)")
	cmd.Flags().StringVar(&directive, "directive", "", "LLM 추가 지시 (선택)")
	cmd.Flags().BoolVar(&apply, "apply", false, "실제 VERSION push / tag 생성 / PR 생성 (기본 dry-run)")
	_ = cmd.MarkFlagRequired("module")
	_ = cmd.MarkFlagRequired("bump")

	return cmd
}

func runReleaseBot(envFile, moduleKey string, bump release.BumpType, owner, repo, baseTag, head, directive string, apply bool) error {
	if envFile != "" {
		_ = godotenv.Load(envFile)
	} else {
		_ = godotenv.Load()
	}

	module, ok := release.FindModule(moduleKey)
	if !ok {
		keys := make([]string, 0, len(release.Modules))
		for _, m := range release.Modules {
			keys = append(keys, m.Key)
		}
		return fmt.Errorf("알 수 없는 --module=%q (등록된 모듈: %s)", moduleKey, strings.Join(keys, "|"))
	}

	// owner/repo 미지정 시 모듈 레지스트리의 값을 사용.
	if owner == "" {
		owner = module.Owner
	}
	if repo == "" {
		repo = module.Repo
	}

	ghToken := os.Getenv("GITHUB_TOKEN")
	if ghToken == "" {
		return fmt.Errorf("GITHUB_TOKEN 환경변수가 필요합니다")
	}
	gptKey := os.Getenv("GPT_API_KEY")
	if gptKey == "" {
		return fmt.Errorf("GPT_API_KEY 환경변수가 필요합니다")
	}

	gh, err := github.NewClient(ghToken)
	if err != nil {
		return fmt.Errorf("GitHub 클라이언트 초기화 실패: %w", err)
	}
	llmCl, err := llm.NewClient(gptKey)
	if err != nil {
		return fmt.Errorf("LLM 클라이언트 초기화 실패: %w", err)
	}

	fmt.Printf("[release-bot] module=%s(%s) bump=%s repo=%s/%s head=%s\n",
		module.DisplayName, module.Key, bump, owner, repo, head)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	// 1) 이전 tag 결정 — 자동 결정 시 commit SHA 도 같이 보관 (release/* 분기점에 사용)
	var baseTagCommitSHA string
	if baseTag == "" {
		fmt.Printf("[release-bot] ListTags (prefix=%s) ...\n", module.TagPrefix)
		t0 := time.Now()
		tags, err := gh.ListTags(ctx, owner, repo)
		if err != nil {
			return fmt.Errorf("ListTags 실패: %w", err)
		}
		fmt.Printf("[release-bot] ListTags 완료: %d건 (elapsed=%s)\n",
			len(tags), time.Since(t0).Round(time.Millisecond))

		names := make([]string, len(tags))
		for i, tg := range tags {
			names[i] = tg.Name
		}
		latest, found := release.ResolveLatestTag(names, module)
		if !found {
			return fmt.Errorf("`%s/v*` 형식 태그를 찾지 못했습니다. --base-tag 로 명시하거나 첫 릴리즈를 직접 만들어주세요", module.TagPrefix)
		}
		baseTag = latest.TagName
		for _, tg := range tags {
			if tg.Name == latest.TagName {
				baseTagCommitSHA = tg.Commit.SHA
				break
			}
		}
		fmt.Printf("[release-bot] 직전 tag 자동 결정: %s (v%s, sha=%s)\n",
			baseTag, latest.Version, shortSHA(baseTagCommitSHA))
	} else {
		fmt.Printf("[release-bot] 직전 tag override: %s\n", baseTag)
	}

	// 2) 현재 VERSION 파일 sanity 확인
	fmt.Printf("[release-bot] GetFile %s @ %s ...\n", module.VersionPath, head)
	fc, err := gh.GetFile(ctx, owner, repo, module.VersionPath, head)
	if err != nil {
		return fmt.Errorf("GetFile 실패 (VERSION 파일 누락?): %w", err)
	}
	curVer, err := release.ParseVersion(string(fc.Content))
	if err != nil {
		return fmt.Errorf("VERSION 파일 파싱 실패 (%s): %w", module.VersionPath, err)
	}
	fmt.Printf("[release-bot] 현재 VERSION = %s (file sha=%s)\n", curVer, fc.SHA[:min(7, len(fc.SHA))])

	// 3) bump 적용
	newVer, err := curVer.Bump(bump)
	if err != nil {
		return fmt.Errorf("bump 실패: %w", err)
	}
	fmt.Printf("[release-bot] 새 버전: %s → %s (%s)\n", curVer, newVer, bump)

	// 4) compare
	fmt.Printf("[release-bot] CompareCommits %s ↔ %s ...\n", baseTag, head)
	t0 := time.Now()
	cmp, err := gh.CompareCommits(ctx, owner, repo, baseTag, head)
	if err != nil {
		return fmt.Errorf("CompareCommits 실패: %w", err)
	}
	fmt.Printf("[release-bot] CompareCommits 완료: status=%s ahead=%d commits=%d files=%d (elapsed=%s)\n",
		cmp.Status, cmp.AheadBy, len(cmp.Commits), len(cmp.Files), time.Since(t0).Round(time.Millisecond))
	if cmp.TotalCommits > len(cmp.Commits) {
		fmt.Printf("[release-bot] WARN: total_commits(%d) > received(%d) — GitHub 250 한계. 호출자 보강 필요.\n",
			cmp.TotalCommits, len(cmp.Commits))
	}

	if len(cmp.Commits) == 0 {
		fmt.Println("[release-bot] 커밋 0건 — base ↔ head 가 동일하거나 head 가 뒤처짐. LLM 호출 skip.")
		return nil
	}

	// 5) LLM 호출
	in := summarize.ReleaseInput{
		ModuleKey:   module.Key,
		DisplayName: module.DisplayName,
		PrevTag:     baseTag,
		PrevVersion: curVer.String(),
		NewVersion:  newVer.String(),
		BumpLabel:   bump.String(),
		Commits:     cmp.Commits,
		Files:       cmp.Files,
		Directive:   directive,
	}
	fmt.Printf("[release-bot] summarize.Release 호출 (commits=%d files=%d directive_runes=%d) ...\n",
		len(in.Commits), len(in.Files), len([]rune(directive)))
	t0 = time.Now()
	resp, err := summarize.Release(ctx, llmCl, in)
	dur := time.Since(t0)
	if err != nil {
		return fmt.Errorf("summarize.Release 실패: %w", err)
	}
	fmt.Printf("[release-bot] LLM 완료 (elapsed=%s, markdown_runes=%d)\n",
		dur.Round(time.Millisecond), len([]rune(resp.Markdown)))

	// 6) 렌더 + stdout
	rendered := render.RenderReleasePRBody(render.ReleaseRenderInput{
		ModuleDisplayName: module.DisplayName,
		NewVersion:        newVer.String(),
		PrevTag:           baseTag,
		NewTag:            newVer.Tag(module),
		CommitCount:       len(cmp.Commits),
		BumpLabel:         bump.String(),
		Response:          resp,
	})

	fmt.Println()
	label := "dry-run"
	if apply {
		label = "apply"
	}
	fmt.Printf("━━━ PR BODY (%s) ━━━\n", label)
	fmt.Println(rendered)
	fmt.Println("━━━ END ━━━")

	prTitle := fmt.Sprintf("[deploy] %s-v%s", module.Key, newVer)
	newTag := newVer.Tag(module)

	fmt.Printf("\n[release-bot] PR 제목: %s\n", prTitle)
	fmt.Printf("[release-bot] 생성 예정 tag: %s\n", newTag)
	fmt.Printf("[release-bot] PR base: %s ← head: %s\n", module.ReleaseBranch, head)
	if !module.HasDeploy {
		fmt.Printf("[release-bot] 주의: 모듈 %s 는 HasDeploy=false (prod 자동배포 워크플로우 없음)\n", module.Key)
	}

	if !apply {
		fmt.Println("\n[release-bot] dry-run 종료. 실제 적용하려면 --apply 플래그를 추가하세요.")
		return nil
	}

	// ─── --apply 분기 ─────────────────────────────────────────────────────
	fmt.Println("\n[release-bot] --apply: 실제 GitHub 호출 시작 ────────")

	// 7) main 의 VERSION 파일 갱신
	fmt.Printf("[release-bot] UpdateFile %s on %s (sha=%s → bump %s)\n",
		module.VersionPath, head, shortSHA(fc.SHA), newVer)
	upd, err := gh.UpdateFile(ctx, owner, repo, github.UpdateFileInput{
		Path:    module.VersionPath,
		Content: []byte(newVer.String() + "\n"),
		SHA:     fc.SHA,
		Message: fmt.Sprintf("chore(%s): bump VERSION to %s", module.Key, newVer),
		Branch:  head,
	})
	if err != nil {
		return fmt.Errorf("UpdateFile 실패: %w", err)
	}
	fmt.Printf("[release-bot] UpdateFile OK: new_commit=%s new_file_sha=%s\n",
		shortSHA(upd.CommitSHA), shortSHA(upd.FileSHA))

	// 8) tag 생성 (lightweight)
	tagRef := "refs/tags/" + newTag
	fmt.Printf("[release-bot] CreateRef %s → %s\n", tagRef, shortSHA(upd.CommitSHA))
	if _, err := gh.CreateRef(ctx, owner, repo, tagRef, upd.CommitSHA); err != nil {
		if errors.Is(err, github.ErrAlreadyExists) {
			fmt.Printf("[release-bot] WARN: tag %s 이미 존재 — 진행 계속\n", newTag)
		} else {
			return fmt.Errorf("CreateRef(tag) 실패: %w", err)
		}
	}

	// 9) release/* 브랜치 존재 확인 + 없으면 생성 (분기점은 base tag commit sha)
	branchRef := "heads/" + module.ReleaseBranch
	if _, err := gh.GetRef(ctx, owner, repo, branchRef); err != nil {
		if !errors.Is(err, github.ErrNotFound) {
			return fmt.Errorf("GetRef(release branch) 실패: %w", err)
		}
		if baseTagCommitSHA == "" {
			// --base-tag override 였던 경우. 그 tag 의 sha 를 조회.
			r, err := gh.GetRef(ctx, owner, repo, "tags/"+baseTag)
			if err != nil {
				return fmt.Errorf("base tag SHA 조회 실패 (release/* 분기점 결정 불가): %w", err)
			}
			baseTagCommitSHA = r.Object.SHA
		}
		fmt.Printf("[release-bot] release branch 없음 — CreateRef refs/%s → %s\n", branchRef, shortSHA(baseTagCommitSHA))
		if _, err := gh.CreateRef(ctx, owner, repo, "refs/"+branchRef, baseTagCommitSHA); err != nil {
			return fmt.Errorf("CreateRef(release branch) 실패: %w", err)
		}
	} else {
		fmt.Printf("[release-bot] release branch %s 존재 — 그대로 사용\n", module.ReleaseBranch)
	}

	// 10) PR 생성 (또는 기존 open PR 본문 갱신 — 멱등)
	fmt.Printf("[release-bot] ListPullRequestsByHead %s ← %s ...\n", module.ReleaseBranch, head)
	existing, err := gh.ListPullRequestsByHead(ctx, owner, repo,
		owner+":"+head, module.ReleaseBranch, "open")
	if err != nil {
		return fmt.Errorf("ListPullRequestsByHead 실패: %w", err)
	}
	var pr *github.PullRequest
	if len(existing) > 0 {
		fmt.Printf("[release-bot] 기존 open PR #%d 발견 — 본문 갱신 (멱등)\n", existing[0].Number)
		pr, err = gh.UpdatePullRequest(ctx, owner, repo, existing[0].Number, github.UpdatePullRequestInput{
			Title: prTitle,
			Body:  rendered,
		})
		if err != nil {
			return fmt.Errorf("UpdatePullRequest #%d 실패: %w", existing[0].Number, err)
		}
	} else {
		fmt.Printf("[release-bot] CreatePullRequest base=%s head=%s title=%q\n",
			module.ReleaseBranch, head, prTitle)
		pr, err = gh.CreatePullRequest(ctx, owner, repo, github.CreatePullRequestInput{
			Title: prTitle,
			Body:  rendered,
			Head:  head,
			Base:  module.ReleaseBranch,
		})
		if err != nil {
			return fmt.Errorf("CreatePullRequest 실패: %w", err)
		}
	}
	fmt.Printf("[release-bot] PR OK: #%d %s\n", pr.Number, pr.HTMLURL)

	fmt.Println("\n[release-bot] 완료. GitHub 에서 직접 머지하세요 (auto-merge 비활성).")
	return nil
}

// shortSHA 는 로그용으로 sha 의 앞 7글자만 반환한다. 빈 입력은 빈 결과.
func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
