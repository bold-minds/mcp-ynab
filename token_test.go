// SPDX-License-Identifier: MIT
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"testing"
)

// The sentinel string is embedded in a Token and then poked through every
// standard formatting path Go offers. A single appearance of the sentinel in
// the output is a redaction failure.
const leakSentinel = "sk-LEAK-sentinel-this-must-never-appear-ABCDEF"

// TestToken_RedactsAllFormatVerbs checks every standard fmt verb and
// confirms the sentinel never appears in the rendered output.
func TestToken_RedactsAllFormatVerbs(t *testing.T) {
	t.Parallel()
	tok := NewToken(leakSentinel)
	cases := []struct {
		name string
		got  string
	}{
		{"%v", fmt.Sprintf("%v", tok)},
		{"%+v", fmt.Sprintf("%+v", tok)},
		{"%s", fmt.Sprintf("%s", tok)},
		{"%q", fmt.Sprintf("%q", tok)},
		{"%#v", fmt.Sprintf("%#v", tok)},
		{"%d", fmt.Sprintf("%d", tok)},
		{"%x", fmt.Sprintf("%x", tok)},
		{"Sprint", fmt.Sprint(tok)},
		{"Sprintln", fmt.Sprintln(tok)},
		{"String()", tok.String()},
		{"GoString()", tok.GoString()},
	}
	for _, c := range cases {
		if strings.Contains(c.got, leakSentinel) {
			t.Errorf("REDACTION FAILURE via %s: %q", c.name, c.got)
		}
		if !strings.Contains(c.got, redactedPlaceholder) {
			t.Errorf("%s: expected placeholder, got %q", c.name, c.got)
		}
	}
}

// TestToken_RedactsViaStructEmbedding verifies that embedding a Token in a
// struct and using %+v still redacts. This is the common leak path —
// developers logging an entire Client or config struct.
func TestToken_RedactsViaStructEmbedding(t *testing.T) {
	t.Parallel()
	type container struct {
		Name  string
		Token Token
	}
	c := container{Name: "test", Token: NewToken(leakSentinel)}
	for _, verb := range []string{"%v", "%+v", "%#v"} {
		out := fmt.Sprintf(verb, c)
		if strings.Contains(out, leakSentinel) {
			t.Errorf("REDACTION FAILURE via struct %s: %q", verb, out)
		}
	}
}

// TestToken_RedactsViaJSON confirms that json.Marshal always emits the
// placeholder — no code path using default encoding/json can leak.
func TestToken_RedactsViaJSON(t *testing.T) {
	t.Parallel()
	tok := NewToken(leakSentinel)
	b, err := json.Marshal(tok)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), leakSentinel) {
		t.Errorf("REDACTION FAILURE via json.Marshal: %s", b)
	}
	if string(b) != `"[REDACTED]"` {
		t.Errorf("unexpected JSON: %s", b)
	}

	// Embedded in a struct.
	type wrap struct {
		Token Token `json:"token"`
	}
	b, err = json.Marshal(wrap{Token: tok})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), leakSentinel) {
		t.Errorf("REDACTION FAILURE via struct json.Marshal: %s", b)
	}
}

// TestToken_RefusesUnmarshal verifies that UnmarshalJSON and UnmarshalText
// refuse, so untrusted input cannot produce a valid Token.
func TestToken_RefusesUnmarshal(t *testing.T) {
	t.Parallel()
	var tok Token
	if err := json.Unmarshal([]byte(`"attacker-supplied"`), &tok); err == nil {
		t.Error("UnmarshalJSON should refuse")
	}
	if err := tok.UnmarshalText([]byte("attacker-supplied")); err == nil {
		t.Error("UnmarshalText should refuse")
	}
}

// TestToken_RedactsViaLog captures stderr from the standard logger and
// confirms that logging a struct containing a Token does not leak.
func TestToken_RedactsViaLog(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	logger := log.New(&buf, "", 0)
	logger.Printf("debug dump: %+v", struct {
		Token Token
	}{Token: NewToken(leakSentinel)})
	out := buf.String()
	if strings.Contains(out, leakSentinel) {
		t.Errorf("REDACTION FAILURE via log: %q", out)
	}
}

// TestToken_RevealReturnsRaw confirms the one escape hatch works — reveal()
// returns the underlying value, and is the ONLY way to access it.
func TestToken_RevealReturnsRaw(t *testing.T) {
	t.Parallel()
	tok := NewToken("plain-value")
	if tok.reveal() != "plain-value" {
		t.Errorf("reveal returned %q", tok.reveal())
	}
}

// TestToken_ZeroValue verifies IsZero behavior for an uninitialized Token.
func TestToken_ZeroValue(t *testing.T) {
	t.Parallel()
	var tok Token
	if !tok.IsZero() {
		t.Error("zero Token should be IsZero=true")
	}
	nonZero := NewToken("x")
	if nonZero.IsZero() {
		t.Error("non-zero Token should be IsZero=false")
	}
}
