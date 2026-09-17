// Package output prints results for two readers: humans (short tables, stderr diagnostics) and agents
// (`--json`: stable JSON on stdout, nothing else). Exit codes follow the plan: 0 ok, 1 error,
// 2 usage/cancelled, 4 authentication required, 5 approval required, 6 stale/conflict.
package output

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/itchyny/gojq"
)

const (
	ExitOK           = 0
	ExitFailure      = 1
	ExitUsage        = 2
	ExitAuthRequired = 4
	ExitApproval     = 5
	ExitConflict     = 6
)

// Exit carries an exit code and, for agents, a structured payload printed as JSON.
type Exit struct {
	Code    int
	Message string
	Payload any
}

func (e *Exit) Error() string { return e.Message }

// Printer knows whether the caller wants JSON.
type Printer struct {
	JSON    bool
	JQ      string // optional jq expression applied to the JSON result (implies JSON)
	Out     io.Writer
	Err     io.Writer
	Quiet   bool
	NoColor bool
}

func New(jsonMode bool) *Printer {
	return &Printer{JSON: jsonMode, Out: os.Stdout, Err: os.Stderr}
}

// Result prints the final value of a command: JSON when asked, otherwise via the human renderer.
func (p *Printer) Result(v any, human func(w io.Writer)) {
	if p.JQ != "" {
		if err := p.jq(v); err != nil {
			fmt.Fprintf(p.Err, "jq: %v\n", err)
		}
		return
	}
	if p.JSON || human == nil {
		enc := json.NewEncoder(p.Out)
		enc.SetIndent("", "  ")
		enc.SetEscapeHTML(false) // URLs with & must survive copy/paste by agents
		_ = enc.Encode(v)
		return
	}
	human(p.Out)
}

// Line is a diagnostic line for humans (stderr, never in JSON mode's stdout).
func (p *Printer) Line(format string, args ...any) {
	if p.Quiet {
		return
	}
	fmt.Fprintf(p.Err, format+"\n", args...)
}

// Table prints rows with aligned columns.
func Table(w io.Writer, header []string, rows [][]string) {
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, strings.Join(header, "\t"))
	for _, row := range rows {
		fmt.Fprintln(tw, strings.Join(row, "\t"))
	}
	_ = tw.Flush()
}

// NDJSON writes one JSON object per line (for `wait` streams).
func NDJSON(w io.Writer, v any) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	_, _ = w.Write(buf.Bytes())
}

// jq applies the expression to v (round-tripped through JSON so structs behave like plain objects) and
// prints each output: raw strings, JSON for everything else — the `gh --jq` convention.
func (p *Printer) jq(v any) error {
	query, err := gojq.Parse(p.JQ)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	var input any
	if err := json.Unmarshal(raw, &input); err != nil {
		return err
	}
	iter := query.Run(input)
	for {
		out, ok := iter.Next()
		if !ok {
			return nil
		}
		if err, isErr := out.(error); isErr {
			return err
		}
		if s, isStr := out.(string); isStr {
			fmt.Fprintln(p.Out, s)
			continue
		}
		enc := json.NewEncoder(p.Out)
		enc.SetEscapeHTML(false)
		_ = enc.Encode(out)
	}
}
