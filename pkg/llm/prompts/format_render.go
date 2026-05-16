package prompts

// Stage 4 — SummarizedContent를 4 포맷별 markdown으로 재렌더하는 LLM 프롬프트 (v3.1, 2026-05-16).
//
// 재작성 사유:
//   - v2 (PR #10): "Show empty sections as (없음)" 명시 → 빈 섹션 (없음) 도배 UX
//   - v2: 봇 결과를 1급 4 섹션으로 노출 → docs/index.html SCENE 09의 "참고 자료" compact 패턴과 거리
//   - 결과: 사람 발화 0건 회의가 "🚀 릴리즈 작업 완료"로 채워지는 환각급 출력
//
// v3 (현재) 정책:
//   - 빈 필드 → 섹션 자체 제거 (헤더도 X). (없음) 표기 금지.
//   - 봇 결과 → "참고 자료" 1 섹션에 compact 1-2 line 통합 (별도 1급 섹션 X).
//   - 인간 결정/액션이 정리본의 본체. 봇 결과는 부수 자료.
//   - 헤더는 자연스러운 한국어로 ("사람 결정사항" → "결정사항"). emoji 최소화.
//   - 4 포맷 모두 동일 원칙. 포맷별로 강조점만 다름.

const formatRenderCommon = `You rerender an already-extracted SummarizedContent JSON into Korean Discord-friendly markdown.

# Hard rules (apply to ALL formats)

1. INPUT IS THE SINGLE SOURCE OF TRUTH.
   - Use only facts present in SummarizedContent. No added context, no fabricated detail.
   - Preserve Korean technical terms verbatim.

2. OMIT EMPTY SECTIONS ENTIRELY — do NOT show "(없음)" or "이번 회의에서는 없음".
   - If a field (decisions/actions/done/topics/...) is empty array, that section's HEADER ALSO disappears.
   - Reasoning: empty section spam buries the actual content. Show only what exists.
   - Exception: only the OVERALL output may be empty. If everything is empty, output a single line
     "정리할 내용이 없습니다." and stop.

3. BOT/REFERENCE RESULTS GO TO ONE COMPACT "참고 자료" SECTION (not separate emoji-headed sections).
   - weekly_reports/release_results/agent_responses/external_refs are rendered as COMPACT bullets,
     one bullet per item. Wrap repo / module / PR refs in single backticks for code styling.
     Format examples (single-line root bullet, optional 1-2 sub-bullets for highlights):
       weekly:   - 주간 리포트 — [REPO_CODE] · 커밋 N건, 지난 N일
                   - {highlight 1}
                   - {highlight 2}
       release:  - 릴리즈 — [MODULE_CODE] v{prev_version} → v{new_version} ({bump_type}) · PR #{pr_number}
       agent:    - AI 응답 — {question (60자 이내)}
                   - {highlight 1}
       external: - 외부 자료 — {title}
     where [REPO_CODE] / [MODULE_CODE] mean wrap that value in single backticks for inline code.
   - These items DO NOT carry decision/action attribution. They are background reference, not commitments.
   - If ALL four bot fields are empty, omit the "참고 자료" section entirely.

4. ATTRIBUTION INTEGRITY.
   - decisions and actions carry input Origin attribution. Use it verbatim. Do NOT invent new authors.
   - Bot/reference items NEVER get human attribution.

5. BULLET DEPTH.
   - At most 2 levels (root bullet + 1 sub-bullet). Never 3+ levels.
   - Sub-bullets only when they add NEW info the parent doesn't have.

6. HEADER STYLE.
   - 자연스러운 한국어. "사람 결정사항"/"사람 발화" 같은 어색한 prefix 금지 — 그냥 "결정사항".
   - emoji는 최소화. 시각 구분에 꼭 필요한 곳만. 권장: 거의 안 씀.
   - H2 (##)만 사용. H3 (###)은 토픽 제목이나 역할 그룹 내부 sub-section에만.

7. OUTPUT.
   - Return JSON only: { "markdown": "..." }
   - markdown is the Discord embed.Description body (target < 4090 chars).
   - No H1 title (caller provides the meeting date header outside the embed body).
   - Use markdown ## for section headers, - for bullets, ### for nested groupings.`

// FormatRenderDecisionStatus — 결정+진행 포맷.
// 사람 결정/액션 중심. 봇 결과는 참고 자료로 묶음. 빈 섹션은 제거.
const FormatRenderDecisionStatus = formatRenderCommon + `

# Format: decision_status (결정 + 진행 상태)

Use case: 스프린트 / 운영 회의 / 작업 공유.

## Section order (each section appears ONLY if its source field is non-empty)

| Section header   | Source field(s)            | Notes |
|------------------|----------------------------|-------|
| 결정사항         | decisions                  | title + 0-3 context bullets (input 그대로). |
| 액션             | actions                    | Each: "- {what} (담당 {origin}, 마감 {deadline})". target_roles 다르면 "({origin_roles} → {target_roles})" 표기. |
| 완료             | done                       | bullet list |
| 진행 중          | in_progress                | bullet list |
| 예정             | planned                    | bullet list |
| 이슈 / 확인 필요 | blockers + open_questions  | 두 필드 합쳐 한 섹션. open_questions는 "확인 필요" 접미사 그대로. |
| 공유 사항        | shared                     | bullet list |
| 참고 자료        | weekly_reports + release_results + agent_responses + external_refs | compact (rule 3 형식) |
| 태그             | tags                       | 한 줄, "#tag1 #tag2" 형식. |

빈 필드 → 섹션 헤더와 내용 모두 출력 X. 절대 "(없음)" 표시 금지.`

// FormatRenderDiscussion — 논의 포맷.
// 토픽별 논의 흐름 중심. 봇 결과는 참고 자료.
const FormatRenderDiscussion = formatRenderCommon + `

# Format: discussion (논의 흐름 중심)

Use case: 1on1 / 회고 / 브레인스토밍.

## Section order

| Section header   | Source field(s)            | Notes |
|------------------|----------------------------|-------|
| 토픽             | topics                     | 각 topic: ### {title} → flow 줄글 → insights는 "> 인사이트:" prefix sub-bullets. |
| 결정사항         | decisions                  | decision_status 포맷과 동일. |
| 확인 필요         | open_questions             | bullet list. |
| 참고 자료        | weekly_reports + release_results + agent_responses + external_refs | compact (rule 3 형식). 논의의 배경 자료. |
| 태그             | tags                       | 한 줄. |

빈 필드 → 섹션 자체 제거. done/in_progress/planned/blockers/shared/actions 필드는 discussion 포맷에서 노출 X (decision_status에 양보).

토픽이 0개여도 (없음) 표시 금지 — 그냥 섹션 제거.`

// FormatRenderRoleBased — 역할별 포맷.
// 역할 그룹 × (자기 발의 / 받은 요청). 봇 결과는 마지막 참고 자료 1 섹션.
const FormatRenderRoleBased = formatRenderCommon + `

# Format: role_based (역할별)

Use case: 역할 분담 / 스탠드업 / 부서별 작업 공유.

## Structure

각 role 그룹마다 ### 헤더 + 자기 발의 액션 / 받은 요청 sub-section.

Role 그룹 식별 — 모든 actions[].origin_roles 와 target_roles 의 union에서 unique role 추출.
대표 라벨 우선순위:
  BACKEND / FRONTEND / PM / DESIGN / QA / OPS / 그 외 SpeakerRoles 라벨 그대로.

한 사람이 여러 role이면 모든 해당 그룹에 액션 노출 (중복 OK).

## Section order

각 role 그룹 — 액션이 1건 이상 있는 그룹만 노출 (빈 그룹은 헤더도 출력 X):

  ### {ROLE_LABEL}
  members: {origin1}, {origin2}  ← origin_roles에 이 role 포함된 사람들

  **자기 발의 액션**  (origin_roles ∩ {ROLE} != ∅ AND target_roles ∩ {ROLE} != ∅)
  - {what} [from {origin}, due {deadline}]

  **받은 요청**  (origin_roles ∩ {ROLE} == ∅ AND target_roles ∩ {ROLE} != ∅)
  - {what} [from {origin} ({origin_roles 합쳐 표기}), due {deadline}]

  (sub-section도 빈 경우 그 라벨 자체 제거. 둘 다 비면 role 그룹 자체 제거.)

그 후 모든 role 공통 섹션 (있는 것만):

| Section          | Source                          |
|------------------|---------------------------------|
| 공유 사항        | shared                          |
| 공통 결정        | decisions                       |
| 확인 필요         | open_questions + blockers       |
| 참고 자료        | weekly_reports + release_results + agent_responses + external_refs |
| 태그             | tags                            |

CRITICAL: weekly_reports/release_results 등 봇 결과는 절대 role 그룹 안에 들어가지 않는다 (그 누구의 액션도 아님).
사용자가 백엔드 weekly를 돌렸어도 weekly_reports는 "참고 자료" 섹션의 1 line일 뿐 — "BACKEND 그룹 액션"이 아님.

빈 그룹 / 빈 sub-section / 빈 공통 섹션은 헤더째 제거. "(없음)" 절대 금지.`

// FormatRenderFreeform — 자율 포맷.
// LLM이 회의 성격 보고 자유롭게 정리.
const FormatRenderFreeform = formatRenderCommon + `

# Format: freeform (자율 정리)

Use case: 비정형 / 단발성 / 회의 성격이 명확하지 않을 때.

## Structure

LLM이 회의 흐름을 보고 가장 자연스러운 섹션 구조를 선택. 정해진 순서 X.

권장 헤더 예시 (모두 출력 의무 X — 회의 내용에 따라 선택, 다른 자연스러운 표현도 OK):
- ## 한 줄 요약
- ## 핵심 결정
- ## 액션 / 후속 작업
- ## 토픽
- ## 참고 자료
- ## 확인 필요

## Hard rules

1. SummarizedContent의 모든 사실은 어떤 섹션에든 1번 등장 (누락 금지). 단 빈 필드는 무시.
2. 같은 사실 다른 섹션 중복 노출 금지 (각 사실 1군데만).
3. 봇 결과는 "참고 자료" 같은 합쳐진 1 섹션으로 통합. role별 그룹화 / 1급 멤버 분리 금지.
4. 빈 필드 → 관련 섹션 자체 제거. "(없음)" 금지.
5. 모든 필드가 비면 단일 문장 "정리할 내용이 없습니다."로 출력.

회의 성격 판단 힌트:
- decisions 0 + topics 0 + actions 0 + 봇 결과만 있음 → "## 참고 자료" 헤더로 봇 결과만 노출
- topics 1+ + decisions 0 → 논의 중심 회의 → ## 한 줄 요약 + ## 토픽 + (참고 자료)
- actions 다수 → 작업 분담 회의 → ## 핵심 결정 + ## 액션 + (참고 자료)`
