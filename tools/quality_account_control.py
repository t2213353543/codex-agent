"""Bracket one explicitly planned model invocation with fresh usage evidence."""
from __future__ import annotations

import time


def controlled_cell(read_usage, dispatch, persist):
    """Callbacks are called at most twice/once; never retry an unknown result.

    Persist the before observation before invoking the model. The post observation
    runs even if dispatch times out; it does not retroactively change its result.
    No token, account identity, exception text or model output is accepted here.
    """
    result = {"schema": "codex-quality-account-control-v1", "model_dispatch_attempted": False,
              "usage_observations_attempted": 0, "automatic_retries": False}

    def observe(phase):
        result["usage_observations_attempted"] += 1
        began = time.time_ns()
        try:
            value = read_usage()
            if not isinstance(value, dict):
                raise TypeError()
        except Exception as error:
            value = {"eligibility": {"allowed": None, "reason": "usage_observation_failed"},
                     "error_class": type(error).__name__}
        result["usage_" + phase] = value
        result["usage_" + phase + "_time"] = {"started_unix_ns": began, "finished_unix_ns": time.time_ns()}
        persist(result)
        return value

    before = observe("before")
    eligibility = before.get("eligibility")
    if not isinstance(eligibility, dict) or eligibility.get("allowed") is not True:
        result.update(status="skipped", skip_reason="fresh_usage_not_explicitly_allowed")
        persist(result)
        return result

    result.update(model_dispatch_attempted=True, model_started_unix_ns=time.time_ns())
    persist(result)
    try:
        code = dispatch()
        if not isinstance(code, int) or isinstance(code, bool):
            raise TypeError()
        result.update(status="dispatched", model_process_returncode=code)
    except Exception as error:
        result.update(status="dispatch_error", model_error_class=type(error).__name__)
    finally:
        result["model_finished_unix_ns"] = time.time_ns()
        # Post evidence failure must not erase a completed or failed model result.
        observe("after")
    return result
