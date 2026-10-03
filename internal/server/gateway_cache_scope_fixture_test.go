package server

// Transport fixtures use the known owner from newTestProxy. Namespace algorithm
// vectors are covered independently; wire tests compare the admitted owner's
// expected scalar and continue comparing every other request field unchanged.
func gatewayFixtureScopedValue(kind, original string) string {
	return gatewayScopeCacheIdentityValue(Account{
		AccountID: "test-account",
		Identity:  &managedAccountIdentity{UserID: "fixture-user", WorkspaceID: "test-account"},
	}, "admin", kind, original)
}
