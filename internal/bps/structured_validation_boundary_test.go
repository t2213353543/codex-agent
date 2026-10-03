package bps

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"testing"
)

func TestStructuredBoundaryAllCardinalityKeywordsRejectUnsupportedCounts(t *testing.T) {
	for _, keyword := range []string{"minLength", "maxLength", "minItems", "maxItems", "minProperties", "maxProperties", "minContains", "maxContains"} {
		for _, value := range []string{"-1", "0.5", `"1"`, "null", "true", "9223372036854775808", "18446744073709551616"} {
			t.Run(keyword+"/"+value, func(t *testing.T) {
				_, err := structuredBoundaryFormat(t, fmt.Sprintf(`{"type":"object","properties":{"value":{%q:%s}}}`, keyword, value))
				var parameter *ParameterError
				if !errors.As(err, &parameter) || parameter.Param != "text.format.schema" {
					t.Fatalf("unsupported cardinality did not return schema parameter error: %v", err)
				}
			})
		}
	}
}

func TestStructuredBoundaryCardinalityIntegerLimitsAndExactSpellings(t *testing.T) {
	maxInt := strconv.FormatUint(uint64(^uint(0)>>1), 10)
	for _, keyword := range []string{"minLength", "maxLength", "minItems", "maxItems", "minProperties", "maxProperties", "minContains", "maxContains"} {
		for _, value := range []string{"0", "-0", "1.000", "1e0", maxInt} {
			t.Run(keyword+"/"+value, func(t *testing.T) {
				schema := fmt.Sprintf(`{"type":"object","properties":{"value":{%q:%s}}}`, keyword, value)
				format, err := structuredBoundaryFormat(t, schema)
				if err != nil || string(format.Schema) != schema {
					t.Fatalf("supported cardinality rejected or schema rewritten: %v", err)
				}
			})
		}
	}
}

func TestStructuredBoundaryCardinalityNamesInDataRemainData(t *testing.T) {
	const values = `{"minLength":18446744073709551616,"maxLength":18446744073709551616,"minItems":18446744073709551616,"maxItems":18446744073709551616,"minProperties":18446744073709551616,"maxProperties":18446744073709551616,"minContains":18446744073709551616,"maxContains":18446744073709551616}`
	for _, annotation := range []string{"const", "enum", "default", "examples"} {
		t.Run(annotation, func(t *testing.T) {
			value := values
			if annotation == "enum" || annotation == "examples" {
				value = "[" + value + "]"
			}
			schema := fmt.Sprintf(`{"type":"object",%q:%s}`, annotation, value)
			format, err := structuredBoundaryFormat(t, schema)
			if err != nil {
				t.Fatal(err)
			}
			if output, err := format.ValidateArguments(values); err != nil || output != values || string(format.Schema) != schema {
				t.Fatalf("data keyword names were constrained or rewritten: %v", err)
			}
		})
	}
}

func TestStructuredBoundaryContainsBoundsStillAssert(t *testing.T) {
	format, err := structuredBoundaryFormat(t, `{"type":"object","properties":{"a":{"type":"array","contains":{"type":"integer"},"minContains":2,"maxContains":3}},"required":["a"]}`)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		input string
		valid bool
	}{{`{"a":[1]}`, false}, {`{"a":[1,2]}`, true}, {`{"a":[1,2,3]}`, true}, {`{"a":[1,2,3,4]}`, false}} {
		output, err := format.ValidateArguments(tc.input)
		if (err == nil) != tc.valid || err == nil && output != tc.input {
			t.Fatalf("contains bounds changed: input=%s err=%v", tc.input, err)
		}
	}
}

func TestStructuredBoundaryUnicodeLexicalEdgesRemainExact(t *testing.T) {
	for _, raw := range []string{`"\ud800"`, `"\udc00"`, `"\udbff\udfff\udc00"`, `"\ud800\u0041"`, `"\ud800\\udc00"`, `{"\udfff":0}`, `{"a":[{"b":"\ud800"}]}`} {
		if _, err := structuredJSON([]byte(raw)); err == nil {
			t.Fatalf("unpaired surrogate accepted: %s", raw)
		}
	}
	for _, raw := range []string{`"\ud800\udc00"`, `"\uDBFF\uDFFF"`, `"\ud83d\ude00\ud800\udc00"`, `"\\ud800"`, `"\"quoted\""`, `"\\\"\ud83d\ude00"`, `{"\ud83d\ude00":"\ufffd","emoji":"😀"}`} {
		if _, err := structuredJSON([]byte(raw)); err != nil {
			t.Fatalf("valid Unicode rejected: %s: %v", raw, err)
		}
	}
}

func TestStructuredBoundaryAllNumbersRejectedBeforeSchemaUse(t *testing.T) {
	for _, raw := range []string{`1e9223372036854775808`, `[1e9223372036854775808]`, `{"const":1e9223372036854775808}`, `{"examples":[1e9223372036854775808]}`} {
		if _, err := structuredJSON([]byte(raw)); err == nil {
			t.Fatalf("unrepresentable number was decoded: %s", raw)
		}
	}
	for _, raw := range []string{`9007199254740993`, `1e400`, `1e-400`, `-0.000e0`} {
		value, err := structuredJSON([]byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		if value.(json.Number).String() != raw {
			t.Fatal("number spelling changed")
		}
	}
}
