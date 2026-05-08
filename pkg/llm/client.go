package llm

import (
	"errors"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
)

// ErrMissingAPIKey는 API 키가 비어 있을 때 반환된다.
var ErrMissingAPIKey = errors.New("llm: GPT_API_KEY is empty")

// Client는 OpenAI SDK 래퍼. 프로젝트 전역에서 이 타입으로만 LLM에 접근한다.
type Client struct {
	api openai.Client
}

// NewClient는 주어진 API 키로 OpenAI 클라이언트를 초기화한다.
// 빈 키를 전달하면 ErrMissingAPIKey를 반환한다.
// SDK 기본 환경변수(OPENAI_API_KEY)에 의존하지 않기 위해 키를 명시 주입한다.
func NewClient(apiKey string, opts ...option.RequestOption) (*Client, error) {
	if apiKey == "" {
		return nil, ErrMissingAPIKey
	}
	all := append([]option.RequestOption{option.WithAPIKey(apiKey)}, opts...)
	return &Client{api: openai.NewClient(all...)}, nil
}

// API는 내부 SDK 클라이언트에 대한 접근자 (후속 Task에서 사용).
func (c *Client) API() *openai.Client {
	return &c.api
}
