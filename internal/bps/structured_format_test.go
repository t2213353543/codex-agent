package bps

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func structuredTestFormat(t *testing.T, schema string, strict bool) *StructuredFormat {
	t.Helper()
	text, err := json.Marshal(map[string]any{"format": map[string]any{"type": "json_schema", "name": "fixture", "strict": strict, "schema": json.RawMessage(schema)}})
	if err != nil {
		t.Fatal(err)
	}
	format, err := ParseStructuredFormat(text)
	if err != nil {
		t.Fatal(err)
	}
	return format
}

func TestStructuredFormatClearingAndPlainText(t *testing.T) {
	for _, text := range []string{"", "null", `{}`, `{"format":null}`, `{"format":{"type":"text"}}`, `{"verbosity":"high","format":{"type":"text"}}`} {
		t.Run(text, func(t *testing.T) {
			got, err := ParseStructuredFormat([]byte(text))
			if err != nil || got != nil {
				t.Fatalf("format=%v error=%v", got, err)
			}
		})
	}
}

func TestStructuredFormatRejectsInvalidControlsPrivately(t *testing.T) {
	for name, text := range map[string]string{
		"array text": `[]`, "duplicate escaped control": `{"format":{},"\u0066ormat":{}}`,
		"array format": `{"format":[]}`, "no kind": `{"format":{}}`, "unknown kind": `{"format":{"type":"PRIVATE_UNKNOWN"}}`,
		"unknown control":        `{"format":{"type":"json_object","PRIVATE_EXTRA":true}}`,
		"missing name":           `{"format":{"type":"json_schema","schema":{"type":"object"}}}`,
		"bad name":               `{"format":{"type":"json_schema","name":"PRIVATE INVALID","schema":{"type":"object"}}}`,
		"null strict":            `{"format":{"type":"json_schema","name":"fixture","strict":null,"schema":{"type":"object"}}}`,
		"string strict":          `{"format":{"type":"json_schema","name":"fixture","strict":"true","schema":{"type":"object"}}}`,
		"null description":       `{"format":{"type":"json_schema","name":"fixture","description":null,"schema":{"type":"object"}}}`,
		"no schema":              `{"format":{"type":"json_schema","name":"fixture"}}`,
		"array schema":           `{"format":{"type":"json_schema","name":"fixture","schema":{"type":"array"}}}`,
		"implicit root":          `{"format":{"type":"json_schema","name":"fixture","schema":{"properties":{}}}}`,
		"duplicate schema field": `{"format":{"type":"json_schema","name":"fixture","schema":{"type":"object","type":"array"}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			format, err := ParseStructuredFormat([]byte(text))
			var parameter *ParameterError
			if format != nil || !errors.As(err, &parameter) || !strings.HasPrefix(parameter.Param, "text") {
				t.Fatalf("expected parameter error, got %v", err)
			}
			if strings.Contains(err.Error(), "PRIVATE") {
				t.Fatal("private input leaked into parameter error")
			}
		})
	}
}

func TestStructuredFormatNestedSchemaValidation(t *testing.T) {
	schema := `{"type":"object","properties":{"answer":{"type":"string","enum":["SCHEMA_OK"]},"details":{"type":"object","properties":{"values":{"type":"array","items":{"anyOf":[{"type":"integer","minimum":5},{"type":"string","enum":["fallback"]}]},"minItems":1,"maxItems":3}},"required":["values"],"additionalProperties":false}},"required":["answer","details"],"additionalProperties":false}`
	format := structuredTestFormat(t, schema, true)
	valid := ` { "answer": "SCHEMA_OK", "details": {"values":[5,"fallback"]} } `
	if got, err := format.ValidateArguments(valid); err != nil || got != valid {
		t.Fatalf("valid result was changed or rejected: %v", err)
	}
	for name, input := range map[string]string{
		"enum":              `{"answer":"PROMPT_VALUE","details":{"values":[5]}}`,
		"required":          `{"answer":"SCHEMA_OK"}`,
		"nested required":   `{"answer":"SCHEMA_OK","details":{}}`,
		"additional root":   `{"answer":"SCHEMA_OK","details":{"values":[5]},"extra":true}`,
		"additional nested": `{"answer":"SCHEMA_OK","details":{"values":[5],"extra":true}}`,
		"item anyOf":        `{"answer":"SCHEMA_OK","details":{"values":[4]}}`,
		"array count":       `{"answer":"SCHEMA_OK","details":{"values":[]}}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := format.ValidateArguments(input); err == nil {
				t.Fatal("schema violation accepted")
			}
		})
	}
}

func TestStructuredFormatLocalRefsAndLargeNumbers(t *testing.T) {
	schema := `{ "type":"object", "$defs":{"number":{"type":"integer","minimum":900719925474099312345,"maximum":900719925474099312345}},"properties":{"n":{"$ref":"#/$defs/number"}},"required":["n"],"additionalProperties":false }`
	text := []byte(`{"format":{"type":"json_schema","name":"fixture","strict":true,"description":"Preserve this description.","schema":` + schema + `}}`)
	format, err := ParseStructuredFormat(text)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(format.Schema, []byte(schema)) {
		t.Fatal("schema raw bytes changed")
	}
	valid := ` {"n":900719925474099312345} `
	if got, err := format.ValidateArguments(valid); err != nil || got != valid {
		t.Fatalf("large exact number changed or rejected: %v", err)
	}
	if _, err := format.ValidateArguments(`{"n":900719925474099312346}`); err == nil {
		t.Fatal("large numbers collapsed through float64")
	}
	definition := format.ToolDefinition()
	if !bytes.Contains(definition, []byte("900719925474099312345")) {
		t.Fatal("forwarded schema number changed")
	}
	var tool map[string]json.RawMessage
	_ = json.Unmarshal(definition, &tool)
	if string(tool["strict"]) != "true" || string(tool["description"]) != `"Preserve this description."` {
		t.Fatal("strict or description dropped")
	}
	if !equalJSON(tool["parameters"], format.Schema) {
		t.Fatal("forwarded schema changed")
	}
}

func TestStructuredFormatRecursiveLocalRef(t *testing.T) {
	format := structuredTestFormat(t, `{"type":"object","properties":{"value":{"type":"integer"},"next":{"anyOf":[{"$ref":"#"},{"type":"null"}]}},"required":["value","next"],"additionalProperties":false}`, true)
	if _, err := format.ValidateArguments(`{"value":1,"next":{"value":2,"next":null}}`); err != nil {
		t.Fatal(err)
	}
	if _, err := format.ValidateArguments(`{"value":1,"next":{"value":"2","next":null}}`); err == nil {
		t.Fatal("recursive violation accepted")
	}
}

func TestStructuredFormatRejectsUnsupportedAndInvalidSchemas(t *testing.T) {
	for name, schema := range map[string]string{
		"bogus type":          `{"type":"object","properties":{"v":{"type":"PRIVATE_BOGUS"}}}`,
		"wrong required":      `{"type":"object","required":"PRIVATE_REQUIRED"}`,
		"bad enum":            `{"type":"object","properties":{"v":{"enum":"PRIVATE_ENUM"}}}`,
		"bad local ref":       `{"type":"object","properties":{"v":{"$ref":"#/$defs/missing"}}}`,
		"unknown assertion":   `{"type":"object","PRIVATE_KEYWORD":true}`,
		"unknown format":      `{"type":"object","properties":{"v":{"type":"string","format":"PRIVATE_FORMAT"}}}`,
		"unimplemented regex": `{"type":"object","properties":{"v":{"type":"string","pattern":"(?=PRIVATE_LOOKAHEAD)"}}}`,
		"custom dialect":      `{"type":"object","$schema":"https://PRIVATE.example/schema"}`,
		"custom vocabulary":   `{"type":"object","$vocabulary":{"https://PRIVATE.example/vocab":true}}`,
		"content assertion":   `{"type":"object","contentEncoding":"base64"}`,
	} {
		t.Run(name, func(t *testing.T) {
			text, _ := json.Marshal(map[string]any{"format": map[string]any{"type": "json_schema", "name": "fixture", "schema": json.RawMessage(schema)}})
			f, err := ParseStructuredFormat(text)
			var parameter *ParameterError
			if f != nil || !errors.As(err, &parameter) || parameter.Param != "text.format.schema" {
				t.Fatalf("wanted schema error; got %v", err)
			}
			if strings.Contains(err.Error(), "PRIVATE") {
				t.Fatal("private schema leaked")
			}
		})
	}
}

func TestStructuredFormatNoExternalSchemaLoads(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		_, _ = w.Write([]byte(`{"type":"integer"}`))
	}))
	defer server.Close()
	for _, ref := range []string{server.URL + "/schema", "file:///private/schema.json", "relative.json", "//private.example/schema", "urn:private:schema"} {
		schema := map[string]any{"type": "object", "properties": map[string]any{"v": map[string]string{"$ref": ref}}}
		text, _ := json.Marshal(map[string]any{"format": map[string]any{"type": "json_schema", "name": "fixture", "schema": schema}})
		if _, err := ParseStructuredFormat(text); err == nil {
			t.Fatal("external reference accepted")
		}
	}
	if requests.Load() != 0 {
		t.Fatal("schema compilation performed HTTP")
	}
	if _, err := (structuredNoLoader{}).Load(server.URL); err == nil {
		t.Fatal("loader did not reject")
	}
	if _, err := (structuredNoLoader{}).Load("file:///private/schema.json"); err == nil {
		t.Fatal("file loader did not reject")
	}
}

func TestStructuredFormatConstDataIsNotSchema(t *testing.T) {
	format := structuredTestFormat(t, `{"type":"object","properties":{"data":{"const":{"$ref":"https://example.invalid/private"}}},"required":["data"],"additionalProperties":false}`, true)
	if _, err := format.ValidateArguments(`{"data":{"$ref":"https://example.invalid/private"}}`); err != nil {
		t.Fatal(err)
	}
}

func TestStructuredFormatArgumentsMustBeUnambiguousObjects(t *testing.T) {
	format, err := ParseStructuredFormat([]byte(`{"format":{"type":"json_object"}}`))
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{
		"trailing value": `{} {}`, "trailing garbage": `{} PRIVATE`, "array": `[]`, "null": `null`, "number": `12`, "duplicate": `{"a":1,"a":2}`, "escaped duplicate": `{"a":1,"\u0061":2}`, "nested duplicate": `{"a":[{"b":1,"b":2}]}`, "truncated": `{"a":`, "invalid utf8": "{\"a\":\"\xff\"}",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := format.ValidateArguments(value); err == nil {
				t.Fatal("invalid output accepted")
			}
		})
	}
	valid := ` {"any_field":{"free":[1,true,null,"text"]},"n":900719925474099312345} `
	if got, err := format.ValidateArguments(valid); err != nil || got != valid {
		t.Fatalf("free object rejected or changed: %v", err)
	}
	var definition map[string]json.RawMessage
	_ = json.Unmarshal(format.ToolDefinition(), &definition)
	if string(definition["strict"]) != "false" {
		t.Fatal("json_object must explicitly disable strict tool parsing")
	}
}

func TestStructuredFormatNonStrictPreservesSchemaWithoutClaimingEnforcement(t *testing.T) {
	schema := `{"type":"object","properties":{"answer":{"enum":["SCHEMA_OK"]}},"required":["answer"],"additionalProperties":false}`
	format := structuredTestFormat(t, schema, false)
	input := `{"answer":"PROMPT_VALUE","extra":true}`
	if got, err := format.ValidateArguments(input); err != nil || got != input {
		t.Fatal("strict=false changed to strict enforcement")
	}
	var definition map[string]json.RawMessage
	_ = json.Unmarshal(format.ToolDefinition(), &definition)
	if string(definition["strict"]) != "false" {
		t.Fatal("strict=false must be explicit to prevent upstream default strictification")
	}
	if !equalJSON(definition["parameters"], []byte(schema)) {
		t.Fatal("schema constraints were discarded")
	}
}

func TestStructuredFormatStableAndDistinctToolNames(t *testing.T) {
	a := structuredTestFormat(t, `{"type":"object","properties":{"v":{"type":"string","enum":["A"]}},"required":["v"]}`, true)
	b := structuredTestFormat(t, ` { "required":["v"], "properties":{"v":{"enum":["A"],"type":"string"}}, "type":"object" } `, true)
	c := structuredTestFormat(t, `{"type":"object","properties":{"v":{"type":"string","enum":["B"]}},"required":["v"]}`, true)
	d := structuredTestFormat(t, `{"type":"object","properties":{"v":{"type":"string","enum":["A"]}},"required":["v"]}`, false)
	if a.ToolName != b.ToolName || a.ToolName == c.ToolName || a.ToolName == d.ToolName {
		t.Fatal("tool identity is unstable or fails to distinguish changed semantics")
	}
	if !structuredFormatName.MatchString(a.ToolName) {
		t.Fatal("generated tool name violates upstream name limits")
	}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Go(func() {
			if got, err := a.ValidateArguments(`{"v":"A"}`); err != nil || got != `{"v":"A"}` {
				t.Error("concurrent validator use failed")
			}
		})
	}
	wg.Wait()
}

func TestStructuredFormatStringFormatIsAsserted(t *testing.T) {
	format := structuredTestFormat(t, `{"type":"object","properties":{"date":{"type":"string","format":"date"}},"required":["date"]}`, true)
	if _, err := format.ValidateArguments(`{"date":"2026-09-28"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := format.ValidateArguments(`{"date":"PRIVATE_NOT_A_DATE"}`); err == nil {
		t.Fatal("format assertion was silently ignored")
	}
}

func TestStructuredFormatPublicTextPreservesOriginalAndOwnsCopies(t *testing.T) {
	text := []byte(` { "verbosity":"high", "format":{"type":"json_schema","name":"public_name","description":"Original description.","strict":true,"schema":{"type":"object","properties":{"n":{"type":"integer","const":900719925474099312345}},"required":["n"],"additionalProperties":false}} } `)
	original := bytes.Clone(text)
	format, err := ParseStructuredFormat(text)
	if err != nil {
		t.Fatal(err)
	}
	text[0] = '\n'
	first := format.PublicText()
	if !bytes.Equal(first, original) {
		t.Fatal("original text controls or exact schema bytes changed")
	}
	first[0] = '\t'
	if !bytes.Equal(format.PublicText(), original) {
		t.Fatal("public text exposed mutable internal storage")
	}
	if bytes.Contains(format.PublicText(), []byte(format.ToolName)) {
		t.Fatal("private formatting tool leaked into public text")
	}
	objectText := []byte(` {"format": {"type": "json_object"}, "verbosity":"low"} `)
	object, err := ParseStructuredFormat(objectText)
	if err != nil || !bytes.Equal(object.PublicText(), objectText) {
		t.Fatal("json_object text configuration changed")
	}
	if (*StructuredFormat)(nil).PublicText() != nil {
		t.Fatal("nil receiver must return nil")
	}
}
