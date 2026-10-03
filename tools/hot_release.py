"""Stage and switch an immutable release through the stable ingress controller.

python tools/hot_release.py --release <package/manifest.json>
No stop-first activation, schema migration, forced drain, or automatic rollback.
"""
import argparse
import hashlib
import inspect
import json
import math
from pathlib import Path, PurePosixPath
import re
import subprocess
import sys

ROOT = Path(__file__).resolve().parents[1]
HASH = re.compile(r"^[a-f0-9]{64}$")
REMOTE_RELEASES = "/opt/codex-agent/releases/"
SHARED = "redis_account_leases_v1"


class HotReleaseError(RuntimeError):
    """A fixed, nonsecret diagnostic suitable for command-line output."""


def require(condition, message):
    if not condition:
        raise HotReleaseError(message)


def read_release(path):
    path = Path(path).resolve()
    try:
        raw = path.read_bytes()
        manifest = json.loads(raw)
    except (OSError, ValueError):
        raise HotReleaseError("release manifest cannot be read") from None
    require(isinstance(manifest, dict), "release manifest must be an object")
    identity = manifest.get("release_id")
    files = manifest.get("files")
    require(isinstance(identity, str) and HASH.fullmatch(identity), "invalid release identity")
    require(isinstance(files, dict) and {"codex-agent", "deploy/reliability_release.py", "deploy/zero_downtime_release.py"} <= files.keys(), "release lacks immutable runtime/controller artifacts")
    for name, digest in files.items():
        member = PurePosixPath(name)
        require(not member.is_absolute() and str(member) == name and ".." not in member.parts and "\\" not in name and isinstance(digest, str) and HASH.fullmatch(digest), "invalid release artifact identity")
    return {"manifest_path": str(path), "release_id": identity,
            "manifest_sha256": hashlib.sha256(raw).hexdigest(), "files": files,
            "remote": REMOTE_RELEASES + identity, "binary_sha256": files["codex-agent"]}


def matches_release(instance, release):
    return (isinstance(instance, dict) and instance.get("release") == release["remote"]
            and instance.get("binary_sha256") == release["binary_sha256"]
            and instance.get("admission_mode") == SHARED)


def instance_identity(instance):
    fields = ("unit", "instance", "pid", "binary_sha256", "listen_port", "admission_mode", "release")
    return {name: instance.get(name) for name in fields}


def validate_status(status):
    require(isinstance(status, dict), "controller status is invalid")
    registry, front = status.get("registry"), status.get("front")
    require(isinstance(registry, dict) and isinstance(front, dict), "active runtime registry or ingress status is missing")
    active, target = registry.get("active"), front.get("active")
    require(isinstance(active, dict) and isinstance(target, dict), "active runtime identity is missing")
    require(type(active.get("pid")) is int and active["pid"] > 0 and isinstance(active.get("binary_sha256"), str) and HASH.fullmatch(active["binary_sha256"]), "active runtime identity is invalid")
    proof = front.get("verified_persisted_target")
    persisted_target_verified = (front.get("last_update_durable") is None
                                 and "last_update_durable" in front
                                 and isinstance(proof, dict) and proof == target
                                 and type(proof.get("revision")) is int
                                 and type(proof.get("held")) is bool)
    require(front.get("last_update_durable") is True or persisted_target_verified, "ingress state durability is unconfirmed; reconcile before switching")
    require(type(target.get("held")) is bool and type(target.get("revision")) is int, "ingress target metadata is invalid")
    if (registry.get("pending") or {}).get("phase") != "switching":
        require(target.get("generation") == active.get("instance") and target.get("address") == "127.0.0.1:" + str(active.get("listen_port")), "registry and ingress target disagree")
    return registry, front


def remote_operation(adapter, payload):
    """Runs inside the adapter's own lock; also exercised with a pure fake."""
    import argparse
    import json
    import math
    import time

    action = payload["action"]
    expected = payload.get("expected_active")
    release = payload["release"]

    def exact_target(actual, wanted):
        return (isinstance(actual, dict) and isinstance(wanted, dict)
                and set(actual) == set(wanted) == {"revision", "generation", "address", "held"}
                and type(actual.get("revision")) is int and actual["revision"] > 0
                and type(wanted.get("revision")) is int and wanted["revision"] > 0
                and type(actual.get("held")) is bool and type(wanted.get("held")) is bool
                and actual == wanted)

    def observed_front():
        observed = dict(adapter.front())
        # Only this locked server-side read can attach the evidence. A null
        # update outcome means startup load, not a failed durability attempt.
        observed.pop("verified_persisted_target", None)
        if "last_update_durable" in observed and observed["last_update_durable"] is None:
            try:
                persisted = json.loads(adapter.INGRESS_STATE.read_text())
            except (OSError, ValueError):
                adapter.require(False, "initial ingress target cannot be verified from disk")
            adapter.require(exact_target(persisted, observed.get("active")), "initial ingress memory/disk targets differ")
            observed["verified_persisted_target"] = persisted
        adapter.require(observed.get("last_update_durable") is True or "verified_persisted_target" in observed, "ingress durability failed or is unobserved")
        return observed

    def confirm_target_persistence(value):
        observed = observed_front()
        before = observed.get("active") or {}
        active = value["active"]
        adapter.require(before.get("generation") == active["instance"] and before.get("address") == "127.0.0.1:" + str(active["listen_port"]), "ingress target changed before persistence confirmation")
        adapter.require(before.get("held") is False, "ingress was held by another operation before activation")
        if observed.get("last_update_durable") is True:
            return
        # This operation has one precise intent: persist the SAME route and
        # hold state under a new revision. It neither activates nor resumes.
        after = dict(before, revision=before["revision"] + 1)
        try:
            confirmed = adapter.front("POST", "/switch", {"expected_revision": before["revision"], "target": after})
        except Exception:
            confirmed = adapter.front()  # One observation, never a second POST.
        adapter.require(exact_target(confirmed.get("active"), after) and confirmed.get("last_update_durable") is True, "same-target persistence outcome unconfirmed; do not retry automatically")

    def snapshot():
        value = adapter.registry()
        active = value["active"]
        live = adapter.runtime(active["unit"])
        adapter.require(live["pid"] == active["pid"] and live["binary_sha256"] == active["binary_sha256"] and live["listen_port"] == active["listen_port"], "active runtime no longer matches registry")
        return {"registry": value, "front": observed_front()}

    def guard(require_candidate=False):
        value = adapter.registry()
        if expected is not None:
            actual = {key: value["active"].get(key) for key in expected}
            adapter.require(actual == expected, "active generation changed; re-read status")
        if require_candidate:
            candidate = value.get("candidate") or {}
            adapter.require(candidate.get("release") == release["remote"] and candidate.get("binary_sha256") == release["binary_sha256"] and candidate.get("admission_mode") == "redis_account_leases_v1", "candidate identity differs from requested release")
        return value

    if action == "status":
        return snapshot()
    if action == "reconcile":
        adapter.reconcile(argparse.Namespace())
        return snapshot()
    if action == "check-candidate":
        value = guard(True)
        candidate = value["candidate"]
        live = adapter.runtime(candidate["unit"])
        adapter.require(live["pid"] == candidate["pid"] and live["binary_sha256"] == candidate["binary_sha256"], "candidate runtime changed")
        adapter.healthy(candidate["listen_port"])
        adapter.require(adapter.lifecycle(candidate).get("admission_mode") == "redis_account_leases_v1", "candidate admission changed")
        return snapshot()
    if action == "resume-legacy":
        value = guard()
        target = adapter.front()
        adapter.require(value["active"].get("admission_mode") == "process_local" and (value.get("pending") or {}).get("phase") != "switching", "legacy resume cannot undo an uncertain switch")
        adapter.require(target.get("last_update_durable") is True and target.get("active", {}).get("generation") == value["active"]["instance"] and target["active"].get("address") == "127.0.0.1:" + str(value["active"]["listen_port"]) and target["active"].get("held") is True, "legacy resume target is not exact")
        adapter.resume_legacy(argparse.Namespace())
        return snapshot()
    if action == "stage":
        value = guard()
        current_front = observed_front()  # Recheck disk proof in the mutation lock.
        target = current_front.get("active") or {}
        adapter.require(target.get("generation") == value["active"]["instance"] and target.get("address") == "127.0.0.1:" + str(value["active"]["listen_port"]) and target.get("held") is False, "ingress changed before candidate staging")
        adapter.require(not value.get("candidate"), "another candidate is already staged")
        adapter.require(not (value.get("pending") or {}).get("phase") == "switching", "unreconciled switch exists")
        occupied = {item["listen_port"] for item in [value["active"], *value.get("draining", [])]}
        ports = [payload["port"]] if payload.get("port") else range(18890, 18910)
        port = next((port for port in ports if adapter.private_backend_port(port) and port not in occupied and adapter.port_free(port)), None)
        adapter.require(port is not None, "no unused private backend port; retain current runtime")
        # Instance/unit identity is binary-based. A different frontend package
        # must not overwrite an existing same-binary generation's unit/env.
        instance = release["binary_sha256"][:20]
        adapter.require(all(item.get("instance") != instance for item in [value["active"], *value.get("draining", [])]), "requested binary already belongs to another generation/package")
        result = adapter.stage(argparse.Namespace(release=release["remote"], port=port))
        return {"result": result, "status": snapshot()}
    if action == "activate":
        value = guard(True)
        adapter.require(value["active"].get("admission_mode") == "redis_account_leases_v1", "legacy migration needs bounded hold")
        adapter.require((value.get("pending") or {}).get("phase") != "switching", "unreconciled switch exists")
        confirm_target_persistence(value)
        guard(True)
        return {"result": adapter.activate(argparse.Namespace()), "status": snapshot()}
    if action == "retire":
        value = guard()
        old = next((item for item in value.get("draining", []) if item.get("unit") == payload.get("unit")), None)
        adapter.require(old is not None and old.get("admission_mode") == "redis_account_leases_v1", "legacy or unknown generation cannot be automatically retired")
        return {"result": adapter.retire(argparse.Namespace(unit=old["unit"])), "status": snapshot()}
    adapter.require(action == "legacy-window", "unsupported wrapper operation")
    wait_seconds = payload["legacy_wait_seconds"]
    adapter.require(isinstance(wait_seconds, (int, float)) and not isinstance(wait_seconds, bool) and math.isfinite(wait_seconds) and 0 <= wait_seconds <= 60, "legacy wait must be between 0 and 60 seconds")
    value = guard(True)
    adapter.require(value["active"].get("admission_mode") == "process_local", "legacy observation requires a legacy active generation")
    initial_front = observed_front()
    adapter.require(initial_front.get("active", {}).get("held") is False, "ingress is already held; reconcile existing operator intent")
    # Readiness is checked again immediately before hold. No model is called.
    candidate = value["candidate"]
    adapter.healthy(candidate["listen_port"])
    adapter.require(adapter.lifecycle(candidate).get("admission_mode") == "redis_account_leases_v1", "candidate admission changed")
    started = time.monotonic()
    deadline = started + wait_seconds
    observation = {"budget_ms": round(wait_seconds * 1000), "waiting_observations": 0,
                   "first": None, "last": None, "minimum_counts": {}}

    def observe_wait(result):
        sample = {"elapsed_ms": round((time.monotonic() - started) * 1000)}
        for name in ("old_backend_connections", "old_front_requests", "waiting_requests"):
            count = result.get(name)
            sample[name] = count if type(count) is int and count >= 0 else None
            minimum = observation["minimum_counts"].get(name)
            if sample[name] is not None and (minimum is None or count < minimum["value"]):
                observation["minimum_counts"][name] = {"value": count, "elapsed_ms": sample["elapsed_ms"]}
        observation["waiting_observations"] += 1
        if observation["first"] is None:
            observation["first"] = sample
        observation["last"] = sample

    def measured_result(result):
        observation["elapsed_ms"] = round((time.monotonic() - started) * 1000)
        return {**result, "legacy_observation": observation}

    held = False
    try:
        # Set intent before sending hold, so a committed hold with a lost reply
        # can be resumed below if registry+front still prove the old target.
        held = True
        adapter.hold_legacy(argparse.Namespace())
        while True:
            guard(True)
            result = adapter.activate(argparse.Namespace())
            if result.get("activated") is True:
                return {"result": measured_result(result), "status": snapshot()}
            adapter.require(result.get("phase") == "waiting_for_legacy_natural_drain", "legacy activation has an unexpected outcome")
            observe_wait(result)
            if time.monotonic() >= deadline:
                adapter.resume_legacy(argparse.Namespace())
                held = False
                return {"result": measured_result({"activated": False, "waiting": True, "legacy_resumed": True, "candidate_retained": True}), "status": snapshot()}
            time.sleep(min(.5, max(0, deadline - time.monotonic())))
    finally:
        if held:
            current = snapshot()
            # No blind rollback/resume after uncertain switching. The outer
            # caller uses status/reconcile to determine the committed target.
            active = current["registry"].get("active") or {}
            target = current["front"].get("active") or {}
            unchanged = {key: active.get(key) for key in expected} == expected
            pending = (current["registry"].get("pending") or {}).get("phase")
            if unchanged and pending != "switching" and target.get("generation") == active.get("instance") and target.get("address") == "127.0.0.1:" + str(active.get("listen_port")) and target.get("held") is True and current["front"].get("last_update_durable") is True:
                adapter.resume_legacy(argparse.Namespace())


def package_probe_script(release):
    return "EXPECTED=" + repr(release) + "\n" + r'''
import hashlib,json,pathlib
root=pathlib.Path(EXPECTED['remote'])
def digest(path):
 h=hashlib.sha256()
 with path.open('rb') as f:
  while True:
   chunk=f.read(1024*1024)
   if not chunk:break
   h.update(chunk)
 return h.hexdigest()
state='mismatch'
try:
 if not root.exists() and not root.is_symlink():state='absent'
 elif root.is_dir() and not any(p.is_symlink() for p in (root,*root.parents)):
  inventory=set()
  valid=digest(root/'manifest.json')==EXPECTED['manifest_sha256']
  for p in root.rglob('*'):
   if p.is_symlink() or not (p.is_file() or p.is_dir()):valid=False;break
   if p.is_file():inventory.add(p.relative_to(root).as_posix())
  valid=valid and inventory==set(EXPECTED['files'])|{'manifest.json'}
  if valid:valid=all(digest(root/name)==value for name,value in EXPECTED['files'].items())
  if valid:state='exact'
except (OSError,ValueError):pass
print(json.dumps({'package':state}))
'''


class RemoteOperations:
    def __init__(self, release):
        from integration_env import connect
        self.release, self.client = release, connect()

    def close(self):
        self.client.close()

    def script(self, script, action, timeout=120):
        try:
            stdin, stdout, stderr = self.client.exec_command("python3 -B -", timeout=timeout)
            stdin.write(script)
            stdin.channel.shutdown_write()
            raw = stdout.read().decode("utf-8", "replace")
            status = stdout.channel.recv_exit_status()
            stderr.read()  # Server diagnostics may contain private environment.
            if status != 0:
                raise HotReleaseError("controller operation failed: " + action)
            value = json.loads(raw)
            require(isinstance(value, dict), "controller response is invalid")
            return value
        except HotReleaseError:
            raise
        except Exception:
            raise HotReleaseError("controller reply unavailable: " + action) from None

    def probe_package(self):
        return self.script(package_probe_script(self.release), "package-check")["package"]

    def upload(self):
        # This is deliberately the existing uploader's STAGE action only.
        # Its stop-first activate/preflight/rollback paths are never called.
        process = subprocess.run([sys.executable, str(ROOT / "tools/reliability_release.py"), "stage", "--release", self.release["manifest_path"]], cwd=ROOT, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        require(process.returncode == 0, "release upload/stage-cli failed; inspect immutable package before retrying")

    def control(self, action, **kwargs):
        payload = {"action": action, "release": self.release, **kwargs}
        script = "PAYLOAD=" + repr(payload) + "\n" + inspect.getsource(remote_operation) + r'''
import fcntl,importlib.util,json,pathlib,sys
sys.dont_write_bytecode=True
path=pathlib.Path(PAYLOAD['release']['remote'])/'deploy/zero_downtime_release.py'
import hashlib
assert hashlib.sha256(path.read_bytes()).hexdigest()==PAYLOAD['release']['files']['deploy/zero_downtime_release.py']
spec=importlib.util.spec_from_file_location('hot_adapter',path)
adapter=importlib.util.module_from_spec(spec);spec.loader.exec_module(adapter)
adapter.require(adapter.os.geteuid()==0,'server-side root required')
adapter.BASE.mkdir(parents=True,exist_ok=True)
with (adapter.BASE/'zero-downtime.lock').open('a') as lock:
 fcntl.flock(lock,fcntl.LOCK_EX)
 result=remote_operation(adapter,PAYLOAD)
print(json.dumps(result))
'''
        # SSH timeout never sends stop/kill to a runtime or controller. Legacy
        # observation executes on the server and resumes even if the UI leaves.
        return self.script(script, action, timeout=300 if action == "stage" else 240 if action == "retire" else 90)


def status_reconciled(operations):
    status = operations.control("status")
    registry = status.get("registry") or {}
    if (registry.get("pending") or {}).get("phase") == "switching":
        operations.control("reconcile")
        status = operations.control("status")
    validate_status(status)
    return status


def result_summary(state, status, release, **extra):
    registry, front = validate_status(status)
    active = registry["active"]
    # Never serialize controller responses, process environments or stderr.
    return {"state": state, "release_id": release["release_id"],
            "active_release_id": (active.get("release") or "").rsplit("/", 1)[-1] if active.get("release") else None,
            "active_binary_sha256": active["binary_sha256"], "active_pid": active["pid"],
            "ingress_held": front["active"]["held"], "ingress_last_update_durable": front.get("last_update_durable"),
            **({"verified_persisted_target": front["verified_persisted_target"]} if "verified_persisted_target" in front else {}),
            "draining_generations": len(registry.get("draining", [])), **extra}


def run_release(operations, release, stage_only=False, activate_only=False, legacy_wait_seconds=10, port=None, retire_drained=False):
    require(not (stage_only and activate_only), "stage-only and activate-only are mutually exclusive")
    require(math.isfinite(legacy_wait_seconds) and 0 <= legacy_wait_seconds <= 60, "legacy wait must be between 0 and 60 seconds")
    package = operations.probe_package()
    require(package in ("absent", "exact"), "existing immutable package differs; refusing overwrite")
    if package == "absent":
        require(not activate_only, "activate-only requires an already staged exact package")
        try:
            operations.upload()
        except HotReleaseError:
            # A lost upload reply can leave a complete package. Verify first;
            # never rerun the uploader over an existing destination.
            require(operations.probe_package() == "exact", "upload outcome incomplete; retain active runtime and inspect package")
        require(operations.probe_package() == "exact", "staged release package verification failed")
    status = status_reconciled(operations)
    registry, front = validate_status(status)
    if matches_release(registry["active"], release):
        return result_summary("already_active", status, release)
    require(front["active"]["held"] is False, "ingress is held by an earlier operation; reconcile it before starting a release")
    active = instance_identity(registry["active"])
    candidate = registry.get("candidate")
    if candidate:
        require(matches_release(candidate, release), "another candidate is staged; refusing to replace it")
    else:
        require(not activate_only, "activate-only requires the requested healthy candidate")
        try:
            operations.control("stage", expected_active=active, port=port)
        except HotReleaseError:
            status = status_reconciled(operations)
            require(matches_release(status["registry"].get("candidate"), release), "candidate staging outcome unconfirmed; no switch attempted")
        status = status_reconciled(operations)
        registry, _ = validate_status(status)
        require(instance_identity(registry["active"]) == active and matches_release(registry.get("candidate"), release), "candidate or active runtime changed during staging")
    if stage_only:
        operations.control("check-candidate", expected_active=active)
        status = status_reconciled(operations)
        return result_summary("staged", status, release)
    legacy = registry["active"].get("admission_mode") == "process_local"
    require(legacy or registry["active"].get("admission_mode") == SHARED, "active runtime admission mode is unknown")
    action = "legacy-window" if legacy else "activate"
    try:
        reply = operations.control(action, expected_active=active, legacy_wait_seconds=legacy_wait_seconds)
        status = status_reconciled(operations)
    except HotReleaseError:
        status = status_reconciled(operations)
        if not matches_release(status["registry"]["active"], release):
            if legacy and instance_identity(status["registry"]["active"]) == active and status["front"]["active"]["held"] is True:
                try:
                    operations.control("resume-legacy", expected_active=active)
                except HotReleaseError:
                    pass
                status = status_reconciled(operations)
            return result_summary("not_switched_after_reconcile", status, release, retry_requires_new_invocation=True)
        reply = {"result": {"activated": True}}
    registry, front = validate_status(status)
    measured = reply.get("result", {}).get("legacy_observation")
    observation_fields = {"legacy_observation": measured} if legacy and isinstance(measured, dict) else {}
    if not matches_release(registry["active"], release):
        require(legacy and reply.get("result", {}).get("waiting") is True and instance_identity(registry["active"]) == active and front["active"]["held"] is False, "activation outcome is unconfirmed; retain all generations")
        return result_summary("waiting_for_legacy_natural_drain", status, release, legacy_resumed=True, candidate_retained=True, **observation_fields)
    require(front["active"]["held"] is False, "new runtime is active but ingress remains held")
    retained = 0
    if retire_drained:
        # One opportunistic attempt per observed shared generation, no waits.
        # The adapter independently proves zero handlers/sockets/persistence.
        for old in list(registry.get("draining", [])):
            if old.get("admission_mode") != SHARED:
                retained += 1
                continue
            try:
                operations.control("retire", expected_active=instance_identity(registry["active"]), unit=old["unit"])
            except HotReleaseError:
                pass  # Never kill, retry, or roll back because retirement waits.
            status = status_reconciled(operations)
            registry, _ = validate_status(status)
            require(matches_release(registry["active"], release), "active runtime changed while observing retirement")
        retained = len(registry.get("draining", []))
    return result_summary("activated", status, release, retained_generations=retained if retire_drained else len(registry.get("draining", [])), **observation_fields)


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--release", required=True, help="prepared immutable package/manifest.json")
    modes = parser.add_mutually_exclusive_group()
    modes.add_argument("--stage-only", action="store_true")
    modes.add_argument("--activate-only", action="store_true")
    modes.add_argument("--status", action="store_true", help="read active-runtime and ingress only; do not reconcile or mutate")
    parser.add_argument("--legacy-wait-seconds", type=float, default=10, help="one natural-drain observation window, 0–60 seconds; default 10")
    parser.add_argument("--port", type=int, help="unused private candidate port; otherwise select 18890–18909")
    parser.add_argument("--retire-drained", action="store_true", help="one verified retirement attempt per old shared generation; never legacy")
    args = parser.parse_args(argv)
    operations = None
    try:
        release = read_release(args.release)
        operations = RemoteOperations(release)
        if args.status:
            require(operations.probe_package() == "exact", "status requires an exact uploaded controller package")
            result = result_summary("status", operations.control("status"), release)
        else:
            result = run_release(operations, release, args.stage_only, args.activate_only, args.legacy_wait_seconds, args.port, args.retire_drained)
        print(json.dumps(result, ensure_ascii=False))
        return 0 if result["state"] in ("activated", "already_active", "staged", "status") else 2
    except (HotReleaseError, KeyboardInterrupt) as error:
        message = str(error) if isinstance(error, HotReleaseError) else "interrupted; server observation may still run, inspect status before another invocation"
        print(json.dumps({"state": "blocked", "error": message}, ensure_ascii=False))
        return 1
    except Exception:
        print(json.dumps({"state": "blocked", "error": "hot release operation failed; inspect status/reconcile before retrying"}))
        return 1
    finally:
        if operations is not None:
            operations.close()


if __name__ == "__main__":
    raise SystemExit(main())
