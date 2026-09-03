package proxy

import (
	"encoding/json"
	"testing"
)

// 非标准工具/include 全部被过滤后应删除整个键，而不是留下空数组。
// 部分上游会把 "include": [] / "tools": [] 判为显式请求"不包含任何内容"，
// 语义上等同于不传。
func TestSanitizeResponsesRequestDropsEmptyToolsAndInclude(t *testing.T) {
	body := []byte(`{"model":"m","tools":[{"type":"x_search"}],"include":["x_search.results"],"stream_tool_calls":true}`)

	out, err := sanitizeResponsesRequest(body)
	if err != nil {
		t.Fatalf("sanitize failed: %v", err)
	}

	var m map[string]json.RawMessage
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	for _, key := range []string{"tools", "include", "stream_tool_calls"} {
		if _, ok := m[key]; ok {
			t.Fatalf("key %q should have been deleted, got: %s", key, out)
		}
	}
	if _, ok := m["model"]; !ok {
		t.Fatalf("unrelated key was lost: %s", out)
	}
}

// 部分过滤时保留剩余条目。
func TestSanitizeResponsesRequestKeepsSurvivingTools(t *testing.T) {
	body := []byte(`{"model":"m","tools":[{"type":"x_search"},{"type":"function","name":"f"}],"include":["reasoning.encrypted_content","x_search.results"]}`)

	out, err := sanitizeResponsesRequest(body)
	if err != nil {
		t.Fatalf("sanitize failed: %v", err)
	}

	var m struct {
		Tools []struct {
			Type string `json:"type"`
		} `json:"tools"`
		Include []string `json:"include"`
	}
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	if len(m.Tools) != 1 || m.Tools[0].Type != "function" {
		t.Fatalf("unexpected tools: %s", out)
	}
	if len(m.Include) != 1 || m.Include[0] != "reasoning.encrypted_content" {
		t.Fatalf("unexpected include: %s", out)
	}
}

// 成员级编辑必须逐字节保留未修改成员及其空白格式。
// 回归背景：早期实现把"前一个保留成员到下一个保留成员之间的全部字节"当分隔符，
// 夹在中间的被删成员会原样混回输出。
func TestSanitizeResponsesRequestBytePreservingMemberEdits(t *testing.T) {
	// 多行格式 + 冒号前后空格 + 被删成员夹在两个保留成员之间
	body := []byte("{\n  \"model\" : \"m\",\n  \"stream_tool_calls\" : true,\n  \"input\" : [1, 2]\n}")

	out, err := sanitizeResponsesRequest(body)
	if err != nil {
		t.Fatalf("sanitize failed: %v", err)
	}
	want := "{\n  \"model\" : \"m\",\n  \"input\" : [1, 2]\n}"
	if string(out) != want {
		t.Fatalf("member edit did not preserve surrounding bytes:\n got: %q\nwant: %q", out, want)
	}
	if !json.Valid(out) {
		t.Fatalf("output is not valid JSON: %s", out)
	}

	// 删除首个成员
	body2 := []byte(`{"stream_tool_calls":true,"model":"m","input":[]}`)
	out2, err := sanitizeResponsesRequest(body2)
	if err != nil {
		t.Fatalf("sanitize failed: %v", err)
	}
	if string(out2) != `{"model":"m","input":[]}` {
		t.Fatalf("leading member deletion failed: %s", out2)
	}

	// 全部成员被删（除无关成员外）
	body3 := []byte(`{"tools":[{"type":"x_search"}],"include":["x"],"keep":1}`)
	out3, err := sanitizeResponsesRequest(body3)
	if err != nil {
		t.Fatalf("sanitize failed: %v", err)
	}
	if string(out3) != `{"keep":1}` {
		t.Fatalf("multi-member deletion failed: %s", out3)
	}
}
