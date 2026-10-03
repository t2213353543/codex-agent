package server

import "testing"

func TestGatewayOwnerRevisionReloadUsesExactID(t *testing.T) {
	p := newTestProxy(t, 1)
	original, err := p.Store.Get("test")
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Store.ImportWith(original.ID, []byte(`{"access_token":"synthetic-shadow","refresh_token":"synthetic-shadow-rt"}`), AccountDefaults{}); err != nil {
		t.Fatal(err)
	}
	if err := p.Store.ApplyAll(func(state *accountState) error {
		state.Accounts[0], state.Accounts[1] = state.Accounts[1], state.Accounts[0]
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	before, err := p.Store.getAccountByID(original.ID)
	if err != nil || before.Name != "test" {
		t.Fatal("ownership lookup did not prefer exact ID")
	}
	updated, err := p.Store.Refresh("test", before.AccessToken, func(a Account) (Account, error) { a.AccessToken = "synthetic-exact-updated"; return a, nil })
	if err != nil || updated.ID != original.ID || updated.AccessToken != "synthetic-exact-updated" || updated.OwnerRevision != before.OwnerRevision {
		t.Fatal("post-commit refresh read was shadowed by another account name")
	}
	current, err := p.gatewayCurrentBoundAccount(original, gatewayAccountRef(before))
	if err != nil || current.ID != original.ID {
		t.Fatal("bound-owner revalidation was shadowed by display name")
	}
}
