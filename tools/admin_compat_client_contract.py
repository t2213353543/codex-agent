"""Run the unmodified Loophub exporter against a parent Go loopback fixture.

The actual curl_cffi request and multipart implementations are used. Only the
exporter's OpenAI privacy destination is replaced with the fake upstream.
"""

import hashlib
import importlib.util
import json
import logging
from pathlib import Path
import sys
import urllib.parse


def require_ok(result, action):
    if not isinstance(result, dict) or result.get("ok") is not True:
        raise AssertionError(f"real exporter failed {action}: {result}")
    return result


def main():
    params = json.load(sys.stdin)
    allowed = {params["base"], params["upstream"]}
    for base in allowed:
        target = urllib.parse.urlsplit(base)
        if target.scheme != "http" or target.hostname != "127.0.0.1":
            raise AssertionError("contract is restricted to parent loopback servers")

    source = Path(params["source"]).resolve()
    spec = importlib.util.spec_from_file_location("admin_compat_real_exporter", source)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    logging.disable(logging.CRITICAL)
    client = module._import_cffi().Session(trust_env=False)
    module._import_cffi_mime()  # Require the real multipart dependency too.

    class LoopbackOnly:
        def __getattr__(self, method):
            call = getattr(client, method)

            def guarded(url, **kwargs):
                target = urllib.parse.urlsplit(url)
                origin = f"{target.scheme}://{target.netloc}"
                if origin not in allowed:
                    raise AssertionError("exporter attempted a non-fixture destination")
                kwargs["allow_redirects"] = False
                return call(url, **kwargs)

            return guarded

    guarded = LoopbackOnly()
    module._import_cffi = lambda: guarded
    module.OPENAI_PRIVACY_ENDPOINT = params["upstream"] + "/exporter-privacy"
    cfg = {
        "sub2api_url": params["base"], "sub2api_api_key": params["key"],
        "sub2api_group_ids": [params["group_id"]], "sub2api_concurrency": 4,
        "sub2api_priority": 17, "sub2api_timeout": 5,
        "cpa_url": params["base"], "cpa_mgmt_key": params["key"], "cpa_timeout": 5,
    }
    silent = lambda *args, **kwargs: None
    email = "real-client@example.invalid"

    def credential(generation, address=email):
        claims = {"exp": 2000000000, "generation": generation,
                  "https://api.openai.com/auth": {
                      "chatgpt_account_id": "synthetic-client-workspace",
                      "chatgpt_user_id": "synthetic-client-user"},
                  "https://api.openai.com/profile": {"email": address}}
        token = module._b64url_json({"alg": "none"}) + "." + module._b64url_json(claims) + ".fixture"
        return {"access_token": token, "id_token": token.removesuffix("fixture") + "identity-fixture",
                "refresh_token": f"synthetic-client-refresh-{generation}",
                "email": address, "account_id": "synthetic-client-workspace"}

    require_ok(module.test_sub2api(cfg), "Sub2API connection")
    require_ok(module.test_cpa(cfg), "CPA connection")
    groups = module.list_sub2api_groups(cfg)
    assert any(item["id"] == params["group_id"] for item in groups)
    first = require_ok(module.export_to_sub2api(credential(1), cfg, log_fn=silent), "Sub2API create and finalize PUT")
    remote_id = first["account_id"]
    assert remote_id and first["operation"] == "created"
    current = module.get_sub2api_account(email, cfg)
    assert str(current["id"]) == str(remote_id)
    assert current["concurrency"] == 4 and current["priority"] == 17
    assert current["group_ids"] == [params["group_id"]]
    assert current["extra"]["privacy_mode"] == "training_off"

    require_ok(module.set_sub2api_account_schedulable(email, cfg, False, account_id=remote_id), "pause")
    assert module.get_cpa_auth_file(email + ".json", cfg)["disabled"] is True
    require_ok(module.set_sub2api_account_schedulable(email, cfg, True, account_id=remote_id), "resume")
    assert module.get_cpa_auth_file(email + ".json", cfg)["disabled"] is False

    cfg["sub2api_priority"] = 9
    updated = require_ok(module.export_to_sub2api(credential(2), cfg, remote_account_id=remote_id, log_fn=silent), "Sub2API update")
    assert updated["operation"] == "updated" and str(updated["account_id"]) == str(remote_id)
    current = module.get_sub2api_account_by_id(remote_id, cfg, account_name=email)
    assert current["priority"] == 9
    require_ok(module.test_sub2api_account(remote_id, cfg, "fixture-model"), "Sub2API SSE model test")
    require_ok(module.clear_sub2api_account_error(email, cfg, account_id=remote_id), "clear error")

    # CPA replaces the same stored account; aliases and numeric IDs remain stable.
    third = credential(3)
    require_ok(module.export_to_cpa(third, cfg, log_fn=silent), "CPA multipart replacement")
    current = module.get_sub2api_account(email, cfg)
    assert str(current["id"]) == str(remote_id) and current["priority"] == 9
    listed = module.get_cpa_auth_file(email + ".json", cfg)
    assert listed and listed["auth_index"] and listed["disabled"] is False
    download = guarded.get(params["base"] + "/v0/management/auth-files/download",
                           params={"name": email + ".json"},
                           headers={"X-Management-Key": params["key"]}, timeout=5)
    assert download.status_code == 200
    exported = download.json()
    assert exported["access_token"] == third["access_token"]
    assert exported["refresh_token"] == third["refresh_token"]
    assert exported["account_id"] == third["account_id"]
    assert exported["id_token"] == third["id_token"]
    bundle = guarded.get(params["base"] + "/api/v1/admin/accounts/data",
                         params={"ids": remote_id, "include_proxies": "false"},
                         headers={"X-API-Key": params["key"]}, timeout=5)
    assert bundle.status_code == 200
    assert bundle.json()["data"]["accounts"][0]["credentials"]["id_token"] == third["id_token"]
    fifth = credential(5)
    require_ok(module.export_to_sub2api(fifth, cfg, remote_account_id=remote_id, log_fn=silent), "Sub2API update after CPA replacement")
    download = guarded.get(params["base"] + "/v0/management/auth-files/download",
                           params={"name": email + ".json"},
                           headers={"X-Management-Key": params["key"]}, timeout=5)
    assert download.status_code == 200
    exported = download.json()
    for field in ("access_token", "refresh_token", "id_token"):
        assert exported[field] == fifth[field], f"stale cross-protocol {field}"
    require_ok(module.delete_cpa_auth_file(email + ".json", cfg), "CPA delete of Sub2API account")
    assert module.get_sub2api_account(email, cfg) is None

    second_email = "cpa-first@example.invalid"
    require_ok(module.export_to_cpa(credential(4, second_email), cfg, log_fn=silent), "CPA multipart create")
    current = module.get_sub2api_account(second_email, cfg)
    assert current and current["id"]
    require_ok(module.delete_sub2api_account(second_email, cfg, account_id=str(current["id"])), "Sub2API delete of CPA account")
    assert module.get_cpa_auth_file(second_email + ".json", cfg) is None
    require_ok(module.delete_cpa_auth_file(second_email + ".json", cfg), "idempotent delete")

    print(json.dumps({"ok": True, "source_sha256": hashlib.sha256(source.read_bytes()).hexdigest(),
                      "source": str(source), "actual_client": "Loophub exporter + curl_cffi",
                      "flows": ["connections", "groups", "Sub2API POST+PUT+GET", "pause/resume",
                                "credential update", "SSE test", "privacy", "CPA multipart replace",
                                "credential download", "bidirectional token synchronization",
                                "bidirectional deletion"]}))


if __name__ == "__main__":
    main()
