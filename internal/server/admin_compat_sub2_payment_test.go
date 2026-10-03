package server

import (
	"net/http"
	"reflect"
	"testing"
	"time"
)

func TestSub2PaymentAvailability(t *testing.T) { exerciseSub2PaymentAvailability(t, webTestSetup) }
func TestPGSub2PaymentAvailability(t *testing.T) {
	exerciseSub2PaymentAvailability(t, adminPGTestProxy)
}

func exerciseSub2PaymentAvailability(t *testing.T, setup func(*testing.T) (*Proxy, http.Handler)) {
	p, _ := setup(t)
	a := managedTestAccount(t, p, "payment-availability", nil)
	for _, tc := range []struct {
		name                             string
		enabled, expired                 bool
		message, wantStatus, wantMessage string
		wantSchedulable                  bool
	}{
		{"active_payment", true, false, gatewayPaymentMessage, "error", gatewayPaymentMessage, false},
		{"disabled_payment", false, false, gatewayPaymentMessage, "error", gatewayPaymentMessage, false},
		{"expired_payment", true, true, gatewayPaymentMessage, "active", "", true},
		{"disabled_expired_payment", false, true, gatewayPaymentMessage, "active", "", false},
		{"other_error_survives_expiry", true, true, "independent account failure", "error", "independent account failure", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			updated, err := p.Store.UpdateAccountVersion(a.ID, 0, func(current *Account) error {
				current.Enabled, current.CredentialStatus, current.LastError = tc.enabled, gatewayPaymentStatus, tc.message
				current.LastCheckedAt = time.Now().Unix()
				if tc.expired {
					current.LastCheckedAt -= int64(gatewayPaymentCooldown/time.Second) + 1
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			row, err := p.compatGetAccount(t.Context(), a.ID)
			if err != nil {
				t.Fatal(err)
			}
			before := row.Account
			dto := sub2AccountDTO(row)
			status, schedulable := sub2AccountStatus(row)
			if status != tc.wantStatus || schedulable != tc.wantSchedulable || dto["status"] != status || dto["schedulable"] != schedulable || dto["error_message"] != tc.wantMessage {
				t.Fatalf("availability contract: status=%s schedulable=%t DTO=%+v", status, schedulable, dto)
			}
			if tc.expired {
				if dto["temp_unschedulable_until"] != nil || dto["temp_unschedulable_reason"] != "" {
					t.Fatal("expired temporary block remained visible")
				}
			} else if dto["temp_unschedulable_until"] == nil || dto["temp_unschedulable_reason"] != gatewayPaymentMessage {
				t.Fatal("active payment block lacks expiry/reason")
			}
			if !reflect.DeepEqual(before, row.Account) {
				t.Fatal("availability read changed user account")
			}
			after, err := p.Store.Get(a.ID)
			if err != nil || after.Version != updated.Version || after.Enabled != tc.enabled || after.LastError != tc.message {
				t.Fatalf("availability view persisted a user setting change: %v", err)
			}
			if tc.enabled && tc.expired && tc.message == gatewayPaymentMessage && accountManagementStatus(after, "ok", time.Now().Unix()) != "ok" {
				t.Fatal("native successful observation status regressed")
			}
		})
	}
}
