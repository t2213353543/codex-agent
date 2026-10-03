#!/usr/bin/env python3
"""Zero-turn Codex 0.155.0 external ChatGPT auth-state probe.

Send one JSON object on stdin: {"access_token": "...", "account_id": "..."}.
Only hashes and allowlisted metadata reach stdout. No credential file, model
request, token refresh, thread, or turn is supported. account/read reflects
local CLI auth state; it does not prove remote token validity. The official
login path may perform background metadata network requests.
"""
from __future__ import annotations

import argparse
import base64
import hashlib
import json
import math
import os
from pathlib import Path
import queue
import re
import shutil
import signal
import stat
import subprocess
import sys
import tempfile
import threading
import time

VERSION = "0.155.0"
SCHEMA = "codex-native-auth-state-probe-v1"
SUCCESS_STATUSES = frozenset(("completed", "completed_with_known_zero_turn_warning"))
MAX_INPUT = 64 * 1024
MAX_LINE = 128 * 1024
MAX_OUTPUT = 2 * 1024 * 1024
HASH = re.compile(r"[0-9a-f]{64}\Z")
PLANS = frozenset("free go plus pro prolite team self_serve_business_prolite self_serve_business_usage_based business ent26 enterprise_cbp_automation enterprise_cbp_usage_based enterprise edu edu_plus edu_pro unknown".split())
AUTH_CLAIM = "https://api.openai.com/auth"
PROFILE_CLAIM = "https://api.openai.com/profile"
# Diagnostic names only: this is NOT an allowlist for accepting notifications.
# Pinned source: 0.155.0 app-server-protocol/schema/typescript/ServerNotification.ts.
PROTOCOL_NOTIFICATION_METHODS = frozenset("""
error thread/started thread/status/changed thread/archived thread/deleted
thread/unarchived thread/closed thread/reverted skills/changed thread/name/updated
thread/attachment/updated thread/goal/updated thread/goal/cleared thread/queue/changed
project/changed thread/project/updated thread/environment/connected
thread/environment/disconnected thread/settings/updated thread/tokenUsage/updated
turn/started hook/started turn/completed hook/completed turn/diff/updated
turn/plan/updated item/started item/autoApprovalReview/started
item/autoApprovalReview/completed autoApprovalReview/strictReviewRequired
item/completed rawResponseItem/completed rawResponse/completed item/agentMessage/delta
item/plan/delta command/exec/outputDelta process/outputDelta process/exited
item/commandExecution/outputDelta item/commandExecution/terminalInteraction
item/fileChange/outputDelta item/fileChange/patchUpdated serverRequest/resolved
item/mcpToolCall/progress mcpServer/oauthLogin/completed mcpServer/startupStatus/updated
mcpServer/event/stream/notification account/updated account/rateLimits/updated
app/list/updated remoteControl/status/changed externalAgentConfig/import/progress
externalAgentConfig/import/completed fs/changed item/reasoning/summaryTextDelta
item/reasoning/summaryPartAdded item/reasoning/textDelta thread/compacted model/rerouted
model/verification modelProvider/authRecoveryStarted modelProvider/authRecoveryCompleted
turn/moderationMetadata model/safetyBuffering/updated warning guardianWarning
deprecationNotice configWarning fuzzyFileSearch/sessionUpdated fuzzyFileSearch/sessionCompleted
thread/realtime/started thread/realtime/itemAdded thread/realtime/item/started
thread/realtime/item/transcript/delta thread/realtime/item/completed
thread/realtime/transcript/delta thread/realtime/transcript/done
thread/realtime/outputAudio/delta thread/realtime/sdp thread/realtime/error
thread/realtime/closed windows/worldWritableWarning windowsSandbox/setupCompleted
account/login/completed
""".split())
# Exact public source constants from codex-rs/sandboxing/src/bwrap.rs. A match
# classifies the diagnostic without copying arbitrary CLI text into evidence.
SOURCE_STARTUP_WARNINGS = {
    "Codex could not find bubblewrap on PATH. Install bubblewrap with your OS package manager. "
    "See the sandbox prerequisites: https://developers.openai.com/codex/concepts/sandboxing#prerequisites. "
    "Codex will use the bundled bubblewrap in the meantime.": "missing_system_bubblewrap",
    "Codex's Linux sandbox uses bubblewrap and needs access to create user namespaces.": "bubblewrap_user_namespace_unavailable",
    "Codex's Linux sandbox uses bubblewrap, which is not supported on WSL1 "
    "because WSL1 cannot create the required user namespaces. "
    "Use WSL2 for sandboxed shell commands.": "bubblewrap_wsl1_unsupported",
}
CONFIG = '''model_provider = "openai"
approval_policy = "never"
sandbox_mode = "read-only"
web_search = "disabled"
cli_auth_credentials_store = "ephemeral"
mcp_oauth_credentials_store = "file"
project_doc_max_bytes = 0
[history]
persistence = "none"
[analytics]
enabled = false
[feedback]
enabled = false
[skills]
include_instructions = false
[skills.bundled]
enabled = false
[features]
shell_tool = false
apps = false
plugins = false
remote_plugin = false
hooks = false
plugin_hooks = false
multi_agent = false
skip_host_skill_discovery = true
'''


class ProbeFailure(Exception):
    """Only fixed internal codes cross the public reporting boundary."""


def sha(value):
    return hashlib.sha256(value.encode() if isinstance(value, str) else value).hexdigest()


def unique_object(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise ProbeFailure("duplicate_json_field")
        result[key] = value
    return result


def decode_json(raw):
    try:
        return json.loads(raw, object_pairs_hook=unique_object,
                          parse_constant=lambda _: (_ for _ in ()).throw(ValueError()))
    except (ValueError, UnicodeError, RecursionError):
        raise ProbeFailure("invalid_json") from None


def remaining(deadline):
    value = deadline - time.monotonic()
    if value <= 0:
        raise ProbeFailure("deadline_exceeded")
    return value


def credential_state(raw, pins, deadline):
    if not isinstance(raw, bytes) or len(raw) > MAX_INPUT:
        raise ProbeFailure("invalid_credential_input")
    credential = decode_json(raw)
    if not isinstance(credential, dict) or set(credential) != {"access_token", "account_id"}:
        raise ProbeFailure("invalid_credential_fields")
    token, account = credential["access_token"], credential["account_id"]
    if not isinstance(token, str) or len(token) > MAX_INPUT or not re.fullmatch(r"[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+", token):
        raise ProbeFailure("invalid_access_token")
    if not isinstance(account, str) or not re.fullmatch(r"[A-Za-z0-9_-]{1,160}", account):
        raise ProbeFailure("invalid_account_id")
    try:
        segment = token.split(".")[1]
        claims = decode_json(base64.b64decode(segment + "=" * (-len(segment) % 4), altchars=b"-_", validate=True))
    except (ValueError, UnicodeError):
        raise ProbeFailure("invalid_token_claims") from None
    if not isinstance(claims, dict) or not isinstance(claims.get(AUTH_CLAIM), dict):
        raise ProbeFailure("missing_auth_claims")
    auth = claims[AUTH_CLAIM]
    user = auth.get("chatgpt_user_id")
    if user is None:
        user = auth.get("user_id")
    profile = claims.get(PROFILE_CLAIM)
    email = claims.get("email")
    if email is None and isinstance(profile, dict):
        email = profile.get("email")
    if auth.get("chatgpt_account_id") != account or not isinstance(user, str) or not user:
        raise ProbeFailure("credential_identity_mismatch")
    if email is not None and (not isinstance(email, str) or not email or len(email) > 1024):
        raise ProbeFailure("invalid_email_claim")
    plan = auth.get("chatgpt_plan_type", "unknown")
    if plan is None:
        plan = "unknown"
    if plan not in PLANS:
        raise ProbeFailure("unsupported_plan_claim")
    expiration = claims.get("exp")
    if type(expiration) not in (int, float) or not math.isfinite(expiration) or expiration <= time.time() + remaining(deadline) + 300:
        raise ProbeFailure("access_token_expiration_insufficient")
    observed = {"token_sha256": sha(token), "account_sha256": sha(account), "user_sha256": sha(user)}
    if any(observed[key] != pins[key] for key in observed):
        raise ProbeFailure("credential_pin_mismatch")
    observed.update(email_sha256=sha(email) if email is not None else None, plan_type=plan,
                    claims_source="unverified_local_jwt", remote_token_validity="not_checked")
    return credential, observed


def isolated_environment(root):
    allowed = {"PATH", "PATHEXT", "SYSTEMROOT", "WINDIR", "COMSPEC", "LANG", "LC_ALL", "LC_CTYPE", "TZ"}
    environment = {key: value for key, value in os.environ.items() if key.upper() in allowed}
    for key in ("HOME", "USERPROFILE", "APPDATA", "LOCALAPPDATA", "TEMP", "TMP", "TMPDIR"):
        environment[key] = str(root)
    environment.update(CODEX_HOME=str(root / "home"), NO_COLOR="1", RUST_LOG="off",
                       CODEX_REFRESH_TOKEN_URL_OVERRIDE="http://127.0.0.1:9/disabled")
    return environment


def reject_system_config():
    directories = [Path("/etc/codex")]
    if os.name == "nt":
        import ctypes
        import uuid
        # The official loader uses the known folder API, not the inherited
        # ProgramData environment value; use that same source of truth.
        identifier = (ctypes.c_byte * 16).from_buffer_copy(uuid.UUID("62ab5d82-fdc1-4dc3-a9dd-070d1d495d97").bytes_le)
        address = ctypes.c_void_p()
        shell = ctypes.WinDLL("shell32")
        shell.SHGetKnownFolderPath.argtypes = [ctypes.c_void_p, ctypes.c_uint, ctypes.c_void_p, ctypes.c_void_p]
        status = shell.SHGetKnownFolderPath(ctypes.byref(identifier), 0, None, ctypes.byref(address))
        if status != 0 or not address.value:
            raise ProbeFailure("system_config_location_unavailable")
        try:
            directories.append(Path(ctypes.wstring_at(address)) / "OpenAI" / "Codex")
        finally:
            ole = ctypes.WinDLL("ole32")
            ole.CoTaskMemFree.argtypes = [ctypes.c_void_p]
            ole.CoTaskMemFree(address)
    for directory in directories:
        for name in ("config.toml", "requirements.toml", "managed_config.toml"):
            try:
                (directory / name).lstat()
            except FileNotFoundError:
                continue
            raise ProbeFailure("system_codex_configuration_present")


class _WindowsJob:
    """Bind a suspended child before it can create descendants."""
    def __init__(self):
        import ctypes
        from ctypes import wintypes as w
        class BASIC(ctypes.Structure):
            _fields_ = [("per_process", ctypes.c_int64), ("per_job", ctypes.c_int64),
                        ("flags", w.DWORD), ("minimum", ctypes.c_size_t), ("maximum", ctypes.c_size_t),
                        ("active", w.DWORD), ("affinity", ctypes.c_size_t), ("priority", w.DWORD), ("scheduling", w.DWORD)]
        class IO(ctypes.Structure):
            _fields_ = [(name, ctypes.c_uint64) for name in ("read_ops", "write_ops", "other_ops", "read_bytes", "write_bytes", "other_bytes")]
        class EXTENDED(ctypes.Structure):
            _fields_ = [("basic", BASIC), ("io", IO), ("process_memory", ctypes.c_size_t),
                        ("job_memory", ctypes.c_size_t), ("peak_process", ctypes.c_size_t), ("peak_job", ctypes.c_size_t)]
        self.kernel = ctypes.WinDLL("kernel32", use_last_error=True)
        self.kernel.CreateJobObjectW.argtypes = [ctypes.c_void_p, w.LPCWSTR]
        self.kernel.CreateJobObjectW.restype = w.HANDLE
        self.kernel.SetInformationJobObject.argtypes = [w.HANDLE, ctypes.c_int, ctypes.c_void_p, w.DWORD]
        self.kernel.AssignProcessToJobObject.argtypes = [w.HANDLE, w.HANDLE]
        self.kernel.TerminateJobObject.argtypes = [w.HANDLE, w.UINT]
        self.kernel.CloseHandle.argtypes = [w.HANDLE]
        self.handle = self.kernel.CreateJobObjectW(None, None)
        if not self.handle:
            raise ProbeFailure("process_tree_guard_failed")
        limits = EXTENDED()
        limits.basic.flags = 0x2000  # JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
        if not self.kernel.SetInformationJobObject(self.handle, 9, ctypes.byref(limits), ctypes.sizeof(limits)):
            self.close()
            raise ProbeFailure("process_tree_guard_failed")

    def attach_and_resume(self, process):
        import ctypes
        if not self.kernel.AssignProcessToJobObject(self.handle, int(process._handle)):
            raise ProbeFailure("process_tree_guard_failed")
        resume = ctypes.WinDLL("ntdll").NtResumeProcess
        resume.argtypes = [ctypes.c_void_p]
        resume.restype = ctypes.c_long
        if resume(int(process._handle)) != 0:
            raise ProbeFailure("process_tree_guard_failed")

    def close(self):
        if self.handle:
            self.kernel.TerminateJobObject(self.handle, 1)
            self.kernel.CloseHandle(self.handle)
            self.handle = None


class _Child:
    def __init__(self, command, root, environment, deadline):
        self.deadline, self.process, self.job = deadline, None, None
        self.incoming, self.outgoing = queue.Queue(maxsize=128), queue.Queue(maxsize=8)
        self.threads, self.lock, self.failed = [], threading.Lock(), threading.Event()
        self.closing = threading.Event()
        self.failure = "child_io_failed"
        self.hashes = {name: hashlib.sha256() for name in ("stdout", "stderr")}
        self.sizes = {"stdout": 0, "stderr": 0}
        try:
            remaining(deadline)
            if os.name == "nt":
                self.job = _WindowsJob()
            self.process = subprocess.Popen(command, cwd=root, env=environment, stdin=subprocess.PIPE,
                stdout=subprocess.PIPE, stderr=subprocess.PIPE, bufsize=0,
                start_new_session=os.name != "nt",
                creationflags=(0x00000004 | subprocess.CREATE_NO_WINDOW) if os.name == "nt" else 0)
            if self.job:
                self.job.attach_and_resume(self.process)
            for target, args in ((self._read, ("stdout", self.process.stdout)),
                                 (self._read, ("stderr", self.process.stderr)), (self._write, ())):
                thread = threading.Thread(target=target, args=args, daemon=True)
                self.threads.append(thread)
                thread.start()
        except BaseException:
            self.close()
            raise

    def _fail(self, code):
        self.failure = code
        self.failed.set()

    def _read(self, name, stream):
        try:
            while True:
                raw = stream.readline(MAX_LINE + 1) if name == "stdout" else stream.read(4096)
                if not raw:
                    if name == "stdout":
                        self.incoming.put_nowait(None)
                    return
                with self.lock:
                    self.hashes[name].update(raw)
                    self.sizes[name] += len(raw)
                    if sum(self.sizes.values()) > MAX_OUTPUT or name == "stdout" and len(raw) > MAX_LINE:
                        self._fail("child_output_limit")
                        return
                if name == "stdout":
                    self.incoming.put_nowait(raw)
        except (OSError, ValueError, queue.Full):
            if not self.closing.is_set():
                self._fail("child_io_failed")

    def _write(self):
        try:
            while True:
                raw = self.outgoing.get()
                if raw is None:
                    return
                pending = memoryview(raw)
                while pending:
                    written = self.process.stdin.write(pending)
                    if not written:
                        raise OSError()
                    pending = pending[written:]
                self.process.stdin.flush()
        except (OSError, ValueError):
            if not self.closing.is_set():
                self._fail("child_io_failed")

    def send(self, message):
        remaining(self.deadline)
        try:
            self.outgoing.put_nowait(json.dumps(message, separators=(",", ":"), ensure_ascii=True).encode() + b"\n")
        except queue.Full:
            raise ProbeFailure("child_input_limit") from None

    def receive(self):
        while True:
            if self.failed.is_set():
                raise ProbeFailure(self.failure)
            try:
                raw = self.incoming.get(timeout=min(.05, remaining(self.deadline)))
                if raw is None:
                    raise ProbeFailure("child_exited")
                return raw
            except queue.Empty:
                continue

    def evidence(self):
        with self.lock:
            return {name: {"bytes": self.sizes[name], "sha256": self.hashes[name].hexdigest()} for name in self.hashes}

    def cleanup_evidence(self):
        return {"child_reaped": self.process is not None and self.process.poll() is not None,
                "io_threads_joined": all(not thread.is_alive() for thread in self.threads)}

    def close(self):
        self.closing.set()
        if self.job:
            self.job.close()
        elif self.process and os.name != "nt":
            try:
                os.killpg(self.process.pid, signal.SIGKILL)
            except ProcessLookupError:
                pass
        if self.process:
            if self.process.poll() is None:
                self.process.kill()
            try:
                self.process.wait(timeout=max(.01, self.deadline - time.monotonic()))
            except subprocess.TimeoutExpired:
                self._fail("process_cleanup_timeout")
            try:
                self.outgoing.put_nowait(None)
            except queue.Full:
                pass
            for stream in (self.process.stdin, self.process.stdout, self.process.stderr):
                stream.close()
            for thread in self.threads:
                thread.join(timeout=max(0, self.deadline - time.monotonic()))
            if not all(self.cleanup_evidence().values()):
                self._fail("process_cleanup_timeout")


def pinned_copy(cli, target, expected, deadline):
    if not isinstance(cli, Path) or not cli.is_absolute():
        raise ProbeFailure("cli_must_be_absolute")
    digest = hashlib.sha256()
    # Execute the verified private copy, never reopen the mutable source path.
    with cli.open("rb") as source, target.open("xb") as output:
        if not stat.S_ISREG(os.fstat(source.fileno()).st_mode):
            raise ProbeFailure("cli_must_be_regular")
        while True:
            remaining(deadline)
            block = source.read(1024 * 1024)
            if not block:
                break
            digest.update(block)
            output.write(block)
    if digest.hexdigest() != expected:
        raise ProbeFailure("cli_hash_mismatch")
    target.chmod(0o700)


def auth_handshake(child, receipt, credential, identity, codex_home):
    notifications = {}
    sent = 0
    def receive_response(request_id):
        nonlocal sent
        while True:
            sent += 1
            if sent > 100:
                raise ProbeFailure("protocol_message_limit")
            raw = child.receive()
            message = decode_json(raw)
            if not isinstance(message, dict):
                raise ProbeFailure("invalid_protocol_message")
            if "method" in message:
                method = message["method"]
                if "id" in message:
                    receipt["server_requests_rejected"] += 1
                    receipt["rejected_method_sha256"] = sha(method) if isinstance(method, str) else None
                    child.send({"id": message["id"], "error": {"code": -32601, "message": "Client requests are disabled"}})
                    raise ProbeFailure("server_request_rejected")
                params = message.get("params")
                # lib.rs emits the current remote-control status directly after
                # initialize, then broadcasts status changes independently of
                # login. These are observations, never requests to run work.
                if method == "remoteControl/status/changed" and request_id != 1:
                    if (not isinstance(params, dict) or
                            set(params) - {"status", "serverName", "installationId", "environmentId"} or
                            params.get("status") not in ("disabled", "connecting", "connected", "errored") or
                            any(not isinstance(params.get(key), str) or len(params[key]) > 4096 for key in ("serverName", "installationId")) or
                            params.get("environmentId") is not None and (not isinstance(params["environmentId"], str) or len(params["environmentId"]) > 4096) or
                            len(receipt["remote_control_status"]) >= 8):
                        raise ProbeFailure("invalid_remote_control_notification")
                    receipt["remote_control_status"].append({
                        "status": params["status"], "scope": "status_observation_only",
                        "server_name_sha256": sha(params["serverName"]),
                        "installation_id_sha256": sha(params["installationId"]),
                        "environment_id_sha256": sha(params["environmentId"]) if params.get("environmentId") is not None else None,
                    })
                    continue
                # The skills filesystem watcher can invalidate its cache while
                # login refreshes plugin caches. Its v2 payload is exactly {}.
                if method == "skills/changed" and request_id != 1:
                    if params != {} or receipt["skills_changed_notifications"] >= 8:
                        raise ProbeFailure("invalid_skills_notification")
                    receipt["skills_changed_notifications"] += 1
                    continue
                if method not in ("account/login/completed", "account/updated") or request_id == 1:
                    # Unknown methods and warning payloads are untrusted and can
                    # contain credentials. Expose only pinned enum names/hashes.
                    diagnostic = {
                        "method_sha256": sha(method) if isinstance(method, str) else None,
                        "known_method": method if isinstance(method, str) and method in PROTOCOL_NOTIFICATION_METHODS else None,
                        "waiting_response_id": request_id,
                    }
                    params = message.get("params")
                    if isinstance(params, dict):
                        diagnostic["params_sha256"] = sha(json.dumps(params, sort_keys=True, separators=(",", ":")))
                        if method == "configWarning":
                            for field in ("summary", "details", "path"):
                                value = params.get(field)
                                diagnostic[field + "_sha256"] = sha(value) if isinstance(value, str) else None
                            summary = params.get("summary")
                            diagnostic["source_warning"] = SOURCE_STARTUP_WARNINGS.get(summary) if isinstance(summary, str) else None
                    # Observed with this pinned CLI: absent system bwrap only
                    # affects tool sandbox setup. This probe cannot create a
                    # turn or execute tools. Never reuse this exception for a
                    # model-reference runner or accept other config warnings.
                    if (method == "configWarning" and request_id != 1 and
                            isinstance(params, dict) and
                            diagnostic.get("source_warning") == "missing_system_bubblewrap" and
                            set(params) <= {"summary", "details", "path", "range"} and
                            "details" in params and params["details"] is None and
                            params.get("path") is None and params.get("range") is None and
                            not receipt["startup_warnings"]):
                        receipt["startup_warnings"].append({**diagnostic, "scope": "zero_turn_local_auth_only"})
                        continue
                    receipt["unexpected_notification"] = diagnostic
                    raise ProbeFailure("unexpected_notification")
                params = message.get("params")
                if not isinstance(params, dict) or method in notifications:
                    raise ProbeFailure("invalid_auth_notification")
                notifications[method] = params
                if request_id is None and len(notifications) == 2:
                    return None
                continue
            if type(message.get("id")) is not int or message["id"] != request_id:
                raise ProbeFailure("unexpected_response_id")
            if "error" in message:
                receipt["rpc_error_sha256"] = sha(json.dumps(message["error"], sort_keys=True))
                raise ProbeFailure("rpc_error")
            if "result" not in message:
                raise ProbeFailure("missing_rpc_result")
            return message["result"]

    def request(request_id, method, params):
        child.send({"id": request_id, "method": method, "params": params})
        receipt["client_methods"].append(method)
        return receive_response(request_id)

    initialized = request(1, "initialize", {"clientInfo": {"name": "codex_native_auth_probe", "title": None, "version": "1"},
        "capabilities": {"experimentalApi": True, "requestAttestation": False}})
    if not isinstance(initialized, dict) or initialized.get("codexHome") != str(codex_home):
        raise ProbeFailure("isolated_home_mismatch")
    ua = initialized.get("userAgent")
    if not isinstance(ua, str) or "/" + VERSION not in ua:
        raise ProbeFailure("runtime_version_mismatch")
    if initialized.get("platformFamily") not in ("unix", "windows") or initialized.get("platformOs") not in ("linux", "macos", "darwin", "windows"):
        raise ProbeFailure("runtime_platform_invalid")
    receipt["runtime"] = {"user_agent_sha256": sha(ua), "platform_family": initialized["platformFamily"], "platform_os": initialized["platformOs"]}
    child.send({"method": "initialized"})
    receipt["client_methods"].append("initialized")
    login = request(2, "account/login/start", {"type": "chatgptAuthTokens", "accessToken": credential["access_token"],
        "chatgptAccountId": credential["account_id"], "chatgptPlanType": None})
    if login != {"type": "chatgptAuthTokens"}:
        raise ProbeFailure("unexpected_login_mode")
    account = request(3, "account/read", {"refreshToken": False})
    if len(notifications) != 2:
        receive_response(None)
    completed, updated = notifications["account/login/completed"], notifications["account/updated"]
    if completed.get("success") is not True or completed.get("error") is not None or completed.get("loginId") is not None:
        raise ProbeFailure("login_not_completed")
    if updated.get("authMode") != "chatgptAuthTokens" or updated.get("planType") != identity["plan_type"]:
        raise ProbeFailure("auth_state_mismatch")
    if not isinstance(account, dict) or account.get("requiresOpenaiAuth") is not True or not isinstance(account.get("account"), dict):
        raise ProbeFailure("account_read_invalid")
    state = account["account"]
    email = state.get("email")
    if state.get("type") != "chatgpt" or state.get("planType") != identity["plan_type"] or (email is not None and not isinstance(email, str)):
        raise ProbeFailure("account_read_mismatch")
    if (sha(email) if email is not None else None) != identity["email_sha256"]:
        raise ProbeFailure("account_email_mismatch")
    receipt["account_read"] = {"type": "chatgpt", "auth_mode": "chatgptAuthTokens", "plan_type": state["planType"],
        "email_sha256": identity["email_sha256"], "requires_openai_auth": True,
        "identity_binding": "pinned_jwt_and_local_auth_state", "remote_token_validity": "not_checked"}


def run_probe(*, cli, pins, raw_credential, deadline, child_factory=_Child):
    started = time.monotonic()
    receipt = {"schema": SCHEMA, "status": "rejected", "cli_version": VERSION,
        "model_calls": 0, "turns": 0, "automatic_retries": False, "server_requests_rejected": 0,
        "client_methods": [], "credential_transport": "stdin_and_memory", "cleanup_completed": False,
        "startup_warnings": [], "zero_turn_only": True, "model_reference_clean": False,
        "remote_control_status": [], "skills_changed_notifications": 0}
    child, root, child_phase = None, None, "version"
    try:
        if type(deadline) not in (int, float) or not math.isfinite(deadline) or deadline - started < .5 or deadline - started > 120:
            raise ProbeFailure("invalid_deadline")
        if not isinstance(pins, dict) or set(pins) != {"cli_sha256", "token_sha256", "account_sha256", "user_sha256"} or any(not isinstance(value, str) or HASH.fullmatch(value) is None for value in pins.values()):
            raise ProbeFailure("invalid_pins")
        credential, identity = credential_state(raw_credential, pins, deadline)
        reject_system_config()
        receipt["identity"] = identity
        receipt["cli_sha256"] = pins["cli_sha256"]
        root = Path(tempfile.mkdtemp(prefix="codex-native-auth-"))
        root.chmod(0o700)
        codex_home = root / "home"
        codex_home.mkdir(mode=0o700)
        (codex_home / "config.toml").write_text(CONFIG, encoding="utf-8")
        target = root / ("codex.exe" if os.name == "nt" else "codex")
        pinned_copy(cli, target, pins["cli_sha256"], deadline)
        environment = isolated_environment(root)
        # Reserve bounded cleanup time inside the single absolute deadline.
        io_deadline = deadline - min(2, (deadline - started) / 4)
        child = child_factory([str(target), "--version"], root, environment, io_deadline)
        if child.receive().strip() != b"codex-cli 0.155.0":
            raise ProbeFailure("cli_version_mismatch")
        child.close()
        receipt["version_cleanup"] = child.cleanup_evidence()
        if child.failed.is_set() or not all(receipt["version_cleanup"].values()):
            raise ProbeFailure("version_process_cleanup_failed")
        child = None
        child_phase = "app_server"
        child = child_factory([str(target), "app-server", "--listen", "stdio://"], root, environment, io_deadline)
        auth_handshake(child, receipt, credential, identity, codex_home)
        receipt["status"] = "completed_with_known_zero_turn_warning" if receipt["startup_warnings"] else "completed"
    except ProbeFailure as error:
        receipt["error"] = str(error)
    except BaseException as error:
        receipt["error"] = "interrupted" if isinstance(error, (KeyboardInterrupt, SystemExit)) else "probe_failed"
    finally:
        if child:
            try:
                child.deadline = deadline
                child.close()
                receipt["child_output"] = child.evidence()
                receipt[child_phase + "_cleanup"] = child.cleanup_evidence()
                if child.failed.is_set():
                    receipt.update(status="rejected", error=child.failure)
            except BaseException:
                receipt.update(status="rejected", error="process_cleanup_failed")
        if root:
            try:
                if (root / "home" / "auth.json").exists():
                    receipt.update(status="rejected", error="unexpected_persisted_auth")
                shutil.rmtree(root)
                receipt["cleanup_completed"] = child is None or all(child.cleanup_evidence().values())
            except OSError:
                receipt.update(status="rejected", error="temporary_cleanup_failed")
        else:
            receipt["cleanup_completed"] = True
        receipt["elapsed_seconds"] = round(time.monotonic() - started, 3)
    return receipt


class Parser(argparse.ArgumentParser):
    def error(self, message):
        raise ProbeFailure("invalid_arguments")


def main(argv=None):
    previous_term = None
    try:
        if threading.current_thread() is threading.main_thread():
            previous_term = signal.getsignal(signal.SIGTERM)
            def terminate(signum, frame):
                raise ProbeFailure("terminated")
            signal.signal(signal.SIGTERM, terminate)
        parser = Parser(description=__doc__)
        parser.add_argument("--cli", type=Path, required=True)
        for field in ("cli", "token", "account", "user"):
            parser.add_argument("--expected-" + field + "-sha256", required=True)
        parser.add_argument("--timeout", type=float, default=30)
        args = parser.parse_args(argv)
        if not math.isfinite(args.timeout) or not 2 <= args.timeout <= 120:
            raise ProbeFailure("invalid_timeout")
        deadline = time.monotonic() + args.timeout
        mailbox = queue.Queue(maxsize=1)
        def read_stdin():
            try:
                # Avoid holding sys.stdin's buffered lock in a blocked daemon:
                # interpreter shutdown must still finish at the deadline.
                chunks, size = [], 0
                while size <= MAX_INPUT:
                    chunk = os.read(sys.stdin.fileno(), min(4096, MAX_INPUT + 1 - size))
                    if not chunk:
                        break
                    chunks.append(chunk)
                    size += len(chunk)
                mailbox.put(b"".join(chunks))
            except BaseException:
                mailbox.put(None)
        threading.Thread(target=read_stdin, daemon=True).start()
        try:
            raw = mailbox.get(timeout=remaining(deadline))
        except queue.Empty:
            raise ProbeFailure("stdin_deadline_exceeded") from None
        result = run_probe(cli=args.cli, pins={field + "_sha256": getattr(args, "expected_" + field + "_sha256")
                           for field in ("cli", "token", "account", "user")}, raw_credential=raw, deadline=deadline)
    except ProbeFailure as error:
        result = {"schema": SCHEMA, "status": "rejected", "error": str(error), "model_calls": 0, "turns": 0}
    except BaseException as error:
        if isinstance(error, SystemExit) and error.code == 0:
            return 0
        result = {"schema": SCHEMA, "status": "rejected", "error": "probe_failed", "model_calls": 0, "turns": 0}
    finally:
        if previous_term is not None:
            signal.signal(signal.SIGTERM, previous_term)
    print(json.dumps(result, sort_keys=True))
    return 0 if result["status"] in SUCCESS_STATUSES else 1


if __name__ == "__main__":
    raise SystemExit(main())
