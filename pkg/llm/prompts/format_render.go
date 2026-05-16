package prompts

// Stage 4 — SummarizedContent를 4 포맷별 markdown으로 재렌더하는 LLM 프롬프트 (v3.2, 2026-05-16).
//
// v3.2 변경 (Copilot review #11 17 items 반영):
//   - 정책 모순 9건 정리 (discussion/freeform highlight 흡수처, role_based CRITICAL 자기모순,
//     토픽 첫 줄 충돌, 확인 필요 중복, external_refs 위치 등)
//   - 스키마 mismatch 5건 정리 (Decision Origin 없음 / target_user 우선 / deadline 빈문자열
//     조건부 / 메타 0값 omit / done·planned 메타 부재)
//   - 흡수 정책 단일화: decision_status/discussion/freeform은 role 매핑 없이 모든 highlight 흡수,
//     role_based만 role 매핑 후 매핑 불가 항목은 공통 섹션 fallback

const formatRenderCommon = `You rerender an already-extracted SummarizedContent JSON into Korean Discord-friendly markdown.

# Hard rules (apply to ALL formats)

1. INPUT IS THE SINGLE SOURCE OF TRUTH.
   - Use only facts present in SummarizedContent. No added context, no fabricated detail.
   - Preserve Korean technical terms verbatim.

2. OMIT EMPTY SECTIONS ENTIRELY — do NOT show "(없음)" or "이번 회의에서는 없음".
   - If a field is empty array, that section's HEADER ALSO disappears.
   - Reasoning: empty section spam buries the actual content. Show only what exists.
   - Exception: only the OVERALL output may be empty. If everything is empty, output a single line
     "정리할 내용이 없습니다." and stop.

3. BOT/REFERENCE RESULTS — split: highlights 흡수 + 메타만 참고 자료.

   핵심 정책:
     - weekly_reports[].highlights[] / release_results[].highlights[] 는 본질적으로 "이번 주에 마무리된 작업".
       이를 "이번 주에 완료한 작업" 섹션 (또는 역할별 그룹의 동명 sub-section) 에 흡수한다.
       각 흡수된 항목 끝에는 sub-bullet으로 "- (주간 리포트 기반 · [REPO_CODE])"
       또는 "- (릴리즈 PR #N · [MODULE_CODE])" 출처 표기를 추가.
       (REPO_CODE / MODULE_CODE는 단일 backtick으로 감싼 inline code 표기.)
     - weekly_reports[] / release_results[] 의 메타정보만 "회의에서 함께 참고한 자료"에 compact 1 line으로 노출.
       메타가 비어 있거나 알 수 없는 부분은 OMIT하라 (아래 메타 형식 규칙 참조).
     - agent_responses[] / external_refs[] 는 "회의에서 함께 참고한 자료" 섹션에만 (배경 자료, 흡수 X).
     - external_refs는 모든 포맷에서 role 그룹 안에는 절대 들어가지 않는다 (공통 참고 자료 전용).

   포맷별 흡수 정책 차이:
     - decision_status / discussion / freeform: role 매핑 없이 weekly/release highlights를 모두
       "이번 주에 완료한 작업"(또는 동등 이름) 섹션에 흡수.
       discussion 포맷에서도 highlight가 1건 이상 있으면 "이번 주에 완료한 작업" 섹션을 새로 추가하여 노출
       (done 본 필드가 0이어도 highlight 있으면 섹션 등장).
     - role_based: highlights를 repo/module 키워드로 role 매핑 후 그 role 그룹의
       "이번 주에 마무리한 작업" sub-section에 흡수. 매핑 불가 항목만 공통 "회의에서 함께 참고한 자료" fallback.

   role 매핑 키워드 (role_based 포맷에서만 사용):
     - "*api-server*", "*-server*", "*backend*", "*-be*", "*server*" 포함 → BACKEND
     - "*admin*", "*frontend*", "*-fe*", "*web*", "*dashboard*", "*-ui*", "*portal*" 포함 → FRONTEND
     - "*design*", "*figma*" → DESIGN
     - "*ops*", "*infra*", "*deploy*", "*k8s*", "*ci*" → OPS
     주의: 한 repo의 weekly 안에 다른 role 작업이 섞여 있어도 그것을 분할하지 말고 repo 전체를
     repo 키워드 기준 role에 귀속.

   메타 line 형식 ("회의에서 함께 참고한 자료" 섹션, 0/빈값은 OMIT):
     weekly:
       기본:   - 주간 리포트 — [REPO_CODE]
       확장:   - 주간 리포트 — [REPO_CODE] · 커밋 N건, 지난 N일
       규칙: period_days==0 이면 "지난 N일" 부분 OMIT. commit_count==0 이면 "커밋 N건" 부분 OMIT.
              둘 다 0이면 " · ..." 부분 전체 OMIT.
     release:
       기본:   - 릴리즈 — [MODULE_CODE]
       확장:   - 릴리즈 — [MODULE_CODE] v{prev_version} → v{new_version} ({bump_type}) · PR #{pr_number}
       규칙: prev_version 또는 new_version이 비면 "v.. → v.." 전체 OMIT.
              bump_type 비면 "(...)" OMIT. pr_number==0 이면 "· PR #..." OMIT.
     agent:    - AI 응답 — {question (60자 이내)}
                 - {highlight 1}
                 - {highlight 2}
     external: - 외부 자료 — {title}
                 - {highlight 1}
                 - {highlight 2}
   (external_refs도 agent와 동일: highlights가 있으면 sub-bullet으로 모두 출력. 누락 금지.
    highlights가 비면 title 1줄만.)
   [REPO_CODE] / [MODULE_CODE]는 그 값을 단일 backtick으로 감싼 inline code 표기.

   ATTRIBUTION: 흡수된 항목과 메타 line 모두 human attribution 없음. 출처 sub-bullet만.
   If ALL four bot fields are empty AND no highlights to absorb anywhere, omit the "회의에서 함께 참고한 자료" section.

4. ATTRIBUTION INTEGRITY.
   - actions만 origin/origin_roles/target_roles/target_user 메타를 가진다.
     담당자 표기 우선순위: target_user > target_roles(첫 항목) > origin.
     "{what} (담당 {chosen_assignee}, 마감 {deadline})" 의 chosen_assignee.
     deadline이 빈 문자열이면 "(담당 {chosen_assignee})" 만, ", 마감" 부분 OMIT.
     target_roles와 origin_roles가 다르면 cross-role 표시를 sub-bullet으로
     "  - 요청: {origin} ({origin_roles 합표기} → {target_roles 합표기})".
   - decisions에는 origin/author 필드 자체가 없다. attribution을 만들어내지 말 것.
     "- {title}" + 필요시 sub-bullet으로 context.
   - done / in_progress / planned / blockers / shared / open_questions / tags 는 단순 string[].
     speaker 메타 없음 → 누가 발화했는지 추론 금지.
   - Bot/reference items NEVER get human attribution.

5. BULLET DEPTH — 1뎁스는 항상 유지.
   - 모든 H2 섹션 본문은 반드시 "- " 불릿으로 시작 (산문 단락 금지).
     예외 1: H2 직후 H3 sub-header가 오는 경우 (토픽 섹션의 "### {topic title}", 역할별 섹션의 "### {ROLE}").
             H3 다음의 본문 첫 줄은 다시 "- "로 시작.
   - 한 줄짜리 내용이어도 불릿. 단일 문장 요약(예: "회의 한 줄 정리")도 1 line bullet으로.
   - 필요시 2뎁스, 3뎁스 sub-bullet OK. sub-bullet은 부모가 가지지 않은 NEW info만.
   - 최대 깊이 3 (root + 2 nested). 4뎁스는 validator가 거부 — fallback renderer로 떨어진다.
   - markdown 표준 2-space indent (1뎁스 0, 2뎁스 2, 3뎁스 4 spaces).

6. HEADER STYLE — 의미를 알 수 있는 문장형.
   - 단어 한 개 헤더 금지. "액션"처럼 의미 모호한 라벨 사용 금지.
   - 짧은 문장/구 형태로, 그 섹션이 무엇인지 한국어로 명확히 설명한다.
   - emoji 사용 금지.
   - H2 (##)만 1급 헤더. H3 (###)은 토픽 제목이나 역할 그룹 sub-section에만.
   - 헤더는 각 포맷 섹션의 표준 라벨 표 그대로 사용. 임의 변형 금지.

7. OUTPUT.
   - Return JSON only: { "markdown": "..." }
   - markdown is the Discord embed.Description body (target < 4090 chars).
   - No H1 title (caller provides the meeting date header outside the embed body).
   - Use markdown ## for section headers, - for bullets, ### for nested groupings.

8. "확인 필요" 중복 방지.
   - Stage 3가 open_questions 항목 끝을 "확인 필요"로 마무리하므로,
     Stage 4 render에서 "{질문} — 확인 필요" 형태로 또 붙이면 중복("... 확인 필요 — 확인 필요").
   - 항목이 이미 "확인 필요"로 끝나면 그대로 출력. 아니면 " — 확인 필요" 접미사 추가.`

// FormatRenderDecisionStatus — 결정+진행 포맷.
const FormatRenderDecisionStatus = formatRenderCommon + `

# Format: decision_status (결정 + 진행 상태)

Use case: 스프린트 / 운영 회의 / 작업 공유.

## Section order (each section appears ONLY if its source field is non-empty)

표준 헤더는 다음 표의 라벨을 그대로 사용한다. 변형 금지.

| 표준 헤더 라벨                          | Source field(s)                                                        | Notes |
|----------------------------------------|------------------------------------------------------------------------|-------|
| 이번 회의에서 합의한 결정              | decisions                                                              | "- {title}" + 0-3개 sub-bullet으로 context (있을 때만). origin 표기 X. |
| 앞으로 진행할 작업                      | actions                                                                | rule 4 형식 ("- {what} (담당 {assignee}[, 마감 {deadline}])"). deadline 빈 문자열이면 omit. cross-role은 sub-bullet "요청: ...". |
| 이번 주에 완료한 작업                  | done + weekly_reports[].highlights + release_results[].highlights      | "- {item}" 1줄씩. 흡수 항목 sub-bullet "- (주간 리포트 기반 · [REPO_CODE])" 또는 "- (릴리즈 PR #N · [MODULE_CODE])". |
| 현재 진행 중인 작업                     | in_progress                                                            | "- {item}" 1줄씩. |
| 곧 시작할 작업                          | planned                                                                | "- {item}" 1줄씩. |
| 더 확인이 필요한 부분                  | blockers + open_questions                                              | 합쳐 한 섹션. open_questions 중복 방지 (rule 8). |
| 팀에 함께 공유한 내용                  | shared                                                                 | "- {item}" 1줄씩. |
| 회의에서 함께 참고한 자료              | weekly_reports 메타 + release_results 메타 + agent_responses + external_refs | rule 3 형식. highlights는 위 "이번 주에 완료한 작업"으로 이동. 메타 1줄만. |
| 관련 키워드                             | tags                                                                   | "- #tag1 #tag2 #tag3" 한 줄 불릿. |

빈 필드 → 섹션 헤더와 내용 모두 출력 X. 절대 "(없음)" 표시 금지.

본문 첫 줄 강제: 모든 1급 섹션 본문 첫 줄은 "- " (rule 5).`

// FormatRenderDiscussion — 논의 포맷.
const FormatRenderDiscussion = formatRenderCommon + `

# Format: discussion (논의 흐름 중심)

Use case: 1on1 / 회고 / 브레인스토밍.

## Section order

표준 헤더 라벨 그대로 사용:

| 표준 헤더 라벨                      | Source field(s)                                                  | Notes |
|-------------------------------------|------------------------------------------------------------------|-------|
| 이번 회의에서 다룬 주제             | topics                                                           | H2 다음 바로 "### {topic title}" 가능 (rule 5 예외). 각 H3 직후 본문 첫 줄은 "- {flow 1줄}" 1뎁스 불릿 2-5개. insights는 sub-bullet "  - 인사이트: ...". |
| 이번 회의에서 합의한 결정           | decisions                                                        | decision_status 포맷과 동일 형식. origin 표기 X. |
| 이번 주에 완료한 작업               | weekly_reports[].highlights + release_results[].highlights       | discussion 포맷은 done 본 필드를 노출하지 않지만, weekly/release highlight가 1건 이상 있으면 본 섹션을 새로 추가하여 흡수 표시 (rule 3). 흡수 항목 sub-bullet으로 출처 표기. |
| 더 확인이 필요한 부분               | open_questions                                                   | "- {질문}" 1줄씩. rule 8로 중복 방지. |
| 회의에서 함께 참고한 자료           | weekly_reports 메타 + release_results 메타 + agent_responses + external_refs | rule 3 형식. highlights는 위 "이번 주에 완료한 작업"으로 이동. 메타만. |
| 관련 키워드                          | tags                                                             | "- #tag1 #tag2" 한 줄 불릿. |

빈 필드 → 섹션 자체 제거. done/in_progress/planned/blockers/shared/actions 본 필드는 discussion 포맷에서 노출 X (decision_status에 양보) — 단 weekly/release highlight 흡수만 위 표대로 예외.

토픽이 0개여도 (없음) 표시 금지 — 그냥 섹션 제거.

본문 첫 줄 강제: 모든 1급 섹션 본문 첫 줄은 "- " (rule 5 예외 토픽 섹션 제외).`

// FormatRenderRoleBased — 역할별 포맷.
const FormatRenderRoleBased = formatRenderCommon + `

# Format: role_based (역할별)

Use case: 역할 분담 / 스탠드업 / 부서별 작업 공유.

## Structure

각 role 그룹마다 "### {ROLE_LABEL}" + 시점 기반 sub-section.
"내 작업 / 받은 요청" 구분은 사용 X. 누가 시켰든 그 role이 실제로 작업하는 것을 모두 묶음.

## Role 그룹 식별 + 항목 분류

actions / done / in_progress / planned / weekly_reports / release_results 각 항목을 role 그룹에 배치.

A) actions: 담당 우선순위 (rule 4) — target_user의 SpeakerRoles > target_roles > origin_roles.
B) weekly_reports / release_results: repo / module 키워드 → role 매핑 (rule 3 키워드 표).
C) done / in_progress / planned: 본 필드는 plain string[]이고 origin 메타가 없다.
   텍스트 키워드만으로 추론:
     "FE/프론트/홈화면/UI/지역 어드민" → FRONTEND
     "BE/백엔드/API/spec/엔드포인트/DB/마이그레이션" → BACKEND
     "기획/정책/이슈 정리/공고/일정" → PM
     "ops/infra/배포 인프라/k8s" → OPS
   추론 불가 → 공통 fallback 섹션 (아래 표) 으로 이동. 임의 role 부여 금지.
D) agent_responses / external_refs: role 그룹에 배치 안 함 — 항상 공통 "회의에서 함께 참고한 자료".

대표 라벨 우선순위: BACKEND / FRONTEND / PM / DESIGN / QA / OPS / 그 외 SpeakerRoles 라벨 그대로.
한 사람이 여러 role이면 모든 해당 그룹에 actions 노출 (중복 OK).

## Section order

각 role 그룹 — 분류된 항목이 1건 이상 있는 그룹만 노출 (빈 그룹은 헤더도 출력 X):

  ### {ROLE_LABEL}
  - members: {origin1}, {origin2}   ← origin_roles에 이 role 포함된 사람들. 1뎁스 불릿.

  **이번 주에 마무리한 작업**  (분류 대상: 이 role에 분류된 done[] + weekly_reports[].highlights + release_results[].highlights)
  - {item}
    - (sub-bullet으로 출처: "- (주간 리포트 기반 · [REPO_CODE])" 또는 "- (릴리즈 PR #N · [MODULE_CODE])")

  **차주 진행할 작업**  (이 role에 분류된 actions + planned + in_progress)
  - {what} (담당 {assignee})   ← rule 4 형식. deadline 비면 omit.
    - (cross-role인 경우 sub-bullet: "요청: {origin} ({origin_roles})")
    - (in_progress 인 경우 sub-bullet: "현재 진행 중")

  **참고 자료**  (이 role에 분류된 weekly_reports / release_results 메타만)
  - 주간 리포트 — [REPO_CODE] · 커밋 N건, 지난 N일   ← 메타만 1줄 (rule 3 형식, 0/빈값 omit)
  - 릴리즈 — [MODULE_CODE] v{prev} → v{new} ({bump}) · PR #{n}
  (agent_responses / external_refs는 이 sub-section에 들어가지 않는다 — 공통 섹션 전용)

  (각 sub-section은 비면 라벨째 제거. 셋 다 비면 role 그룹 자체 제거.)

그 후 모든 role 공통 섹션 (있는 것만, 표준 헤더 라벨 그대로):

| 표준 헤더 라벨                      | Source                                                                              |
|-------------------------------------|-------------------------------------------------------------------------------------|
| 팀에 함께 공유한 내용               | shared                                                                              |
| 모두에게 적용되는 공통 결정         | decisions                                                                           |
| 이번 주에 완료한 작업 (공통)        | role 매핑 실패한 done[] 항목 + role 매핑 실패한 weekly/release highlights          |
| 현재 진행 중인 작업 (공통)          | role 매핑 실패한 in_progress[] 항목                                                |
| 곧 시작할 작업 (공통)               | role 매핑 실패한 planned[] 항목                                                    |
| 더 확인이 필요한 부분               | open_questions + blockers (rule 8 중복 방지)                                       |
| 회의에서 함께 참고한 자료           | role 매핑 안 된 weekly/release 메타 + 모든 agent_responses + 모든 external_refs    |
| 관련 키워드                          | tags                                                                                |

매핑 실패 작업 항목은 어디에도 누락되지 않게 위 "(공통)" 헤더로 흡수. 빈 경우 헤더째 제거.

CRITICAL (정책 통일):
  - 매핑 가능한 weekly/release: highlights는 해당 role 그룹의 "이번 주에 마무리한 작업"에 흡수,
    메타는 해당 role 그룹의 "참고 자료" sub-section에 1줄.
  - 매핑 불가능한 weekly/release: highlights와 메타 모두 공통 "회의에서 함께 참고한 자료"로.
  - agent_responses / external_refs: 항상 공통 "회의에서 함께 참고한 자료" (role 그룹 안 절대 X).
  - 봇 결과 항목 자체에 attribution(담당자)은 부여하지 않음.

빈 그룹 / 빈 sub-section / 빈 공통 섹션은 헤더째 제거. "(없음)" 절대 금지.

본문 첫 줄 강제: 모든 1급 섹션 본문 및 role 그룹 H3 직후 첫 줄은 "- " (rule 5).`

// FormatRenderFreeform — 자율 포맷.
const FormatRenderFreeform = formatRenderCommon + `

# Format: freeform (자율 정리)

Use case: 비정형 / 단발성 / 회의 성격이 명확하지 않을 때.

## Structure

LLM이 회의 흐름을 보고 가장 자연스러운 섹션 구조를 선택. 정해진 순서 X.

표준 헤더 라벨 (필요한 것만 골라 사용 — 단어 한 개 헤더 금지, 임의 변형 금지):
- ## 회의 한 줄 정리             ← 전체 요약 1줄 (1뎁스 불릿)
- ## 회의 핵심 결정              ← decisions
- ## 이번 주에 완료한 작업       ← done + weekly_reports.highlights + release_results.highlights (rule 3 흡수)
- ## 현재 진행 중인 작업         ← in_progress
- ## 곧 시작할 작업              ← planned
- ## 후속으로 진행할 작업        ← actions
- ## 이번 회의에서 다룬 주제     ← topics
- ## 더 확인이 필요한 부분       ← open_questions + blockers
- ## 팀에 함께 공유한 내용       ← shared
- ## 회의에서 함께 참고한 자료   ← weekly_reports 메타 + release_results 메타 + agent_responses + external_refs
- ## 관련 키워드                 ← tags

## Hard rules

1. SummarizedContent의 모든 사실은 어떤 섹션에든 1번 등장 (누락 금지). 단 빈 필드는 무시.
   - done / in_progress / planned / shared 도 노출 헤더가 위 표준 목록에 있으므로 누락하지 말 것.
2. 같은 사실 다른 섹션 중복 노출 금지 (각 사실 1군데만).
3. 봇 결과 흡수: rule 3 (common) 적용 — weekly/release highlight는 "이번 주에 완료한 작업"에,
   메타와 agent/external은 "회의에서 함께 참고한 자료"에.
4. 빈 필드 → 관련 섹션 자체 제거. "(없음)" 금지.
5. 모든 필드가 비면 단일 문장 "정리할 내용이 없습니다."로 출력.
6. 모든 섹션 본문 첫 줄은 "- " (rule 5). "회의 한 줄 정리"도 1뎁스 불릿 1줄.

회의 성격 판단 힌트:
- decisions 0 + topics 0 + actions 0 + done 0 + 봇 결과만 있음 →
  highlight는 "## 이번 주에 완료한 작업"에 흡수, 메타는 "## 회의에서 함께 참고한 자료"에.
- topics 1+ + decisions 0 → 논의 중심 → ## 회의 한 줄 정리 + ## 이번 회의에서 다룬 주제 + (참고 자료)
- actions 다수 → 작업 분담 → ## 회의 핵심 결정 + ## 후속으로 진행할 작업 + (참고 자료)`
