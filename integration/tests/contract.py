"""HTTP contract probes for an explicitly declared isolated synthetic service.

This module performs no external OAuth/gateway requests. More fault-injection
cases are evidence requirements in the acceptance runner, never assumed passes.
"""
from __future__ import annotations

import http.cookiejar
import importlib.util
import json
from pathlib import Path
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid


class Client:
    def __init__(self, base: str, key: str = ""):
        self.base, self.key = base.rstrip("/"), key
        self.cookies = http.cookiejar.CookieJar()
        self.opener = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(self.cookies))

    def call(self, path: str, method: str = "GET", body=None, authenticated: bool = True):
        headers = {"Accept": "application/json"}
        if authenticated and self.key:
            headers["Authorization"] = "Bearer " + self.key
        data = None
        if body is not None:
            data = json.dumps(body).encode()
            headers["Content-Type"] = "application/json"
        request = urllib.request.Request(self.base + path, data=data, method=method, headers=headers)
        try:
            response = self.opener.open(request, timeout=15)
        except urllib.error.HTTPError as error:
            response = error
        with response:
            raw = response.read(4 * 1024 * 1024)
            try:
                value = json.loads(raw)
            except (ValueError, UnicodeDecodeError):
                value = None
            return response.status, value, dict(response.headers)


def validate_environment(environment: dict, base_url: str) -> None:
    if environment.get("purpose") != "codex-agent-integration" or environment.get("synthetic_data") is not True:
        raise ValueError("environment must declare purpose=codex-agent-integration and synthetic_data=true")
    if environment.get("base_url", "").rstrip("/") != base_url.rstrip("/"):
        raise ValueError("base_url does not match the isolated environment manifest")
    parsed = urllib.parse.urlparse(base_url)
    if parsed.scheme not in ("http", "https") or parsed.hostname not in ("127.0.0.1", "localhost", "::1"):
        raise ValueError("contract tests require a loopback URL (SSH tunnel is supported)")
    database, redis = environment.get("database", {}), environment.get("redis", {})
    if not database.get("name") or not database.get("user") or not redis.get("namespace"):
        raise ValueError("independent database name/user and Redis namespace are required")


def run(environment: dict, base_url: str, key: str = "") -> list[dict]:
    validate_environment(environment, base_url)
    checks = []

    def case(name, callback):
        started = time.perf_counter()
        try:
            detail = callback()
            status = "pass"
        except Exception as error:
            # Never include a response body: it might contain a leaked secret.
            status, detail = "fail", f"{type(error).__name__}: {error}"
        checks.append({"id": name, "status": status, "detail": detail,
                       "duration_seconds": round(time.perf_counter() - started, 3)})

    def health():
        status, value, _ = Client(base_url).call("/health", authenticated=False)
        assert status == 200, f"health returned HTTP {status}"
        assert isinstance(value, dict), "health response is not a JSON object"
        return "GET /health returned HTTP 200 and JSON"

    def denied():
        for path in ("/api/accounts", "/api/proxies", "/api/logs", "/api/settings", "/api/overview"):
            status, _, _ = Client(base_url).call(path, authenticated=False)
            assert status in (401, 403), f"unauthenticated {path} returned HTTP {status}"
        return "five management endpoints reject missing credentials"

    def invalid_key():
        status, _, _ = Client(base_url, "SYNTHETIC-INVALID-ADMIN-KEY").call("/api/accounts")
        assert status in (401, 403), f"invalid key returned HTTP {status}"
        return "invalid management credentials are rejected"

    case("api.health", health)
    case("api.unauthorized", denied)
    case("api.invalid_auth", invalid_key)

    spec = importlib.util.spec_from_file_location("contract_synthetic", Path(__file__).parents[1] / "fixtures/synthetic.py")
    synthetic = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(synthetic)
    prefix = "acceptance-" + uuid.uuid4().hex[:12]
    admin = Client(base_url, key)
    created_accounts, created_groups, created_proxies = set(), set(), set()

    def get(path, expected=200):
        status, value, headers = admin.call(path)
        assert status == expected, f"GET {path.split('?')[0]} returned HTTP {status}, expected {expected}"
        return value

    def post(path, value, expected=(200, 201)):
        status, result, _ = admin.call(path, "POST", value)
        assert status in expected, f"POST {path} returned HTTP {status}, expected {expected}"
        return result

    def import_raw(raw, **options):
        result = post("/api/accounts", {"raw": raw, **options})
        assert isinstance(result, dict), "import response must be an object"
        for item in result.get("results", []):
            if item.get("id") and item.get("status") in ("added", "replaced"):
                created_accounts.add(item["id"])
        assert result.get("imported") == result.get("added", 0) + result.get("replaced", 0), "imported != added + replaced"
        return result

    def require_added(result, count):
        assert result.get("added") == count and result.get("failed") == 0, f"expected added={count}, failed=0"
        ids = [row.get("id") for row in result.get("results", []) if row.get("status") == "added"]
        assert len(ids) == count and len(set(ids)) == count and all(ids), "added results require unique stable IDs"
        return ids

    def account_detail(identifier):
        result = get("/api/accounts/" + urllib.parse.quote(identifier, safe=""))
        assert isinstance(result.get("account"), dict), "detail requires account object"
        return result["account"]

    def one_account(suffix):
        row = {"name": prefix + "-" + suffix, "access_token": synthetic.jwt(prefix + "-" + suffix),
               "refresh_token": "SYNTHETIC-UNUSABLE-REFRESH", "expires_at": synthetic.EXPIRY}
        identifier = require_added(import_raw(json.dumps(row)), 1)[0]
        return identifier, account_detail(identifier)

    def cookie_session():
        session = Client(base_url)
        status, _, _ = session.call("/api/session", "POST", {"admin_key": "SYNTHETIC-INVALID-ADMIN-KEY"}, authenticated=False)
        assert status == 401, f"invalid session login returned HTTP {status}"
        status, value, headers = session.call("/api/session", "POST", {"admin_key": key}, authenticated=False)
        assert status == 200 and value.get("authenticated") is True, f"session login failed with HTTP {status}"
        assert key not in json.dumps(value), "login echoed admin key"
        cookie = next((v for k, v in headers.items() if k.lower() == "set-cookie"), "").lower()
        assert "httponly" in cookie and "samesite=" in cookie, "session cookie must set HttpOnly and SameSite"
        if base_url.startswith("https:"):
            assert "secure" in cookie, "HTTPS session cookie must set Secure"
        status, _, headers = session.call("/api/accounts?limit=1", authenticated=False)
        assert status == 200, f"cookie-only account access returned HTTP {status}"
        assert "no-store" in next((v.lower() for k, v in headers.items() if k.lower() == "cache-control"), ""), "management responses require no-store"
        status, value, _ = session.call("/api/session", authenticated=False)
        assert status == 200 and value.get("authenticated") is True, "session introspection failed"
        status, _, _ = session.call("/api/session", "DELETE", authenticated=False)
        assert status in (200, 204), "logout failed"
        status, _, _ = session.call("/api/accounts", authenticated=False)
        assert status == 401, "logged-out session still accesses accounts"
        return "invalid login denied; cookie-only auth, cookie flags, no-store and logout passed"

    def jwt_collision():
        tokens = [synthetic.jwt(prefix + "-jwt-" + str(i)) for i in range(2)]
        assert tokens[0][:16] == tokens[1][:16] and tokens[0] != tokens[1]
        raw = "\n".join(tokens)
        before = get("/api/accounts?limit=1")
        preview = post("/api/accounts/import/preview", {"raw": raw})
        assert preview.get("dry_run") is True and preview.get("added") == 2, "preview must predict two additions"
        after = get("/api/accounts?limit=1")
        assert before == after, "preview changed account state"
        ids = require_added(import_raw(raw), 2)
        rows = [account_detail(identifier) for identifier in ids]
        assert rows[0]["id"] != rows[1]["id"], "JWT identities share an account ID"
        repeat = import_raw(raw)
        assert repeat.get("added") == 0 and repeat.get("duplicate") == 2, "repeated token import must report two duplicates"
        return "two JWTs sharing their first 16 characters keep distinct IDs; preview is read-only and replay reports duplicates"

    def structured_formats():
        rows = [
            {"name": prefix + "-json-object", "tokens": {"access_token": synthetic.jwt(prefix + "-json-object"), "expires_at": synthetic.EXPIRY}},
            [{"name": prefix + "-array-" + str(i), "credentials": {"access_token": synthetic.jwt(prefix + "-array-" + str(i)), "expires_at": synthetic.EXPIRY}} for i in range(2)],
        ]
        require_added(import_raw(json.dumps(rows[0], indent=2)), 1)
        require_added(import_raw(json.dumps(rows[1], indent=2)), 2)
        raw = "\n".join(json.dumps({"name": prefix + "-jsonl-" + str(i), "access_token": synthetic.jwt(prefix + "-jsonl-" + str(i))}) for i in range(2))
        require_added(import_raw(raw), 2)
        return "pretty multiline JSON tokens object, credentials array and JSONL preserve the intended record count"

    def invalid_line():
        row = json.dumps({"name": prefix + "-invalid-a", "access_token": synthetic.jwt(prefix + "-invalid-a")})
        raw = row + '\n{"access_token":\n'
        status, result, _ = admin.call("/api/accounts/import/preview", "POST", {"raw": raw})
        assert status in (200, 400), f"malformed JSONL returned HTTP {status}"
        assert isinstance(result, dict), "invalid import must return a structured error"
        if status == 200:
            assert result.get("failed", 0) >= 1, "malformed JSON must not become a token"
            failures = [r for r in result.get("results", []) if r.get("status") in ("failed", "invalid", "error")]
            assert any(r.get("index") == 2 or r.get("line") == 2 or r.get("line_number") == 2 for r in failures), "invalid row 2 is not identifiable (contract positions are 1-based)"
        else:
            message = str(result.get("error", {}).get("message", ""))
            assert "2" in message and any(label in message.lower() for label in ("line", "行")), "format error must identify line 2"
        status, result, _ = admin.call("/api/accounts/import/preview", "POST", {"raw": '{\n  "tokens": {\n    "access_token":\n  }\n}'})
        assert status == 400 or (status == 200 and result.get("failed", 0) > 0), "malformed whole JSON became a credential"
        return "malformed JSON is rejected and the second JSONL line is identifiable"

    def same_name_identity():
        name = prefix + "-same-name"
        result = import_raw(json.dumps([{"name": name, "access_token": synthetic.jwt(prefix + "-same-name-user-" + str(i)),
                                        "expires_at": synthetic.EXPIRY - i * 100} for i in range(2)]))
        if result.get("added") == 2:
            ids = require_added(result, 2)
            assert all(account_detail(identifier).get("name") == name for identifier in ids), "display names changed unexpectedly"
            status, _, _ = admin.call("/api/accounts/" + urllib.parse.quote(name))
            assert status == 409, f"ambiguous name returned HTTP {status}, expected 409"
            return "same display name keeps two identities; name-only lookup reports 409"
        assert result.get("added") == 1 and result.get("failed") == 1 and result.get("replaced") == 0, "same-name identities must be distinct or explicitly rejected without overwrite"
        failures = [row for row in result.get("results", []) if row.get("status") == "failed"]
        assert len(failures) == 1 and failures[0].get("code") == "name_conflict", "name collision requires an explicit name_conflict result"
        identifier = next(row["id"] for row in result["results"] if row.get("status") == "added")
        assert account_detail(identifier).get("expires_at") == synthetic.EXPIRY, "rejected second identity overwrote the first account"
        return "second identity sharing the display name is explicitly rejected as name_conflict; first account remains unchanged"

    def identity_update():
        identifier, initial = one_account("identity")
        updated = {"name": prefix + "-identity", "access_token": synthetic.jwt(prefix + "-identity", revision=2),
                   "expires_at": synthetic.EXPIRY - 60}
        duplicate = import_raw(json.dumps(updated))
        assert duplicate.get("added") == 0 and duplicate.get("replaced") == 0, "credential update without replace must not create or overwrite an account"
        assert duplicate.get("duplicate") == 1 or (duplicate.get("failed") == 1 and duplicate.get("results", [{}])[0].get("code") == "name_conflict"), "credential conflict must be explicitly reported"
        replaced = import_raw(json.dumps(updated), replace=True)
        assert replaced.get("replaced") == 1 and replaced.get("added") == 0, "explicit identity update must replace one account"
        assert replaced["results"][0].get("id") == identifier, "identity replacement changed stable account ID"
        latest = account_detail(identifier)
        assert latest.get("version", 0) > initial.get("version", 0), "identity update did not increment version"
        assert latest.get("expires_at") == updated["expires_at"], "identity update expiry was not persisted"
        return "same-name credential update requires explicit replacement and keeps its ID while updating expiry/version; cross-name upstream identity deduplication is not inferred"

    def version_conflict():
        identifier, current = one_account("version")
        version = current.get("version")
        assert isinstance(version, int) and version >= 1, "account version missing"
        path = "/api/accounts/" + urllib.parse.quote(identifier)
        status, updated, _ = admin.call(path, "PUT", {"version": version, "notes": "Synthetic first browser edit"})
        assert status == 200 and updated.get("version", 0) > version, "first edit must increment version"
        status, result, _ = admin.call(path, "PUT", {"version": version, "notes": "Synthetic stale browser edit"})
        assert status == 409, f"stale edit returned HTTP {status}, expected 409"
        assert result.get("error", {}).get("code") == "version_conflict", "stale edit must return version_conflict"
        assert account_detail(identifier).get("notes") == "Synthetic first browser edit", "stale edit overwrote the first edit"
        return "two readers sharing one version produce one accepted edit and one 409; accepted value remains"

    def proxy_redaction():
        body = {"name": prefix + "-proxy", "type": "http", "host": "192.0.2.10", "port": 9,
                "username": "synthetic", "password": "SYNTHETIC-UNUSABLE-PASSWORD", "enabled": False}
        result = post("/api/proxies", body)
        identifier = result.get("id")
        assert identifier, "proxy create did not return an ID"
        created_proxies.add(identifier)
        assert not result.get("password") and result.get("has_password") is True, "proxy create echoed or lost its password"
        path = "/api/proxies/" + urllib.parse.quote(identifier)
        update = dict(body, password="", version=result["version"])
        status, result, _ = admin.call(path, "PUT", update)
        assert status == 200 and result.get("has_password") is True, "empty password must preserve the previous password"
        update.update(version=result["version"], clear_password=True)
        status, result, _ = admin.call(path, "PUT", update)
        assert status == 200 and result.get("has_password") is False, "clear_password must remove the password"
        return "create/update omit plaintext; empty password preserves it and explicit clear removes it"

    def account_cursor():
        search = urllib.parse.quote(prefix + "-page")
        import_result = import_raw(json.dumps([{"name": prefix + "-page-" + str(i), "access_token": synthetic.jwt(prefix + "-page-" + str(i))} for i in range(3)]))
        expected_ids = set(require_added(import_result, 3))
        seen, cursor = set(), ""
        for _ in range(4):
            result = get(f"/api/accounts?limit=1&search={search}&cursor={urllib.parse.quote(cursor)}")
            rows = result.get("accounts")
            assert isinstance(rows, list) and len(rows) <= 1, "account limit was ignored"
            assert result.get("total_kind") == "not_computed", "account pagination should not issue a full count"
            ids = {row["id"] for row in rows}
            assert not seen.intersection(ids), "cursor returned duplicate account IDs"
            seen.update(ids)
            if not result.get("has_more"):
                break
            cursor = result.get("next_cursor", "")
            assert cursor, "has_more requires next_cursor"
        assert seen == expected_ids, "cursor pagination omitted or invented accounts"
        return "filtered limit=1 cursor pages contain every synthetic account exactly once"

    def redacted():
        sensitive = {"access_token", "refresh_token", "id_token", "password", "admin_key"}

        def inspect(value):
            if isinstance(value, dict):
                for field, item in value.items():
                    assert not (field in sensitive and item), f"sensitive field {field} exposed"
                    inspect(item)
            elif isinstance(value, list):
                for item in value:
                    inspect(item)

        for path in ("/api/accounts?limit=25", "/api/proxies"):
            status, value, _ = Client(base_url, key).call(path)
            assert status == 200, f"authenticated {path} returned HTTP {status}"
            inspect(value)
        return "account/proxy lists omit credential contents"

    if key:
        case("api.cookie_session", cookie_session)
        case("api.jwt_collision", jwt_collision)
        case("api.import_formats", structured_formats)
        case("api.import_invalid_line", invalid_line)
        case("api.same_name_identity", same_name_identity)
        case("api.identity_update", identity_update)
        case("api.version_conflict", version_conflict)
        case("api.proxy_password", proxy_redaction)
        case("api.account_cursor", account_cursor)
        case("api.sensitive_fields", redacted)

        def cleanup():
            failures = []
            for kind, identifiers in (("accounts", created_accounts), ("groups", created_groups), ("proxies", created_proxies)):
                for identifier in identifiers:
                    status, _, _ = admin.call(f"/api/{kind}/" + urllib.parse.quote(identifier), "DELETE")
                    if status not in (200, 204, 404):
                        failures.append(f"{kind}: HTTP {status}")
            assert not failures, "synthetic fixture cleanup failed: " + ", ".join(failures)
            return "only IDs created by this run were deleted; no gateway requests were sent"

        case("api.cleanup", cleanup)
    else:
        checks.append({"id": "api.sensitive_fields", "status": "blocked",
                       "detail": "isolated admin key/session was not supplied"})
    return checks
