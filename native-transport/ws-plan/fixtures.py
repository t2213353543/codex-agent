"""Generate proposed WS IPC fixtures and check pinned source assumptions.

This is not a WebSocket engine or a native helper runtime test.
"""
from __future__ import annotations

import argparse
import base64
import hashlib
import json
from pathlib import Path
import struct
import tomllib
import urllib.request


ROOT = Path(__file__).resolve().parent
REPO = ROOT.parents[1]
OFFICIAL = REPO / ".local/codex-source-0.154.0/codex-rs"
PROTOCOL = json.loads((ROOT / "protocol.json").read_text(encoding="utf-8"))
PINNED = {
    "tungstenite-handshake-client.rs": "https://raw.githubusercontent.com/openai-oss-forks/tungstenite-rs/4fffad30fe373adbdcffab9545e9e9bf4f2fc19f/src/handshake/client.rs",
    "tokio-tungstenite-proxy.rs": "https://raw.githubusercontent.com/openai-oss-forks/tokio-tungstenite/0e5b2d73aa18dd9f0a50ee9ff199d5aef7594186/src/proxy.rs",
}


def b64(raw):
    return base64.b64encode(raw).decode("ascii")


def pack(name, payload=b""):
    spec = PROTOCOL["frames"][name]
    if isinstance(payload, dict):
        payload = json.dumps(payload, separators=(",", ":"), ensure_ascii=True).encode()
    if len(payload) > PROTOCOL["max_message_bytes"] + 9:
        raise ValueError("frame_too_large")
    if spec["encoding"] == "json" and len(payload) > PROTOCOL["max_metadata_bytes"]:
        raise ValueError("metadata_too_large")
    return struct.pack("!BI", spec["code"], len(payload)) + payload


def unpack(raw):
    if len(raw) < 5:
        raise ValueError("truncated_prefix")
    code, size = struct.unpack("!BI", raw[:5])
    if size > PROTOCOL["max_message_bytes"] + 9:
        raise ValueError("frame_too_large")
    if size != len(raw) - 5:
        raise ValueError("truncated_or_extra_payload")
    names = {value["code"]: name for name, value in PROTOCOL["frames"].items()}
    if code not in names:
        raise ValueError("unknown_frame")
    if PROTOCOL["frames"][names[code]]["encoding"] == "json" and size > PROTOCOL["max_metadata_bytes"]:
        raise ValueError("metadata_too_large")
    return names[code], raw[5:]


def message(sequence, kind, raw):
    if kind not in (1, 2) or sequence < 1:
        raise ValueError("invalid_message_envelope")
    if kind == 1:
        raw.decode("utf-8", errors="strict")
    return struct.pack("!QB", sequence, kind) + raw


def handshake(status=101, tail=b"", headers=None):
    limit = PROTOCOL["max_handshake_tail_bytes"]
    return {"status": status, "headers": headers or [],
            "body_scope": "none" if status == 101 else "handshake_tail",
            "body_complete": None, "body_base64": b64(tail[:limit]), "tail_truncated": len(tail) > limit}


def step(name, payload=b""):
    raw = pack(name, payload)
    return {"frame": name, "wire_hex": raw.hex()}


def trace_valid(steps):
    """Validate the proposed per-connection IPC lifecycle, not RFC6455."""
    state, accepted, pending, acked = "dialing", False, set(), set()
    for item in steps:
        name, payload = unpack(bytes.fromhex(item["wire_hex"]))
        if name != item["frame"] or state == "ended":
            raise ValueError("frame_state")
        encoding = PROTOCOL["frames"][name]["encoding"]
        if encoding == "json":
            body = json.loads(payload)
            if any(key not in body for key in PROTOCOL["frames"][name]["required"]):
                raise ValueError("missing_metadata")
        if name == "HANDSHAKE":
            if state != "dialing" or not 100 <= body["status"] <= 599:
                raise ValueError("handshake_state")
            if body["status"] == 101:
                if body["body_scope"] != "none" or body["body_base64"]:
                    raise ValueError("accepted_body")
                state, accepted = "open", True
            else:
                if body["body_scope"] != "handshake_tail" or body["body_complete"] is not None:
                    raise ValueError("invented_complete_body")
                tail = base64.b64decode(body["body_base64"], validate=True)
                if len(tail) > PROTOCOL["max_handshake_tail_bytes"] or not isinstance(body["tail_truncated"], bool):
                    raise ValueError("invalid_handshake_tail")
                state = "rejected"
        elif name in ("SEND", "MESSAGE"):
            if state != "open" or len(payload) < 9:
                raise ValueError("message_state")
            sequence, kind = struct.unpack("!QB", payload[:9])
            message(sequence, kind, payload[9:])
            if name == "SEND":
                if sequence in pending or sequence in acked:
                    raise ValueError("replayed_send_sequence")
                pending.add(sequence)
        elif name == "WRITE_ACK":
            if state != "open" or len(payload) != 8:
                raise ValueError("ack_state")
            sequence, = struct.unpack("!Q", payload)
            if sequence not in pending:
                raise ValueError("unknown_ack")
            pending.remove(sequence)
            acked.add(sequence)
        elif name in ("CLOSE", "SEND_CLOSE"):
            if state not in ("open", "closing"):
                raise ValueError("close_state")
            if body["code"] is not None and (not isinstance(body["code"], int) or not 1000 <= body["code"] <= 4999 or body["code"] in (1004, 1005, 1006, 1015)):
                raise ValueError("invalid_close_code")
            if not isinstance(body["reason"], str) or len(body["reason"].encode()) > 123:
                raise ValueError("invalid_close_reason")
            state = "closing"
        elif name == "ERROR":
            if body["accepted"] != accepted:
                raise ValueError("invented_acceptance")
            state = "failed"
        elif name == "CANCEL":
            if payload:
                raise ValueError("cancel_payload")
            state = "cancelled"
        elif name == "END":
            if payload or state not in ("rejected", "closing", "failed", "cancelled"):
                raise ValueError("end_without_terminal_transport_state")
            state = "ended"
    if state != "ended":
        raise ValueError("truncated_session")
    return {"accepted": accepted, "unacknowledged_sends": len(pending), "replay_allowed": False}


def fixture_cases():
    complete = b'{"type":"response.completed","response":{"id":"fixture-response","output":[]}}'
    create = b'{"type":"response.create","model":"fixture","input":[]}'
    common = [step("HANDSHAKE", handshake()), step("SEND", message(1, 1, create)), step("WRITE_ACK", struct.pack("!Q", 1))]
    cases = []
    def add(name, steps, runtime):
        cases.append({"name": name, "steps": steps, "model_result": trace_valid(steps), "runtime_verified": False, "required_runtime_assertions": runtime})
    for status, body, headers in [
        (401, b'{"error":"fixture-expired"}', [["x-request-id", b64(b"fixture-rejection")]]),
        (401, b"", []),
        (401, b'1a\r\n{"error":"partial', [["transfer-encoding", b64(b"chunked")]]),
        (429, b"\xff\x00fixture-tail", [["retry-after", b64(b"3")], ["x-fixture", b64(b"a")], ["x-fixture", b64(b"b")]]),
    ]:
        add(f"target_{status}_tail_{len(body)}", [step("HANDSHAKE", handshake(status, body, headers)), step("END")], ["Preserve target status and duplicate headers", "Do not claim complete or UTF-8-only error body", "401 retry belongs to Go and remains bounded"])
    add("proxy_connect_rejection", [step("ERROR", {"phase": "proxy_connect", "code": "proxy_connect_failed", "accepted": False}), step("END")], ["Proxy 407 never triggers target OAuth recovery", "No direct fallback or second connection"])
    add("handshake_timeout", [step("ERROR", {"phase": "handshake", "code": "timeout", "accepted": False}), step("END")], ["Unknown target HTTP status stays absent", "Handshake timeout cancels dial"])
    add("tool_turn_complete", common + [step("MESSAGE", message(1, 1, complete)), step("SEND_CLOSE", {"code": 1000, "reason": "fixture done"}), step("CLOSE", {"code": 1000, "reason": ""}), step("END")], ["Deflate dictionaries owned once by Rust", "Exact decoded tool payload bytes and stream_id preserved"])
    tool = b'{"type":"response.output_item.done","stream_id":"fixture-stream","item":{"type":"function_call","name":"shell","arguments":"{}","call_id":"fixture-call"}}'
    output = b'{"type":"response.create","model":"fixture","stream_id":"fixture-stream","input":[{"type":"function_call_output","call_id":"fixture-call","output":"fixture-output"}]}'
    add("tool_result_next_turn_same_socket", common + [step("MESSAGE", message(1, 1, tool)), step("MESSAGE", message(2, 1, complete)), step("SEND", message(2, 1, output)), step("WRITE_ACK", struct.pack("!Q", 2)), step("MESSAGE", message(3, 1, complete)), step("CLOSE", {"code": 1000, "reason": ""}), step("END")], ["One handshake covers both turns", "Tool call_id/output and stream_id bytes unchanged", "Each turn retains independent Go admission / permissions / usage"])
    add("accepted_ambiguous_write", [step("HANDSHAKE", handshake()), step("SEND", message(1, 1, create)), step("ERROR", {"phase": "write", "code": "connection_lost", "accepted": True}), step("END")], ["Missing WRITE_ACK is not proof of unsent request", "No helper or Go replay / account switch"])
    add("fragmented_binary_message", [step("HANDSHAKE", handshake()), step("MESSAGE", message(1, 2, b"\x00\xfffixture-binary")), step("CLOSE", {"code": 1000, "reason": ""}), step("END")], ["Official fork reassembles fragments before IPC", "Gateway can retain existing text-only Responses policy"])
    add("ping_then_policy_close", [step("HANDSHAKE", handshake()), step("CLOSE", {"code": 1008, "reason": "fixture policy"}), step("END")], ["Upstream Ping gets identical Pong locally", "Ping/Pong absent from application messages and TTFT", "101 without a create is not a successful model turn"])
    add("parent_cancellation", common + [step("CANCEL"), step("END")], ["Parent socket/pipe cancellation drops only the session", "No invented successful close or terminal response"])
    add("slow_ipc_consumer", common + [step("ERROR", {"phase": "ipc_write", "code": "backpressure_timeout", "accepted": True}), step("END")], ["Queue bytes bounded under incompressible large messages", "Other sessions continue", "No dropped message disguised as completion"])
    add("invalid_compressed_message", [step("HANDSHAKE", handshake()), step("ERROR", {"phase": "read", "code": "websocket_protocol", "accepted": True}), step("END")], ["Official fork rejects invalid RSV / deflate data", "No Go re-decoding or recovery replay"])
    add("decompressed_limit", [step("HANDSHAKE", handshake()), step("ERROR", {"phase": "read", "code": "message_too_large", "accepted": True}), step("END")], ["32 MiB limit is decompressed message size", "Fragmented expansion cannot bypass limit"])
    return cases


def source_checks(fetch=False, source_root=OFFICIAL):
    cache = ROOT / "pinned-source"
    if fetch:
        cache.mkdir(exist_ok=True)
        for name, url in PINNED.items():
            with urllib.request.urlopen(url, timeout=30) as response:
                raw = response.read(2 << 20)
            if len(raw) >= 2 << 20:
                raise ValueError("pinned_source_too_large")
            (cache / name).write_bytes(raw)
    sentinels = {
        "websocket-client/src/lib.rs": ["pub async fn connect(", "async fn connect_with_route(", "self.connect_with_route(request, config, proxy_route, /*loopback_direct*/ false)", "Some(build_rustls_client_config_with_custom_ca()?)", "type Item = Result<Message, WebSocketError>;"],
        "websocket-client/src/dialer.rs": ["OutboundProxyRoute::TransportDefault", "no_proxy: None", "connect_via_proxy(stream, &proxy.config, host, port).await?", "tls_config.map(Connector::Rustls)"],
        "http-client/src/outbound_proxy.rs": ["pub enum OutboundProxyPolicy", "ReqwestDefault", "RespectSystemProxy", "pub enum OutboundProxyRoute", "pub const fn new(outbound_proxy_policy: OutboundProxyPolicy)"],
        "codex-api/src/endpoint/responses_websocket.rs": ["Some(DeflateConfig::default())", "Ok(Message::Ping(payload))", "inner.send(Message::Pong(payload)).await", "Ok(Message::Pong(_)) => {}", "WsError::Http(response)", "String::from_utf8(bytes.clone()).ok()", "mpsc::unbounded_channel::<Result<Message, WsError>>()"],
    }
    fork_checks = {
        "tungstenite-handshake-client.rs": ["pub type Response = HttpResponse<Option<Vec<u8>>>;", "*e.body_mut() = Some(tail);", "response.status() != StatusCode::SWITCHING_PROTOCOLS", "permessage-deflate; client_max_window_bits"],
        "tokio-tungstenite-proxy.rs": ["ProxyScheme::Socks5 | tungstenite::proxy::ProxyScheme::Socks5h", "if !(200..300).contains(&status)", "UrlError::ProxyConnect", "MAX_CONNECT_RESPONSE_SIZE: usize = 8192"],
    }
    proofs = []
    for relative, required in sentinels.items():
        path = source_root / relative
        raw = path.read_bytes()
        text = raw.decode()
        checks = []
        for sentinel in required:
            if sentinel not in text:
                raise AssertionError(f"source_changed: {relative}: {sentinel}")
            checks.append({"sentinel": sentinel, "line": text[:text.index(sentinel)].count("\n") + 1})
        if relative.endswith("websocket-client/src/lib.rs") and "pub async fn connect_with_route(" in text:
            raise AssertionError("private_route_boundary_changed")
        proofs.append({"source": relative, "sha256": hashlib.sha256(raw).hexdigest(), "checks": checks})
    for name, required in fork_checks.items():
        path = cache / name
        if not path.exists():
            raise FileNotFoundError("Run with --fetch-pinned once to inspect the exact two public fork files")
        raw = path.read_bytes()
        text = raw.decode()
        checks = []
        for sentinel in required:
            if sentinel not in text:
                raise AssertionError(f"fork_source_changed: {name}: {sentinel}")
            checks.append({"sentinel": sentinel, "line": text[:text.index(sentinel)].count("\n") + 1})
        proofs.append({"source": PINNED[name], "sha256": hashlib.sha256(raw).hexdigest(), "checks": checks})
    manifest = tomllib.loads((source_root / "Cargo.toml").read_text())
    lock = tomllib.loads((source_root / "Cargo.lock").read_text())
    versions = {p["name"]: p for p in lock["package"] if p["name"] in ("tungstenite", "tokio-tungstenite")}
    for name, revision in {"tungstenite": "4fffad30fe373adbdcffab9545e9e9bf4f2fc19f", "tokio-tungstenite": "0e5b2d73aa18dd9f0a50ee9ff199d5aef7594186"}.items():
        assert manifest["patch"]["crates-io"][name]["rev"] == revision
        assert versions[name]["source"].endswith("#" + revision)
    assert {"deflate", "proxy"} <= set(manifest["workspace"]["dependencies"]["tungstenite"]["features"])
    assert {"proxy", "rustls-tls-native-roots"} <= set(manifest["workspace"]["dependencies"]["tokio-tungstenite"]["features"])
    return {"scope": "static_source_sentinels_and_structured_cargo_pins", "runtime_verified": False, "passed": True, "proofs": proofs, "forks": versions}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--fetch-pinned", action="store_true")
    parser.add_argument("--write", action="store_true")
    parser.add_argument("--source-root", type=Path, default=OFFICIAL)
    args = parser.parse_args()
    evidence = source_checks(args.fetch_pinned, args.source_root)
    cases = fixture_cases()
    vectors = {"scope": "proposed_ipc_not_rfc6455", "runtime_verified": False, "cases": cases}
    coverage = {"scope": "native_websocket_runtime_acceptance_plan", "implemented": False, "cases": [{"name": case["name"], "runtime_verified": False, "assertions": case["required_runtime_assertions"]} for case in cases], "routing_cases": ["fixed_http_proxy", "fixed_https_proxy", "fixed_socks5h_proxy", "no_proxy_environment_ignored", "proxy_rotation_rejects_new_handshake", "accepted_proxy_generation_does_not_migrate", "http_native_and_ws_go_explicit_diagnostics"]}
    if args.write:
        output = ROOT / "generated"
        output.mkdir(exist_ok=True)
        for name, value in {"source-checks.json": evidence, "protocol-vectors.json": vectors, "fixtures.json": coverage}.items():
            (output / name).write_text(json.dumps(value, indent=2, ensure_ascii=True) + "\n", encoding="utf-8")
    print(json.dumps({"source_checks_passed": True, "ipc_model_cases": len(cases), "runtime_verified": False, "production_modified": False}))


if __name__ == "__main__":
    main()
