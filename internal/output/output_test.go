package output

import (
	"bytes"
	"testing"
)

func TestJQ(t *testing.T) {
	var out bytes.Buffer
	p := &Printer{JSON: true, JQ: ".data[].id", Out: &out, Err: &out}
	p.Result(map[string]any{"data": []any{map[string]any{"id": "a"}, map[string]any{"id": "b"}}}, nil)
	if out.String() != "a\nb\n" {
		t.Fatalf("got %q", out.String())
	}
	out.Reset()
	p.JQ = "{n: (.data | length)}"
	p.Result(map[string]any{"data": []any{1, 2}}, nil)
	if out.String() != "{\"n\":2}\n" {
		t.Fatalf("got %q", out.String())
	}
	out.Reset()
	p.JQ = ".["
	p.Result(map[string]any{}, nil)
	if !bytes.Contains(out.Bytes(), []byte("jq:")) {
		t.Fatalf("expected a jq error, got %q", out.String())
	}
}
