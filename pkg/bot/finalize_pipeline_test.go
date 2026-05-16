package bot

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"chatbot-alpha-1/pkg/db"
	"chatbot-alpha-1/pkg/llm"

	"github.com/bwmarrin/discordgo"
)

type finalizeScenario struct {
	name             string
	notes            []Note
	content          *llm.SummarizedContent
	markdown         string
	wantSpeakers     []string
	wantHumanNotes   int
	wantContextNotes int
	wantSections     []string
	wantOrder        []string
	wantCounts       summarizedCounts
}

type summarizedCounts struct {
	decisions int
	actions   int
	topics    int
	weekly    int
	release   int
	agent     int
	external  int
}

func TestFinalizeSummarized_PhaseARegressionScenarios(t *testing.T) {
	scenarios := phaseARegressionScenarios()
	now := time.Date(2026, 5, 16, 10, 0, 0, 0, time.UTC)

	for _, tc := range scenarios {
		t.Run(tc.name, func(t *testing.T) {
			sess := newFinalizeScenarioSession(tc.name)
			for _, n := range tc.notes {
				sess.AddNoteWithMeta(n)
			}
			msg := &fakeMessenger{}
			summ := &fakeSummarizer{
				extractContentResp: tc.content,
				renderFormatResps: map[llm.NoteFormat]string{
					llm.FormatDecisionStatus: tc.markdown,
				},
				callCounts: map[string]int{},
			}

			keep := FinalizeSummarized(context.Background(), msg, summ, sess, now)

			if keep {
				t.Fatalf("keepSession = true, want false")
			}
			assertCallCount(t, summ.callCounts, "ExtractContent", 1)
			assertCallCount(t, summ.callCounts, "RenderFormat:decision_status", 1)
			if !reflect.DeepEqual(summ.lastExtract.Speakers, tc.wantSpeakers) {
				t.Fatalf("Speakers = %v, want %v", summ.lastExtract.Speakers, tc.wantSpeakers)
			}
			if len(summ.lastExtract.HumanNotes) != tc.wantHumanNotes {
				t.Fatalf("HumanNotes = %d, want %d", len(summ.lastExtract.HumanNotes), tc.wantHumanNotes)
			}
			if len(summ.lastExtract.ContextNotes) != tc.wantContextNotes {
				t.Fatalf("ContextNotes = %d, want %d", len(summ.lastExtract.ContextNotes), tc.wantContextNotes)
			}
			assertSummarizedCounts(t, tc.content, tc.wantCounts)
			assertAttributionGuard(t, tc.content, summ.lastExtract.Speakers)
			assertBotSummariesHaveNoOriginAuthorField(t)

			if len(msg.complexPayloads) != 1 || len(msg.complexPayloads[0].Embeds) != 1 {
				t.Fatalf("summarized embed send missing: %#v", msg.complexPayloads)
			}
			gotMarkdown := msg.complexPayloads[0].Embeds[0].Description
			if gotMarkdown != tc.markdown {
				t.Fatalf("markdown = %q, want %q", gotMarkdown, tc.markdown)
			}
			assertMarkdownContains(t, gotMarkdown, tc.wantSections)
			assertMarkdownOrder(t, gotMarkdown, tc.wantOrder)
			assertNoFourSpaceIndentedLines(t, gotMarkdown)
		})
	}
}

func TestFinalizeSummarized_PhaseACacheToggleRegression(t *testing.T) {
	ctx := context.Background()
	d := newBotTestDB(t)

	oldDBConn := dbConn
	oldSummarizer := summarizer
	t.Cleanup(func() {
		dbConn = oldDBConn
		summarizer = oldSummarizer
	})
	dbConn = d

	if err := d.InsertSession(ctx, db.Session{
		ID:       "sess_phase_a_cache",
		ThreadID: "thread-phase-a-cache",
		GuildID:  "guild-phase-a-cache",
		OwnerID:  "owner-phase-a-cache",
		OpenedAt: time.Unix(1700000000, 0),
		Status:   db.SessionActive,
	}); err != nil {
		t.Fatalf("InsertSession: %v", err)
	}

	scenario := phaseARegressionScenarios()[0]
	sess := newFinalizeScenarioSession("phase-a-cache")
	sess.ThreadID = "thread-phase-a-cache"
	sess.DBSessionID = "sess_phase_a_cache"
	for _, n := range scenario.notes {
		sess.AddNoteWithMeta(n)
	}

	summ := &fakeSummarizer{
		extractContentResp: scenario.content,
		renderFormatResps: map[llm.NoteFormat]string{
			llm.FormatDecisionStatus: scenario.markdown,
			llm.FormatDiscussion:     "## 논의\n\n- discussion render from LLM\n",
		},
		callCounts: map[string]int{},
	}
	summarizer = summ

	keep := FinalizeSummarized(ctx, &fakeMessenger{}, summ, sess, time.Date(2026, 5, 16, 10, 0, 0, 0, time.UTC))
	if keep {
		t.Fatalf("keepSession = true, want false")
	}
	assertCallCount(t, summ.callCounts, "ExtractContent", 1)
	assertCallCount(t, summ.callCounts, "RenderFormat:decision_status", 1)
	assertCallCount(t, summ.callCounts, "RenderFormat:discussion", 0)

	var runs int
	if err := d.QueryRowContext(ctx, "SELECT COUNT(*) FROM finalize_runs").Scan(&runs); err != nil {
		t.Fatalf("count finalize_runs: %v", err)
	}
	if runs != 1 {
		t.Fatalf("finalize_runs after default render = %d, want 1", runs)
	}

	rt := &recordingRoundTripper{}
	discordSession, err := discordgo.New("Bot test-token")
	if err != nil {
		t.Fatalf("discordgo.New: %v", err)
	}
	discordSession.Client = &http.Client{Transport: rt}
	interaction := &discordgo.InteractionCreate{
		Interaction: &discordgo.Interaction{
			AppID: "app-phase-a-cache",
			Token: "token-phase-a-cache",
		},
	}

	// wantHTTPCalls: cache miss는 placeholder edit + final edit = 2, cache hit은 final edit만 = 1.
	toggleAndAssert := func(customID, callKey string, wantCalls int, wantRuns int, wantHTTPCalls int, wantBody string) {
		t.Helper()
		beforeHTTP := len(rt.calls)
		HandleFormatToggle(ctx, discordSession, interaction, sess, customID)
		assertCallCount(t, summ.callCounts, callKey, wantCalls)
		if delta := len(rt.calls) - beforeHTTP; delta != wantHTTPCalls {
			t.Fatalf("HTTP calls delta = %d, want %d", delta, wantHTTPCalls)
		}
		if !strings.Contains(rt.calls[len(rt.calls)-1].body, wantBody) {
			t.Fatalf("final edit body missing %q:\n%s", wantBody, rt.calls[len(rt.calls)-1].body)
		}
		if wantHTTPCalls == 2 && !strings.Contains(rt.calls[len(rt.calls)-2].body, "다시 만드는 중") {
			t.Fatalf("placeholder edit body missing '다시 만드는 중':\n%s", rt.calls[len(rt.calls)-2].body)
		}
		var count int
		if err := d.QueryRowContext(ctx, "SELECT COUNT(*) FROM finalize_runs").Scan(&count); err != nil {
			t.Fatalf("count finalize_runs: %v", err)
		}
		if count != wantRuns {
			t.Fatalf("finalize_runs = %d, want %d", count, wantRuns)
		}
	}

	// 1. decision_status (cache hit — FinalizeSummarized가 미리 캐시 채움) → 1 HTTP
	toggleAndAssert(customIDFormatToggleDecisionStatus, "RenderFormat:decision_status", 1, 1, 1, "📊 주간 분석")
	// 2. discussion (cache miss) → placeholder + final = 2 HTTP
	toggleAndAssert(customIDFormatToggleDiscussion, "RenderFormat:discussion", 1, 2, 2, "discussion render from LLM")
	// 3. decision_status (cache hit) → 1 HTTP
	toggleAndAssert(customIDFormatToggleDecisionStatus, "RenderFormat:decision_status", 1, 2, 1, "📊 주간 분석")
	// 4. discussion (cache hit) → 1 HTTP
	toggleAndAssert(customIDFormatToggleDiscussion, "RenderFormat:discussion", 1, 2, 1, "discussion render from LLM")

	cacheMissDrivenLLMCalls := summ.callCounts["ExtractContent"] + summ.callCounts["RenderFormat:discussion"]
	if cacheMissDrivenLLMCalls != 2 {
		t.Fatalf("cache miss driven LLM calls = %d, want 2", cacheMissDrivenLLMCalls)
	}
}

func phaseARegressionScenarios() []finalizeScenario {
	return []finalizeScenario{
		{
			name: "scenario_1_bot_tools_only",
			notes: []Note{
				contextNote("[weekly]", "repo alpha 주간 분석", db.SourceWeeklyDump),
				contextNote("[weekly]", "repo beta 주간 분석", db.SourceWeeklyDump),
			},
			content: &llm.SummarizedContent{
				WeeklyReports: []llm.WeeklyReportSummary{
					{Repo: "alpha/api", PeriodDays: 7, CommitCount: 12, Highlights: []string{"인증 안정화", "쿼리 최적화"}},
					{Repo: "beta/web", PeriodDays: 7, CommitCount: 8, Highlights: []string{"대시보드 개선", "릴리즈 준비"}},
				},
				ReleaseResults: []llm.ReleaseResultSummary{},
				AgentResponses: []llm.AgentResponseSummary{},
				ExternalRefs:   []llm.ExternalRefSummary{},
				Decisions:      []llm.Decision{},
				Actions:        []llm.SummaryAction{},
				Topics:         []llm.Topic{},
			},
			markdown: strings.Join([]string{
				"## 📊 주간 분석",
				"- alpha/api: 인증 안정화",
				"- beta/web: 대시보드 개선",
				"",
				"## 🗣️ 사람 결정사항",
				"(없음)",
				"",
			}, "\n"),
			wantSpeakers:     []string{},
			wantHumanNotes:   0,
			wantContextNotes: 2,
			wantSections:     []string{"## 📊 주간 분석", "alpha/api", "beta/web", "## 🗣️ 사람 결정사항", "(없음)"},
			wantOrder:        []string{"## 📊 주간 분석", "## 🗣️ 사람 결정사항"},
			wantCounts:       summarizedCounts{weekly: 2},
		},
		{
			name: "scenario_2_human_bot_mix",
			notes: []Note{
				contextNote("[weekly]", "checkout 주간 분석", db.SourceWeeklyDump),
				contextNote("[release]", "product 1.1.5 릴리즈", db.SourceReleaseResult),
				humanNote("alice", "u_alice", []string{"BE"}, "결제 모듈 API는 idempotency key로 고정"),
				humanNote("bob", "u_bob", []string{"QA"}, "QA는 회귀 범위를 checkout으로 제한"),
				humanNote("_deadwhale", "u_deadwhale", []string{"BE"}, "rollback 기준은 오류율 2%로 결정"),
			},
			content: &llm.SummarizedContent{
				WeeklyReports:  []llm.WeeklyReportSummary{{Repo: "checkout/api", PeriodDays: 7, CommitCount: 9, Highlights: []string{"결제 안정화"}}},
				ReleaseResults: []llm.ReleaseResultSummary{{Module: "product", PrevVersion: "1.1.4", NewVersion: "1.1.5", BumpType: "patch", PRNumber: 42, Highlights: []string{"checkout fix"}}},
				Decisions: []llm.Decision{
					{Title: "결제 API idempotency key 적용"},
					{Title: "QA 회귀 범위 checkout 한정"},
					{Title: "rollback 기준 오류율 2%"},
					{Title: "배포 후 모니터링 30분 유지"},
				},
				Actions: []llm.SummaryAction{
					{What: "idempotency key 구현", Origin: "alice", OriginRoles: []string{"BE"}, TargetRoles: []string{"BE"}},
					{What: "checkout 회귀 테스트", Origin: "bob", OriginRoles: []string{"QA"}, TargetRoles: []string{"QA"}},
					{What: "배포 모니터링", Origin: "_deadwhale", OriginRoles: []string{"BE"}, TargetRoles: []string{"BE"}},
				},
				Topics: []llm.Topic{{Title: "결제 안정화", Flow: []string{"API 방향 확정", "QA 범위 합의"}}},
			},
			markdown: strings.Join([]string{
				"## 📊 주간 분석",
				"- checkout/api: 결제 안정화",
				"",
				"## 🚀 릴리즈",
				"- product v1.1.5",
				"",
				"## 🗣️ 사람 결정사항",
				"- alice: 결제 API idempotency key 적용",
				"- bob: QA 회귀 범위 checkout 한정",
				"- _deadwhale: rollback 기준 오류율 2%",
				"- alice: 배포 후 모니터링 30분 유지",
				"",
				"## 액션",
				"- BE: idempotency key 구현",
				"- QA: checkout 회귀 테스트",
				"- BE: 배포 모니터링",
				"",
			}, "\n"),
			wantSpeakers:     []string{"_deadwhale", "alice", "bob"},
			wantHumanNotes:   3,
			wantContextNotes: 2,
			wantSections:     []string{"## 📊 주간 분석", "## 🚀 릴리즈", "## 🗣️ 사람 결정사항", "## 액션", "alice", "bob", "_deadwhale"},
			wantOrder:        []string{"## 📊 주간 분석", "## 🚀 릴리즈", "## 🗣️ 사람 결정사항", "## 액션"},
			wantCounts:       summarizedCounts{decisions: 4, actions: 3, topics: 1, weekly: 1, release: 1},
		},
		{
			name: "scenario_3_human_only",
			notes: []Note{
				humanNote("alice", "u_alice", []string{"BE"}, "결제 모듈은 payment-v2로 분리"),
				humanNote("bob", "u_bob", []string{"QA"}, "승인 취소 케이스를 우선 검증"),
				humanNote("alice", "u_alice", []string{"BE"}, "DB 마이그레이션은 expand-contract로 진행"),
				humanNote("_deadwhale", "u_deadwhale", []string{"BE"}, "배포 순서는 API 다음 worker"),
			},
			content: &llm.SummarizedContent{
				Decisions: []llm.Decision{
					{Title: "결제 모듈 payment-v2 분리"},
					{Title: "승인 취소 케이스 우선 검증"},
					{Title: "DB 마이그레이션 expand-contract 적용"},
				},
				Actions: []llm.SummaryAction{
					{What: "payment-v2 모듈 분리", Origin: "alice", OriginRoles: []string{"BE"}, TargetRoles: []string{"BE"}},
					{What: "승인 취소 회귀 테스트", Origin: "bob", OriginRoles: []string{"QA"}, TargetRoles: []string{"QA"}},
				},
				Topics: []llm.Topic{{Title: "결제 모듈", Flow: []string{"분리 방식 결정", "검증 범위 합의"}}},
			},
			markdown: strings.Join([]string{
				"## 📊 주간 분석",
				"(없음)",
				"",
				"## 🚀 릴리즈",
				"(없음)",
				"",
				"## 🤖 Agent",
				"(없음)",
				"",
				"## 🗣️ 사람 결정사항",
				"- alice: 결제 모듈 payment-v2 분리",
				"- bob: 승인 취소 케이스 우선 검증",
				"- _deadwhale: 배포 순서 API 다음 worker",
				"",
			}, "\n"),
			wantSpeakers:     []string{"_deadwhale", "alice", "bob"},
			wantHumanNotes:   4,
			wantContextNotes: 0,
			wantSections:     []string{"## 📊 주간 분석", "(없음)", "## 🚀 릴리즈", "## 🤖 Agent", "## 🗣️ 사람 결정사항"},
			wantOrder:        []string{"## 📊 주간 분석", "## 🚀 릴리즈", "## 🤖 Agent", "## 🗣️ 사람 결정사항"},
			wantCounts:       summarizedCounts{decisions: 3, actions: 2, topics: 1},
		},
		{
			name: "scenario_4_agent_and_human",
			notes: []Note{
				humanNote("alice", "u_alice", []string{"PM"}, "인프라 이슈 우선순위 먼저 정하자"),
				contextNote("[agent]", "Redis failover 원인 분석", db.SourceAgentOutput),
				humanNote("bob", "u_bob", []string{"SRE"}, "Redis timeout부터 처리"),
				humanNote("_deadwhale", "u_deadwhale", []string{"BE"}, "배포 전 connection pool을 낮추자"),
			},
			content: &llm.SummarizedContent{
				AgentResponses: []llm.AgentResponseSummary{{Question: "Redis failover 원인", Highlights: []string{"primary 전환 중 timeout 증가"}}},
				Decisions: []llm.Decision{
					{Title: "Redis timeout 최우선 처리"},
					{Title: "배포 전 connection pool 하향"},
					{Title: "failover runbook 보강"},
				},
				Actions: []llm.SummaryAction{{What: "runbook 보강", Origin: "bob", OriginRoles: []string{"SRE"}, TargetRoles: []string{"SRE"}}},
			},
			markdown: strings.Join([]string{
				"## 🤖 AI 에이전트",
				"- Redis failover 원인: primary 전환 중 timeout 증가",
				"",
				"## 🗣️ 사람 결정사항",
				"- alice: 인프라 우선순위 결정",
				"- bob: Redis timeout 최우선 처리",
				"- _deadwhale: connection pool 하향",
				"",
			}, "\n"),
			wantSpeakers:     []string{"_deadwhale", "alice", "bob"},
			wantHumanNotes:   3,
			wantContextNotes: 1,
			wantSections:     []string{"## 🤖 AI 에이전트", "Redis failover", "## 🗣️ 사람 결정사항", "alice", "bob", "_deadwhale"},
			wantOrder:        []string{"## 🤖 AI 에이전트", "## 🗣️ 사람 결정사항"},
			wantCounts:       summarizedCounts{decisions: 3, actions: 1, agent: 1},
		},
		{
			name: "scenario_5_external_human_release",
			notes: []Note{
				humanNote("alice", "u_alice", []string{"PM"}, "vendor latency 보고서를 보고 배포 판단하자"),
				contextNote("vendor", "p95 latency 420ms 보고서", db.SourceExternalPaste),
				humanNote("bob", "u_bob", []string{"QA"}, "latency 기준은 p95 450ms 미만으로 결정"),
				humanNote("alice", "u_alice", []string{"PM"}, "product v1.1.6 릴리즈는 진행"),
				contextNote("[release]", "product 1.1.6 릴리즈 PR 생성", db.SourceReleaseResult),
			},
			content: &llm.SummarizedContent{
				ExternalRefs:   []llm.ExternalRefSummary{{Title: "vendor latency 보고서", Highlights: []string{"p95 latency 420ms"}}},
				ReleaseResults: []llm.ReleaseResultSummary{{Module: "product", PrevVersion: "1.1.5", NewVersion: "1.1.6", BumpType: "patch", PRNumber: 43, Highlights: []string{"latency guard"}}},
				Decisions: []llm.Decision{
					{Title: "latency 기준 p95 450ms 미만"},
					{Title: "product v1.1.6 릴리즈 진행"},
				},
				Actions: []llm.SummaryAction{{What: "릴리즈 후 latency 확인", Origin: "bob", OriginRoles: []string{"QA"}, TargetRoles: []string{"QA"}}},
			},
			markdown: strings.Join([]string{
				"## 외부 자료",
				"- vendor latency 보고서: p95 latency 420ms",
				"",
				"## 🚀 릴리즈",
				"- product v1.1.6",
				"",
				"## 🗣️ 사람 결정사항",
				"- bob: latency 기준 p95 450ms 미만",
				"- alice: product v1.1.6 릴리즈 진행",
				"",
			}, "\n"),
			wantSpeakers:     []string{"alice", "bob"},
			wantHumanNotes:   3,
			wantContextNotes: 2,
			wantSections:     []string{"## 외부 자료", "vendor latency", "## 🚀 릴리즈", "product v1.1.6", "## 🗣️ 사람 결정사항"},
			wantOrder:        []string{"## 외부 자료", "## 🚀 릴리즈", "## 🗣️ 사람 결정사항"},
			wantCounts:       summarizedCounts{decisions: 2, actions: 1, release: 1, external: 1},
		},
	}
}

func newFinalizeScenarioSession(name string) *Session {
	return &Session{
		Mode:          ModeMeeting,
		State:         StateMeeting,
		ThreadID:      "thread-" + name,
		UserID:        "owner-" + name,
		RolesSnapshot: map[string][]string{},
	}
}

func humanNote(author, authorID string, roles []string, content string) Note {
	return Note{
		Author:      author,
		AuthorID:    authorID,
		AuthorRoles: roles,
		Content:     content,
		Source:      db.SourceHuman,
	}
}

func contextNote(author, content string, source db.NoteSource) Note {
	return Note{
		Author:  author,
		Content: content,
		Source:  source,
	}
}

func assertCallCount(t *testing.T, got map[string]int, key string, want int) {
	t.Helper()
	if got[key] != want {
		t.Fatalf("%s calls = %d, want %d", key, got[key], want)
	}
}

func assertSummarizedCounts(t *testing.T, c *llm.SummarizedContent, want summarizedCounts) {
	t.Helper()
	if len(c.Decisions) != want.decisions ||
		len(c.Actions) != want.actions ||
		len(c.Topics) != want.topics ||
		len(c.WeeklyReports) != want.weekly ||
		len(c.ReleaseResults) != want.release ||
		len(c.AgentResponses) != want.agent ||
		len(c.ExternalRefs) != want.external {
		t.Fatalf("counts = decisions:%d actions:%d topics:%d weekly:%d release:%d agent:%d external:%d, want %+v",
			len(c.Decisions), len(c.Actions), len(c.Topics), len(c.WeeklyReports),
			len(c.ReleaseResults), len(c.AgentResponses), len(c.ExternalRefs), want)
	}
}

func assertAttributionGuard(t *testing.T, c *llm.SummarizedContent, speakers []string) {
	t.Helper()
	allowed := make(map[string]bool, len(speakers))
	for _, s := range speakers {
		allowed[s] = true
	}
	if len(speakers) == 0 && (len(c.Decisions) != 0 || len(c.Actions) != 0) {
		t.Fatalf("speakers empty but decisions/actions present: decisions=%d actions=%d", len(c.Decisions), len(c.Actions))
	}
	for idx, action := range c.Actions {
		if !allowed[action.Origin] {
			t.Fatalf("actions[%d].Origin = %q, want one of %v", idx, action.Origin, speakers)
		}
	}
}

func assertBotSummariesHaveNoOriginAuthorField(t *testing.T) {
	t.Helper()
	for _, typ := range []reflect.Type{
		reflect.TypeOf(llm.WeeklyReportSummary{}),
		reflect.TypeOf(llm.ReleaseResultSummary{}),
		reflect.TypeOf(llm.AgentResponseSummary{}),
		reflect.TypeOf(llm.ExternalRefSummary{}),
	} {
		if _, ok := typ.FieldByName("OriginAuthor"); ok {
			t.Fatalf("%s unexpectedly has OriginAuthor field", typ.Name())
		}
	}
	c := llm.SummarizedContent{
		WeeklyReports:  []llm.WeeklyReportSummary{{Repo: "repo"}},
		ReleaseResults: []llm.ReleaseResultSummary{{Module: "product"}},
		AgentResponses: []llm.AgentResponseSummary{{Question: "q"}},
		ExternalRefs:   []llm.ExternalRefSummary{{Title: "external"}},
	}
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("marshal summarized content: %v", err)
	}
	if strings.Contains(string(raw), "origin_author") {
		t.Fatalf("bot summary JSON contains origin_author: %s", raw)
	}
}

func assertMarkdownContains(t *testing.T, markdown string, wants []string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(markdown, want) {
			t.Fatalf("markdown missing %q:\n%s", want, markdown)
		}
	}
}

func assertMarkdownOrder(t *testing.T, markdown string, wants []string) {
	t.Helper()
	prev := -1
	for _, want := range wants {
		idx := strings.Index(markdown, want)
		if idx < 0 {
			t.Fatalf("markdown missing ordered marker %q:\n%s", want, markdown)
		}
		if idx <= prev {
			t.Fatalf("markdown marker %q is out of order:\n%s", want, markdown)
		}
		prev = idx
	}
}

func assertNoFourSpaceIndentedLines(t *testing.T, markdown string) {
	t.Helper()
	for idx, line := range strings.Split(markdown, "\n") {
		if strings.HasPrefix(line, "    ") {
			t.Fatalf("line %d starts with 4 spaces: %q\n%s", idx+1, line, markdown)
		}
	}
}
