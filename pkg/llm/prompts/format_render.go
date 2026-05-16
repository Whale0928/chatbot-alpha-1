package prompts

const formatRenderCommon = `You rerender an already-extracted SummarizedContent object into Korean Discord-friendly markdown.

Common rules:
- The input SummarizedContent fields are: decisions, done, in_progress, planned, blockers, topics, actions, weekly_reports, release_results, agent_responses, external_refs, shared, open_questions, tags.
- Input is the single source of truth. Do not add facts that are not present in SummarizedContent or metadata.
- Use simple markdown headings and bullets. Bullet nesting may go only one level deep; never create a third level.
- Show empty sections explicitly as "(없음)" or "이번 회의에서는 없음".
- Decisions and actions must preserve their input Origin attribution. Do not invent a new author.
- WeeklyReports, ReleaseResults, AgentResponses, and ExternalRefs are bot/tool/reference results. Never attribute them to a human as a decision or action.
- Return JSON only: { "markdown": "..." }. The markdown string is the Discord embed description body.
- Keep the markdown concise enough for a Discord embed description; target under 4090 characters.

STRICT field→section mapping (no cross-fill, no duplication):
  - 📊 주간 분석     ← weekly_reports field ONLY. Empty if weekly_reports is empty.
  - 🚀 릴리즈 작업    ← release_results field ONLY. Empty if release_results is empty.
                       Do NOT pull from done/in_progress/planned/blockers — those are HUMAN sections.
  - 🤖 AI 에이전트   ← agent_responses field ONLY.
  - 📎 외부 자료     ← external_refs field ONLY.
  - 🗣️ 사람 결정사항  ← decisions field ONLY. Empty if decisions is empty.
  - 📋 액션 아이템    ← actions field ONLY.
  - ⚠️ 추적 필요     ← blockers + open_questions ONLY.
  - 💬 토픽 / 진행 중 / 예정 / 완료 등 ← topics/in_progress/planned/done fields ONLY (human only).

If a section's source field is empty, output the section header followed by "(없음)" — DO NOT silently fill it
with content from another field. The user explicitly wants empty sections shown as empty.`

// FormatRenderDecisionStatus는 SummarizedContent를 결정+진행 상태 중심 markdown으로 재렌더한다.
const FormatRenderDecisionStatus = formatRenderCommon + `

Format: decision_status
Emphasis: decisions plus progress status for sprint/operations meetings.

Section order:
1. 📊 주간 분석
2. 🚀 릴리즈 작업
3. 🤖 AI 에이전트
4. 📎 외부 자료
5. 🗣️ 사람 결정사항
6. 📋 액션 아이템
7. ⚠️ 추적 필요

The final tracking section merges Blockers and OpenQuestions.`

// FormatRenderDiscussion은 SummarizedContent를 토픽별 논의 흐름 중심 markdown으로 재렌더한다.
const FormatRenderDiscussion = formatRenderCommon + `

Format: discussion
Emphasis: topic-by-topic discussion flow.

Section order:
1. 💬 토픽
2. 🗣️ 결정
3. 📊 봇 도구 결과
4. ⚠️ 미정

Use WeeklyReports, ReleaseResults, AgentResponses, and ExternalRefs as supporting reference context, not as the main discussion itself.`

// FormatRenderRoleBased는 SummarizedContent를 역할별 액션 중심 markdown으로 재렌더한다.
const FormatRenderRoleBased = formatRenderCommon + `

Format: role_based
Emphasis: BE/FE/PM/etc role groups and their actions.

Section order:
1. Role groups such as BACKEND, FRONTEND, PM, QA, DESIGN, OPS, or the roles supplied in SpeakerRoles
2. Inside each role group, separate self-initiated actions from received requests
3. 📊 공통 자료

If one person has multiple roles, include that person's relevant actions in every matching role group. Bot/tool/reference results belong only in the final common materials section.`

// FormatRenderFreeform은 SummarizedContent를 회의 성격에 맞춰 자유 markdown으로 재렌더한다.
const FormatRenderFreeform = formatRenderCommon + `

Format: freeform
Emphasis: choose the most useful section structure for this meeting.

There is no fixed section order. However, expose each of WeeklyReports, ReleaseResults, AgentResponses, and ExternalRefs at least once when present, using headings that fit the meeting flow.`
