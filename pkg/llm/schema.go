package llm

import (
	"encoding/json"

	"github.com/invopop/jsonschema"
)

// GenerateSchema는 Go 타입 T로부터 OpenAI Structured Output에 사용할
// JSON Schema를 생성한다. additionalProperties=false, 참조 전개 (inline) 옵션을 적용한다.
func GenerateSchema[T any]() map[string]any {
	reflector := jsonschema.Reflector{
		AllowAdditionalProperties: false,
		DoNotReference:            true,
	}
	var v T
	schema := reflector.Reflect(v)
	data, err := json.Marshal(schema)
	if err != nil {
		panic("llm: schema marshal failed: " + err.Error())
	}
	var result map[string]any
	if err := json.Unmarshal(data, &result); err != nil {
		panic("llm: schema unmarshal failed: " + err.Error())
	}
	return result
}
