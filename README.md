# chatbot-alpha-1

![Go](https://img.shields.io/badge/Go-1.25-00ADD8?logo=go&logoColor=white)
![discordgo](https://img.shields.io/badge/discordgo-v0.29-5865F2?logo=discord&logoColor=white)
![OpenAI](https://img.shields.io/badge/OpenAI-Structured%20Output-412991?logo=openai&logoColor=white)
![Cobra](https://img.shields.io/badge/CLI-Cobra-7F52FF)
![Deploy](https://img.shields.io/badge/deploy-ArgoCD%20%2B%20Kustomize-EF7B4D)

> 회의·운영·이슈 트래킹을 한 디스코드 스레드에서 끝내는 LLM 봇.

---

## Introduction

chatbot-alpha-1은 소규모 팀의 일상적인 운영 부담 — 매주 회의 노트, 매주 이슈 정리, "그 PR 어디까지 됐어?" 같은 즉문즉답 — 을 디스코드 채팅 한 줄 또는 버튼 한 번으로 처리하기 위해 만들어진 Go 기반 LLM 봇이다.

미팅 메모는 자동으로 구조화된 노트가 되고, GitHub 상태는 LLM이 정리해서 던져주며, "인프라 관련 열려있는 이슈만 보여줘" 같은 자유 지시도 같은 스레드에서 그 자리에서 처리한다.

| | |
|---|---|
| **대상** | GitHub + Discord를 함께 쓰는 5–20인 규모 엔지니어링 팀 |
| **스택** | Go 1.25 / discordgo / OpenAI Structured Output (`gpt-5.4-mini`) / Cobra CLI |
| **배포** | distroless Docker 이미지 + Argo CD + Kustomize + SOPS |

---

## Why chatbot-alpha-1?

기존 미팅 노트 봇과 GitHub 디지스트 봇은 각자 도메인 하나만 책임진다. 그래서 운영 흐름은 항상 **창을 옮기는 일**로 끝난다 — 회의가 끝나면 별도 도구에서 노트 정리, 이슈 점검은 또 다른 대시보드, "그거 어떻게 됐지?" 즉문즉답은 사람이 직접 GitHub을 뒤지는 식이다.

chatbot-alpha-1은 다음을 다르게 한다.

- **단일 스레드 워크플로우** — 미팅 finalize → 주간 분석 → 분석 결과를 미팅 첫 노트로 주입 → 다시 미팅. 도구 간 컨텍스트 스위치가 없다.
- **환각을 신뢰하지 않는 설계** — 모든 응답을 원본 노트와 substring/토큰 겹침으로 1차 검증한다. "이슈 닫기" 같은 비가역 액션은 LLM 추천을 그대로 실행하지 않고 별도 schema 필드로 ground truth를 따로 받아 사용자 확인 prompt를 거친다.
- **4 포맷 + 자연어 directive** — 같은 회의 데이터를 결정+진행 / 논의 / 역할별 / 자율 4가지 관점으로 정리할 수 있고, "프론트/백엔드/기획 H3 섹션으로 묶어줘" 같은 한 줄 지시로 스타일을 추가 변형할 수 있다.
- **디스코드 없이 검증 가능** — `llm-bot` / `git-bot` 서브커맨드로 stdout에서 LLM·GitHub 파이프라인을 직접 돌려 골든 비교한다. 프롬프트 튜닝과 회귀 검증이 봇을 띄우는 절차 없이 가능하다.
- **운영 안전장치** — 모든 렌더에서 `@` 멘션 prefix를 의도적으로 제거한다. 노트는 한 명이 대신 적는 형태라 다른 사용자에게 푸시 알림이 가면 안 되는 운영 요건을 그대로 반영한다.

---

## Features

### 1. 미팅 모드
디스코드 스레드 안에서 메모를 자유롭게 쌓고 "미팅 종료"로 마무리하면 LLM이 구조화된 노트를 생성한다.

| 항목 | 값 |
|---|---|
| 정리 포맷 | 결정+진행 / 논의 / 역할별 / 자율(freeform) |
| 추가 요청 | 자연어 directive — 같은 회의 데이터에 다른 관점 적용 |
| 중간 요약 | 진행 중 누적 노트를 즉석 정리 |
| 컨트롤 메시지 | sticky — 채팅이 흘러도 종료 버튼이 항상 하단 |
| Idle 타임아웃 | 미팅 모드 2h / 일반 모드 5m |

### 2. 주간 정리
등록된 GitHub 레포의 **현재 OPEN 이슈 전체 + 지난 N일(기본 14일) 커밋**을 LLM에 dump해 운영 진단 마크다운을 생성한다.

| 항목 | 값 |
|---|---|
| 출력 섹션 | 한 줄 요약 / 주요 활동 / 닫아도 될 이슈 / 블로커 / 라벨·메타 정합성 / 다음 주 우선순위 |
| 일괄 close | `closeable[]` 스키마 필드 → GitHub PATCH로 한 번에 close |
| Follow-up | 추가 요청 / 기간 변경(14·30일) / 재분석 / 분석 결과를 미팅 첫 노트로 주입 |

### 3. 에이전트 모드
정형 버튼 흐름과 별개로 자유 자연어 지시를 받는 모드.

> "워크스페이스에서 인프라 관련 열려있는 이슈들 가져와"
> "BE에서 지난 14일 동안 가장 큰 변경 요약해줘"

등록된 4개 레포 데이터를 병렬로 fetch해 단일 dump로 LLM에 전달, 의도 매칭은 LLM이 처리한다.

### 4. CLI 검증 모드
디스코드를 띄우지 않고 stdout에서 동일 파이프라인을 돌린다.

| 서브커맨드 | 용도 |
|---|---|
| `llm-bot` | 미팅 시나리오 시뮬레이션 (interactive stdin / 시나리오 파일 / 4 포맷 골든 비교) |
| `git-bot` | GitHub fetch + LLM 정제 검증 (`--repo`, `--days`, `--directive`) |

### 5. 세션 연결
미팅 finalize, 주간 분석, 에이전트 답변 — 어떤 작업이 끝나도 세션은 유지되고 [처음 메뉴]로 같은 스레드에서 다음 작업으로 이어간다.

---

## Installation

### 요구사항
- Go **1.25** 이상
- Discord 봇 토큰 (Discord Developer Portal에서 생성)
- OpenAI API 키
- GitHub Personal Access Token (`repo` 권한)

### 1. 환경 변수
프로젝트 루트에 `.env` 파일을 생성한다.

| 변수 | 필수 | 용도 |
|---|:--:|---|
| `DISCORD_BOT_TOKEN` | O | Discord 봇 토큰 |
| `GPT_API_KEY` | O | OpenAI API 키 |
| `GITHUB_TOKEN` | O | GitHub PAT (`repo` 권한) |
| `GITHUB_ORG` | O | 기본 GitHub org |
| `GITHUB_REPO` | O | 기본 GitHub repo |

```env
DISCORD_BOT_TOKEN=<your_discord_bot_token>
GPT_API_KEY=<your_openai_api_key>
GITHUB_TOKEN=<your_github_pat>
GITHUB_ORG=<your_org>
GITHUB_REPO=<your_default_repo>
```

### 2. 로컬 실행

```bash
git clone <repo-url> chatbot-alpha-1
cd chatbot-alpha-1
go run .
```

서브커맨드 없이 실행하면 디스코드 봇이 기동된다. 명시적으로 띄우려면 `go run . bot`.

### 3. CLI 검증 모드 (디스코드 불필요)

```bash
# 미팅 시나리오 시뮬레이션 (4 포맷)
go run . llm-bot --format=decision_status
go run . llm-bot --file docs/시나리오_01.md --format=role_based

# GitHub 주간 분석
go run . git-bot --repo bottle-note/workspace
go run . git-bot --repo bottle-note/workspace --days 30
```

### 4. Docker / 프로덕션 배포

```bash
docker build -t chatbot-alpha-1 .
```

이미지는 distroless 기반 amd64 단일 타겟 multi-stage 빌드. 쿠버네티스 배포는 `deploy/overlays/production/` (Argo CD Application + Kustomize + SOPS 암호화 시크릿)을 참조한다.
