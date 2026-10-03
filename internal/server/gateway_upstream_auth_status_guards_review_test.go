package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestBPSUpstreamAuthenticationProjectionGuards(t *testing.T) {
	for _, mode := range []string{"upstream_401", "nil_record", "delivered", "commit_conflict", "public_200",
		"record_200", "record_success", "client_source", "missing_key", "missing_account",
		"free_text_401", "nil_failure", "protocol_gateway", "protocol_403", "protocol_429", "cancelled", "deadline"} {
		t.Run(mode, func(t *testing.T) {
			out := httptest.NewRecorder()
			writer := &officialFailoverWriter{target: out, header: http.Header{"Content-Type": {"application/json"}, "WWW-Authenticate": {"synthetic"}}, status: 401}
			writer.body.WriteString("original")
			record := &RequestRecord{KeyID: "verified-key", AccountID: "selected-account", Status: "error", HTTPCode: 401, ErrorSource: "upstream", Error: "original diagnostic"}
			var failure error = &officialProtocolError{status: 401, source: "upstream", code: "unauthorized"}
			switch mode {
			case "nil_record":
				record = nil
			case "delivered":
				writer.delivered = true
			case "commit_conflict":
				writer.commitErr = errors.New("CAS conflict")
			case "public_200":
				writer.status = 200
			case "record_200":
				record.HTTPCode = 200
			case "record_success":
				record.Status = "ok"
			case "client_source":
				record.ErrorSource = "client"
			case "missing_key":
				record.KeyID = ""
			case "missing_account":
				record.AccountID = ""
			case "free_text_401":
				failure = errors.New("401 unauthorized")
			case "nil_failure":
				failure = nil
			case "protocol_gateway":
				failure = &officialProtocolError{status: 401, source: "gateway"}
			case "protocol_403":
				failure = &officialProtocolError{status: 403, source: "upstream"}
			case "protocol_429":
				failure = &officialProtocolError{status: 429, source: "upstream"}
			case "cancelled":
				failure = &officialProtocolError{status: 401, source: "upstream", cause: context.Canceled}
			case "deadline":
				failure = &officialProtocolError{status: 401, source: "upstream", cause: context.DeadlineExceeded}
			}
			headers, body, status := writer.header.Clone(), writer.body.String(), writer.status
			var prior RequestRecord
			if record != nil {
				prior = *record
			}
			writer.projectUpstreamAuthenticationFailure(record, failure)
			if mode == "upstream_401" {
				if writer.status != 503 || record.HTTPCode != 503 || record.Error != "original diagnostic" || record.ErrorSource != "upstream" || writer.header.Get("WWW-Authenticate") != "" {
					t.Fatal("eligible projection failed")
				}
				once := writer.body.String()
				writer.projectUpstreamAuthenticationFailure(record, failure)
				if writer.body.String() != once {
					t.Fatal("projection not idempotent")
				}
			} else {
				if !reflect.DeepEqual(writer.header, headers) || writer.body.String() != body || writer.status != status || record != nil && !reflect.DeepEqual(*record, prior) {
					t.Fatal("untrusted, unrelated or already-delivered error was rewritten")
				}
			}
		})
	}
}
