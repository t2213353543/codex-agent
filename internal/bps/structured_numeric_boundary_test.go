package bps

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

func structuredBoundaryFormat(t *testing.T, schema string) (*StructuredFormat, error) {
	t.Helper()
	return ParseStructuredFormat(json.RawMessage(`{"format":{"type":"json_schema","name":"review","strict":true,"schema":` + schema + `}}`))
}

func TestStructuredBoundaryValidationPanicIsPrivateFailure(t *testing.T) {
	format := structuredTestFormat(t, `{"type":"object","properties":{"value":{"type":"string"}},"required":["value"]}`, true)
	// Exercise the actual dependency callback boundary. No package-global test
	// hook or production replacement validator is needed to induce the failure.
	format.validator.Properties["value"].Format = &jsonschema.Format{
		Name:     "private-fixture",
		Validate: func(any) error { panic("PRIVATE_SCHEMA_AND_VALUE") },
	}
	result, err := format.ValidateArguments(`{"value":"PRIVATE_OUTPUT"}`)
	if err == nil || result != "" || strings.Contains(err.Error(), "PRIVATE") {
		t.Fatal("dependency panic escaped, returned output, or disclosed private content")
	}
}

func TestStructuredBoundaryExtremeSchemaRejectedPrivately(t *testing.T) {
	for _, exponent := range []string{"1e9223372036854775808", "-1e9223372036854775808", "1e-9223372036854775808"} {
		text := json.RawMessage(`{"format":{"type":"json_schema","name":"fixture","strict":true,"schema":{"type":"object","description":"PRIVATE_SCHEMA","properties":{"n":{"minimum":` + exponent + `}}}}}`)
		format, err := ParseStructuredFormat(text)
		var parameter *ParameterError
		if format != nil || !errors.As(err, &parameter) || strings.Contains(err.Error(), "PRIVATE") || strings.Contains(err.Error(), exponent) {
			t.Fatal("extreme schema was accepted or exposed private constraint contents")
		}
	}
}

func TestStructuredBoundaryUnicodeIntegrity(t *testing.T) {
	t.Run("schema_surrogate", func(t *testing.T) {
		_, err := structuredBoundaryFormat(t, `{"type":"object","properties":{"value":{"type":"string","enum":["\ud800"]}},"required":["value"],"additionalProperties":false}`)
		if err == nil {
			t.Fatal("schema with lone surrogate accepted after decoder replacement")
		}
	})
	t.Run("output_surrogate", func(t *testing.T) {
		f, err := structuredBoundaryFormat(t, `{"type":"object","properties":{"value":{"type":"string"}},"required":["value"],"additionalProperties":false}`)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.ValidateArguments(`{"value":"\ud800"}`); err == nil {
			t.Fatal("output with lone surrogate validated as replacement character")
		}
	})
}

func TestStructuredBoundaryNumericOutputNoPanic(t *testing.T) {
	f, err := structuredBoundaryFormat(t, `{"type":"object","properties":{"n":{"type":"number","minimum":0}},"required":["n"],"additionalProperties":false}`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if p := recover(); p != nil {
			t.Errorf("runtime schema validation panicked: %v", p)
		}
	}()
	if _, err := f.ValidateArguments(`{"n":1e9223372036854775808}`); err == nil {
		t.Fatal("numeric output outside validator range accepted")
	}
}

func TestStructuredBoundaryNumericSchemaNoDroppedBound(t *testing.T) {
	defer func() {
		if p := recover(); p != nil {
			t.Errorf("runtime schema compilation panicked: %v", p)
		}
	}()
	f, err := structuredBoundaryFormat(t, `{"type":"object","properties":{"n":{"type":"number","minimum":1e9223372036854775808}},"required":["n"],"additionalProperties":false}`)
	if err != nil {
		return
	}
	if _, err := f.ValidateArguments(`{"n":1}`); err == nil {
		t.Fatal("huge minimum silently disappeared")
	}
}

func TestStructuredBoundaryLengthBoundsNoIntegerWrap(t *testing.T) {
	for _, tc := range []struct {
		name, schema, valid string
		mustReject          bool
	}{
		{"minLength", `{"type":"string","minLength":18446744073709551616}`, `""`, true},
		{"minItems", `{"type":"array","minItems":18446744073709551616}`, `[]`, true},
		{"minProperties", `{"type":"object","minProperties":18446744073709551616}`, `{}`, true},
		{"maxLength", `{"type":"string","maxLength":9223372036854775808}`, `"a"`, false},
		{"maxItems", `{"type":"array","maxItems":9223372036854775808}`, `[1]`, false},
		{"maxProperties", `{"type":"object","maxProperties":9223372036854775808}`, `{"a":1}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, err := structuredBoundaryFormat(t, `{"type":"object","properties":{"value":`+tc.schema+`},"required":["value"],"additionalProperties":false}`)
			if err != nil {
				return
			}
			_, err = f.ValidateArguments(`{"value":` + tc.valid + `}`)
			if tc.mustReject && err == nil {
				t.Fatal("minimum bound wrapped and accepted invalid output")
			}
			if !tc.mustReject && err != nil {
				t.Fatal("maximum bound wrapped and rejected valid output")
			}
		})
	}
}

func TestStructuredBoundarySupportedPrecisionAndUnicodeRemainUsable(t *testing.T) {
	for _, tc := range []struct{ name, schema, output string }{
		{"exact_integer", `{"type":"integer","const":9007199254740993}`, `9007199254740993`},
		{"large_exact_decimal", `{"type":"number","minimum":1e400,"maximum":2e400}`, `1.5e400`},
		{"small_exact_decimal", `{"type":"number","multipleOf":1e-400}`, `3e-400`},
		{"large_valid_cardinality", `{"type":"string","maxLength":1152921504606846976}`, `"a"`},
		{"surrogate_pair", `{"type":"string"}`, `"\ud83d\ude00"`},
		{"escaped_literal", `{"type":"string"}`, `"\\ud800"`},
		{"replacement_character", `{"type":"string"}`, `"\ufffd"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, err := structuredBoundaryFormat(t, `{"type":"object","properties":{"value":`+tc.schema+`},"required":["value"],"additionalProperties":false}`)
			if err != nil {
				t.Fatalf("supported schema rejected: %v", err)
			}
			input := `{"value":` + tc.output + `}`
			output, err := f.ValidateArguments(input)
			if err != nil || output != input {
				t.Fatalf("exact supported value changed or rejected: %s %v", output, err)
			}
		})
	}
}
