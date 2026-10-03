package bps

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// StructuredFormat is immutable after parsing. Schema retains the original
// JSON bytes; only the private tool identity uses a canonical representation.
type StructuredFormat struct {
	Kind     string
	Strict   bool
	Name     string
	ToolName string
	Schema   json.RawMessage

	description string
	validator   *jsonschema.Schema
	textConfig  json.RawMessage
}

var structuredFormatName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// ParseStructuredFormat validates the format without dropping unsupported
// semantics. Other text controls are handled by the ordinary request parser.
func ParseStructuredFormat(text json.RawMessage) (result *StructuredFormat, parseErr error) {
	defer func() {
		if recover() != nil {
			result = nil
			parseErr = unsupportedParameter("text.format.schema", "format schema could not be safely compiled")
		}
	}()
	if len(bytes.TrimSpace(text)) == 0 || bytes.Equal(bytes.TrimSpace(text), []byte("null")) {
		return nil, nil
	}
	value, err := structuredJSON(text)
	if err != nil {
		return nil, unsupportedParameter("text", "text must be a valid JSON object without duplicate members")
	}
	if _, ok := value.(map[string]any); !ok {
		return nil, unsupportedParameter("text", "text must be an object")
	}
	var controls map[string]json.RawMessage
	_ = json.Unmarshal(text, &controls)
	formatRaw := controls["format"]
	if len(formatRaw) == 0 || bytes.Equal(bytes.TrimSpace(formatRaw), []byte("null")) {
		return nil, nil
	}
	var format map[string]json.RawMessage
	if json.Unmarshal(formatRaw, &format) != nil || format == nil {
		return nil, unsupportedParameter("text.format", "text.format must be an object")
	}
	var kind string
	if json.Unmarshal(format["type"], &kind) != nil {
		return nil, unsupportedParameter("text.format.type", "text.format.type must be a supported format name")
	}
	allowed := map[string]bool{"type": true}
	if kind == "json_schema" {
		for _, key := range []string{"name", "description", "strict", "schema"} {
			allowed[key] = true
		}
	}
	for key := range format {
		if !allowed[key] {
			return nil, unsupportedParameter("text.format", "text.format contains an unsupported control")
		}
	}
	if kind == "text" {
		return nil, nil
	}
	if kind != "json_schema" && kind != "json_object" {
		return nil, unsupportedParameter("text.format.type", "requested text format is not supported")
	}
	f := &StructuredFormat{Kind: kind, textConfig: bytes.Clone(text)}
	var schemaValue any
	if kind == "json_object" {
		f.Schema = json.RawMessage(`{"type":"object","properties":{},"additionalProperties":true}`)
		schemaValue, _ = structuredJSON(f.Schema)
	} else {
		if json.Unmarshal(format["name"], &f.Name) != nil || !structuredFormatName.MatchString(f.Name) {
			return nil, unsupportedParameter("text.format.name", "format name must contain 1 to 64 ASCII letters, digits, underscores or hyphens")
		}
		if strict, ok := format["strict"]; ok {
			if !bytes.Equal(bytes.TrimSpace(strict), []byte("true")) && !bytes.Equal(bytes.TrimSpace(strict), []byte("false")) {
				return nil, unsupportedParameter("text.format.strict", "format strict must be a boolean")
			}
			f.Strict = bytes.Equal(bytes.TrimSpace(strict), []byte("true"))
		}
		if description, ok := format["description"]; ok {
			if bytes.Equal(bytes.TrimSpace(description), []byte("null")) || json.Unmarshal(description, &f.description) != nil {
				return nil, unsupportedParameter("text.format.description", "format description must be a string")
			}
		}
		f.Schema = bytes.Clone(format["schema"])
		schemaValue, err = structuredJSON(f.Schema)
		if err != nil {
			return nil, unsupportedParameter("text.format.schema", "format schema must be valid JSON without duplicate members")
		}
		root, ok := schemaValue.(map[string]any)
		if !ok || root["type"] != "object" {
			return nil, unsupportedParameter("text.format.schema", "format schema must explicitly declare an object root")
		}
	}
	if err := structuredSchemaSupported(schemaValue); err != nil {
		return nil, err
	}
	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft2020)
	compiler.UseLoader(structuredNoLoader{})
	compiler.AssertFormat()
	const resource = "urn:codex-agent:structured-format:schema"
	if err := compiler.AddResource(resource, schemaValue); err != nil {
		return nil, unsupportedParameter("text.format.schema", "format schema could not be registered")
	}
	f.validator, err = compiler.Compile(resource)
	if err != nil {
		// Compiler errors can contain private schema values or reference URLs.
		return nil, unsupportedParameter("text.format.schema", "format schema is invalid or uses an unsupported constraint")
	}
	canonical, err := json.Marshal(schemaValue)
	if err != nil {
		return nil, unsupportedParameter("text.format.schema", "format schema could not be encoded")
	}
	identity, _ := json.Marshal([]any{"codex-agent:structured-output:v1", kind, f.Strict, json.RawMessage(canonical), f.description})
	sum := sha256.Sum256(identity)
	f.ToolName = "emit_structured_" + hex.EncodeToString(sum[:20])
	return f, nil
}

// PublicText returns the caller's original text configuration without exposing
// mutable storage or the private formatting tool used by the upstream adapter.
func (f *StructuredFormat) PublicText() json.RawMessage {
	if f == nil {
		return nil
	}
	return bytes.Clone(f.textConfig)
}

// ToolDefinition returns a real function declaration. No schema constraint is
// removed, including under strict=false; only output validation differs.
func (f *StructuredFormat) ToolDefinition() json.RawMessage {
	if f == nil {
		return nil
	}
	definition := map[string]any{"type": "function", "name": f.ToolName, "parameters": f.Schema, "strict": f.Strict}
	if f.description != "" {
		definition["description"] = f.description
	}
	encoded, _ := json.Marshal(definition)
	return encoded
}

// ValidateArguments returns the exact model-produced string on success. It
// never repairs JSON, fills defaults, coerces numbers or rewrites whitespace.
func (f *StructuredFormat) ValidateArguments(arguments string) (result string, validationErr error) {
	defer func() {
		if recover() != nil {
			result = ""
			validationErr = errors.New("structured output could not be safely validated")
		}
	}()
	if f == nil {
		return "", errors.New("structured output format is unavailable")
	}
	value, err := structuredJSON([]byte(arguments))
	if err != nil {
		return "", errors.New("structured output is not valid unambiguous JSON")
	}
	if _, ok := value.(map[string]any); !ok {
		return "", errors.New("structured output must be a JSON object")
	}
	if f.Strict {
		if f.validator == nil || f.validator.Validate(value) != nil {
			return "", errors.New("structured output does not satisfy the requested schema")
		}
	}
	return arguments, nil
}
