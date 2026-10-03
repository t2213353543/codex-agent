"""Run one fixed, bounded account comparison using caller-owned live adapters.

No capture, subprocess, credential files, retries, or environment discovery occur
here. All I/O is injected. ``save(relative_path, public_record)`` must persist
before returning. Other adapters receive keyword arguments and must obey BOTH
``timeout`` (operation budget) and ``deadline`` (absolute monotonic deadline,
including their cleanup). A Python callback that ignores its deadline cannot be
forcibly interrupted by this synchronous core. ``dispatch`` must issue at most
one model request and must not retry internally.

    read_epoch(alias=..., timeout=..., deadline=...) -> epoch mapping
    read_usage(alias=..., secret=..., phase=..., timeout=..., deadline=...)
        -> native_usage report
    dispatch(cell_id=..., alias=..., secret=..., body=..., head=..., pins=...,
             timeout=..., deadline=...) -> native_request report

Epochs require identity_sha256, user_sha256, token_sha256, workspace_sha256.
The caller derives the stable user hash from the actual credential claims.
Secrets require access_token and account_id; an optional user_id is cross-checked.
``body`` and ``head`` are the original captured bytes, with separately pinned
SHA-256 values. A callback may decode head but must preserve those captured bytes.
"""
from __future__ import annotations

from collections.abc import Mapping
from copy import deepcopy
import hashlib
import math
import re
import time

from quality_account_identity import require_distinct_accounts


SCHEMA = "codex-quality-control-batch-v1"
_HASH = re.compile(r"[0-9a-f]{64}\Z")
_LABEL = re.compile(r"[A-Za-z0-9_-]{1,64}\Z")
_EPOCH_FIELDS = ("identity_sha256", "user_sha256", "token_sha256", "workspace_sha256",
                 "assigned_at", "oauth_updated_at")
_PRIVATE_KEYS = frozenset(("access_token", "refresh_token", "id_token", "authorization",
                           "account_id", "workspace_id", "user_id", "email", "secret"))


class _Rejected(Exception):
    pass


class _PersistenceFailed(Exception):
    pass


def _sha(value):
    return hashlib.sha256(value).hexdigest()


def _number(value):
    return type(value) in (int, float) and math.isfinite(value)


def _error_class(error):
    # Exception messages, and even custom exception class names, are untrusted.
    return "timeout" if isinstance(error, TimeoutError) else "callback_failed"


def _safe(value, hidden, depth=0):
    """Copy JSON evidence without accepting credential fields or private text."""
    if depth > 40:
        return "[DEPTH_LIMIT]"
    if value is None or type(value) in (bool, int):
        return value
    if type(value) is float:
        return value if math.isfinite(value) else None
    if isinstance(value, str):
        for secret in hidden:
            value = value.replace(secret, "[REDACTED]")
        return value
    if isinstance(value, Mapping):
        result = {}
        for key, item in value.items():
            if not isinstance(key, str):
                continue
            clean_key = _safe(key, hidden, depth + 1)
            # native_usage already represents observed IDs by only state/hash.
            # Preserve that evidence while rejecting raw credential fields.
            identity_summary = (key.lower() in ("account_id", "workspace_id", "user_id") and
                isinstance(item, Mapping) and set(item) <= {"state", "sha256", "present"} and
                item.get("state") in ("matched", "mismatch", "invalid", "not_observed", "not_comparable") and
                ("sha256" not in item or isinstance(item["sha256"], str) and _HASH.fullmatch(item["sha256"]) is not None))
            result[clean_key] = ("[REDACTED]" if key.lower() in _PRIVATE_KEYS and not identity_summary
                                 else _safe(item, hidden, depth + 1))
        return result
    if isinstance(value, (list, tuple)):
        return [_safe(item, hidden, depth + 1) for item in value]
    return "[NON_JSON_VALUE]"


def run_control_batch(*, order, epochs, secrets, body, head, pins,
                      read_epoch, read_usage, dispatch, save, deadline,
                      failure_policy="stop", max_requests=None,
                      usage_timeout=35, model_timeout=240, epoch_timeout=25,
                      after_cleanup_reserve=75, cleanup_reserve=5,
                      usage_cleanup_reserve=4, model_cleanup_reserve=10,
                      monotonic=time.monotonic):
    """Return a public receipt; call each planned model cell at most once.

``order`` is a sequence of (cell_id, alias). Repeated aliases are permitted;
distinct aliases must identify distinct users AND identities AND token epochs.
Shared workspaces are permitted. The failure policy is saved before any I/O:
``stop`` aborts remaining cells after the first model failure; ``continue`` runs
the remaining predeclared cells, each with a fresh usage gate. No cell is retried.
An epoch change or persistence failure always ends the batch. ``max_requests``
defaults to len(order) and may only lower that hard bound.
"""
    started = monotonic()
    hidden = set()
    if isinstance(secrets, Mapping):
        for secret in secrets.values():
            if isinstance(secret, Mapping):
                hidden.update(value for value in secret.values() if isinstance(value, str) and value)
    hidden = sorted(hidden, key=len, reverse=True)
    receipt = {"schema": SCHEMA, "started_monotonic": started,
        "deadline_monotonic": deadline if _number(deadline) else None,
        "failure_policy": failure_policy if failure_policy in ("stop", "continue") else "invalid",
        "automatic_retries": False, "model_dispatch_attempts": 0,
        "cases": [], "finished": False, "status": "preflight"}

    def public(value):
        return _safe(value, hidden)

    def persist(path, value):
        try:
            save(path, public(value))
        except Exception:
            raise _PersistenceFailed() from None

    def save_receipt():
        persist("execution.json", receipt)

    try:
        # Normalize without trusting caller-controlled objects in persisted data.
        if not isinstance(order, (list, tuple)) or not order:
            raise _Rejected("invalid_order")
        cells = []
        for item in order:
            if not isinstance(item, (list, tuple)) or len(item) != 2 or any(
                    not isinstance(part, str) or _LABEL.fullmatch(part) is None for part in item):
                raise _Rejected("invalid_order")
            cells.append(tuple(item))
        if len({cell for cell, _ in cells}) != len(cells):
            raise _Rejected("duplicate_cell_id")
        aliases = list(dict.fromkeys(alias for _, alias in cells))
        if not isinstance(epochs, Mapping) or not isinstance(secrets, Mapping) or any(
                alias not in epochs or alias not in secrets for alias in aliases):
            raise _Rejected("missing_account_state")
        frozen_epochs = {alias: deepcopy(epochs[alias]) for alias in aliases}
        frozen_secrets = {alias: deepcopy(secrets[alias]) for alias in aliases}
        if not require_distinct_accounts(frozen_epochs):
            raise _Rejected("account_identity_or_token_collision")
        users = [row.get("user_sha256") for row in frozen_epochs.values()]
        if any(not isinstance(user, str) or _HASH.fullmatch(user) is None for user in users):
            raise _Rejected("stable_user_identity_missing")
        if len(set(users)) != len(users):
            raise _Rejected("stable_user_identity_collision")
        for alias, secret in frozen_secrets.items():
            epoch = frozen_epochs[alias]
            if not isinstance(secret, Mapping) or any(
                    not isinstance(secret.get(key), str) or not secret[key]
                    for key in ("access_token", "account_id")):
                raise _Rejected("credential_missing")
            if _sha(secret["access_token"].encode()) != epoch["token_sha256"] or _sha(
                    secret["account_id"].encode()) != epoch["workspace_sha256"]:
                raise _Rejected("credential_epoch_mismatch")
            if "user_id" in secret and (not isinstance(secret["user_id"], str) or
                    _sha(secret["user_id"].encode()) != epoch["user_sha256"]):
                raise _Rejected("credential_user_mismatch")
        if not isinstance(body, bytes) or not body or not isinstance(head, bytes) or not head or not isinstance(pins, Mapping):
            raise _Rejected("invalid_captured_request")
        frozen_pins = dict(pins)
        if any(not isinstance(frozen_pins.get(key), str) or _HASH.fullmatch(frozen_pins[key]) is None
               for key in ("body_sha256", "head_sha256")) or _sha(body) != frozen_pins["body_sha256"] or _sha(head) != frozen_pins["head_sha256"]:
            raise _Rejected("capture_pin_mismatch")
        limit = len(cells) if max_requests is None else max_requests
        if type(limit) is not int or not 0 <= limit <= len(cells):
            raise _Rejected("invalid_max_requests")
        numbers = (deadline, usage_timeout, model_timeout, epoch_timeout, after_cleanup_reserve,
                   cleanup_reserve, usage_cleanup_reserve, model_cleanup_reserve)
        if any(not _number(number) or number <= 0 for number in numbers) or deadline <= started or not (
                0 < usage_timeout <= 35 and 1 <= model_timeout <= 240):
            raise _Rejected("invalid_deadline_budget")
        if after_cleanup_reserve < usage_timeout + usage_cleanup_reserve + epoch_timeout + cleanup_reserve:
            raise _Rejected("insufficient_after_cleanup_reserve")
        if failure_policy not in ("stop", "continue"):
            raise _Rejected("invalid_failure_policy")
        receipt.update(status="running", order=[{"id": cell, "account_alias": alias} for cell, alias in cells],
            max_model_calls=limit, capture_pins={key: frozen_pins[key] for key in ("body_sha256", "head_sha256")},
            epochs={alias: {key: row.get(key) for key in _EPOCH_FIELDS} for alias, row in frozen_epochs.items()},
            budgets={"usage_timeout": usage_timeout, "model_timeout": model_timeout,
                     "epoch_timeout": epoch_timeout, "after_cleanup_reserve": after_cleanup_reserve,
                     "cleanup_reserve": cleanup_reserve, "usage_cleanup_reserve": usage_cleanup_reserve,
                     "model_cleanup_reserve": model_cleanup_reserve})
        save_receipt()  # The policy and full order exist before all live callbacks.

        def budget(cap, end, reserve=0):
            available = end - monotonic() - reserve
            if available <= 0:
                raise _Rejected("batch_deadline_reached")
            timeout = min(cap, available)
            return timeout, min(end, monotonic() + timeout + reserve)

        def check_epoch(alias, end):
            timeout, call_deadline = budget(epoch_timeout, end)
            try:
                current = read_epoch(alias=alias, timeout=timeout, deadline=call_deadline)
            except Exception:
                raise _Rejected("epoch_observation_failed") from None
            if not isinstance(current, Mapping) or any(current.get(key) != frozen_epochs[alias].get(key) for key in _EPOCH_FIELDS):
                raise _Rejected("credential_epoch_changed")
            if monotonic() > call_deadline:
                raise _Rejected("epoch_callback_deadline_exceeded")
            return {key: current.get(key) for key in _EPOCH_FIELDS}

        def observe(cell, alias, phase, end):
            cell["usage_" + phase + "_attempted"] = False
            began = monotonic()
            try:
                timeout, call_deadline = budget(usage_timeout, end, usage_cleanup_reserve)
                cell["usage_" + phase + "_attempted"] = True
                value = read_usage(alias=alias, secret=deepcopy(frozen_secrets[alias]), phase=phase,
                                   timeout=timeout, deadline=call_deadline)
                if not isinstance(value, Mapping):
                    raise TypeError()
                value = deepcopy(dict(value))
                if monotonic() > call_deadline:
                    value["eligibility"] = {"allowed": None, "reason": "usage_callback_deadline_exceeded"}
            except Exception as error:
                value = {"eligibility": {"allowed": None, "reason": "usage_observation_failed"},
                         "error_class": _error_class(error)}
            cell["usage_" + phase] = value
            cell["usage_" + phase + "_interval"] = {"started_monotonic": began, "finished_monotonic": monotonic()}
            persist(cell["id"] + "/usage-" + phase + ".json", value)
            persist(cell["id"] + "/control.json", cell)
            return value

        for cell_id, alias in cells:
            if receipt["model_dispatch_attempts"] >= limit:
                receipt.update(status="stopped", stop_reason="maximum_model_requests_reached")
                break
            model_end = deadline - after_cleanup_reserve
            # Leave at least one second of model time plus model cleanup.
            before_end = model_end - model_cleanup_reserve - 1
            if monotonic() >= before_end:
                receipt.update(status="stopped", stop_reason="batch_deadline_reached")
                break
            cell = {"id": cell_id, "account_alias": alias, "status": "preflight",
                    "model_dispatch_attempted": False, "automatic_retries": False}
            receipt["cases"].append(cell)
            try:
                cell["identity_before"] = check_epoch(alias, before_end)
                persist(cell_id + "/identity-before.json", cell["identity_before"])
                before = observe(cell, alias, "before", before_end)
                epoch = frozen_epochs[alias]
                live_evidence = (before.get("schema") == "codex-quality-usage-v1" and
                    before.get("method") == "GET" and before.get("endpoint") == "/backend-api/wham/usage" and
                    before.get("protocol_success") is True and before.get("automatic_retries") is False and
                    type(before.get("model_calls")) is int and before["model_calls"] == 0 and
                    type(before.get("native_dispatch_count")) is int and before["native_dispatch_count"] == 1 and
                    before.get("token_sha256") == epoch["token_sha256"] and
                    before.get("workspace_sha256") == epoch["workspace_sha256"])
                eligibility = before.get("eligibility")
                if not live_evidence or not isinstance(eligibility, Mapping) or eligibility.get("allowed") is not True:
                    cell.update(status="skipped", skip_reason="fresh_usage_not_explicitly_allowed" if live_evidence else "fresh_usage_evidence_invalid")
                    persist(cell_id + "/control.json", cell)
                    save_receipt()
                    continue
                # Detect rotation during the usage read before handing out old AT.
                cell["identity_before_dispatch"] = check_epoch(alias, before_end)
                timeout, call_deadline = budget(model_timeout, model_end, model_cleanup_reserve)
                if timeout < 1:
                    raise _Rejected("batch_deadline_reached")
                cell.update(status="dispatching", model_dispatch_attempted=True,
                            model_timeout=timeout, model_deadline_monotonic=call_deadline)
                receipt["model_dispatch_attempts"] += 1
                persist(cell_id + "/control.json", cell)
                save_receipt()
                persistence_failed = False
                try:
                    value = dispatch(cell_id=cell_id, alias=alias, secret=deepcopy(frozen_secrets[alias]),
                                     body=body, head=head, pins=deepcopy(frozen_pins),
                                     timeout=timeout, deadline=call_deadline)
                    if not isinstance(value, Mapping):
                        raise TypeError()
                    cell["model"] = deepcopy(dict(value))
                    cell["status"] = "completed" if value.get("protocol_success") is True else "model_failed"
                    if monotonic() > call_deadline:
                        cell["model_callback_deadline_exceeded"] = True
                        cell["status"] = "model_failed"
                except Exception as error:
                    cell.update(status="model_failed", model_error_class=_error_class(error))
                finally:
                    cell["model_finished_monotonic"] = monotonic()
                    # Even a timeout or model failure is followed by a real read.
                    try:
                        observe(cell, alias, "after", deadline - epoch_timeout - cleanup_reserve)
                    except _PersistenceFailed:
                        persistence_failed = True
                    try:
                        cell["identity_after"] = check_epoch(alias, deadline - cleanup_reserve)
                    except _Rejected as error:
                        cell["post_epoch_error"] = error.args[0]
                if persistence_failed:
                    raise _PersistenceFailed()
                persist(cell_id + "/control.json", cell)
                save_receipt()
                if "post_epoch_error" in cell:
                    raise _Rejected(cell["post_epoch_error"])
                if cell["status"] != "completed" and failure_policy == "stop":
                    receipt.update(status="stopped", stop_reason="model_failure_policy_stop")
                    break
            except _Rejected as error:
                cell.setdefault("stop_reason", error.args[0])
                if cell["status"] == "preflight":
                    cell["status"] = "stopped"
                persist(cell_id + "/control.json", cell)
                raise
        else:
            receipt["status"] = "completed"
    except _Rejected as error:
        receipt.update(status="stopped", stop_reason=error.args[0])
    except _PersistenceFailed:
        receipt.update(status="stopped", stop_reason="persistence_failed")
    except Exception as error:
        receipt.update(status="stopped", stop_reason="runner_error", error_class=_error_class(error))
    finally:
        receipt.update(finished=True, finished_monotonic=monotonic(),
                       deadline_exceeded=_number(deadline) and monotonic() > deadline)
        try:
            save_receipt()
        except _PersistenceFailed:
            receipt.update(status="stopped", stop_reason="persistence_failed")
    return public(receipt)


__all__ = ["run_control_batch"]
