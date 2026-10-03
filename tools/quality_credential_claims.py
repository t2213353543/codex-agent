"""Pure offline, redacted comparison of credential claims; no token verification.

summarize_credential_claims(access_token, id_token=None, metadata=None, *, now=None)
compare_credential_claims([{alias, access_token, id_token?, metadata?}], *, now=None)

Inputs stay in memory. Reports contain fixed field names, hashes, bounded numeric
times, and explicit public enums. Unknown names/values are hashed. JWT decoding
does not verify signatures, issuer, audience, permissions, quota, or routing.
The stable-user selection follows CLI 0.155.0 token_data.rs: chatgpt_user_id,
falling back to auth.user_id only when the primary is missing or null; never sub.
"""
from __future__ import annotations

import base64
from collections.abc import Mapping
from decimal import Decimal, InvalidOperation
import hashlib
from itertools import combinations
import json
import math
import re
import time

SCHEMA = "codex-quality-credential-claims-v1"
COMPARE_SCHEMA = "codex-quality-credential-claims-compare-v1"
AUTH = "https://api.openai.com/auth"
PROFILE = "https://api.openai.com/profile"
MAX_TOKEN_BYTES = 256 << 10
MAX_JSON_BYTES = 128 << 10
MAX_DEPTH = 32
MAX_NODES = 8192
MAX_CASES = 16
MAX_DIFFS = 1024
MISSING = object()
PUBLIC_SCOPES = frozenset(("openid", "profile", "email", "offline_access", "api.connectors.read", "api.connectors.invoke"))
PUBLIC_AMR = frozenset(("pwd", "mfa", "otp", "sms", "email", "hwk", "swk", "fpt", "face", "iris", "kba", "pin", "sc", "user", "mca", "rba", "wia"))
PUBLIC_ACR = frozenset(("0", "1", "2", "urn:mace:incommon:iap:bronze", "urn:mace:incommon:iap:silver"))
PUBLIC_ALGS = frozenset(("RS256", "RS384", "RS512", "ES256", "ES384", "ES512", "PS256", "PS384", "PS512", "HS256", "HS384", "HS512", "EdDSA", "none"))
PUBLIC_PLANS = frozenset(("guest", "free", "go", "plus", "pro", "prolite", "team", "business", "ent26", "enterprise", "enterprise_cbp_automation",
    "enterprise_cbp_usage_based", "self_serve_business_prolite", "self_serve_business_usage_based", "education", "edu", "edu_plus", "edu_pro", "hc", "quorum", "k12"))
ROOT_FIELDS = frozenset(("iss", "sub", "aud", "azp", "client_id", "app_id", "iat", "exp", "nbf", "auth_time", "pwd_auth_time", "jti", "sid", "session_id", "device_id",
    "scope", "scp", "amr", "acr", "email", "email_verified", "nonce", "at_hash", "c_hash", "gty", "idp", "auth_provider", AUTH, PROFILE))
AUTH_FIELDS = frozenset(("chatgpt_user_id", "user_id", "chatgpt_account_id", "chatgpt_account_user_id", "chatgpt_plan_type", "chatgpt_account_is_fedramp",
    "chatgpt_accounts", "organizations", "organization_id", "org_id", "workspace_id", "client_id", "app_id", "device_id", "session_id", "sid",
    "is_personal", "is_default", "project_id", "projects", "roles", "permissions", "user_role", "account_id"))
PROFILE_FIELDS = frozenset(("email", "email_verified", "name", "picture", "preferred_username"))
META_FIELDS = frozenset(("account_id", "workspace_id", "chatgpt_account_id", "user_id", "chatgpt_user_id", "email", "plan_type", "chatgpt_plan_type",
    "client_id", "app_id", "device_id", "session_id", "sid", "organization_id", "org_id", "expires_at", "exp", "iat", "auth_time", "identity",
    "source", "verified", "updated_at", "created_at", "oauth_updated_at", "assigned_at"))
HEADER_FIELDS = frozenset(("alg", "typ", "kid", "jku", "x5u", "x5c", "x5t", "cty", "crit", "b64"))
KNOWN_NAMES = ROOT_FIELDS | AUTH_FIELDS | PROFILE_FIELDS | META_FIELDS | HEADER_FIELDS
SAFE_ALIAS = re.compile(r"(?:q[0-9]+|g[0-9]+|new[0-9]+|old[0-9]+|case[0-9]+|before|after)(?:[_-][A-Za-z0-9]{1,16}){0,3}\Z")


class ClaimsError(Exception):
    """Fixed labels only. Never attach a token, claim, or input exception text."""


class _Number(str):
    pass


def _sha(raw):
    return hashlib.sha256(raw).hexdigest()


def _canonical(value):
    if isinstance(value, _Number):
        return value.encode("ascii")
    if isinstance(value, dict):
        return b"{" + b",".join(_canonical(key)+b":"+_canonical(value[key]) for key in sorted(value)) + b"}"
    if isinstance(value, list):
        return b"[" + b",".join(_canonical(item) for item in value) + b"]"
    return json.dumps(value, ensure_ascii=False, allow_nan=False, separators=(",", ":")).encode("utf-8")


def _unique(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise ClaimsError("duplicate_json_member")
        result[key] = value
    return result


def _invalid(_):
    raise ClaimsError("nonfinite_json")


def _parse(raw):
    if not isinstance(raw, bytes) or not raw or len(raw) > MAX_JSON_BYTES:
        raise ClaimsError("json_size_limit")
    try:
        value = json.loads(raw.decode("utf-8"), object_pairs_hook=_unique, parse_int=_Number, parse_float=_Number, parse_constant=_invalid)
        pending, count = [(value, 0)], 0
        while pending:
            item, depth = pending.pop()
            count += 1
            if depth > MAX_DEPTH or count > MAX_NODES:
                raise ClaimsError("json_structure_limit")
            if isinstance(item, _Number):
                if len(item) > 128 or not math.isfinite(float(item)):
                    raise ClaimsError("nonfinite_or_excessive_json_number")
            elif isinstance(item, str):
                item.encode("utf-8")
            elif isinstance(item, dict):
                for name, child in item.items():
                    name.encode("utf-8")
                    pending.append((child, depth+1))
            elif isinstance(item, list):
                pending.extend((child, depth+1) for child in item)
        return value
    except ClaimsError:
        raise
    except (ValueError, TypeError, UnicodeError, RecursionError, OverflowError) as error:
        raise ClaimsError("invalid_json") from error


def _type(value):
    if isinstance(value, _Number):
        return "number"
    return {type(None): "null", str: "string", dict: "object", list: "array", bool: "boolean"}.get(type(value), "invalid")


def _label(name):
    return name if name in KNOWN_NAMES else "name_sha256:"+_sha(name.encode())


def _describe(value=MISSING, *, names=()):
    if value is MISSING:
        return {"present": False, "state": "missing", "type": "missing"}
    raw = _canonical(value)
    result = {"present": True, "state": "null" if value is None else "present", "type": _type(value),
              "canonical_sha256": _sha(raw), "canonical_json_bytes": len(raw)}
    if isinstance(value, str) and not isinstance(value, _Number):
        result.update(utf8_sha256=_sha(value.encode()), utf8_bytes=len(value.encode()))
    if isinstance(value, dict):
        keys = set(value) | set(names)
        fields = {}
        for key in sorted(keys):
            child_names = AUTH_FIELDS if key == AUTH else PROFILE_FIELDS if key == PROFILE else META_FIELDS if key == "identity" else ()
            fields[_label(key)] = _describe(value.get(key, MISSING), names=child_names)
        result.update(fields=fields, observed_field_count=len(value), complete=True)
    elif isinstance(value, list):
        result.update(items=[_describe(item) for item in value], item_count=len(value), complete=True)
    return result


def _base64url(part):
    # CLI's payload decoder uses URL_SAFE_NO_PAD. Canonical trailing-bit checks
    # avoid two text encodings being mistaken for two different payloads.
    if not isinstance(part, str) or not re.fullmatch(r"[A-Za-z0-9_-]+", part) or len(part) % 4 == 1:
        raise ClaimsError("invalid_base64url")
    try:
        raw = base64.b64decode(part+"="*(-len(part)%4), altchars=b"-_", validate=True)
    except (ValueError, UnicodeError) as error:
        raise ClaimsError("invalid_base64url") from error
    if base64.urlsafe_b64encode(raw).decode().rstrip("=") != part:
        raise ClaimsError("noncanonical_base64url")
    return raw


def _identifier(value=MISSING):
    result = _describe(value)
    result["comparable"] = (type(value) is str and 1 <= len(value.encode()) <= 1024
                            and not any(char.isspace() or ord(char) < 32 or ord(char) == 127 for char in value))
    return result


def _selection(claims):
    auth = claims.get(AUTH, MISSING)
    primary = auth.get("chatgpt_user_id", MISSING) if isinstance(auth, dict) else MISSING
    fallback = auth.get("user_id", MISSING) if isinstance(auth, dict) else MISSING
    use_fallback = primary is MISSING or primary is None
    selected = fallback if use_fallback else primary
    workspace = auth.get("chatgpt_account_id", MISSING) if isinstance(auth, dict) else MISSING
    user_summary, workspace_summary = _identifier(selected), _identifier(workspace)
    user_summary.update(source="auth.user_id" if use_fallback else "auth.chatgpt_user_id",
                        fallback_used=use_fallback and fallback is not MISSING, sub_used=False,
                        selector_rule="primary_missing_or_null_only; invalid_or_empty_primary_does_not_fallback")
    pair = {"stable_user": user_summary, "workspace": workspace_summary,
            "claim_namespace_type": _type(auth) if auth is not MISSING else "missing",
            "complete_comparable_owner": user_summary["comparable"] and workspace_summary["comparable"]}
    if pair["complete_comparable_owner"]:
        pair.update(identity_sha256=_sha((workspace+":"+selected).encode()),
                    identity_hash_basis="UTF8(workspace_id + ':' + stable_user_id)",
                    tuple_canonical_sha256=_sha(_canonical([workspace, selected])))
    # Match the CLI's Option::or for email but retain source ambiguity explicitly.
    profile = claims.get(PROFILE)
    email = claims.get("email", MISSING)
    email_from_profile = email is MISSING or email is None
    if email_from_profile:
        email = profile.get("email", MISSING) if isinstance(profile, dict) else MISSING
    pair["email"] = dict(_describe(email), source="profile.email" if email_from_profile else "email")
    return pair


def _enum(value, allowed):
    result = _describe(value)
    result["enum_state"] = "missing" if value is MISSING else "null" if value is None else "known" if type(value) is str and value in allowed else "unknown" if type(value) is str else "wrong_type"
    if result["enum_state"] == "known":
        result["value"] = value
    return result


def _scope(value=MISSING):
    result = _describe(value)
    if type(value) is not str:
        result["tokenization"] = "missing" if value is MISSING else "null" if value is None else "wrong_type"
        return result
    # OAuth scope is separated by ASCII SP; do not normalize other Unicode
    # whitespace into fabricated scopes. Preserve order and duplicates in hashes.
    tokens = value.split(" ")
    normalized = [item for item in tokens if item]
    valid = all(re.fullmatch(r"[\x21\x23-\x5b\x5d-\x7e]+", item) is not None for item in normalized)
    result.update(tokenization="valid" if valid else "invalid_scope_character", normalized_count=len(normalized),
                  ordered_tokens_canonical_sha256=_sha(_canonical(normalized)),
                  sorted_unique_tokens_canonical_sha256=_sha(_canonical(sorted(set(normalized)))),
                  token_order_preserved=True, permissions_verified=False,
                  tokens=[_enum(item, PUBLIC_SCOPES) for item in normalized])
    return result


def _audience(value=MISSING):
    result = _describe(value)
    if type(value) is str:
        normalized = [value]
        result["normalization"] = "string_to_singleton_array"
    elif isinstance(value, list) and all(type(item) is str for item in value):
        normalized = value
        result["normalization"] = "array_order_and_duplicates_preserved"
    else:
        result["normalization"] = "missing" if value is MISSING else "null" if value is None else "wrong_type"
        return result
    result.update(ordered_normalized_sha256=_sha(_canonical(normalized)), normalized_count=len(normalized),
                  sorted_unique_sha256=_sha(_canonical(sorted(set(normalized)))), audience_verified=False)
    return result


def _scp(value=MISSING):
    if not isinstance(value, list):
        return _scope(value)
    result = _describe(value)
    valid = all(type(item) is str for item in value)
    result.update(tokenization="string_array" if valid else "wrong_element_type",
                  tokens=[_enum(item, PUBLIC_SCOPES) for item in value], permissions_verified=False,
                  token_order_preserved=True)
    if valid:
        result.update(ordered_tokens_canonical_sha256=_sha(_canonical(value)),
                      sorted_unique_tokens_canonical_sha256=_sha(_canonical(sorted(set(value)))),
                      normalized_count=len(value), unique_count=len(set(value)))
    return result


def _timestamp(value=MISSING, *, unit="seconds", unit_basis="jwt_numericdate_seconds"):
    result = _describe(value)
    result.update(numeric_valid=False, unit=unit, unit_basis=unit_basis)
    if not isinstance(value, _Number):
        result["numeric_state"] = "missing" if value is MISSING else "null" if value is None else "wrong_type"
        return result
    try:
        number = Decimal(value)
        if not number.is_finite() or number < 0 or number > 9007199254740991:
            result["numeric_state"] = "out_of_range"
            return result
        observed = int(number) if number == number.to_integral_value() else float(number)
        result.update(numeric_valid=True, numeric_state="valid")
        if unit == "seconds":
            result["seconds"] = observed
        else:
            # Private numeric claims do not inherit NumericDate units just
            # because their names contain "time" or their magnitudes look known.
            result.update(numeric_value=observed, numeric_lexeme=str(value))
    except (ValueError, InvalidOperation, OverflowError):
        result["numeric_state"] = "invalid_number"
    return result


def _times(claims, now):
    result = {key: _timestamp(claims.get(key, MISSING)) for key in ("iat", "exp", "nbf")}
    result["auth_time"] = _timestamp(claims.get("auth_time", MISSING), unit_basis="openid_connect_auth_time_seconds")
    result["pwd_auth_time"] = _timestamp(claims.get("pwd_auth_time", MISSING), unit="unspecified",
                                        unit_basis="private_claim_no_unit_contract_verified")
    result.update(interpretation="iat_is_token_issuance_not_account_registration; auth_time_is_claimed_authentication_not_registration; pwd_auth_time_unit_unspecified",
                  timestamp_unit_policy="field_specific_v2", temporal_validation_is_local=True, signature_verified=False)
    if result["iat"]["numeric_valid"] and result["exp"]["numeric_valid"]:
        lifetime = Decimal(claims["exp"])-Decimal(claims["iat"])
        result.update(lifetime_seconds=float(lifetime), lifetime_days=float(lifetime/Decimal(86400)),
                      lifetime_nonnegative=lifetime >= 0)
    if result["exp"]["numeric_valid"]:
        remaining = Decimal(claims["exp"])-Decimal(str(now))
        result.update(remaining_seconds=float(remaining), remaining_days=float(remaining/Decimal(86400)), expired_at_comparison_time=remaining <= 0)
    if result["iat"]["numeric_valid"]:
        result["issued_after_comparison_time"] = result["iat"]["seconds"] > now
    return result


def _token_summary(token, now, optional=False):
    result = {"present": token is not None, "signature_verified": False, "issuer_verified": False, "audience_verified": False,
              "permission_or_quota_verified": False, "parse_status": "absent" if token is None and optional else "not_parsed"}
    if token is None and optional:
        return result
    if not isinstance(token, str):
        result.update(parse_status="invalid", error_code="token_must_be_string", input_type=type(token).__name__ if type(token) in (type(None), int, float, list, dict, bool, bytes) else "other")
        return result
    try:
        encoded = token.encode("utf-8")
    except UnicodeError:
        result.update(parse_status="invalid", error_code="invalid_token_unicode")
        return result
    result.update(token_sha256=_sha(encoded), token_bytes=len(encoded))
    try:
        if not 1 <= len(encoded) <= MAX_TOKEN_BYTES or not token.isascii() or any(char.isspace() for char in token):
            raise ClaimsError("invalid_token_envelope")
        pieces = token.split(".")
        if len(pieces) != 3 or not all(pieces):
            raise ClaimsError("invalid_token_envelope")
        header_raw, payload_raw, signature = (_base64url(part) for part in pieces)
        header, claims = _parse(header_raw), _parse(payload_raw)
        if not isinstance(header, dict) or not isinstance(claims, dict):
            raise ClaimsError("jwt_header_and_payload_must_be_objects")
        result.update(parse_status="decoded_unverified", header=_describe(header, names=HEADER_FIELDS),
                      claims=_describe(claims, names=ROOT_FIELDS), payload_wire_sha256=_sha(payload_raw),
                      header_wire_sha256=_sha(header_raw), signature_bytes=len(signature),
                      parser_policy="strict_three_parts_canonical_base64url_duplicate_reject; stricter_than_CLI_envelope; no_signature_verification",
                      owner=_selection(claims), audience=_audience(claims.get("aud", MISSING)),
                      scopes=_scope(claims.get("scope", MISSING)), times=_times(claims, now),
                      scp=_scp(claims.get("scp", MISSING)),
                      algorithm=_enum(header.get("alg", MISSING), PUBLIC_ALGS),
                      acr=_enum(claims.get("acr", MISSING), PUBLIC_ACR))
        auth = claims.get(AUTH)
        result["plan"] = _enum(auth.get("chatgpt_plan_type", MISSING) if isinstance(auth, dict) else MISSING, PUBLIC_PLANS)
        amr = claims.get("amr", MISSING)
        result["amr"] = _describe(amr)
        if isinstance(amr, list):
            result["amr"]["methods"] = [_enum(item, PUBLIC_AMR) for item in amr]
            result["amr"]["all_methods_strings"] = all(type(item) is str for item in amr)
    except ClaimsError as error:
        result.update(parse_status="invalid", error_code=error.args[0])
    return result


def _metadata_summary(metadata):
    if metadata is None:
        return {"present": False, "parse_status": "absent", "authority": "caller_metadata_not_verified_token_claims"}
    try:
        if not isinstance(metadata, Mapping):
            raise ClaimsError("metadata_must_be_object")
        raw = json.dumps(metadata, ensure_ascii=False, allow_nan=False, separators=(",", ":")).encode()
        value = _parse(raw)
        return {"present": True, "parse_status": "parsed", "authority": "caller_metadata_not_verified_token_claims",
                "fields": _describe(value, names=META_FIELDS)}
    except ClaimsError as error:
        return {"present": True, "parse_status": "invalid", "error_code": error.args[0]}
    except (ValueError, TypeError, UnicodeError, RecursionError) as error:
        return {"present": True, "parse_status": "invalid", "error_code": "metadata_not_finite_json"}


def _relation(first, second):
    if not first or not second or not first.get("comparable") or not second.get("comparable"):
        return "not_comparable"
    return "matched" if first["utf8_sha256"] == second["utf8_sha256"] else "mismatch"


def _metadata_hint(metadata_report, names):
    fields = metadata_report.get("fields", {}).get("fields", {})
    candidates = []
    for name in names:
        value = fields.get(name, {})
        if value.get("type") == "string" and value.get("utf8_bytes", 0) > 0:
            candidates.append(value["utf8_sha256"])
    identity = fields.get("identity", {}).get("fields", {})
    for name in names:
        value = identity.get(name, {})
        if value.get("type") == "string" and value.get("utf8_bytes", 0) > 0:
            candidates.append(value["utf8_sha256"])
    if not candidates:
        return {"state": "not_comparable"}
    if len(set(candidates)) != 1:
        return {"state": "conflicting_metadata_candidates", "candidate_count": len(candidates)}
    return {"state": "comparable_hint", "utf8_sha256": candidates[0], "candidate_count": len(candidates)}


def _client_hint(token_report, field):
    root = token_report.get("claims", {}).get("fields", {})
    names = root.get(field, {})
    auth = root.get(AUTH, {}).get("fields", {}).get(field, {})
    candidates = [value["utf8_sha256"] for value in (names, auth) if value.get("type") == "string" and value.get("utf8_bytes", 0) > 0]
    if not candidates:
        return {"state": "not_comparable"}
    if len(set(candidates)) != 1:
        return {"state": "conflicting_claim_candidates", "candidate_count": len(candidates)}
    return {"state": "comparable_hint", "utf8_sha256": candidates[0], "candidate_count": len(candidates)}


def summarize_credential_claims(access_token, id_token=None, metadata=None, *, now=None):
    """Return only redacted in-memory evidence; malformed tokens are reportable."""
    now = time.time() if now is None else now
    if type(now) not in (int, float) or not math.isfinite(now) or not 0 <= now <= 9007199254740991:
        raise ClaimsError("invalid_comparison_time")
    access = _token_summary(access_token, now)
    identity = _token_summary(id_token, now, optional=True)
    meta = _metadata_summary(metadata)
    owner_a, owner_i = access.get("owner", {}), identity.get("owner", {})
    hints = {"workspace": _metadata_hint(meta, ("account_id", "workspace_id", "chatgpt_account_id")),
             "stable_user": _metadata_hint(meta, ("user_id", "chatgpt_user_id"))}
    comparisons = {"access_vs_id": {name: _relation(owner_a.get(name), owner_i.get(name)) for name in ("stable_user", "workspace")}}
    for source_name, owner in (("access", owner_a), ("id", owner_i)):
        comparisons[source_name+"_vs_metadata"] = {}
        for name, hint in hints.items():
            actual = owner.get(name, {})
            comparisons[source_name+"_vs_metadata"][name] = (
                "not_comparable" if not actual.get("comparable") or hint.get("state") != "comparable_hint"
                else "matched" if actual["utf8_sha256"] == hint["utf8_sha256"] else "mismatch")
    stored_client = _metadata_hint(meta, ("client_id",))
    client_fields = {source: {field: _client_hint(token, field) for field in ("client_id", "azp", "app_id")}
                     for source, token in (("access", access), ("id", identity))}
    client_comparisons = {}
    for source, fields in client_fields.items():
        client_comparisons[source] = {field: ("not_comparable" if stored_client["state"] != "comparable_hint" or hint["state"] != "comparable_hint"
            else "matched" if hint["utf8_sha256"] == stored_client["utf8_sha256"] else "mismatch") for field, hint in fields.items()}
    return {"schema": SCHEMA, "comparison_time_seconds": now, "signature_verified": False,
            "hash_basis": "SHA256 canonical JSON sorted object keys, normalized string escapes, original numeric lexemes; UTF8 hashes explicitly separate",
            "interpretation": "claim differences are observations, not proof of account age, risk flags, permissions, quota, model capability, or routing",
            "access_token": access, "id_token": identity, "metadata": meta, "metadata_identity_hints": hints, "identity_comparisons": comparisons,
            "client_identity_hints": client_fields, "stored_client_hint": stored_client, "client_vs_stored_metadata": client_comparisons,
            "client_comparison_scope": "distinct claim fields are compared separately; azp/client_id/app_id are not presumed equivalent or authorized"}


def _flatten(summary):
    result = {}
    def visit(value, path):
        if not isinstance(value, dict):
            return
        if "present" in value and "type" in value:
            result[path] = {key: value[key] for key in ("present", "state", "type", "canonical_sha256", "canonical_json_bytes") if key in value}
        for name, item in value.get("fields", {}).items():
            visit(item, path+"/"+name)
        for index, item in enumerate(value.get("items", [])):
            visit(item, path+"/"+str(index))
    for token in ("access_token", "id_token"):
        for tree in ("header", "claims"):
            visit(summary[token].get(tree, {}), token+"/"+tree)
    visit(summary["metadata"].get("fields", {}), "metadata")
    return result


def compare_credential_claims(cases, *, now=None):
    """Compare raw in-memory cases, persisting neither credentials nor raw claims.

    Synthetic qN/gN/newN/oldN/caseN/before/after aliases may appear in reports;
    other caller labels are hashed and represented by positional case IDs.
    """
    if not isinstance(cases, (list, tuple)) or not 1 <= len(cases) <= MAX_CASES:
        raise ClaimsError("invalid_case_count")
    now = time.time() if now is None else now
    rows = []
    for index, case in enumerate(cases):
        if not isinstance(case, Mapping) or set(case) - {"alias", "access_token", "id_token", "metadata"} or "access_token" not in case:
            raise ClaimsError("invalid_case_schema")
        alias = case.get("alias")
        if not isinstance(alias, str) or len(alias) > 128:
            raise ClaimsError("invalid_case_alias")
        try:
            alias_hash = _sha(alias.encode())
        except UnicodeError as error:
            raise ClaimsError("invalid_case_alias") from error
        label = alias if SAFE_ALIAS.fullmatch(alias) else "case-%d" % (index+1)
        summary = summarize_credential_claims(case["access_token"], case.get("id_token"), case.get("metadata"), now=now)
        rows.append({"case_id": index+1, "alias": label, "alias_sha256": alias_hash, "summary": summary})
    pairs = []
    for left, right in combinations(rows, 2):
        a, b = left["summary"], right["summary"]
        fields_a, fields_b = _flatten(a), _flatten(b)
        differing = [path for path in sorted(set(fields_a)|set(fields_b)) if fields_a.get(path) != fields_b.get(path)]
        owners_a, owners_b = a["access_token"].get("owner", {}), b["access_token"].get("owner", {})
        token_a, token_b = a["access_token"].get("token_sha256"), b["access_token"].get("token_sha256")
        pairs.append({"left_case_id": left["case_id"], "right_case_id": right["case_id"],
                      "access_token_bytes_relation": "not_comparable" if not token_a or not token_b else "matched" if token_a == token_b else "different",
                      "access_stable_user": _relation(owners_a.get("stable_user"), owners_b.get("stable_user")),
                      "access_workspace": _relation(owners_a.get("workspace"), owners_b.get("workspace")),
                      "differing_field_count": len(differing), "differences_complete": len(differing) <= MAX_DIFFS,
                      "differences": [{"path": path, "left": fields_a.get(path, _describe()), "right": fields_b.get(path, _describe())} for path in differing[:MAX_DIFFS]],
                      "parse_status": {"left_access": a["access_token"]["parse_status"], "right_access": b["access_token"]["parse_status"],
                                       "left_id": a["id_token"]["parse_status"], "right_id": b["id_token"]["parse_status"]}})
    return {"schema": COMPARE_SCHEMA, "comparison_time_seconds": now, "case_count": len(rows), "cases": rows,
            "comparisons": pairs, "signature_verified": False, "permission_or_quota_inference": False,
            "raw_credentials_retained": False, "raw_claims_retained": False}


__all__ = ["ClaimsError", "summarize_credential_claims", "compare_credential_claims"]
