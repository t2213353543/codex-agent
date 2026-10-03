"""Deterministic, deliberately unusable credentials and legacy migration data.

Never reads a real account directory. JWT signatures are intentionally invalid;
all addresses are reserved example/documentation addresses. The generated files
can be removed with their isolated test directory after a run.
"""
from __future__ import annotations

import argparse
import base64
import hashlib
import json
from pathlib import Path

ANCHOR = 1789601400  # Fixed reference instant; use --anchor for current-day tests.
EXPIRY = 4102444800


def jwt(subject: str, workspace: str = "synthetic-workspace", revision: int = 1) -> str:
    def encode(value: dict) -> str:
        return base64.urlsafe_b64encode(json.dumps(value, sort_keys=True).encode()).decode().rstrip("=")
    header = encode({"alg": "HS256", "typ": "JWT"})
    claims = encode({"sub": subject, "exp": EXPIRY, "iat": ANCHOR, "revision": revision,
                     "https://api.openai.com/auth": {"chatgpt_account_id": workspace,
                                                     "chatgpt_user_id": subject}})
    return f"{header}.{claims}.c3ludGhldGljLWludmFsaWQtc2lnbmF0dXJl"


def account(index: int, anchor: int = ANCHOR) -> dict:
    return {"name": f"synthetic-account-{index:05d}",
            "access_token": jwt(f"synthetic-user-{index:05d}"),
            "refresh_token": f"SYNTHETIC-UNUSABLE-REFRESH-{index:05d}",
            "account_id": "synthetic-workspace", "expires_at": EXPIRY,
            "enabled": True, "groups": ["g_synthetic"], "proxies": ["p_synthetic"],
            "concurrency": 1, "weight": 1, "window_days": 30, "created_at": anchor - 86400}


def build(output: Path, account_count: int = 100, log_count: int = 10000,
          anchor: int = ANCHOR) -> dict:
    if not 2 <= account_count <= 100000 or not 0 <= log_count <= 5000000:
        raise ValueError("accounts must be 2..100000; logs must be 0..5000000")
    output.mkdir(parents=True, exist_ok=True)
    accounts = [account(i, anchor) for i in range(account_count)]
    documents = {
        "accounts.json": {"active": accounts[0]["name"], "accounts": accounts},
        "groups.json": {"groups": [{"id": "g_synthetic", "name": "Synthetic group",
                                     "description": "generated fixture", "created_at": anchor - 86400}]},
        "proxies.json": {"proxies": [{"id": "p_synthetic", "name": "Synthetic proxy",
                                       "type": "http", "host": "192.0.2.10", "port": 9,
                                       "username": "synthetic", "password": "SYNTHETIC-UNUSABLE-PASSWORD",
                                       "enabled": False, "created_at": anchor - 86400}]},
        "keys.json": {"keys": []}, "settings.json": {"admin_api": True},
        "import-object.json": {"name": "Synthetic multiline", "tokens": {
            "access_token": jwt("synthetic-multiline"), "account_id": "synthetic-workspace",
            "refresh_token": "SYNTHETIC-UNUSABLE-MULTILINE", "expires_at": EXPIRY}},
        "import-array.json": [{"name": "Same display name", "credentials": {
            "access_token": jwt(f"synthetic-array-{i}"), "expires_at": EXPIRY}} for i in range(2)],
    }
    for name, value in documents.items():
        (output / name).write_text(json.dumps(value, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    token_a, token_b = jwt("synthetic-jwt-a"), jwt("synthetic-jwt-b")
    (output / "import-tokens.txt").write_text(token_a + "\n" + token_b + "\n", encoding="utf-8")
    lines = [json.dumps({"name": "Same display name", "access_token": jwt(f"synthetic-jsonl-{i}"),
                        "expires_at": EXPIRY}) for i in range(2)]
    (output / "import-jsonl.jsonl").write_text("\n".join(lines) + "\n", encoding="utf-8")
    (output / "import-invalid.jsonl").write_text(lines[0] + '\n{"access_token":\n' + lines[1] + "\n", encoding="utf-8")
    (output / "import-identity-update.json").write_text(json.dumps({
        "name": "Renamed same identity", "access_token": jwt("synthetic-jwt-a", revision=2),
        "expires_at": EXPIRY}, indent=2) + "\n", encoding="utf-8")
    totals = {"requests": log_count, "input_tokens": 0, "output_tokens": 0, "cached_tokens": 0,
              "ok": 0, "errors": 0}
    with (output / "logs.jsonl").open("w", encoding="utf-8", newline="\n") as handle:
        for i in range(log_count):
            # Equal timestamps exercise the ID tie-breaker. At least 120 days
            # separates old records from the current window for retention tests.
            when = anchor - (log_count - i) // 2
            if i == 0:
                when = anchor - 120 * 86400
            ok = i % 10 != 0
            row = {"time": when, "account": accounts[i % account_count]["name"],
                   "groups": ["g_synthetic"], "model": "synthetic-model", "endpoint": "responses",
                   "status": "ok" if ok else "error", "http_code": 200 if ok else 429,
                   "latency_ms": 30 + i % 500, "input_tokens": 100,
                   "output_tokens": 25, "cached_tokens": 80,
                   "error": "" if ok else "Synthetic rate-limit test", "ip": "192.0.2.20"}
            handle.write(json.dumps(row, separators=(",", ":")) + "\n")
            totals["input_tokens"] += 100
            totals["output_tokens"] += 25
            totals["cached_tokens"] += 80
            totals["ok" if ok else "errors"] += 1
    files = {path.name: {"sha256": hashlib.sha256(path.read_bytes()).hexdigest(),
                         "bytes": path.stat().st_size}
             for path in sorted(output.iterdir()) if path.is_file() and path.name != "manifest.json"}
    manifest = {"purpose": "codex-agent-integration", "synthetic_data": True, "anchor_unix": anchor,
                "accounts": account_count, "unique_user_identities": account_count,
                "shared_workspace_count": 1, "group_relationships": account_count,
                "proxy_relationships": account_count, "expires_at": EXPIRY,
                "totals": totals, "cache_rate": 0.8 if log_count else None,
                "invalid_import_line": 2, "files": files}
    (output / "manifest.json").write_text(json.dumps(manifest, indent=2) + "\n", encoding="utf-8")
    return manifest


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--accounts", type=int, default=100)
    parser.add_argument("--logs", type=int, default=10000)
    parser.add_argument("--anchor", type=int, default=ANCHOR)
    args = parser.parse_args()
    result = build(args.output, args.accounts, args.logs, args.anchor)
    print(json.dumps({"output": str(args.output.resolve()), "accounts": result["accounts"],
                      "logs": result["totals"]["requests"], "synthetic_data": True}))
