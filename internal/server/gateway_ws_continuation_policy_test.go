package server

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/coder/websocket"
)

// Real TCP on both sides: a settings change is invisible to the CLI when it
// decides to transmit only its input delta. It must not silently change the
// non-input properties while retaining previous_response_id.
func TestGatewayWSContinuationRejectsChangedGatewayPrompt(t *testing.T) {
	for _, change := range []string{"text", "mode", "disable", "compatibility"} {
		t.Run(change, func(t *testing.T) {
			s := newGatewayWSSessionAudit(t, "policy-chain", func(settings *Settings) {
				settings.BuiltinPromptEnabled = true
				settings.BuiltinPrompt = "original gateway prefix"
				settings.BuiltinPromptMode = "prepend"
				settings.CompatibilityNormalizationEnabled = true
			})
			first := `{"type":"response.create","model":"fixture","instructions":"client original","temperature":0.5,"input":[{"role":"user","content":"first"}]}`
			s.send(t, first)
			before := s.forwarded(t)
			if !strings.Contains(string(before), "original gateway prefix") {
				t.Fatal("test setup did not apply original policy")
			}
			s.event(t, `{"type":"response.completed","response":{"id":"policy-first","status":"completed","output":[]}}`)
			settings, err := s.p.Settings.Get()
			if err != nil {
				t.Fatal(err)
			}
			switch change {
			case "text":
				settings.BuiltinPrompt = "new gateway prefix"
			case "mode":
				settings.BuiltinPromptMode = "append"
			case "disable":
				settings.BuiltinPromptEnabled = false
			case "compatibility":
				settings.CompatibilityNormalizationEnabled = false
			}
			if err := s.p.Settings.Set(settings); err != nil {
				t.Fatal(err)
			}
			s.send(t, `{"type":"response.create","model":"fixture","instructions":"client original","previous_response_id":"policy-first","temperature":0.5,"input":[]}`)
			// Whichever outcome occurs is observed without a sleep/timeout race.
			// A buggy forward receives a synthetic terminal so both peers drain.
			reply := make(chan []byte, 1)
			go func() { _, raw, _ := s.client.Read(t.Context()); reply <- raw }()
			select {
			case raw := <-s.requests:
				_ = s.upstream.Write(t.Context(), websocket.MessageText, []byte(`{"type":"response.completed","response":{"id":"bad-delta","status":"completed"}}`))
				<-reply
				t.Fatalf("changed gateway policy forwarded previous_response_id delta: %s", raw)
			case raw := <-reply:
				if !strings.Contains(string(raw), "resend full input") {
					t.Fatalf("unexpected rejection: %s", raw)
				}
			}
			if s.p.inFlight("test") != 0 {
				t.Fatal("rejected delta acquired a slot")
			}
			// This is recoverable on the same connection with full context.
			s.send(t, first)
			current := s.forwarded(t)
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(current, &fields); err != nil {
				t.Fatal(err)
			}
			if _, exists := fields["previous_response_id"]; exists {
				t.Fatal("full retry retained prior identity")
			}
			s.event(t, `{"type":"response.completed","response":{"id":"policy-new-full","status":"completed","output":[]}}`)
			waitGatewayWSReleased(t, s.p)
		})
	}
}

func TestGatewayWSContinuationPolicyNormalWarmupBranchesAndCallerChanges(t *testing.T) {
	s := newGatewayWSSessionAudit(t, "normal-policy", func(settings *Settings) {
		settings.BuiltinPromptEnabled, settings.BuiltinPrompt = true, "original prefix"
	})
	s.send(t, `{"type":"response.create","model":"fixture","instructions":"first client","generate":false,"input":[]}`)
	s.forwarded(t)
	s.event(t, `{"type":"response.completed","response":{"id":"warmup-original","status":"completed","output":[]}}`)
	settings, _ := s.p.Settings.Get()
	settings.UpstreamStreamIdleTimeoutSeconds = 60
	if err := s.p.Settings.Set(settings); err != nil {
		t.Fatal(err)
	}
	// Caller changes are not gateway policy drift: forwarding remains opaque.
	s.send(t, `{"type":"response.create","model":"fixture","instructions":"second client explicit","previous_response_id":"warmup-original","input":[]}`)
	if raw := s.forwarded(t); !strings.Contains(string(raw), "second client explicit") {
		t.Fatal("caller instructions were replaced")
	}
	s.event(t, `{"type":"response.completed","response":{"id":"normal-second","status":"completed","output":[]}}`)
	settings, _ = s.p.Settings.Get()
	settings.BuiltinPrompt = "new prefix"
	if err := s.p.Settings.Set(settings); err != nil {
		t.Fatal(err)
	}
	// A new full chain is admitted with new policy without rewriting history.
	s.send(t, `{"type":"response.create","model":"fixture","input":[]}`)
	if raw := s.forwarded(t); !strings.Contains(string(raw), "new prefix") {
		t.Fatal("new full chain missed new policy")
	}
	s.event(t, `{"type":"response.completed","response":{"id":"new-chain","status":"completed","output":[]}}`)
	s.send(t, `{"type":"response.create","model":"fixture","previous_response_id":"warmup-original","input":[]}`)
	if raw := gatewayWSRead(t, s.client); !strings.Contains(string(raw), "resend full input") {
		t.Fatal("old warmup chain inherited a new chain policy")
	}
	// Unknown explicit IDs retain their previous compatibility behavior.
	s.send(t, `{"type":"response.create","model":"fixture","previous_response_id":"not-observed","input":[]}`)
	if raw := s.forwarded(t); !strings.Contains(string(raw), `"previous_response_id":"not-observed"`) {
		t.Fatal("unknown identity was invented or stripped")
	}
	s.event(t, `{"type":"response.completed","response":{"id":"unknown-result","status":"completed","output":[]}}`)
	waitGatewayWSReleased(t, s.p)
}

func TestGatewayWSRewritePolicyDigestEffectiveAndPrivate(t *testing.T) {
	settings := Settings{BuiltinPromptEnabled: false, BuiltinPrompt: "unused private prompt", BuiltinPromptMode: "append"}
	first := gatewayWSRewritePolicyDigest(settings)
	settings.BuiltinPrompt = "other unused private prompt"
	settings.BuiltinPromptMode = "if_empty"
	if gatewayWSRewritePolicyDigest(settings) != first {
		t.Fatal("disabled prompt metadata changed effective policy")
	}
	settings.BuiltinPromptEnabled = true
	settings.BuiltinPrompt = " \n "
	if gatewayWSRewritePolicyDigest(settings) != first {
		t.Fatal("empty prompt changed effective policy")
	}
	settings.BuiltinPrompt = "active private prompt"
	settings.BuiltinPromptMode = ""
	active := gatewayWSRewritePolicyDigest(settings)
	settings.BuiltinPromptMode = "prepend"
	if gatewayWSRewritePolicyDigest(settings) != active || active == first {
		t.Fatal("effective default/prepend policy identity mismatch")
	}
	encoded, err := json.Marshal(gatewayWSCachePolicy{rewriteTracked: true, rewriteDigest: active})
	if err != nil || string(encoded) != "{}" {
		t.Fatal("connection-local private policy escaped JSON")
	}
}

func TestGatewayWSContinuationCannotCrossReauthorizedOwner(t *testing.T) {
	s := newGatewayWSSessionAudit(t, "owner-continuation", nil)
	s.send(t, `{"type":"response.create","model":"fixture","input":[]}`)
	s.forwarded(t)
	s.event(t, `{"type":"response.completed","response":{"id":"prior-owner","status":"completed","output":[]}}`)
	if err := s.p.Store.UpdateAccount("test", func(a *Account) error {
		a.Identity = &managedAccountIdentity{UserID: "different-user", WorkspaceID: a.AccountID, Verified: true}
		a.AccessToken = "synthetic-new-owner-access"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	s.send(t, `{"type":"response.create","model":"fixture","previous_response_id":"prior-owner","input":[]}`)
	raw := gatewayWSRead(t, s.client)
	if !strings.Contains(string(raw), `"status":403`) {
		t.Fatalf("owner replacement did not reject: %s", raw)
	}
	select {
	case <-s.requests:
		t.Fatal("new owner credentials reused old response context")
	default:
	}
	waitGatewayWSReleased(t, s.p)
}
