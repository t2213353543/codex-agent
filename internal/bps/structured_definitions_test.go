package bps

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"
)

func schemaReviewFormat(t *testing.T,schema string)*StructuredFormat{
	t.Helper()
	text:=raw(map[string]any{"format":map[string]any{"type":"json_schema","name":"compatibility","strict":true,"schema":json.RawMessage(schema)}})
	format,err:=ParseStructuredFormat(text)
	if err!=nil{t.Fatalf("valid schema rejected: %v",err)}
	return format
}

func TestSchemaReviewExistingStandardFieldsAreAlreadyEnforced(t *testing.T){
	schema:=`{"$schema":"https://json-schema.org/draft/2020-12/schema","$comment":"synthetic annotation","title":"Example","description":"Existing field verification","type":"object","properties":{"name":{"type":"string","default":"fallback"},"extra":{"type":"boolean"}},"propertyNames":{"pattern":"^[a-z]+$"},"dependentRequired":{"name":["extra"]},"minProperties":1,"maxProperties":2,"default":{"$ref":"this is data, not schema"},"examples":[{"name":"example","extra":true}]}`
	format:=schemaReviewFormat(t,schema)
	for _,value:=range []string{`{"extra":false}`,`{"name":"OK","extra":true}`} {if actual,err:=format.ValidateArguments(value);err!=nil||actual!=value{t.Fatal("valid instance/default false changed")}}
	for _,value:=range []string{`{}`,`{"name":"OK"}`,`{"bad-name":true}`,`{"a":1,"b":2,"c":3}`} {if _,err:=format.ValidateArguments(value);err==nil{t.Fatalf("existing constraint ignored: %s",value)}}
}

// This test is intentionally red against the current canonical whitelist,
// and green only after the narrow private definitions-container patch.
func TestSchemaReviewDefinitionsCompatibilityRetainsReferenceAssertions(t *testing.T){
	schema:=`{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","definitions":{"Answer":{"type":"object","properties":{"n":{"type":"integer","minimum":9007199254740993}},"required":["n"],"additionalProperties":false}},"properties":{"answer":{"$ref":"#/definitions/Answer"}},"required":["answer"],"additionalProperties":false}`
	format:=schemaReviewFormat(t,schema)
	valid:=` {"answer":{"n":9007199254740993}} `
	if got,err:=format.ValidateArguments(valid);err!=nil||got!=valid{t.Fatal("exact referenced number rejected or rewritten")}
	for _,value:=range []string{`{"answer":{"n":9007199254740992}}`,`{"answer":{"n":"9007199254740993"}}`,`{"answer":{}}`,`{"answer":{"n":9007199254740993,"extra":true}}`} {if _,err:=format.ValidateArguments(value);err==nil{t.Fatal("definitions local reference constraint silently dropped")}}
	if !equalJSON(format.Schema,[]byte(schema)){t.Fatal("caller schema was rewritten")}
	var definition map[string]json.RawMessage
	if json.Unmarshal(format.ToolDefinition(),&definition)!=nil||!bytes.Contains(definition["parameters"],[]byte(`"definitions"`))||!bytes.Contains(definition["parameters"],[]byte(`"#/definitions/Answer"`)){t.Fatal("upstream function declaration lost original schema/ref")}
}

func TestSchemaReviewDefinitionsChildrenRemainStrictlyChecked(t *testing.T){
	for _,schema:=range []string{
		`{"type":"object","definitions":[]}`,
		`{"type":"object","definitions":{"hidden":{"PRIVATE_UNKNOWN_CONSTRAINT":true}}}`,
		`{"type":"object","definitions":{"hidden":{"$ref":"https://example.invalid/schema"}}}`,
		`{"type":"object","definitions":{"hidden":{"properties":{"n":{"maxLength":18446744073709551616}}}}}`,
		`{"type":"object","definitions":{"hidden":{"type":"PRIVATE_INVALID"}}}`,
	}{
		text:=raw(map[string]any{"format":map[string]any{"type":"json_schema","name":"compatibility","strict":true,"schema":json.RawMessage(schema)}})
		format,err:=ParseStructuredFormat(text);var parameter *ParameterError
		if format!=nil||!errors.As(err,&parameter)||parameter.Param!="text.format.schema"{t.Fatal("invalid or unsupported definition escaped preflight")}
	}
}

func TestSchemaReviewNestedDefinitionsAndDataRefRemainDistinct(t *testing.T){
	schema:=`{"type":"object","definitions":{"Nested":{"type":"object","definitions":{"N":{"type":"integer","minimum":1}},"properties":{"n":{"$ref":"#/definitions/Nested/definitions/N"}},"required":["n"],"additionalProperties":false}},"properties":{"nested":{"$ref":"#/definitions/Nested"},"data":{"const":{"definitions":{"$ref":"literal"}}}},"required":["nested","data"],"additionalProperties":false}`
	format:=schemaReviewFormat(t,schema)
	if _,err:=format.ValidateArguments(`{"nested":{"n":1},"data":{"definitions":{"$ref":"literal"}}}`);err!=nil{t.Fatal(err)}
	if _,err:=format.ValidateArguments(`{"nested":{"n":0},"data":{"definitions":{"$ref":"literal"}}}`);err==nil{t.Fatal("nested definition assertion ignored")}
}
