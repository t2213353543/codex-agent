"""Pure synthetic claims: no key files, networking, model calls or JWT validation."""
import base64
import copy
import hashlib
import json
import math
import unittest
from unittest import mock

import quality_credential_claims as probe

NOW = 1900000000
PRIVATE_USER = "private-real-user"
PRIVATE_WORKSPACE = "private-workspace-id"
PRIVATE_EMAIL = "private-person@example.test"


def encode(raw):
    return base64.urlsafe_b64encode(raw).decode().rstrip("=")


def token(value=None, *, payload_raw=None, header=None, signature=b"private-signature"):
    if value is None:
        value = claims()
    raw = json.dumps(value, ensure_ascii=False, separators=(",", ":")).encode() if payload_raw is None else payload_raw
    return encode(json.dumps(header or {"alg": "RS256", "typ": "JWT", "kid": "private-key-id"}).encode()) + "." + encode(raw) + "." + encode(signature)


def claims(**updates):
    value = {"iss": "https://private-issuer.test/token", "sub": "private-auth-subject", "aud": ["private-client", "private-api"],
             "iat": NOW-60, "exp": NOW+3600, "auth_time": NOW-120, "email": PRIVATE_EMAIL,
             "scope": "openid profile email offline_access api.connectors.read api.connectors.invoke private.scope api.private.secret",
             "amr": ["pwd", "mfa", "private-method"], "acr": "private-assurance",
             "client_id": "private-auth-client", "azp": "private-auth-client", "device_id": "private-device", "sid": "private-session",
             probe.AUTH: {"chatgpt_user_id": PRIVATE_USER, "user_id": "private-secondary-user", "chatgpt_account_id": PRIVATE_WORKSPACE,
                          "chatgpt_plan_type": "business", "organizations": [{"id": "private-org", "role": "private-org-role"}]}}
    value.update(updates)
    return value


class ClaimsTests(unittest.TestCase):
    def summary(self, value=None, **kwargs):
        return probe.summarize_credential_claims(token(value), now=kwargs.pop("now", NOW), **kwargs)

    def test_every_private_identifier_unknown_name_and_token_stays_hashed(self):
        value = claims()
        value["private-claim-name"] = {"private-nested-key": "private-nested-value"}
        value[probe.PROFILE] = {"email": "private-profile@example.test", "name": "private display name"}
        value[probe.AUTH]["session_id"] = "private-auth-session"
        value[probe.AUTH]["app_id"] = "private-app-id"
        credential = token(value)
        report = probe.summarize_credential_claims(credential, metadata={"client_id": "private-auth-client", "email": PRIVATE_EMAIL,
            "private-metadata-key": {"access_token": credential, "refresh_token": "private-refresh-token"}}, now=NOW)
        encoded = json.dumps(report)
        for secret in (credential, PRIVATE_USER, PRIVATE_WORKSPACE, PRIVATE_EMAIL, "private-key-id", "private-auth-subject", "private-api", "private-client",
                       "private-auth-client", "private-device", "private-session", "private-org", "private-method", "private-assurance", "private.scope",
                       "api.private.secret", "private-claim-name", "private-nested-key", "private-nested-value", "private-profile@example.test",
                       "private display name", "private-auth-session", "private-app-id", "private-metadata-key", "private-refresh-token", "private-issuer"):
            self.assertNotIn(secret, encoded)
        self.assertFalse(report["signature_verified"])
        self.assertFalse(report["access_token"]["permission_or_quota_verified"])
        self.assertEqual(report["access_token"]["plan"]["value"], "business")
        self.assertEqual(report["access_token"]["algorithm"]["value"], "RS256")
        self.assertEqual(report["access_token"]["owner"]["identity_sha256"], hashlib.sha256((PRIVATE_WORKSPACE+":"+PRIVATE_USER).encode()).hexdigest())

    def test_primary_user_precedence_missing_null_only_fallback_no_sub(self):
        for primary, expected in (("primary", "primary"), (None, "fallback"), (probe.MISSING, "fallback")):
            value = claims()
            value[probe.AUTH] = {"chatgpt_account_id": PRIVATE_WORKSPACE, "user_id": "fallback"}
            if primary is not probe.MISSING:
                value[probe.AUTH]["chatgpt_user_id"] = primary
            user = self.summary(value)["access_token"]["owner"]["stable_user"]
            self.assertTrue(user["comparable"])
            self.assertEqual(user["utf8_sha256"], hashlib.sha256(expected.encode()).hexdigest())
            self.assertEqual(user["fallback_used"], primary in (None, probe.MISSING))
        for bad in ("", " ", "bad\nuser", "bad user", 1, True, [], {}, ["primary"]):
            value = claims()
            value[probe.AUTH]["chatgpt_user_id"] = bad
            value[probe.AUTH]["user_id"] = "valid-fallback"
            owner = self.summary(value)["access_token"]["owner"]
            self.assertFalse(owner["complete_comparable_owner"])
            self.assertFalse(owner["stable_user"]["fallback_used"])
            self.assertEqual(owner["stable_user"]["source"], "auth.chatgpt_user_id")
        value = claims()
        value[probe.AUTH] = {"chatgpt_account_id": PRIVATE_WORKSPACE}
        value["sub"] = "valid-looking-human-user"
        owner = self.summary(value)["access_token"]["owner"]
        self.assertFalse(owner["complete_comparable_owner"])
        self.assertFalse(owner["stable_user"]["sub_used"])

    def test_same_workspace_distinct_user_and_same_user_distinct_workspace(self):
        first = claims()
        second = copy.deepcopy(first)
        second[probe.AUTH]["chatgpt_user_id"] = "other-person"
        third = copy.deepcopy(first)
        third[probe.AUTH]["chatgpt_account_id"] = "other-workspace"
        report = probe.compare_credential_claims([{ "alias": alias, "access_token": token(value)}
            for alias, value in (("new2", first), ("g7", second), ("q0", third))], now=NOW)
        a, b = report["comparisons"][:2]
        self.assertEqual((a["access_stable_user"], a["access_workspace"]), ("mismatch", "matched"))
        self.assertEqual((b["access_stable_user"], b["access_workspace"]), ("matched", "mismatch"))
        self.assertFalse(report["permission_or_quota_inference"])

    def test_refresh_epoch_changes_are_distinct_from_same_owner(self):
        before, after = claims(), claims(iat=NOW, exp=NOW+7200, sid="private-new-session")
        after["scope"] = "openid profile email offline_access"
        report = probe.compare_credential_claims([
            {"alias": "before", "access_token": token(before)}, {"alias": "after", "access_token": token(after)}], now=NOW)
        compared = report["comparisons"][0]
        self.assertEqual(compared["access_stable_user"], "matched")
        self.assertEqual(compared["access_workspace"], "matched")
        self.assertEqual(compared["access_token_bytes_relation"], "different")
        self.assertTrue(any(item["path"] == "access_token/claims/iat" for item in compared["differences"]))
        self.assertTrue(any(item["path"] == "access_token/claims/scope" for item in compared["differences"]))

    def test_at_id_metadata_identity_conflicts_are_not_merged_or_verified(self):
        at, identity = claims(), claims()
        identity[probe.AUTH]["chatgpt_user_id"] = "different-id-user"
        report = probe.summarize_credential_claims(token(at), token(identity),
            {"account_id": PRIVATE_WORKSPACE, "user_id": PRIVATE_USER, "identity": {"user_id": PRIVATE_USER}}, now=NOW)
        self.assertEqual(report["identity_comparisons"]["access_vs_id"], {"stable_user": "mismatch", "workspace": "matched"})
        self.assertEqual(report["identity_comparisons"]["access_vs_metadata"]["stable_user"], "matched")
        self.assertEqual(report["identity_comparisons"]["id_vs_metadata"]["stable_user"], "mismatch")
        self.assertFalse(report["signature_verified"])
        conflict = self.summary(metadata={"account_id": PRIVATE_WORKSPACE, "workspace_id": "another"})
        self.assertEqual(conflict["metadata_identity_hints"]["workspace"]["state"], "conflicting_metadata_candidates")
        self.assertEqual(conflict["identity_comparisons"]["access_vs_metadata"]["workspace"], "not_comparable")

    def test_stored_client_id_vs_azp_and_client_claims_compared_separately(self):
        value = claims()
        value["azp"] = "different-authorized-party"
        result = self.summary(value, metadata={"client_id": "private-auth-client"})
        self.assertEqual(result["client_vs_stored_metadata"]["access"]["client_id"], "matched")
        self.assertEqual(result["client_vs_stored_metadata"]["access"]["azp"], "mismatch")
        self.assertEqual(result["client_vs_stored_metadata"]["access"]["app_id"], "not_comparable")
        value[probe.AUTH]["client_id"] = "conflicting-auth-client"
        result = self.summary(value, metadata={"client_id": "private-auth-client"})
        self.assertEqual(result["client_identity_hints"]["access"]["client_id"]["state"], "conflicting_claim_candidates")
        self.assertEqual(result["client_vs_stored_metadata"]["access"]["client_id"], "not_comparable")

    def test_personal_team_plan_org_differences_do_not_imply_account_quality(self):
        first, second = claims(), claims()
        first[probe.AUTH].update(chatgpt_plan_type="plus", is_personal=True, organizations=[])
        second[probe.AUTH].update(chatgpt_plan_type="self_serve_business_prolite", is_personal=False, organizations=[{"id": "private-business-org"}])
        result = probe.compare_credential_claims([{"alias": "new2", "access_token": token(first)}, {"alias": "g7", "access_token": token(second)}], now=NOW)
        self.assertEqual(result["comparisons"][0]["access_stable_user"], "matched")
        self.assertEqual(result["cases"][0]["summary"]["access_token"]["plan"]["value"], "plus")
        self.assertEqual(result["cases"][1]["summary"]["access_token"]["plan"]["value"], "self_serve_business_prolite")
        self.assertTrue(any(item["path"].endswith("/organizations") for item in result["comparisons"][0]["differences"]))
        self.assertNotIn("private-business-org", json.dumps(result))

    def test_present_null_wrong_type_and_unknown_fields_are_preserved_in_summaries(self):
        value = claims()
        value["email"] = None
        value["scope"] = ["openid"]
        value["private-unknown"] = {"private-key": False}
        result = self.summary(value)["access_token"]
        fields = result["claims"]["fields"]
        self.assertEqual(fields["email"]["state"], "null")
        self.assertTrue(fields["email"]["present"])
        self.assertFalse(fields["nonce"]["present"])
        self.assertEqual(fields["nonce"]["state"], "missing")
        self.assertEqual(fields["scope"]["type"], "array")
        self.assertEqual(result["scopes"]["tokenization"], "wrong_type")
        label = "name_sha256:"+hashlib.sha256(b"private-unknown").hexdigest()
        self.assertIn(label, fields)
        self.assertEqual(fields[label]["observed_field_count"], 1)
        self.assertTrue(fields[label]["complete"])

    def test_canonical_hash_ignores_object_order_string_escape_not_number_lexemes(self):
        raw1 = b'{"private":9007199254740993,"email":"a\\u0062","x":{"one":1.0,"two":null}}'
        raw2 = b'{"x":{"two":null,"one":1.0}, "email":"ab", "private":9007199254740993}'
        raw3 = b'{"x":{"two":null,"one":1}, "email":"ab", "private":9007199254740993}'
        first = probe.summarize_credential_claims(token(payload_raw=raw1), now=NOW)["access_token"]
        same = probe.summarize_credential_claims(token(payload_raw=raw2), now=NOW)["access_token"]
        different = probe.summarize_credential_claims(token(payload_raw=raw3), now=NOW)["access_token"]
        self.assertEqual(first["claims"]["canonical_sha256"], same["claims"]["canonical_sha256"])
        self.assertNotEqual(first["payload_wire_sha256"], same["payload_wire_sha256"])
        self.assertNotEqual(first["claims"]["canonical_sha256"], different["claims"]["canonical_sha256"])
        exact = first["claims"]["fields"]["name_sha256:"+hashlib.sha256(b"private").hexdigest()]
        self.assertEqual(exact["canonical_sha256"], hashlib.sha256(b"9007199254740993").hexdigest())

    def test_audience_string_array_normalization_preserves_type_order_and_duplicates(self):
        string = self.summary(claims(aud="private-audience"))["access_token"]["audience"]
        singleton = self.summary(claims(aud=["private-audience"]))["access_token"]["audience"]
        self.assertEqual(string["ordered_normalized_sha256"], singleton["ordered_normalized_sha256"])
        self.assertNotEqual(string["canonical_sha256"], singleton["canonical_sha256"])
        one = self.summary(claims(aud=["A", "B", "A"]))["access_token"]["audience"]
        two = self.summary(claims(aud=["B", "A"]))["access_token"]["audience"]
        self.assertNotEqual(one["ordered_normalized_sha256"], two["ordered_normalized_sha256"])
        self.assertEqual(one["sorted_unique_sha256"], two["sorted_unique_sha256"])
        self.assertEqual(self.summary(claims(aud=["A", 1]))["access_token"]["audience"]["normalization"], "wrong_type")

    def test_scope_whitelist_is_exact_not_api_prefix_and_scp_stays_separate(self):
        result = self.summary(claims(scope="openid  api.connectors.read private.scope openid", scp=["email", "private.scp", 7]))["access_token"]
        self.assertEqual([item.get("value") for item in result["scopes"]["tokens"]], ["openid", "api.connectors.read", None, "openid"])
        self.assertEqual(result["scopes"]["normalized_count"], 4)
        self.assertFalse(result["scopes"]["permissions_verified"])
        self.assertEqual(result["scp"]["tokenization"], "wrong_element_type")
        self.assertEqual(result["scp"]["tokens"][0]["value"], "email")
        self.assertNotIn("sorted_unique_tokens_canonical_sha256", result["scp"])
        result = self.summary(claims(scope="openid\tprofile"))["access_token"]
        self.assertEqual(result["scopes"]["tokenization"], "invalid_scope_character")
        self.assertNotIn("openid\tprofile", json.dumps(result))

    def test_scp_string_array_preserves_order_duplicates_and_set_hash(self):
        first=self.summary(claims(scp=["email","openid","email"]))["access_token"]["scp"]
        second=self.summary(claims(scp=["openid","email"]))["access_token"]["scp"]
        different=self.summary(claims(scp=["openid","profile"]))["access_token"]["scp"]
        self.assertNotEqual(first["ordered_tokens_canonical_sha256"],second["ordered_tokens_canonical_sha256"])
        self.assertEqual(first["sorted_unique_tokens_canonical_sha256"],second["sorted_unique_tokens_canonical_sha256"])
        self.assertNotEqual(second["sorted_unique_tokens_canonical_sha256"],different["sorted_unique_tokens_canonical_sha256"])
        self.assertEqual((first["normalized_count"],first["unique_count"]),(3,2))

    def test_timestamp_numeric_nonbool_and_lifetime_not_registration(self):
        result = self.summary(claims(iat=NOW-86400, exp=NOW+86400, auth_time=NOW-100000, pwd_auth_time=NOW-90000))["access_token"]["times"]
        self.assertEqual(result["lifetime_days"], 2)
        self.assertEqual(result["remaining_days"], 1)
        self.assertFalse(result["expired_at_comparison_time"])
        self.assertIn("not_account_registration", result["interpretation"])
        for bad in (True, "1900000000", {}, [], None, -1, 9007199254740992):
            value = self.summary(claims(iat=bad))["access_token"]["times"]
            self.assertFalse(value["iat"]["numeric_valid"])
            self.assertNotIn("lifetime_seconds", value)
        backwards = self.summary(claims(iat=NOW+5, exp=NOW-1))["access_token"]["times"]
        self.assertFalse(backwards["lifetime_nonnegative"])
        self.assertTrue(backwards["issued_after_comparison_time"])
        self.assertTrue(backwards["expired_at_comparison_time"])
        fractional = self.summary(claims(iat=10.25, exp=11.75))["access_token"]["times"]
        self.assertEqual(fractional["lifetime_seconds"], 1.5)

    def test_private_pwd_auth_time_has_no_inferred_unit_or_seconds_conversion(self):
        # A 13-digit value is not evidence of a millisecond contract. Keep it
        # verbatim as numeric evidence without interpreting it as Unix seconds.
        report = self.summary(claims(pwd_auth_time=1790305335942))["access_token"]["times"]
        value = report["pwd_auth_time"]
        self.assertNotIn("seconds", value)
        self.assertEqual(value["unit"], "unspecified")
        self.assertEqual(value["unit_basis"], "private_claim_no_unit_contract_verified")
        self.assertEqual(value["numeric_value"], 1790305335942)
        self.assertEqual(value["numeric_lexeme"], "1790305335942")
        self.assertEqual(value["canonical_sha256"], hashlib.sha256(b"1790305335942").hexdigest())
        self.assertNotIn("milliseconds", value)
        self.assertEqual(report["lifetime_seconds"], 3660)
        for field in ("iat", "exp", "nbf", "auth_time"):
            self.assertEqual(report[field]["unit"], "seconds")

    def test_private_time_missing_null_wrong_type_and_numeric_lexeme_stay_distinct(self):
        for raw in (b'1790305335942', b'1790305335942.0', b'1.790305335942e12'):
            payload = b'{"pwd_auth_time":'+raw+b'}'
            value = probe.summarize_credential_claims(token(payload_raw=payload), now=NOW)["access_token"]["times"]["pwd_auth_time"]
            self.assertEqual(value["numeric_value"], 1790305335942)
            self.assertEqual(value["numeric_lexeme"], raw.decode())
            self.assertEqual(value["canonical_sha256"], hashlib.sha256(raw).hexdigest())
            self.assertEqual(value["unit"], "unspecified")
            self.assertNotIn("seconds", value)
        for candidate in (probe.MISSING, None, True, "1790305335942", {}, -1):
            claims_value = claims()
            if candidate is not probe.MISSING:
                claims_value["pwd_auth_time"] = candidate
            value = self.summary(claims_value)["access_token"]["times"]["pwd_auth_time"]
            self.assertFalse(value["numeric_valid"])
            self.assertEqual(value["unit"], "unspecified")
            self.assertNotIn("seconds", value)
            self.assertNotIn("numeric_value", value)

    def test_amr_acr_known_enum_and_unknown_identifier_never_raw(self):
        result = self.summary(claims(amr=["pwd", "mfa", "private-factor", 1], acr="urn:mace:incommon:iap:silver"))["access_token"]
        self.assertFalse(result["amr"]["all_methods_strings"])
        self.assertEqual(result["amr"]["methods"][0]["value"], "pwd")
        self.assertNotIn("value", result["amr"]["methods"][2])
        self.assertEqual(result["acr"]["value"], "urn:mace:incommon:iap:silver")
        self.assertNotIn("private-factor", json.dumps(result))

    def test_malformed_duplicate_nonfinite_bad_base64_and_wrong_root_safe_fail(self):
        candidates = [None, 1, {}, b"private-token", "private-opaque-token", "a.b", "a.b.c.d", token()+" ",
                      token(payload_raw=b'{"exp":1,"exp":2}'), token(payload_raw=b'{"nested":{"x":1,"x":2}}'),
                      token(payload_raw=b'{"exp":NaN}'), token(payload_raw=b'{"exp":Infinity}'), token(payload_raw=b'{"exp":1e9999}'),
                      token(payload_raw=b'{"bad":"\\ud800"}'), token(payload_raw=b'\xff'), token(payload_raw=b'[]'),
                      encode(b'[]')+"."+encode(b'{}')+"."+encode(b'sig'), token().replace('.', '.=', 1),
                      "e30.e31.c2ln", token(payload_raw=b'['*70+b']'*70)]
        for candidate in candidates:
            with self.subTest(index=candidates.index(candidate)):
                result = probe.summarize_credential_claims(candidate, now=NOW)
                self.assertEqual(result["access_token"]["parse_status"], "invalid")
                self.assertNotIn("private-token", json.dumps(result))
                self.assertNotIn("private-opaque-token", json.dumps(result))
                self.assertFalse(result["signature_verified"])

    def test_token_size_depth_and_node_limits_are_explicit_not_partial_success(self):
        value = claims()
        value["large"] = [0]*8192
        self.assertEqual(self.summary(value)["access_token"]["error_code"], "json_structure_limit")
        result = probe.summarize_credential_claims("x"*(probe.MAX_TOKEN_BYTES+1), now=NOW)
        self.assertEqual(result["access_token"]["parse_status"], "invalid")
        with mock.patch.object(probe, "MAX_JSON_BYTES", 64):
            self.assertEqual(self.summary()["access_token"]["error_code"], "json_size_limit")

    def test_metadata_wrong_values_do_not_leak_and_no_credentials_loaded(self):
        for metadata in ("private-raw-metadata", {"secret": math.inf}, {"private": object()}, {"private": b"private-data"}):
            result = self.summary(metadata=metadata)
            self.assertEqual(result["metadata"]["parse_status"], "invalid")
            self.assertNotIn("private-raw-metadata", json.dumps(result))
        value = {"account_id": PRIVATE_WORKSPACE, "user_id": PRIVATE_USER, "created_at": "private-created-at"}
        before = copy.deepcopy(value)
        self.summary(metadata=value)
        self.assertEqual(value, before)
        self.assertEqual(self.summary()["id_token"]["parse_status"], "absent")

    def test_email_selection_missing_null_primary_matches_source_but_stays_hash_only(self):
        value = claims(email=None)
        value[probe.PROFILE] = {"email": "private-profile-email"}
        selected = self.summary(value)["access_token"]["owner"]["email"]
        self.assertEqual(selected["source"], "profile.email")
        self.assertEqual(selected["utf8_sha256"], hashlib.sha256(b'private-profile-email').hexdigest())
        value["email"] = ""
        selected = self.summary(value)["access_token"]["owner"]["email"]
        self.assertEqual(selected["source"], "email")
        self.assertEqual(selected["utf8_bytes"], 0)

    def test_optional_identity_bad_token_not_silently_substituted_with_access_token(self):
        result = self.summary(id_token="bad-private-identity-token")
        self.assertEqual(result["id_token"]["parse_status"], "invalid")
        self.assertEqual(result["access_token"]["parse_status"], "decoded_unverified")
        self.assertEqual(result["identity_comparisons"]["access_vs_id"]["stable_user"], "not_comparable")

    def test_compare_unknown_aliases_and_fields_are_anonymous_and_differences_complete(self):
        first, second = claims(), claims()
        second["private-field-name"] = {"private-child": 1}
        result = probe.compare_credential_claims([{"alias": PRIVATE_EMAIL, "access_token": token(first)},
            {"alias": "g7", "access_token": token(second)}], now=NOW)
        self.assertEqual(result["cases"][0]["alias"], "case-1")
        self.assertEqual(result["cases"][1]["alias"], "g7")
        self.assertNotIn(PRIVATE_EMAIL, json.dumps(result))
        self.assertNotIn("private-field-name", json.dumps(result))
        self.assertTrue(result["comparisons"][0]["differences_complete"])
        self.assertGreater(result["comparisons"][0]["differing_field_count"], 0)
        with mock.patch.object(probe, "MAX_DIFFS", 1):
            result = probe.compare_credential_claims([{"alias": "before", "access_token": token(first)}, {"alias": "after", "access_token": token(second)}], now=NOW)
        self.assertFalse(result["comparisons"][0]["differences_complete"])
        self.assertEqual(len(result["comparisons"][0]["differences"]), 1)

    def test_bad_batch_configuration_fixed_error_codes(self):
        for values in ([], {}, [{"alias": "q0"}], [{"alias": "q0", "access_token": token(), "refresh_token": "private-refresh"}],
                       [{"alias": None, "access_token": token()}], [{"alias": "q0", "access_token": token()}]*17):
            with self.assertRaises(probe.ClaimsError) as error:
                probe.compare_credential_claims(values, now=NOW)
            self.assertNotIn("private", str(error.exception))
        for now in (True, float("nan"), float("inf"), -1, "private-time"):
            with self.assertRaisesRegex(probe.ClaimsError, "invalid_comparison_time"):
                self.summary(now=now)


if __name__ == "__main__":
    unittest.main()
