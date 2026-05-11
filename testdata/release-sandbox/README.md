# release-sandbox

릴리즈 봇 흐름 검증용 더미 모듈.

- 실제 prod 배포 없음 — chatbot 레포 자체에 PR/태그만 생성하여 봇 동작 시나리오만 굴린다.
- 태그 컨벤션: `sandbox-{module}/v{version}` (chatbot 자체 release 워크플로우의 `v*.*.*` 매칭 회피).
- 릴리즈 브랜치: `release/sandbox-{module}` (봇이 없으면 main 시점으로 생성).

## 모듈

| Key | 라인 | 디스플레이 | VERSION |
|-----|-----|-----------|---------|
| product | backend | 프로덕트 | testdata/release-sandbox/product/VERSION |
| admin   | backend | 어드민   | testdata/release-sandbox/admin/VERSION |
| batch   | backend | 배치     | testdata/release-sandbox/batch/VERSION (prod 자동배포 없음) |
