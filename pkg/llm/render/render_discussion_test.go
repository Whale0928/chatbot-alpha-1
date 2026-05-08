package render

import (
	"testing"
	"time"

	"chatbot-alpha-1/pkg/llm"
)

func TestRenderDiscussion_Full(t *testing.T) {
	in := DiscussionRenderInput{
		Date:         time.Date(2026, 4, 21, 0, 0, 0, 0, time.UTC),
		Participants: []string{"hgkim", "manager"},
		Response: &llm.DiscussionResponse{
			Topics: []llm.Topic{
				{
					Title:    "최근 업무 부담",
					Flow:     []string{"크롤러 안정화 시간 비중 공유", "혼자 끌고 가는 느낌 언급"},
					Insights: []string{"분담 가능 영역 식별을 다음 회의에서 합의"},
				},
				{
					Title: "커리어 방향",
					Flow:  []string{"백엔드 vs 데이터 파이프라인 고민"},
				},
			},
			OpenQuestions: []string{"분담 가능 영역 - 확인 필요"},
			Tags:          []string{"1on1", "커리어"},
		},
	}
	out := RenderDiscussion(in)

	mustContain(t, out, "# 2026-04-21 미팅 노트")
	mustContain(t, out, "## 논의 토픽")
	mustContain(t, out, "### 1. 최근 업무 부담")
	mustContain(t, out, "- 크롤러 안정화 시간 비중 공유")
	mustContain(t, out, "**도출된 관점**")
	mustContain(t, out, "- 분담 가능 영역 식별을 다음 회의에서 합의")
	mustContain(t, out, "### 2. 커리어 방향")
	mustContain(t, out, "## 확인 필요")
	mustContain(t, out, "참석자: `hgkim`, `manager`")
	mustContain(t, out, "태그: #1on1 #커리어")
}

func TestRenderDiscussion_TopicWithoutInsights(t *testing.T) {
	in := DiscussionRenderInput{
		Date:         time.Date(2026, 4, 21, 0, 0, 0, 0, time.UTC),
		Participants: []string{"hgkim"},
		Response: &llm.DiscussionResponse{
			Topics: []llm.Topic{{Title: "단일 토픽", Flow: []string{"흐름1"}}},
		},
	}
	out := RenderDiscussion(in)
	mustContain(t, out, "### 1. 단일 토픽")
	mustNotContain(t, out, "**도출된 관점**")
}

func TestRenderDiscussion_NoTopics(t *testing.T) {
	in := DiscussionRenderInput{
		Date:         time.Date(2026, 4, 21, 0, 0, 0, 0, time.UTC),
		Participants: []string{"hgkim"},
		Response:     &llm.DiscussionResponse{},
	}
	out := RenderDiscussion(in)
	mustContain(t, out, "## 논의 토픽\n- (기록되지 않음)")
}
