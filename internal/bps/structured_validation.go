package bps

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"strconv"
	"strings"
	"unicode/utf8"
)

// UseLoader is set before compilation so even an overlooked reference can
// never read a local file or initiate a network request. Standard metaschemas
// are embedded by the pinned validator library.
type structuredNoLoader struct{}

func (structuredNoLoader) Load(string) (any, error) {
	return nil, errors.New("external schema loading is disabled")
}

// structuredJSON rejects duplicate decoded object names at every depth. The
// standard decoder alone accepts ambiguous duplicates with last-value wins.
func structuredJSON(raw []byte) (any, error) {
	if !utf8.Valid(raw) || !json.Valid(raw) {
		return nil, errors.New("invalid JSON")
	}
	if err := structuredUnicodeEscapes(raw); err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var read func() (any, error)
	read = func() (any, error) {
		token, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		if number, ok := token.(json.Number); ok {
			// The pinned validator uses big.Rat but does not always check the
			// conversion result. Keep the original spelling while preventing
			// unrepresentable values from dropping bounds or causing a panic.
			if _, valid := new(big.Rat).SetString(number.String()); !valid {
				return nil, errors.New("JSON number is outside the supported exact range")
			}
		}
		delim, ok := token.(json.Delim)
		if !ok {
			return token, nil
		}
		switch delim {
		case '{':
			object := map[string]any{}
			for decoder.More() {
				keyToken, err := decoder.Token()
				if err != nil {
					return nil, err
				}
				key, ok := keyToken.(string)
				if !ok {
					return nil, errors.New("invalid object member")
				}
				if _, exists := object[key]; exists {
					return nil, errors.New("duplicate object member")
				}
				value, err := read()
				if err != nil {
					return nil, err
				}
				object[key] = value
			}
			if close, err := decoder.Token(); err != nil || close != json.Delim('}') {
				return nil, errors.New("invalid object")
			}
			return object, nil
		case '[':
			array := []any{}
			for decoder.More() {
				value, err := read()
				if err != nil {
					return nil, err
				}
				array = append(array, value)
			}
			if close, err := decoder.Token(); err != nil || close != json.Delim(']') {
				return nil, errors.New("invalid array")
			}
			return array, nil
		default:
			return nil, errors.New("unexpected JSON delimiter")
		}
	}
	value, err := read()
	if err != nil {
		return nil, err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("trailing JSON content")
	}
	return value, nil
}

// encoding/json replaces unpaired UTF-16 escapes with U+FFFD. Check the raw
// representation first, because validation must describe the bytes delivered
// to the client, not a different string manufactured by the JSON decoder.
func structuredUnicodeEscapes(raw []byte) error {
	inside := false
	for i := 0; i < len(raw); i++ {
		if raw[i] == '"' {
			inside = !inside
			continue
		}
		if !inside || raw[i] != '\\' {
			continue
		}
		i++
		if i >= len(raw) || raw[i] != 'u' {
			continue
		}
		if i+4 >= len(raw) {
			return errors.New("incomplete Unicode escape")
		}
		code, err := strconv.ParseUint(string(raw[i+1:i+5]), 16, 16)
		if err != nil {
			return errors.New("invalid Unicode escape")
		}
		i += 4
		if code >= 0xdc00 && code <= 0xdfff {
			return errors.New("unpaired Unicode surrogate")
		}
		if code >= 0xd800 && code <= 0xdbff {
			if i+6 >= len(raw) || raw[i+1] != '\\' || raw[i+2] != 'u' {
				return errors.New("unpaired Unicode surrogate")
			}
			low, err := strconv.ParseUint(string(raw[i+3:i+7]), 16, 16)
			if err != nil || low < 0xdc00 || low > 0xdfff {
				return errors.New("unpaired Unicode surrogate")
			}
			i += 6
		}
	}
	return nil
}

// Validate schema positions rather than every object: values under const,
// default and examples are data and may legitimately contain a '$ref' key.
func structuredSchemaSupported(value any) error {
	if _, ok := value.(bool); ok {
		return nil
	}
	schema, ok := value.(map[string]any)
	if !ok {
		return unsupportedParameter("text.format.schema", "schema nodes must be objects or booleans")
	}
	for keyword, value := range schema {
		switch keyword {
		case "$schema":
			if value != "https://json-schema.org/draft/2020-12/schema" && value != "https://json-schema.org/draft/2020-12/schema#" {
				return unsupportedParameter("text.format.schema", "only the JSON Schema 2020-12 dialect is supported")
			}
		case "$ref", "$dynamicRef":
			ref, ok := value.(string)
			if !ok || !strings.HasPrefix(ref, "#") {
				return unsupportedParameter("text.format.schema", "schema references must remain within the supplied document")
			}
		case "$defs", "definitions", "properties", "patternProperties", "dependentSchemas":
			children, ok := value.(map[string]any)
			if !ok {
				return unsupportedParameter("text.format.schema", "schema child declarations must be objects")
			}
			for _, child := range children {
				if err := structuredSchemaSupported(child); err != nil {
					return err
				}
			}
		case "additionalProperties", "unevaluatedProperties", "propertyNames", "items", "contains", "unevaluatedItems", "not", "if", "then", "else":
			if err := structuredSchemaSupported(value); err != nil {
				return err
			}
		case "allOf", "anyOf", "oneOf", "prefixItems":
			children, ok := value.([]any)
			if !ok {
				return unsupportedParameter("text.format.schema", "schema alternatives must be arrays")
			}
			for _, child := range children {
				if err := structuredSchemaSupported(child); err != nil {
					return err
				}
			}
		case "format":
			format, ok := value.(string)
			if !ok || !structuredKnownFormat(format) {
				return unsupportedParameter("text.format.schema", "schema string format is not supported")
			}
		case "minLength", "maxLength", "minItems", "maxItems", "minProperties", "maxProperties", "minContains", "maxContains":
			// These bounds are stored as int by the pinned compiler. Its
			// metaschema accepts larger integers, whose conversion would wrap.
			number, ok := value.(json.Number)
			if !ok {
				return unsupportedParameter("text.format.schema", "schema count constraints must be supported nonnegative integers")
			}
			count, valid := new(big.Rat).SetString(number.String())
			if !valid || !count.IsInt() || count.Sign() < 0 || count.Num().BitLen() >= strconv.IntSize {
				return unsupportedParameter("text.format.schema", "schema count constraints must be supported nonnegative integers")
			}
		case "$id", "$anchor", "$dynamicAnchor", "$comment", "title", "description", "default", "examples", "deprecated", "readOnly", "writeOnly",
			"type", "enum", "const", "required", "dependentRequired",
			"minimum", "maximum", "exclusiveMinimum", "exclusiveMaximum", "multipleOf",
			"pattern", "uniqueItems":
			// The pinned compiler's embedded metaschema validates these values.
		default:
			// Unknown keywords would otherwise be silently treated as annotations.
			// Never claim strict validation of a constraint we did not evaluate.
			return unsupportedParameter("text.format.schema", "schema contains an unsupported keyword")
		}
	}
	return nil
}

func structuredKnownFormat(value string) bool {
	switch value {
	case "json-pointer", "relative-json-pointer", "uuid", "duration", "period", "ipv4", "ipv6", "hostname", "email", "date", "time", "date-time", "uri", "iri", "uri-reference", "iri-reference", "uri-template", "semver", "regex":
		return true
	default:
		return false
	}
}
