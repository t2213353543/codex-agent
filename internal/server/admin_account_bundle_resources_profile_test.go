package server

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// These are local CPU/allocation diagnostics, not endpoint SLO evidence.
// Every benchmark operation processes a full 10k-resource request.
func BenchmarkManagedResourceImportPreparation10K(b *testing.B) {
	input := managedResourcePipelineFixture(0, 10000, "profile")
	raw, err := json.Marshal(input)
	if err != nil {
		b.Fatal(err)
	}
	block, _ := aes.NewCipher(make([]byte, 32))
	aead, _ := cipher.NewGCM(block)
	db := &Database{cipher: aead, KeyID: "profile"}
	b.Run("body_decode", func(b *testing.B) {
		b.ReportAllocs()
		b.SetBytes(int64(len(raw)))
		for b.Loop() {
			r := httptest.NewRequest(http.MethodPost, "/api/accounts/import-resources", bytes.NewReader(raw))
			var parsed managedBundleResourcesInput
			if err := readJSONLimit(httptest.NewRecorder(), r, &parsed, 8<<20); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("eligible_all_valid", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if !managedResourcePipelineEligible(input) && !managedResourceMixedPipelineEligible(input) {
				b.Fatal("normal input rejected")
			}
		}
	})
	b.Run("eligible_mixed", func(b *testing.B) {
		mixed := input
		mixed.Resources.Proxies = append([]managedBackupProxy(nil), input.Resources.Proxies...)
		mixed.Resources.Proxies[len(mixed.Resources.Proxies)/2].Port = 0
		b.ReportAllocs()
		for b.Loop() {
			if !managedResourcePipelineEligible(mixed) && !managedResourceMixedPipelineEligible(mixed) {
				b.Fatal("mixed input rejected")
			}
		}
	})
	b.Run("plan_validation", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if len(managedResourceLocalResults(input, nil, nil, nil, nil)) != 10000 {
				b.Fatal("plan size changed")
			}
		}
	})
	b.Run("encrypt_and_ids", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			for _, v := range input.Resources.Proxies {
				if _, err := db.Encrypt([]byte(v.Password)); err != nil {
					b.Fatal(err)
				}
				_ = newID("p_")
			}
		}
	})
	b.Run("cache_publish", func(b *testing.B) {
		entries := make([]ProxyEntry, len(input.Resources.Proxies))
		for i, v := range input.Resources.Proxies {
			entries[i] = ProxyEntry{ID: v.ID, Name: v.Name, Password: v.Password, Version: 1}
		}
		b.ReportAllocs()
		for b.Loop() {
			p := &Proxy{Proxies: &ProxyStore{}}
			for i, entry := range entries {
				p.publishManagedResourceProxy(entry, nil, i == 0)
			}
		}
	})
	b.Run("response_encode", func(b *testing.B) {
		reply := managedBundleResourceReply{Results: make([]managedBundleResourceResult, len(input.Resources.Proxies)), ProxyMapping: map[string]string{}, Succeeded: len(input.Resources.Proxies)}
		for i, v := range input.Resources.Proxies {
			id := "p_" + v.ID
			reply.Results[i] = managedBundleResourceResult{Kind: "proxy", SourceID: v.ID, ID: id, Status: "created"}
			reply.ProxyMapping[v.ID] = id
		}
		b.ReportAllocs()
		for b.Loop() {
			if err := json.NewEncoder(io.Discard).Encode(reply); err != nil {
				b.Fatal(err)
			}
		}
	})
}
