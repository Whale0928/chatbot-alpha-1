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

3. BOT/REFERENCE RESULTS — split: highlights 흡수 + 메타만 참고 자료.

   핵심 정책 변경 (사용자 피드백 반영):
     - weekly_reports[].highlights[] / release_results[].highlights[] 는 본질적으로 "이번 주에 마무리한 작업".
       이를 "이번 주에 마무리한 작업" 섹션 (또는 역할별 그룹의 동명 sub-section) 에 ROLE 매핑하여 흡수한다.
       각 흡수된 항목 끝에는 "(주간 리포트 기반)" 또는 "(릴리즈 PR #N)" 출처 표기를 sub-bullet으로 추가.
     - weekly_reports[] / release_results[] 의 메타정보 (repo, period_days, commit_count, module, version bump,
       PR url) 만 "회의에서 함께 참고한 자료" 섹션에 compact 1 line으로 노출.
     - agent_responses[] 와 external_refs[] 는 "회의에서 함께 참고한 자료" 섹션에만 (배경 자료).

   Role 매핑 — weekly_reports.repo / release_results.module 키워드로:
     - "*api-server*", "*-server*", "*backend*", "*-be*", "*server*" → BACKEND
     - "*admin*", "*frontend*", "*-fe*", "*web*", "*dashboard*", "*-ui*", "*portal*" → FRONTEND
     - "*ops*", "*infra*", "*deploy*", "*k8s*", "*ci*" → OPS
     - 매핑 불가 → 흡수하지 않고 그대로 "회의에서 함께 참고한 자료"에만 (메타 + highlights 1-2개).

   메타 line 형식 ("회의에서 함께 참고한 자료" 섹션에서):
     weekly:   - 주간 리포트 — [REPO_CODE] · 커밋 N건, 지난 N일  (highlights는 분리되어 done에 흡수됨)
     release:  - 릴리즈 — [MODULE_CODE] v{prev_version} → v{new_version} ({bump_type}) · PR #{pr_number}
     agent:    - AI 응답 — {question (60자 이내)}
                 - {highlight 1}
                 - {highlight 2}
     external: - 외부 자료 — {title}
   where [REPO_CODE] / [MODULE_CODE] mean wrap that value in single backticks for inline code.

   ATTRIBUTION: 흡수된 항목과 메타 line 모두 human attribution 없음. 출처 sub-bullet으로 "(주간 리포트 기반)" 같은 출처만 표기.
   If ALL four bot fields are empty AND no highlights to absorb, omit the "회의에서 함께 참고한 자료" section.

4. ATTRIBUTION INTEGRITY.
   - decisions and actions carry input Origin attribution. Use it verbatim. Do NOT invent new authors.
   - Bot/reference items NEVER get human attribution.

5. BULLET DEPTH — 1뎁스는 항상 유지.
   - 모든 섹션 본문은 반드시 "- " 불릿으로 시작한다. 산문 단락(<p>/줄글)으로 본문을 쓰지 않는다.
   - 한 줄짜리 내용이어도 불릿. 단일 문장 요약(예: "회의 한 줄 정리")도 1 line bullet으로.
   - 필요시 2뎁스, 3뎁스 sub-bullet OK. sub-bullet은 부모가 가지지 않은 NEW info만 추가.
   - 깊이 제한은 3. 4 이상 금지.

6. HEADER STYLE — 의미를 알 수 있는 문장형.
   - 단어 한 개 헤더 금지. "액션"처럼 의미 모호한 라벨 사용 금지.
   - 짧은 문장/구 형태로, 그 섹션이 무엇인지 한국어로 명확히 설명한다.
     예: "앞으로 진행할 작업", "이번 주에 완료한 작업", "더 확인이 필요한 부분".
   - emoji 사용 금지 (단, ##/### prefix로 시각 구분).
   - H2 (##)만 1급 헤더. H3 (###)은 토픽 제목이나 역할 그룹 sub-section에만.
   - 헤더는 다음 표의 표준 라벨을 그대로 사용 (각 포맷 섹션 참조). 임의 변형 금지.

7. OUTPUT.
   - Return JSON only: { "markdown": "..." }
   - markdown is the Discord embed.Description body (target < 4090 chars).
   - No H1 title (caller provides the meeting date header outside the embed body).
   - Use markdown ## for section headers, - for bullets, ### for nested groupings.
   - 모든 1급 섹션 본문 첫 줄은 "- "로 시작 (산문 금지).`

// FormatRenderDecisionStatus — 결정+진행 포맷.
// 사람 결정/액션 중심. 봇 결과는 참고 자료로 묶음. 빈 섹션은 제거.
const FormatRenderDecisionStatus = formatRenderCommon + `

# Format: decision_status (결정 + 진행 상태)

Use case: 스프린트 / 운영 회의 / 작업 공유.

## Section order (each section appears ONLY if its source field is non-empty)

표준 헤더는 다음 표의 라벨을 그대로 사용한다. 변형 금지.

| 표준 헤더 라벨                          | Source field(s)            | Notes |
|----------------------------------------|----------------------------|-------|
| 이번 회의에서 합의한 결정              | decisions                  | "- {title}" + 0-3개 sub-bullet으로 context (있을 때만). |
| 앞으로 진행할 작업                      | actions                    | "- {what} (담당 {origin}, 마감 {deadline})". target_roles 다르면 sub-bullet로 "({origin_roles} → {target_roles})" 부연. |
| 이번 주에 완료한 작업                  | done + weekly_reports.highlights + release_results.highlights | "- {item}" 1줄씩. weekly/release highlight 흡수 항목은 sub-bullet "- (주간 리포트 기반)" 또는 "- (릴리즈 PR #N)" 출처 표기. |
| 현재 진행 중인 작업                     | in_progress                | "- {item}" 1줄씩. |
| 곧 시작할 작업                          | planned                    | "- {item}" 1줄씩. |
| 더 확인이 필요한 부분                  | blockers + open_questions  | 합쳐 한 섹션. open_questions 끝 "확인 필요" 접미사 유지. |
| 팀에 함께 공유한 내용                  | shared                     | "- {item}" 1줄씩. |
| 회의에서 함께 참고한 자료              | weekly_reports 메타 + release_results 메타 + agent_responses + external_refs | compact (rule 3 형식). highlights는 "이번 주에 완료한 작업"으로 이동. 여기에는 메타 line만. |
| 관련 키워드                             | tags                       | "- #tag1 #tag2 #tag3" 한 줄 불릿. |

빈 필드 → 섹션 헤더와 내용 모두 출력 X. 절대 "(없음)" 표시 금지.

본문 첫 줄 강제: 위 모든 섹션의 첫 줄은 반드시 "- "로 시작한다.`

// FormatRenderDiscussion — 논의 포맷.
// 토픽별 논의 흐름 중심. 봇 결과는 참고 자료.
const FormatRenderDiscussion = formatRenderCommon + `

# Format: discussion (논의 흐름 중심)

Use case: 1on1 / 회고 / 브레인스토밍.

## Section order

표준 헤더 라벨 그대로 사용:

| 표준 헤더 라벨                      | Source field(s)            | Notes |
|-------------------------------------|----------------------------|-------|
| 이번 회의에서 다룬 주제             | topics                     | 각 topic: "### {title}" 다음, flow는 줄글이 아니라 "- {flow 1줄}" 1뎁스 불릿 2-5개. insights는 sub-bullet "  - 인사이트: ...". |
| 이번 회의에서 합의한 결정           | decisions                  | decision_status 포맷과 동일 형식. |
| 더 확인이 필요한 부분               | open_questions             | "- {질문} — 확인 필요" 1줄씩. |
| 회의에서 함께 참고한 자료           | weekly_reports + release_results + agent_responses + external_refs | compact (rule 3 형식). |
| 관련 키워드                          | tags                       | "- #tag1 #tag2" 한 줄 불릿. |

빈 필드 → 섹션 자체 제거. done/in_progress/planned/blockers/shared/actions 필드는 discussion 포맷에서 노출 X (decision_status에 양보).

토픽이 0개여도 (없음) 표시 금지 — 그냥 섹션 제거.

본문 첫 줄 강제: 모든 1급 섹션 본문 첫 줄은 "- "로 시작.`

// FormatRenderRoleBased — 역할별 포맷.
// 역할 그룹 × (이번 주에 마무리한 작업 / 차주 진행할 작업 / 참고 자료). 봇 결과도 role로 분류.
const FormatRenderRoleBased = formatRenderCommon + `

# Format: role_based (역할별)

Use case: 역할 분담 / 스탠드업 / 부서별 작업 공유.

## Structure

각 role 그룹마다 ### 헤더 + 시점 기반 sub-section.
정책: "내 작업 / 받은 요청" 구분은 사용 X. 누가 시켰든 그 role이 실제로 작업하는 것을 모두 묶음.

Role 그룹 식별 — 모든 actions[]/done[]/planned[]/in_progress[]/weekly_reports[]/release_results[] 가 어느 role에 속하는지 추론하여 그 role 그룹에 분류.

## Role 추론 — 매우 중요

각 항목의 분류 기준 (우선순위 순):

A) actions: target_roles[]가 명시되어 있으면 그 role 사용.
   비어 있으면 origin_roles[] 사용. 둘 다 비면 텍스트 키워드 추론 (D).

B) done / in_progress / planned: 항목 텍스트 키워드 + 발화한 화자(origin)의 SpeakerRoles로 추론.
   화자 origin이 명시되지 않은 경우는 D 단계의 텍스트 키워드만으로.

C) weekly_reports / release_results / agent_responses: repo / module / question 키워드로 role 추론.

   repo / module 키워드 → role 매핑 힌트 (user-specific. 정확한 라벨은 회의 SpeakerRoles 기준으로 정렬):
   - "*api-server*", "*-server*", "*backend*", "*-be*", "*server*" 포함 → BACKEND
   - "*admin*", "*frontend*", "*-fe*", "*web*", "*dashboard*", "*-ui*", "*portal*" 포함 → FRONTEND
   - "*design*", "*figma*" → DESIGN
   - "*ops*", "*infra*", "*deploy*", "*k8s*" → OPS
   - 매핑 불가 → 공통 (마지막 공통 섹션 fallback)

   주의: 한 repo의 weekly 안에 다른 role 작업(예: 백엔드 repo의 어드민 API)이 섞여 있어도 그것을 분할하지 말고
   repo 전체를 repo 키워드 기준 role에 귀속. 사용자가 회의 흐름상 인지하는 그대로.

D) 텍스트 키워드 추론 (마지막 수단):
   "FE/프론트/홈화면/UI/지역 어드민/배포(FE)" → FRONTEND
   "BE/백엔드/API/spec/엔드포인트/DB/마이그레이션" → BACKEND
   "기획/정책/이슈 정리/공고/일정" → PM
   해당 없음 → 공통 섹션 fallback (아래 D-fallback 참조)

대표 라벨 우선순위: BACKEND / FRONTEND / PM / DESIGN / QA / OPS / 그 외 SpeakerRoles 라벨 그대로.
한 사람이 여러 role이면 모든 해당 그룹에 액션 노출 (중복 OK).

## Section order

각 role 그룹 — 분류된 항목이 1건 이상 있는 그룹만 노출 (빈 그룹은 헤더도 출력 X):

  ### {ROLE_LABEL}
  - members: {origin1}, {origin2}   ← origin_roles에 이 role 포함된 사람들. 1뎁스 불릿으로 시작.

  **이번 주에 마무리한 작업**
  분류 대상:
    1) 이 role에 분류된 done[] 항목
    2) 이 role에 매핑된 weekly_reports[].highlights[] (각 항목 끝 sub-bullet "- (주간 리포트 기반 · [REPO_CODE])")
    3) 이 role에 매핑된 release_results[].highlights[] (각 항목 끝 sub-bullet "- (릴리즈 PR #N · [MODULE_CODE])")
  - {item}
    - (sub-bullet으로 출처 / 부연 정보 가능)

  **차주 진행할 작업**  (이 role에 분류된 actions[] + planned[] + in_progress[])
  - {what} (마감 {deadline})
    - (cross-role인 경우 sub-bullet: "요청: {origin} ({origin_roles})")
    - (현재 진행 중이면 sub-bullet: "현재 진행 중 — 진척 X%")

  **참고 자료**  (이 role에 분류된 weekly_reports / release_results / agent_responses의 메타만)
  - 주간 리포트 — [REPO_CODE] · 커밋 N건, 지난 N일   ← highlights는 위 "이번 주에 마무리한 작업"으로 이동했으므로 메타만 1줄
  - 릴리즈 — [MODULE_CODE] v{prev} → v{new} ({bump}) · PR #{n}
  - AI 응답 — {question (60자)}
    - {highlight 1}
  (위 [REPO_CODE] / [MODULE_CODE]는 단일 backtick으로 감싼 inline code 표기)
  메타가 없는 (agent / external만 있는) 경우에도 위 형식.

  (각 sub-section은 비면 라벨째 제거. 셋 다 비면 role 그룹 자체 제거.)

D-fallback: 어느 role에도 분류되지 않은 항목은 마지막 공통 섹션에 들어감.

그 후 모든 role 공통 섹션 (있는 것만, 표준 헤더 라벨 그대로):

| 표준 헤더 라벨                      | Source                                          |
|-------------------------------------|-------------------------------------------------|
| 팀에 함께 공유한 내용               | shared                                          |
| 모두에게 적용되는 공통 결정         | decisions                                       |
| 더 확인이 필요한 부분               | open_questions + blockers                       |
| 회의에서 함께 참고한 자료           | 어느 role에도 분류 안 된 weekly/release/agent + external_refs |
| 관련 키워드                          | tags                                            |

CRITICAL CHANGES from old policy:
  - 봇 결과를 무조건 마지막 "참고 자료"로 모았던 이전 정책은 폐기.
  - 백엔드 weekly는 BACKEND 그룹의 "참고 자료" sub-section에 들어간다.
  - 어드민 repo weekly는 FRONTEND 그룹의 "참고 자료" sub-section에 들어간다 (admin = FE).
  - 봇 결과 항목 자체에 attribution(담당자)은 부여하지 않음 — 어느 role의 작업이 아니라 그 role의 배경 자료일 뿐.

빈 그룹 / 빈 sub-section / 빈 공통 섹션은 헤더째 제거. "(없음)" 절대 금지.

본문 첫 줄 강제: 모든 1급 섹션 본문 및 role 그룹 헤더 직후 첫 줄은 "- "로 시작.`

// FormatRenderFreeform — 자율 포맷.
// LLM이 회의 성격 보고 자유롭게 정리.
const FormatRenderFreeform = formatRenderCommon + `

# Format: freeform (자율 정리)

Use case: 비정형 / 단발성 / 회의 성격이 명확하지 않을 때.

## Structure

LLM이 회의 흐름을 보고 가장 자연스러운 섹션 구조를 선택. 정해진 순서 X.

표준 헤더 라벨 (필요한 것만 골라 사용 — 단어 한 개 헤더 금지, 임의 변형 금지):
- ## 회의 한 줄 정리           ← 전체 요약 1줄
- ## 회의 핵심 결정            ← decisions
- ## 후속으로 진행할 작업     ← actions
- ## 이번 회의에서 다룬 주제  ← topics
- ## 더 확인이 필요한 부분    ← open_questions + blockers
- ## 회의에서 함께 참고한 자료 ← weekly_reports + release_results + agent_responses + external_refs
- ## 관련 키워드               ← tags

## Hard rules

1. SummarizedContent의 모든 사실은 어떤 섹션에든 1번 등장 (누락 금지). 단 빈 필드는 무시.
2. 같은 사실 다른 섹션 중복 노출 금지 (각 사실 1군데만).
3. 봇 결과는 "회의에서 함께 참고한 자료" 1 섹션으로 통합. role별 그룹화 / 1급 멤버 분리 금지.
4. 빈 필드 → 관련 섹션 자체 제거. "(없음)" 금지.
5. 모든 필드가 비면 단일 문장 "정리할 내용이 없습니다."로 출력.
6. 모든 섹션 본문 첫 줄은 "- "로 시작 (산문 단락 금지). "회의 한 줄 정리"도 1뎁스 불릿 1줄.

회의 성격 판단 힌트:
- decisions 0 + topics 0 + actions 0 + 봇 결과만 있음 → "## 회의에서 함께 참고한 자료"만 노출
- topics 1+ + decisions 0 → 논의 중심 → ## 회의 한 줄 정리 + ## 이번 회의에서 다룬 주제 + (참고 자료)
- actions 다수 → 작업 분담 → ## 회의 핵심 결정 + ## 후속으로 진행할 작업 + (참고 자료)`
