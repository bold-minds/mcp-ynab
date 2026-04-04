// SPDX-License-Identifier: MIT
//
// Money is the canonical representation of a currency amount in this
// codebase. It stores the authoritative int64 milliunit value and a
// pre-formatted decimal string. No code path uses float64 for currency
// arithmetic or display — this is the "do not use floats anywhere in the
// money path" rule from the security brief.
//
// YNAB's API represents all monetary amounts as integer milliunits (1/1000
// of the smallest currency unit). $12.34 = 12340 milliunits. This preserves
// exactness across currencies with different decimal places and eliminates
// the accumulation error that plagues float-based money code.
//
// Money.Decimal is formatted with 3 fractional digits because that is the
// milliunit precision: the raw value divided by 1000 is always exact to 3
// decimal places in every currency YNAB supports. For USD, JOD, JPY, etc.
// the representation is the same: integer part, dot, three digits.

package main

import "strconv"

// Money represents a currency amount with milliunit precision. The JSON
// output is an object with both the int64 authoritative value and a
// human-readable decimal string. LLMs read Decimal for display; the raw
// int64 is available in Milliunits for exact arithmetic if ever needed.
type Money struct {
	// Milliunits is the authoritative amount, 1/1000 of the smallest
	// currency unit. A USD amount of $12.34 is 12340. Negative for
	// outflows.
	Milliunits int64 `json:"milliunits" jsonschema:"signed amount in milliunits (1/1000 of the currency's smallest unit); authoritative"`
	// Decimal is a human-readable formatted string with exactly 3 decimal
	// places. Exposed for display purposes only.
	Decimal string `json:"decimal" jsonschema:"formatted decimal string with 3 fractional digits (milliunit precision)"`
}

// NewMoney constructs a Money from an int64 milliunit value. The decimal
// field is computed via integer arithmetic — no floating-point.
func NewMoney(milliunits int64) Money {
	return Money{
		Milliunits: milliunits,
		Decimal:    formatMilliunits(milliunits),
	}
}

// formatMilliunits renders an int64 milliunit amount as a decimal string
// with exactly 3 fractional digits, using integer arithmetic only. No
// float64 division is involved at any point.
//
// Examples:
//
//	12340  → "12.340"
//	-5000  → "-5.000"
//	1      → "0.001"
//	0      → "0.000"
func formatMilliunits(m int64) string {
	negative := m < 0
	if negative {
		// Handle math.MinInt64 carefully: negating it overflows. In
		// practice YNAB amounts never come near this, but we do not want
		// to panic. Fall through with the negative sign prepended and
		// work with the absolute value of the un-negated int via uint64.
		if m == -9223372036854775808 { // math.MinInt64
			return "-9223372036854775.808"
		}
		m = -m
	}
	whole := m / 1000
	frac := m % 1000
	// Pad fractional to 3 digits with leading zeros.
	var buf [4]byte
	buf[0] = '.'
	buf[1] = byte('0' + (frac/100)%10)
	buf[2] = byte('0' + (frac/10)%10)
	buf[3] = byte('0' + frac%10)
	out := strconv.FormatInt(whole, 10) + string(buf[:])
	if negative {
		return "-" + out
	}
	return out
}
