"""Read-only dedicated CLI key preflight; never prints credentials or response bodies."""
from __future__ import annotations

from collections import Counter
from datetime import datetime, timezone
import json
from pathlib import Path
import socket
import time
from urllib import error, request


ROOT = Path(__file__).resolve().parents[1]
BASE = "http://43.225.196.170:18879"
BODY_LIMIT = 4 * 1024 * 1024
TIMEOUT_SECONDS = 20
REPORT = ROOT / ".local/native-live-preflight.json"


class NoRedirect(request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


def read_key(path: Path) -> str:
    value = path.read_text(encoding="utf-8-sig").strip()
    if not value or len(value) > 16384 or "\r" in value or "\n" in value:
        raise ValueError("invalid_local_key_file")
    return value


def get_once(path: str, key: str, direct: bool) -> tuple[dict, object | None]:
    if path not in {"/v1/models?client_version=0.155.0", "/api/accounts"}:
        raise ValueError("endpoint_not_allowlisted")
    proxy = request.ProxyHandler({}) if direct else request.ProxyHandler()
    opener = request.build_opener(proxy, NoRedirect())
    req = request.Request(BASE + path, method="GET", headers={"Authorization": "Bearer " + key,
                          "Accept": "application/json", "User-Agent": "codex-agent-readonly-preflight/1"})
    started = time.monotonic()
    result = {"route": "direct" if direct else "system_proxy_config", "method": "GET"}
    payload = None
    try:
        with opener.open(req, timeout=TIMEOUT_SECONDS) as response:
            result["http_status"] = response.status
            raw = response.read(BODY_LIMIT + 1)
            if len(raw) > BODY_LIMIT:
                result["outcome"] = "response_limit"
            else:
                try:
                    payload = json.loads(raw)
                    result["outcome"] = "json_response"
                except (ValueError, UnicodeError):
                    result["outcome"] = "invalid_json"
    except error.HTTPError as failure:
        # The body can contain identifiers or upstream details. It is neither
        # retained nor echoed; HTTP responses never trigger another attempt.
        result.update(http_status=failure.code, outcome="http_error")
        failure.close()
    except (error.URLError, TimeoutError, socket.timeout, ConnectionError, OSError):
        result["outcome"] = "network_error"
    result["duration_seconds"] = round(time.monotonic() - started, 3)
    return result, payload


def bounded_get(path: str, key: str) -> tuple[list[dict], object | None]:
    first, payload = get_once(path, key, direct=False)
    attempts = [first]
    if first["outcome"] == "network_error":
        second, payload = get_once(path, key, direct=True)
        attempts.append(second)
    return attempts, payload


def summarize_accounts(payload: object) -> dict:
    if not isinstance(payload, dict) or not isinstance(payload.get("accounts"), list):
        return {"parsed": False}
    accounts = payload["accounts"]
    if any(not isinstance(account, dict) for account in accounts):
        return {"parsed": False}
    statuses = {"ok", "unverified", "error", "disabled", "expired", "quota_exceeded", "busy"}
    credential_statuses = {"unverified", "valid", "invalid", "error", "refresh_failed", "failed"}
    now = time.time()
    def count(field: str, allowed: set[str]) -> dict:
        values = (account.get(field) for account in accounts)
        return dict(sorted(Counter(value if isinstance(value, str) and value in allowed else "unknown"
                                   for value in values).items()))
    return {
        "parsed": True,
        "observed_count": len(accounts),
        "total_count": len(accounts) if payload.get("has_more") is False else None,
        "has_more": payload.get("has_more"),
        "enabled_count": sum(account.get("enabled") is True for account in accounts),
        "has_refresh_token_count": sum(account.get("has_refresh_token") is True for account in accounts),
        "expired_access_token_count": sum(isinstance(account.get("expires_at"), (int, float))
                                          and 0 < account["expires_at"] <= now for account in accounts),
        "status_counts": count("status", statuses),
        "credential_status_counts": count("credential_status", credential_statuses),
        "refresh_status_scope": "credential_status metadata only; no refresh operation requested",
        "dedicated_group_usable_count": None,
        "dedicated_group_count_scope": "No key-to-group mapping is returned by this authorized endpoint; no account identity was inferred.",
    }


def main() -> int:
    report = {"schema": "codex-agent-dedicated-readonly-preflight-v1", "started_at": datetime.now(timezone.utc).isoformat(),
              "scope": "dedicated fifth-group models GET and aggregate account metadata GET only",
              "inference_requested": False, "refresh_requested": False, "configuration_writes": False,
              "redirects_followed": False, "proxy_configuration_changed": False,
              "system_http_proxy_configured": bool(request.getproxies().get("http"))}
    for label, path, key_path in (
        ("dedicated_models", "/v1/models?client_version=0.155.0", ROOT / ".local/workbench-live/cli-key"),
        ("accounts", "/api/accounts", ROOT / ".local/production-client-key"),
    ):
        try:
            key = read_key(key_path)
        except (OSError, ValueError):
            report[label] = {"local_key_available": False}
            continue
        attempts, payload = bounded_get(path, key)
        entry = {"local_key_available": True, "attempts": attempts}
        if label == "accounts":
            entry["summary"] = summarize_accounts(payload)
        else:
            models = payload.get("models") if isinstance(payload, dict) else None
            if not isinstance(models, list) and isinstance(payload, dict):
                models = payload.get("data")
            entry.update(models_endpoint_usable=attempts[-1].get("http_status") == 200 and isinstance(models, list),
                         model_count=len(models) if isinstance(models, list) else None,
                         inference_usability_proven=False)
        report[label] = entry
    report["completed_at"] = datetime.now(timezone.utc).isoformat()
    REPORT.write_text(json.dumps(report, indent=2) + "\n", encoding="utf-8")
    print(json.dumps(report, ensure_ascii=True))
    return 0 if report.get("dedicated_models", {}).get("models_endpoint_usable") else 1


if __name__ == "__main__":
    raise SystemExit(main())
