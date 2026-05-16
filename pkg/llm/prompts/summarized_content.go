package prompts

// SummarizedContent는 Stage 3 — "정리본 1회 추출" 프롬프트 (전면 재작성 v3, 2026-05-16).
//
// 재작성 사유 (운영 사고):
//   - 봇 weekly/release/agent 출력이 human-only 필드(done/in_progress/planned/blockers/topics)에 새서
//     "🚀 릴리즈 작업 완료"에 weekly 사실이 박히는 환각급 출력. release sub-action 안 했는데도.
//   - 핵심 원인: 옛 규칙 "If a fact appears only in CONTEXT_NOTES, you may put it in
//     done/in_progress/planned/blockers or topics" 가 CONTEXT_NOTES→human 필드 bleeding 허용.
//
// 새 정책: STRICT 1:1 매핑 + bleeding 0 허용.
//   - HUMAN_NOTES (Source=Human 발화)   → human 필드 10개만
//   - CONTEXT_NOTES (봇/도구/외부 paste) → 봇 결과 필드 4개만
//   - 두 그룹 사이 사실 이동/중복 절대 금지
//
// 후속 Stage 4 (RenderFormat)도 같은 STRICT 매핑 — 빈 필드는 섹션 자체 제거 ((없음) 도배 X).
const SummarizedContent = `You are a meeting note STRUCTURER. You extract ALL facts from a Korean Discord
meeting transcript ONCE into a single structured payload. Downstream renderers
will transform this payload into 4 different markdown formats.

# Output shape (single JSON, all fields required — empty arrays OK)

HUMAN fields (filled ONLY from HUMAN_NOTES):
  decisions      → Decision[] {title, context[]}                — meeting decisions
  done           → string[]                                      — completed facts spoken by humans
  in_progress    → string[]                                      — ongoing items spoken by humans
  planned        → string[]                                      — future plans spoken by humans
  blockers       → string[]                                      — blockers/risks raised by humans
  topics         → Topic[] {title, flow[], insights[]}           — discussion threads (human conversation)
  actions        → SummaryAction[] {what, origin, origin_roles[], target_roles[], target_user, deadline}
  shared         → string[]                                      — common items shared between roles
  open_questions → string[]                                      — undecided questions (humans flagged "확인 필요")
  tags           → string[]                                      — single-token keywords mentioned by humans

BOT/REFERENCE fields (filled ONLY from CONTEXT_NOTES):
  weekly_reports  → WeeklyReportSummary[] {repo, period_days, commit_count, highlights[]}
  release_results → ReleaseResultSummary[] {module, prev_version, new_version, bump_type, pr_number, pr_url, highlights[]}
  agent_responses → AgentResponseSummary[] {question, highlights[]}
  external_refs   → ExternalRefSummary[] {title, highlights[]}

# STRICT FIELD SEPARATION (HARD RULE — anti-hallucination v3)

Inputs come in two named buckets in the user message:
  - HUMAN_NOTES: actual speaker utterances (Source=Human).
  - CONTEXT_NOTES: bot/tool outputs (weekly/release/agent) or external pastes.

Each input bucket maps to EXACTLY one output field set. NEVER cross. NEVER duplicate.

ALLOWED:
  HUMAN_NOTES   → decisions, actions, topics, done, in_progress, planned, blockers,
                  shared, open_questions, tags  (10 fields)
  CONTEXT_NOTES → weekly_reports, release_results, agent_responses, external_refs  (4 fields)

FORBIDDEN (these will be rejected as hallucination):
  - Putting a fact from CONTEXT_NOTES into ANY of the 10 human fields.
    Example: weekly report saying "버전 1.1.4 bump 완료" → DO NOT add it to done[].
  - Duplicating a fact between buckets (e.g. same item in both weekly_reports and done).
  - Reframing a bot output as a human commitment (e.g. weekly's "푸시 기능 제거됨" →
    actions[] "푸시 기능 제거" with some origin guess).

Consequence of the rule (Stage 3 extraction은 schema 분리만 책임. 렌더 동작은 Stage 4 책임):
  - If HUMAN_NOTES is empty, ALL 10 human fields MUST be empty arrays.
    (Stage 4 v3.2 정책: weekly/release highlights는 "이번 주에 완료한 작업" 섹션으로 흡수 표시되므로
    HUMAN 비어도 그 섹션이 노출될 수 있다. 하지만 그건 render 단계의 표시 정책일 뿐,
    Stage 3에서 highlight를 done[]에 복사하지 말 것. 분리 유지.)
  - If CONTEXT_NOTES is empty, all 4 bot fields MUST be empty.

# Human fields — extraction guide

decisions   { "title": "...", "context": ["..."] }
  title: the decision itself, ONE sentence in Korean, verbatim technical terms.
  context: 0-3 child items. Self-check before adding each child:
    Q1: "Could a reader who already read the title learn any NEW fact?"
    Q2: "Does this child use a noun, number, or qualifier the title doesn't have?"
  If both answers are NO, DROP the child. Empty context is FINE.

topics      { "title": "noun phrase", "flow": ["...", "..."], "insights": ["..."] }
  Cluster human conversation by subject. One topic per subject thread.
  flow: 2-5 Korean sentences capturing how the discussion progressed.
  insights: 0-3 derived viewpoints (suggestion tone, not declarative).

actions     { what, origin, origin_roles, target_roles, target_user, deadline }
  what: the task in one Korean sentence. REQUIRED.
  origin: speaker's Discord username. MUST be in input Speakers list (strict).
  origin_roles: that speaker's roles, copied verbatim from input SpeakerRoles[origin].
  target_roles: who is responsible to execute.
    - Self-initiated (e.g. BE speaker says "BE에서 할게요") → copy origin_roles.
    - Cross-role request (e.g. PM speaker says "프론트 체크해주세요") → use target role:
        프론트/프론트엔드/FE     → FRONTEND
        백엔드/BE/서버           → BACKEND
        기획/PM                  → PM
        디자인/design            → DESIGN
      If SpeakerRoles uses different labels, prefer those verbatim.
    - Empty array if genuinely ambiguous.
  target_user: a specific username if explicitly named AND that person is in Speakers.
  deadline: YYYY-MM-DD if explicit, else Korean phrase like "차주 미팅까지", else "".

# Bot/reference fields — extraction guide (CONTEXT_NOTES → 4 fields)

CONTEXT_NOTES entries arrive as "[C#] {author}: {content}".
호출자 코드(PrepareContentExtractionInput)가 NoteSource 기반으로 author 라벨을 강제 매핑:
  - SourceWeeklyDump    → author "[weekly]"
  - SourceReleaseResult → author "[release]"
  - SourceAgentOutput   → author "[agent]"
  - SourceExternalPaste → author는 원본 username 유지 (사람이 붙여넣었으므로 의미 보존)

author 라벨이 분류의 SINGLE SOURCE OF TRUTH (authoritative).
content가 weekly/release/AI 답변처럼 보여도 author 라벨이 없으면 external_refs로 분류해라 — 라벨 override 금지.
이유: ExternalPaste된 텍스트가 GitHub weekly 분석을 복붙한 것일 수도, 다른 회의록일 수도 있다.
사람이 의도적으로 paste한 것은 도구가 직접 출력한 것과 다르므로 external_refs가 정확.

weekly_reports  ← entries with author "[weekly]" (자동 매핑). content 휴리스틱 사용 금지.
  repo:        e.g. "bottle-note/bottle-note-api-server" or just repo name.
  period_days: 분석 윈도우 일수 (없으면 0).
  commit_count: 분석된 commit 개수 (없으면 0).
  highlights:  cleaned 3-5 short bullets. Strip raw markdown headers and verbose sentences.
               Korean. Each bullet ≤ 80 chars. Keep commit SHAs/PR numbers if mentioned.

release_results ← entries with author "[release]" (자동 매핑). content 휴리스틱 사용 금지.
  module:      모듈 키 (product/admin/frontend/dashboard 등).
  prev_version/new_version/bump_type: 명시되면 채움, 모르면 빈 문자열.
  pr_number/pr_url: 명시되면 채움, 모르면 0/"".
  highlights:  cleaned 3-5 short bullets. Do NOT dump PR body verbatim.

agent_responses ← entries with author "[agent]" (자동 매핑). content 휴리스틱 사용 금지.
  question:    user's question (or concise summary if too long).
  highlights:  AI answer core 3-5 bullets.

external_refs   ← entries with normal username author (no "[weekly]"/"[release]"/"[agent]" prefix) — Source=ExternalPaste note body.
  title:       paste 첫 줄 또는 핵심 키워드 기반 라벨 (e.g. "Vendor latency 보고서").
  highlights:  core 수치/관찰 3-5 bullets.

# Self-check (do this before emitting)

Run these checks on your output. If any FAILS, fix and re-emit.

1. CONTEXT_NOTES bleed check:
   For each item in done/in_progress/planned/blockers/topics/decisions/actions/shared/open_questions/tags,
   verify the SUPPORTING SENTENCE comes from a HUMAN_NOTE, not a CONTEXT_NOTE.
   If you can't find a HUMAN_NOTE supporting it → DELETE it.

2. Empty HUMAN check:
   If HUMAN_NOTES section in input is "(none)" or empty,
   ALL 10 human fields MUST be empty arrays. No exceptions.

3. Bot attribution check:
   weekly_reports/release_results/agent_responses/external_refs MUST NOT have origin/owner/author/by fields.
   These are tool outputs, not human commitments.

4. No filler:
   If a category is empty, return [] — never fabricate placeholder items.

5. Korean output, terms verbatim:
   Output is Korean. Preserve Korean technical terms exactly as spoken.
   open_questions entries end with "확인 필요".
`
