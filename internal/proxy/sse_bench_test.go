package proxy

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

func BenchmarkSSEPassThrough(b *testing.B) {
	stream := buildBenchmarkSSEStream(200, false)
	b.SetBytes(int64(len(stream)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r := newResponsesSSEFilter(bytes.NewReader(stream))
		_, _ = io.Copy(io.Discard, r)
	}
}

func BenchmarkSSEWithTransform(b *testing.B) {
	stream := buildBenchmarkSSEStream(200, true)
	b.SetBytes(int64(len(stream)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r := newResponsesSSEFilter(bytes.NewReader(stream))
		_, _ = io.Copy(io.Discard, r)
	}
}

func BenchmarkSSESenseNovaTransform(b *testing.B) {
	line := []byte(`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call-1","type":"function","function":{"name":"lookup","arguments":"{}"}}]}}]}` + "\n")
	b.SetBytes(int64(len(line)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = transformSenseNovaSSELine(line)
	}
}

func BenchmarkSSESenseNovaContinuation(b *testing.B) {
	line := []byte(`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"","type":"function","function":{"name":"","arguments":"x"}}]},"finish_reason":""}]}` + "\n")
	b.SetBytes(int64(len(line)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = transformSenseNovaSSELine(line)
	}
}

func buildBenchmarkSSEStream(events int, includeReasoning bool) []byte {
	var buf bytes.Buffer
	for i := 0; i < events; i++ {
		if includeReasoning {
			buf.WriteString("event: response.reasoning.delta\n")
			buf.WriteString(`data: {"type":"response.reasoning.delta","delta":"thinking"}` + "\n\n")
		}
		buf.WriteString(`data: {"type":"response.output_text.delta","delta":"hello"}` + "\n\n")
		if i%50 == 0 {
			buf.WriteString("data: ping\n\n")
		}
	}
	buf.WriteString("data: [DONE]\n\n")
	return buf.Bytes()
}

func TestSSEPingFilteringRemovesPings(t *testing.T) {
	stream := []byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"a\"}\n\n" +
		"data: ping\n\n" +
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\"b\"}\n\n" +
		"data: [DONE]\n\n")
	r := newResponsesSSEFilter(bytes.NewReader(stream))
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "ping") {
		t.Fatalf("ping was not filtered: %s", out)
	}
	if !strings.Contains(string(out), "[DONE]") {
		t.Fatalf("[DONE] was lost: %s", out)
	}
}

func TestSSEReasoningRenameEndToEnd(t *testing.T) {
	stream := []byte("event: response.reasoning.delta\ndata: {\"type\":\"response.reasoning.delta\",\"delta\":\"x\"}\n\n")
	r := newResponsesSSEFilter(bytes.NewReader(stream))
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "response.reasoning.delta") {
		t.Fatalf("legacy reasoning event was not renamed: %s", out)
	}
	if !strings.Contains(string(out), "response.reasoning_text.delta") {
		t.Fatalf("renamed event was missing: %s", out)
	}
}
