package schema

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"
)

// Numbers, written so that where one is read back from cannot change what it is.
//
// "A number written without a fraction or an exponent is an int in an expression, and any other
// a double." A value keeps the way it was written by being a json.Number, which an expression
// takes as an int where it parses as one and as a double otherwise. What is left is to make that
// spelling survive being stored: PostgreSQL keeps a number in jsonb at the scale it was written
// with, so 1.0 comes back 1.0, but it writes an exponent out, so 1e3 comes back 1000, which is an
// int. Canonical writes such a number out first, as the same decimal with a point, so that a run
// read back from the database sees the double agk run --local saw.

// Canonical returns v with every number written with an exponent written out in full with a
// point, 1e3 as 1000.0 and 15e-1 as 1.5, and every other value as it was. The digits are moved
// rather than recomputed, so the number is the one that was written, to its last digit, and
// validates as it did. v is a decoded JSON value, read with UseNumber; a map or a list is copied
// rather than written into.
func Canonical(v any) any {
	switch v := v.(type) {
	case json.Number:
		return canonicalNumber(v)
	case map[string]any:
		c := make(map[string]any, len(v))
		for k, e := range v {
			c[k] = Canonical(e)
		}
		return c
	case []any:
		c := make([]any, len(v))
		for i, e := range v {
			c[i] = Canonical(e)
		}
		return c
	default:
		return v
	}
}

// Double is f as a number an expression takes as a double however whole it is: 1 is written 1.0.
// It is how a number that arrived as a float64, from a YAML loader, is put on the same terms as
// one that was read as written. Infinity and NaN, which JSON cannot write, are left as
// strconv writes them, to be refused by whatever writes them down.
func Double(f float64) json.Number {
	written := strconv.FormatFloat(f, 'g', -1, 64)
	if math.IsInf(f, 0) || math.IsNaN(f) {
		return json.Number(written)
	}
	if !strings.ContainsAny(written, ".eE") {
		written += ".0"
	}
	return canonicalNumber(json.Number(written))
}

// canonicalNumber writes out the exponent of one number, leaving a number with none as it is.
// Anything that is not the JSON grammar of a number is left alone too: it is not this function's
// to refuse.
func canonicalNumber(n json.Number) json.Number {
	written := string(n)
	e := strings.IndexAny(written, "eE")
	if e < 0 {
		return n
	}
	mantissa, exponent := written[:e], written[e+1:]
	shift, err := strconv.Atoi(exponent)
	if err != nil || shift > 1<<16 || shift < -(1<<16) {
		// Past what any number read here may hold: the API refuses an exponent past a few
		// hundred, and a float64 past about 308.
		return n
	}
	sign := ""
	if strings.HasPrefix(mantissa, "-") {
		sign, mantissa = "-", mantissa[1:]
	}
	whole, fraction, _ := strings.Cut(mantissa, ".")
	digits := whole + fraction
	if digits == "" || strings.Trim(digits, "0123456789") != "" {
		return n
	}
	point := len(whole) + shift
	switch {
	case point <= 0:
		digits = strings.Repeat("0", 1-point) + digits
		point = 1
	case point > len(digits):
		digits += strings.Repeat("0", point-len(digits))
	}
	whole, fraction = strings.TrimLeft(digits[:point], "0"), digits[point:]
	if whole == "" {
		whole = "0"
	}
	if fraction == "" {
		fraction = "0"
	}
	return json.Number(sign + whole + "." + fraction)
}
