package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// Preserve the previous decoder as an independent semantic/performance oracle.
func routingHintMapReference(prepared []byte) (model, tier string, err error) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(prepared, &fields) != nil || fields == nil || json.Unmarshal(fields["model"], &model) != nil || !gatewayRoutingHintValue(model, 512) {
		return "", "", errGatewayRoutingHint
	}
	if raw, exists := fields["service_tier"]; exists {
		var value *string
		if json.Unmarshal(raw, &value) != nil {
			return "", "", errGatewayRoutingHint
		}
		if value == nil {
			return model, "", nil
		}
		if !gatewayRoutingHintValue(*value, 128) {
			return "", "", errGatewayRoutingHint
		}
		tier = *value
	}
	return model, tier, nil
}

func routingHintAssertReference(t *testing.T, raw []byte) {
	t.Helper()
	before := bytes.Clone(raw)
	wantModel, wantTier, wantErr := routingHintMapReference(raw)
	model, tier, err := gatewayPreparedRoutingHint(raw)
	if model != wantModel || tier != wantTier || (err != nil) != (wantErr != nil) {
		t.Fatalf("routing control result differs: got (%q,%q,%v), want (%q,%q,%v), input bytes=%d", model, tier, err, wantModel, wantTier, wantErr, len(raw))
	}
	if !bytes.Equal(before, raw) {
		t.Fatal("routing inspection mutated request bytes")
	}
}

func TestRoutingHintAllocationExactFieldParity(t *testing.T) {
	for index, raw := range []string{
		`{"model":"m","service_tier":"priority","input":[{"model":"nested","service_tier":"flex"}]}`,
		`{"model":false,"\u006dodel":"m","service_tier":[],"service_\u0074ier":null}`,
		`{"model":"m","Model":null,"Service_Tier":[],"service_tier":"flex"}`,
		`{"model":"m","model":null}`, `{"model":"m","model":false}`,
		`{"model":"m","service_tier":"flex","service_tier":false}`,
		`{"model":"m","service_tier":null}`, `{"model":"m"}`, `{"service_tier":"flex"}`, `{}`,
		`{"model":"m","future":{"broken":true,}}`, `{"model":"m"} false`,
		"\v{\"model\":\"m\"}", "\u00a0{\"model\":\"m\"}",
		" \r\n{\"model\":\"m\",\"service_tier\":\"default\"}\t ",
		`null`, `[]`, `42`, `"m"`, "",
		`{"model":"m","future":"` + string([]byte{0xff}) + `"}`,
		`{"model":"` + string([]byte{0xff}) + `"}`,
		`{"model":"m","future":[` + strings.Repeat("[", 9998) + `0` + strings.Repeat("]", 9998) + `]}`,
		`{"model":"m","future":[` + strings.Repeat("[", 10000) + `0` + strings.Repeat("]", 10000) + `]}`,
	} {
		t.Run(fmt.Sprint(index), func(t *testing.T) { routingHintAssertReference(t, []byte(raw)) })
	}
}

func TestRoutingHintAllocationLargeOpaqueInputPreserved(t *testing.T) {
	for _, count := range []int{1024, 32<<20 - 512} {
		raw := []byte(`{"model":"m","input":[{"type":"reasoning","encrypted_content":"` + strings.Repeat("x", count) + `"}],"service_tier":"priority","unknown":{"large":9007199254740993,"escaped":"\u0061"}}`)
		routingHintAssertReference(t, raw)
	}
}

func FuzzRoutingHintAllocationParity(f *testing.F) {
	for _, raw := range []string{`{"model":"m","service_tier":"priority"}`, `{"model":null,"model":"m","future":[1,{"service_tier":false}]}`, `{"\u006dodel":"m","Service_Tier":{},"service_tier":null}`, "\v{\"model\":\"m\"}"} {
		f.Add([]byte(raw))
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		if len(raw) > 1<<20 {
			t.Skip()
		}
		routingHintAssertReference(t, raw)
	})
}

func BenchmarkRoutingHintOpaqueRequest(b *testing.B) {
	for _, size := range []int{1024, 4 << 20, 32<<20 - 512} {
		raw := []byte(`{"model":"m","input":[{"type":"reasoning","encrypted_content":"` + strings.Repeat("x", size) + `"}],"service_tier":"priority"}`)
		for _, impl := range []struct {
			name string
			fn   func([]byte) (string, string, error)
		}{{"map_reference", routingHintMapReference}, {"current", gatewayPreparedRoutingHint}} {
			b.Run(fmt.Sprintf("%d/%s", len(raw), impl.name), func(b *testing.B) {
				b.ReportAllocs()
				b.SetBytes(int64(len(raw)))
				for b.Loop() {
					model, tier, err := impl.fn(raw)
					if err != nil || model != "m" || tier != "priority" {
						b.Fatal("incorrect routing controls")
					}
				}
			})
		}
	}
}

func BenchmarkRoutingHintManyItems(b *testing.B) {
	const item = `{"type":"function_call_output","call_id":"synthetic","output":{"nested":[1,2,3],"text":"x\\\"y"}}`
	for _, size := range []int{4 << 20, 32<<20 - 512} {
		count := size / (len(item) + 1)
		raw := []byte(`{"model":"m","input":[` + strings.Repeat(item+",", count-1) + item + `],"service_tier":"priority"}`)
		for _, impl := range []struct {
			name string
			fn   func([]byte) (string, string, error)
		}{{"map_reference", routingHintMapReference}, {"current", gatewayPreparedRoutingHint}} {
			b.Run(fmt.Sprintf("%d/%s", len(raw), impl.name), func(b *testing.B) {
				b.ReportAllocs()
				b.SetBytes(int64(len(raw)))
				for b.Loop() {
					model, tier, err := impl.fn(raw)
					if err != nil || model != "m" || tier != "priority" {
						b.Fatal("incorrect routing controls")
					}
				}
			})
		}
	}
}
