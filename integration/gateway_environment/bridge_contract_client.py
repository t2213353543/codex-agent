"""Python bridge under test, talking to real loopback Go management handlers."""
import argparse
import base64
import copy
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import socket
import sys
import urllib.parse
from unittest import mock


def check(condition, message):
    if not condition:
        raise AssertionError(message)


def token(generation=1, identity="synthetic-workspace", user="synthetic-user"):
    claims = {"sub": user, "exp": 2000000000 + generation, "https://api.openai.com/auth": {"chatgpt_account_id": identity, "chatgpt_user_id": user, "chatgpt_plan_type": "pro"}}
    return "synthetic." + base64.urlsafe_b64encode(json.dumps(claims).encode()).decode().rstrip("=") + ".signature"


class Source:
    def __init__(self):
        self.token = token()
        self.job = {"id": "synthetic-job", "status": "succeeded", "recharge_status": "success", "recharge_plan_type": "pro5", "has_refresh_token": True, "is_refunded": False, "has_active_refund": False}

    def call(self, path, data=None, method=None):
        check(path.startswith("/api/admin/registrations"), "unexpected fake source route")
        if "/export-sub?" in path:
            return {"accounts": [{"credentials": {"access_token": self.token, "refresh_token": "source-only-synthetic-refresh"}}]}
        if "?" in path:
            return {"items": [copy.deepcopy(self.job)], "total": 1}
        return copy.deepcopy(self.job)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--scenario", required=True)
    scenario = parser.parse_args().scenario
    spec = importlib.util.spec_from_file_location("bridge_under_contract", os.environ["BRIDGE_CONTRACT_BRIDGE"])
    bridge = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(bridge)
    base = os.environ["BRIDGE_CONTRACT_BASE"]
    parsed = urllib.parse.urlsplit(base)
    check(parsed.hostname == "127.0.0.1" and parsed.scheme == "http", "destination must be actual local httptest")
    root = Path(os.environ["BRIDGE_CONTRACT_TEMP"])
    store = Path(os.environ["BRIDGE_CONTRACT_STORE"])
    key = root / "synthetic-client-key"
    key.write_text(os.environ["BRIDGE_CONTRACT_KEY"])
    source = Source()
    calls, checks = [], []
    real_api = bridge.API
    armed = {"conflict": False, "add_rt": False, "snapshot": None}

    class ActualDestination(real_api):
        def call(self, path, data=None, method=None):
            verb = method or ("POST" if data is not None else "GET")
            check(path.startswith("/api/") and "refresh" not in path and "test-stream" not in path, "non-management route refused")
            if armed["conflict"] and path.endswith("/reauthorize"):
                armed["conflict"] = False
                account_path = path.removesuffix("/reauthorize")
                current = super().call(account_path)["account"]
                if armed["add_rt"]:
                    super().call(path, {"version": current["version"], "credentials": {"access_token": token(), "refresh_token": "concurrent-operator-synthetic-refresh", "account_id": "synthetic-workspace"}, "allow_identity_change": False}, "POST")
                else:
                    super().call(account_path, {"version": current["version"], "notes": "concurrent-operator-write"}, "PUT")
                armed["snapshot"] = store.read_bytes()
            result = super().call(path, data, method)
            calls.append((verb, path, copy.deepcopy(data), copy.deepcopy(result)))
            return result

    destination = ActualDestination(base, key.read_text())
    original_connect = socket.create_connection

    def loopback_connect(address, *args, **kwargs):
        check(address == ("127.0.0.1", parsed.port), "non-httptest network request refused")
        return original_connect(address, *args, **kwargs)

    def state():
        return json.loads((root / "state.json").read_text())

    def account_id():
        return state()["accounts"][source.job["id"]]["account_id"]

    def account():
        return destination.call("/api/accounts/" + account_id())["account"]

    def import_once():
        report = bridge.run(apply=True)
        check(report["counts"] == {"added": 1}, "real bridge initial import did not report added")
        current = account()
        check(current["enabled"] is True and current["concurrency"] == 100 and not current["has_refresh_token"], "first import initialization contract differs")
        check(bridge.TAG in current["tags"] and state()["group_id"] in current["groups"], "real import discarded bridge ownership or group")
        writes = [body for verb, path, body, _ in calls if verb == "POST" and path == "/api/accounts"]
        check(len(writes) == 1 and "refresh_token" not in writes[0]["accounts"][0]["credentials"], "bridge forwarded source refresh token")
        checks.append("AT-only first import through real HTTP retained tags and initialized group/configuration")
        return current

    def expected_error(callback, code):
        try:
            callback()
        except bridge.SafeError as error:
            check(str(error) == code, "unexpected real API failure code")
            return
        raise AssertionError("expected actual API failure was accepted")

    expected = {}
    with mock.patch.object(bridge, "ROOT", root), mock.patch.object(bridge, "Path", lambda _: key), mock.patch.object(bridge, "source_api", return_value=(source, "synthetic-source")), mock.patch.object(bridge, "API", return_value=destination), mock.patch.object(bridge.subprocess, "check_output", side_effect=AssertionError("docker forbidden")), mock.patch.object(socket, "create_connection", side_effect=loopback_connect):
        current = import_once()
        id = current["id"]
        path = "/api/accounts/" + id
        accepted_token = source.token
        accepted_refresh = ""
        if scenario == "initial_update":
            destination.call(path, {"version": current["version"], "name": "Operator renamed account", "enabled": False, "concurrency": 7, "priority": 13, "weight": 9}, "PUT")
            before = account()
            source.token = token(2)
            report = bridge.run(apply=True)
            check(report["counts"] == {"reauthorized": 1}, "bridge failed real reauthorize response contract")
            response = [response for _, route, _, response in calls if route.endswith("/reauthorize")][-1]
            check(response.get("ok") is True and response.get("id") == id and response.get("version") == before["version"] + 1, "actual reauthorize response must return ok/id/version")
            after = account()
            for field in ("id", "name", "enabled", "concurrency", "priority", "weight", "groups", "tags"):
                check(after[field] == before[field], "reauthorize changed operator metadata: " + field)
            check(state()["accounts"][source.job["id"]]["token_sha256"] == bridge.digest(source.token), "bridge failed to checkpoint accepted token fingerprint")
            accepted_token = source.token
            checks.extend(["real reauthorize returns ok/id/version and bridge consumes it", "stable ID, rename, disabled state, concurrency, priority, weight and relations preserved"])
        elif scenario == "identity_rejection":
            before_store, before_state = store.read_bytes(), (root / "state.json").read_bytes()
            source.token = token(3, identity="other-synthetic-workspace")
            report = bridge.run(apply=True)
            check(report["counts"] == {"failed": 1} and report["items"][0]["error_code"] == "source_identity_changed", "bridge must reject source identity change")
            check(store.read_bytes() == before_store and (root / "state.json").read_bytes() == before_state, "bridge identity rejection mutated state")
            expected_error(lambda: destination.call(path + "/reauthorize", {"version": current["version"], "allow_identity_change": False, "credentials": {"access_token": source.token, "account_id": "other-synthetic-workspace"}}, "POST"), "http_409")
            check(store.read_bytes() == before_store, "real identity 409 partially wrote accounts")
            checks.extend(["bridge refuses changed workspace identity without a destination write", "real Go identity confirmation gate returns HTTP 409 with byte-identical account storage"])
        elif scenario in ("cas_conflict", "cas_refresh_token_added"):
            before_state = (root / "state.json").read_bytes()
            source.token = token(4)
            armed["conflict"] = True
            armed["add_rt"] = scenario == "cas_refresh_token_added"
            report = bridge.run(apply=True)
            check(report["counts"] == {"failed": 1} and report["items"][0]["error_code"] == "http_409", "real stale-version response must fail bridge delivery")
            check(armed["snapshot"] is not None and store.read_bytes() == armed["snapshot"], "failed reauthorize modified account after concurrent operator commit")
            check((root / "state.json").read_bytes() == before_state, "failed CAS advanced bridge fingerprint checkpoint")
            if armed["add_rt"]:
                accepted_refresh = "concurrent-operator-synthetic-refresh"
                persisted = next(row for row in json.loads(store.read_text())["accounts"] if row["id"] == id)
                check(persisted.get("refresh_token") == accepted_refresh and account()["has_refresh_token"] is True, "bridge stale CAS cleared concurrently added RT")
                checks.append("RT added by real reauthorize after bridge metadata GET: old version returns 409, RT and bridge checkpoint preserved")
            else:
                checks.append("real concurrent PUT causes reauthorize HTTP 409; account and bridge checkpoint remain atomic")
        elif scenario in ("refresh_token_preserved", "reauthorize_existing_rt"):
            raw = {"name": "manual-refresh-owned", "credentials": {"access_token": token(10, identity="manual-workspace"), "refresh_token": "manual-synthetic-refresh", "account_id": "manual-workspace"}, "enabled": False, "tags": [bridge.TAG]}
            created = destination.call("/api/accounts", {"accounts": [raw], "replace": False})["results"][0]
            manual = destination.call("/api/accounts/" + created["id"])["account"]
            check(manual["has_refresh_token"] is True, "synthetic manual fixture lacks refresh token")
            expected[manual["id"]] = {"access_sha256": bridge.digest(token(10, identity="manual-workspace")), "refresh_sha256": bridge.digest("manual-synthetic-refresh")}
            mapping = state()
            mapping["accounts"][source.job["id"]]["account_id"] = manual["id"]
            bridge.atomic(root / "state.json", mapping)
            before_store = store.read_bytes()
            refused = bridge.run(apply=True)
            check(refused["counts"] == {"failed": 1} and refused["items"][0]["error_code"] == "destination_ownership_changed", "bridge must refuse refresh-token-owned destination")
            check(store.read_bytes() == before_store, "bridge modified refresh-token-owned account")
            checks.append("bridge refuses existing refresh-token owner and preserves complete file-store bytes")
            if scenario == "reauthorize_existing_rt":
                response = destination.call("/api/accounts/" + manual["id"] + "/reauthorize", {"version": manual["version"], "credentials": {"access_token": token(11, identity="manual-workspace"), "account_id": "manual-workspace"}, "allow_identity_change": False}, "POST")
                check(response.get("id") == manual["id"] and response.get("version") == manual["version"] + 1, "manual AT-only reauthorize response differs")
                persisted = next(row for row in json.loads(store.read_text())["accounts"] if row["id"] == manual["id"])
                check(not persisted.get("refresh_token"), "explicit replacement unexpectedly mixed prior refresh credentials")
                expected[manual["id"]]["access_sha256"] = bridge.digest(token(11, identity="manual-workspace"))
                expected[manual["id"]]["refresh_sha256"] = bridge.digest("")
                checks.append("observed_explicit_replacement_not_oauth_refresh: omitted RT is cleared by documented replacement semantics")
        else:
            raise AssertionError("unknown local contract scenario")
        expected[id] = {"access_sha256": bridge.digest(accepted_token), "refresh_sha256": bridge.digest(accepted_refresh)}
        evidence = (root / "state.json").read_text() + (root / "latest.json").read_text()
        check(all(secret not in evidence for secret in (source.token, "source-only-synthetic-refresh", key.read_text())), "bridge report/checkpoint leaked raw synthetic credentials")
        checks.append("bridge artifacts omit access/refresh tokens and management key")
    print(json.dumps({"scenario": scenario, "checks": checks, "expected": expected, "destination": "real Go httptest HTTP; file store", "source": "synthetic fake", "upstream_requests": 0}))


if __name__ == "__main__":
    try:
        main()
    except Exception as error:
        print(json.dumps({"status": "fail", "error": str(error) if isinstance(error, AssertionError) else type(error).__name__}))
        sys.exit(1)
