// SPDX-License-Identifier: MIT
//
// Token is a redacting wrapper around the YNAB personal access token. It
// exists to make it impossible (outside this package) to accidentally
// serialize, log, or format the raw value. Every standard formatting path
// (fmt.Stringer, fmt.GoStringer, fmt.Formatter, json.Marshaler) returns
// "[REDACTED]". JSON unmarshaling is explicitly refused so an attacker who
// gets a Token field from an untrusted payload cannot inject one.
//
// The raw token is accessible only via the package-private reveal() method,
// which is called in exactly ONE place in this codebase: when setting the
// Authorization header on an outbound YNAB HTTP request in client.go.
// Any new caller of reveal() is a security change that requires explicit
// review.

package main

import (
	"errors"
	"fmt"
	"io"
)

// redactedPlaceholder is what every redacting path returns. Intentionally
// distinctive so a grep of logs or JSON dumps makes leak audits trivial.
const redactedPlaceholder = "[REDACTED]"

// Token wraps a YNAB personal access token in a type whose standard
// formatting and serialization paths all emit a placeholder.
type Token struct {
	raw string
}

// NewToken constructs a Token from a raw string. The string is stored as-is
// (no trimming — loadToken handles that).
func NewToken(raw string) Token { return Token{raw: raw} }

// IsZero reports whether the token is the empty value.
func (t Token) IsZero() bool { return t.raw == "" }

// String implements fmt.Stringer.
func (t Token) String() string { return redactedPlaceholder }

// GoString implements fmt.GoStringer, which is what %#v uses.
func (t Token) GoString() string { return redactedPlaceholder }

// Format implements fmt.Formatter, catching every verb (%v, %+v, %s, %q, %d,
// etc.) so no fmt path can bypass redaction by picking an unusual verb.
func (t Token) Format(f fmt.State, _ rune) {
	_, _ = io.WriteString(f, redactedPlaceholder)
}

// MarshalJSON always returns the placeholder, never the underlying value.
func (t Token) MarshalJSON() ([]byte, error) {
	return []byte(`"` + redactedPlaceholder + `"`), nil
}

// UnmarshalJSON refuses. A Token should only ever be constructed from a
// trusted source via NewToken — never from untrusted JSON input.
func (t *Token) UnmarshalJSON([]byte) error {
	return errors.New("token: refusing to unmarshal from untrusted JSON")
}

// MarshalText, like MarshalJSON, returns only the placeholder. Implementing
// this prevents libraries that try encoding.TextMarshaler (e.g. slog, some
// logging frameworks) from reaching the raw value.
func (t Token) MarshalText() ([]byte, error) {
	return []byte(redactedPlaceholder), nil
}

// UnmarshalText refuses for the same reason as UnmarshalJSON.
func (t *Token) UnmarshalText([]byte) error {
	return errors.New("token: refusing to unmarshal from untrusted text")
}

// reveal returns the raw token string. It is package-private BY DESIGN:
// every call site is a potential leak vector and must be reviewed.
//
// As of this commit, reveal() is called from exactly one place:
// client.go:(*Client).doJSON, to set the Authorization header.
func (t Token) reveal() string { return t.raw }
