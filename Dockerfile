# syntax=docker/dockerfile:1
# Go static 바이너리를 distroless 위에 올리는 multi-stage 빌드.
# amd64 단일 타겟 (homelab pve-pod-1/2 만 사용).

FROM golang:1.25-alpine AS build
WORKDIR /src

# 의존성 캐시 레이어
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w" -o /out/chatbot-alpha-1 .

FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app
COPY --from=build /out/chatbot-alpha-1 /app/chatbot-alpha-1
USER nonroot:nonroot
ENTRYPOINT ["/app/chatbot-alpha-1"]
