package proxy

import (
	"encoding/json"
	"io"
	"strings"
	"testing"
)

// DeepSeek 不支持任何 include 值（reasoning 恒以明文返回），整个字段移除。
func TestDeepSeekDropsIncludeEntirely(t *testing.T) {
	body := []byte(`{"model":"deepseek-v4-flash","include":["reasoning.encrypted_content"],"input":"hi"}`)
	out, err := DeepSeekResponsesAdapter{}.TransformRequestBody(body)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "include") {
		t.Fatalf("include survived: %s", out)
	}
	if !strings.Contains(string(out), `"input":"hi"`) {
		t.Fatalf("unrelated fields were lost: %s", out)
	}
}

// reasoning 输入项：明文 content（reasoning_content 的载体）必须保留——
// 带 tools 的请求要求完整回传思维链，否则 DeepSeek 返回 400；
// summary 与 encrypted_content 不受支持，剔除。
func TestDeepSeekKeepsReasoningContentButStripsUnsupportedFields(t *testing.T) {
	body := []byte(`{"input":[` +
		`{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"sum"}],"content":[{"type":"reasoning_text","text":"chain of thought"}],"encrypted_content":"gAAAA"},` +
		`{"type":"message","role":"user","content":"hi"}]}`)
	out, err := DeepSeekResponsesAdapter{}.TransformRequestBody(body)
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Input []map[string]any `json:"input"`
	}
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatal(err)
	}
	reasoning := payload.Input[0]
	if _, hasSummary := reasoning["summary"]; hasSummary {
		t.Fatalf("reasoning summary survived: %s", out)
	}
	if _, hasEncrypted := reasoning["encrypted_content"]; hasEncrypted {
		t.Fatalf("encrypted_content survived: %s", out)
	}
	content, ok := reasoning["content"].([]any)
	if !ok || len(content) != 1 || content[0].(map[string]any)["text"] != "chain of thought" {
		t.Fatalf("reasoning content was not preserved: %s", out)
	}
	if payload.Input[1]["type"] != "message" {
		t.Fatalf("non-reasoning item was changed: %s", out)
	}
}

// reasoning.effort 原样透传（none/minimal/low/medium/high/xhigh/max 均不改写）。
func TestDeepSeekPassesReasoningEffortThroughUntouched(t *testing.T) {
	for _, effort := range []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"} {
		body := []byte(`{"model":"deepseek-v4-pro","reasoning":{"effort":"` + effort + `"},"input":"hi"}`)
		out, err := DeepSeekResponsesAdapter{}.TransformRequestBody(body)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(out), `"effort":"`+effort+`"`) {
			t.Fatalf("effort %q was rewritten: %s", effort, out)
		}
	}
}

// 共享的标准净化在 DeepSeek 网关同样生效：stream_tool_calls、x_search、
// excluded_domains web_search 一并剔除；无违规字段的请求逐字节透传。
func TestDeepSeekAppliesStandardSanitization(t *testing.T) {
	adapter := DeepSeekResponsesAdapter{}
	body := []byte(`{"model":"deepseek-v4-flash","stream_tool_calls":true,` +
		`"tools":[{"type":"x_search"},{"type":"web_search","filters":{"excluded_domains":["a.example"]}},{"type":"function","name":"lookup","parameters":{"type":"object"}}],` +
		`"input":"hi"}`)
	out, err := adapter.TransformRequestBody(body)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "stream_tool_calls") || strings.Contains(string(out), "x_search") {
		t.Fatalf("xAI extensions survived: %s", out)
	}
	tools := len(strings.Split(string(out), `"type":"function"`)) - 1
	if tools != 1 {
		t.Fatalf("function tool was lost: %s", out)
	}

	conformant := []byte(`{"model":"deepseek-v4-flash","reasoning":{"effort":"high"},"input":"hi"}`)
	noop, err := adapter.TransformRequestBody(conformant)
	if err != nil {
		t.Fatal(err)
	}
	if string(noop) != string(conformant) {
		t.Fatalf("conformant body was rewritten: %s", noop)
	}
}

// DeepSeek 的事件名全部落在客户端词汇表内（含 custom_tool_call_input.delta、
// web_search_call 状态事件），流末尾没有 data: [DONE]——按 EOF 收尾，原样保留。
func TestDeepSeekSSEPassesKnownEventsWithoutDONE(t *testing.T) {
	adapter := DeepSeekResponsesAdapter{}
	input := "event: response.created\ndata: {\"type\":\"response.created\",\"sequence_number\":0,\"response\":{\"id\":\"r1\",\"object\":\"response\",\"created_at\":1,\"status\":\"in_progress\",\"model\":\"m\",\"output\":[]}}\n\n" +
		"event: response.reasoning_text.delta\ndata: {\"type\":\"response.reasoning_text.delta\",\"sequence_number\":1,\"item_id\":\"rs_1\",\"output_index\":0,\"content_index\":0,\"delta\":\"think\"}\n\n" +
		"event: response.custom_tool_call_input.delta\ndata: {\"type\":\"response.custom_tool_call_input.delta\",\"sequence_number\":2,\"item_id\":\"ct_1\",\"output_index\":1,\"delta\":\"***\"}\n\n" +
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"sequence_number\":3,\"response\":{\"id\":\"r1\",\"object\":\"response\",\"created_at\":1,\"status\":\"completed\",\"model\":\"m\",\"output\":[]}}\n\n"
	reader := adapter.TransformSSE(strings.NewReader(input))
	body, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != input {
		t.Fatalf("DeepSeek stream was rewritten:\nwant: %q\ngot:  %q", input, body)
	}
	if strings.Contains(string(body), "[DONE]") {
		t.Fatalf("a [DONE] sentinel was injected: %s", body)
	}
}
