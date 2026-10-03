package server

// Request audit rows remain durable across replacements, but only an explicit
// current credential generation may describe the current account's status.
func managedCurrentRequestStatusSQL(stateAlias string) string {
	return "(CASE WHEN " + stateAlias + ".credential_version=(SELECT credential_version FROM account_credentials current_credential WHERE current_credential.account_id=a.id AND credential_version>0) THEN " + stateAlias + ".record->>'status' ELSE '' END)"
}

// Expressions are internal SQL fragments; account status precedence matches
// accountManagementStatus. One joined state row serves filtering and sorting.
func managedAccountStatusSQL(nowPlaceholder, lastStatusExpression string) string {
	last := "COALESCE(" + lastStatusExpression + ",'')"
	return "(CASE WHEN NOT a.enabled THEN 'disabled' WHEN a.expires_at>0 AND a.expires_at<" + nowPlaceholder + " THEN 'expired'" +
		" WHEN a.credential_status IN ('error','invalid','refresh_failed','failed') OR (a.credential_status='payment_required' AND a.last_checked_at+1800>" + nowPlaceholder + ") THEN 'error'" +
		" WHEN " + last + " NOT IN ('','ok') THEN 'error'" +
		" WHEN " + last + "='ok' OR (a.last_checked_at>0 AND a.credential_status='valid') THEN 'ok' ELSE 'unverified' END)"
}
