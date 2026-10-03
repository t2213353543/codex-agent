//! Boundary tests exercise real WebSocket framing where wire behavior matters,
//! and isolated IPC/pump halves where precise failure timing matters.

use super::*;
use std::pin::Pin;
use std::task::{Context, Poll};
use tokio::io::DuplexStream;
use tokio::net::TcpListener;

const TEST_TIMEOUT: Duration = Duration::from_millis(50);

async fn send(stream: &mut DuplexStream, kind: u8, data: &[u8]) {
    stream.write_u8(kind).await.unwrap();
    stream.write_u32(data.len() as u32).await.unwrap();
    stream.write_all(data).await.unwrap();
}

fn message(sequence: u64, kind: u8, bytes: &[u8]) -> Vec<u8> {
    let mut data = sequence.to_be_bytes().to_vec();
    data.push(kind);
    data.extend_from_slice(bytes);
    data
}

async fn packet(stream: &mut DuplexStream) -> (u8, Vec<u8>) {
    timeout(Duration::from_secs(5), async {
        let kind = stream.read_u8().await.unwrap();
        let length = stream.read_u32().await.unwrap() as usize;
        assert!(length <= MAX_BODY + 9);
        let mut bytes = vec![0; length];
        stream.read_exact(&mut bytes).await.unwrap();
        (kind, bytes)
    }).await.unwrap()
}

async fn input_fixture(accepted: bool, limit: usize) -> (DuplexStream, mpsc::Receiver<Command>, Arc<AtomicBool>, tokio::task::JoinHandle<Result<()>>) {
    let (client, mut server) = tokio::io::duplex(4096);
    let (commands, receiver) = mpsc::channel(1);
    let busy = Arc::new(AtomicBool::new(false));
    let input_busy = busy.clone();
    let task = tokio::spawn(async move {
        input(&mut server, commands, Arc::new(AtomicBool::new(accepted)), input_busy, limit, TEST_TIMEOUT).await
    });
    (client, receiver, busy, task)
}

async fn transaction_fixture(origin: String, limit: usize, headers: Vec<(&str, &str)>) -> (DuplexStream, tokio::task::JoinHandle<()>) {
    transaction_budget_fixture(origin, limit, headers, None).await
}

async fn transaction_budget_fixture(origin: String, limit: usize, headers: Vec<(&str, &str)>, total_timeout_ms: Option<u64>) -> (DuplexStream, tokio::task::JoinHandle<()>) {
    let config: crate::Config = serde_json::from_value(serde_json::json!({
        "protocol": 1, "listen": "127.0.0.1:0", "upstream": origin,
        "ipc_token": "a".repeat(64), "headers_timeout_ms": if total_timeout_ms.is_some() { 0 } else { 2000 }, "idle_timeout_ms": 0
    })).unwrap();
    let state = crate::build_state(&config).unwrap();
    let pairs: Vec<_> = headers.into_iter().map(|(name, value)| (name.to_owned(), BASE64.encode(value))).collect();
    let head: RequestHead = serde_json::from_value(serde_json::json!({
        "protocol": 1, "ipc_token": "a".repeat(64), "method": "GET", "path": "/responses?fixture=1",
        "headers": pairs, "body_length": 0, "operation": "websocket_message_v1",
        "max_message_bytes": limit, "write_timeout_ms": 2000, "total_timeout_ms": total_timeout_ms
    })).unwrap();
    let (client, server) = tokio::io::duplex(4096);
    let task = tokio::spawn(async move {
        let (method, url, headers) = crate::request_parts(&head, &state).unwrap();
        let (mut read, mut write) = tokio::io::split(server);
        transaction(&state, &head, method, url, headers, &mut read, &mut write).await;
    });
    (client, task)
}

#[tokio::test]
async fn explicit_total_budget_cancels_unfinished_tls_handshake_and_closes_peer() {
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let origin = format!("https://{}", listener.local_addr().unwrap());
    let server = tokio::spawn(async move {
        let (mut stream, _) = listener.accept().await.unwrap();
        let mut hello = [0; 5];
        stream.read_exact(&mut hello).await.unwrap();
        assert_eq!(hello[0], 22); // Actual TLS handshake; deliberately no ServerHello.
        let mut tail = Vec::new();
        timeout(Duration::from_secs(2), stream.read_to_end(&mut tail)).await.unwrap().unwrap();
    });
    let (mut ipc, task) = transaction_budget_fixture(origin, MAX_BODY, vec![], Some(150)).await;
    let (kind, bytes) = packet(&mut ipc).await;
    assert_eq!(kind, ERROR);
    let error: serde_json::Value = serde_json::from_slice(&bytes).unwrap();
    assert_eq!(error, serde_json::json!({"phase":"handshake","code":"total_timeout","accepted":false}));
    timeout(Duration::from_secs(2), task).await.unwrap().unwrap();
    server.await.unwrap();
}

#[tokio::test]
async fn explicit_total_budget_covers_established_socket_and_keeps_accepted_true() {
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let origin = format!("http://{}", listener.local_addr().unwrap());
    let server = tokio::spawn(async move {
        let (stream, _) = listener.accept().await.unwrap();
        let mut ws = tokio_tungstenite::accept_async_with_config(stream, Some(websocket_config(MAX_BODY))).await.unwrap();
        let result = timeout(Duration::from_secs(5), ws.next()).await.unwrap();
        assert!(!matches!(result, Some(Ok(Message::Text(_)))));
    });
    let (mut ipc, task) = transaction_budget_fixture(origin, MAX_BODY, vec![], Some(2000)).await;
    assert_eq!(packet(&mut ipc).await.0, HANDSHAKE);
    let (kind, bytes) = packet(&mut ipc).await;
    assert_eq!(kind, ERROR);
    let error: serde_json::Value = serde_json::from_slice(&bytes).unwrap();
    assert_eq!(error, serde_json::json!({"phase":"message","code":"total_timeout","accepted":true}));
    task.await.unwrap();
    server.await.unwrap();
}

#[tokio::test]
async fn caller_cancel_preempts_total_budget_without_error_packet() {
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let origin = format!("https://{}", listener.local_addr().unwrap());
    let (mut ipc, task) = transaction_budget_fixture(origin, MAX_BODY, vec![], Some(1000)).await;
    let (mut upstream, _) = listener.accept().await.unwrap();
    let mut hello = [0; 5];upstream.read_exact(&mut hello).await.unwrap();
    send(&mut ipc, CANCEL, &[]).await;
    timeout(Duration::from_millis(500), task).await.unwrap().unwrap();
    let mut output = Vec::new();ipc.read_to_end(&mut output).await.unwrap();
    assert!(output.is_empty());
    let mut tail = Vec::new();
    timeout(Duration::from_millis(500), upstream.read_to_end(&mut tail)).await.unwrap().unwrap();
}

#[test]
fn application_limit_preserves_cli_frame_and_deflate_configuration() {
    for limit in [1, 125, 1024, MAX_BODY] {
        let config = websocket_config(limit);
        assert_eq!(config.max_message_size, Some(limit));
        assert_eq!(config.max_frame_size, WebSocketConfig::default().max_frame_size);
        assert!(config.extensions.permessage_deflate.is_some());
    }
}

#[tokio::test]
async fn tiny_messages_allow_compressed_wire_overhead_and_full_control_payloads() {
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let origin = format!("http://{}", listener.local_addr().unwrap());
    let server = tokio::spawn(async move {
        let (stream, _) = listener.accept().await.unwrap();
        let mut ws = tokio_tungstenite::accept_hdr_async_with_config(stream, |request: &tungstenite::handshake::server::Request, response| {
            assert_eq!(request.uri().path_and_query().unwrap().as_str(), "/responses?fixture=1");
            assert_eq!(request.headers().get_all("x-repeat").iter().count(), 2);
            assert_eq!(request.headers().get("sec-websocket-extensions").unwrap(), "permessage-deflate; client_max_window_bits");
            Ok(response)
        }, Some(websocket_config(MAX_BODY))).await.unwrap();
        ws.send(Message::Binary(vec![42].into())).await.unwrap();
        ws.send(Message::Ping(vec![3; 125].into())).await.unwrap();
        assert_eq!(ws.next().await.unwrap().unwrap(), Message::Pong(vec![3; 125].into()));
        ws.send(Message::Close(Some(CloseFrame { code: 1000.into(), reason: "x".repeat(123).into() }))).await.unwrap();
        let _ = ws.next().await;
    });
    let (mut ipc, task) = transaction_fixture(origin, 1, vec![("x-repeat", "one"), ("x-repeat", "two")]).await;
    assert_eq!(packet(&mut ipc).await.0, HANDSHAKE);
    assert_eq!(packet(&mut ipc).await, (MESSAGE, message(1, 2, &[42])));
    let (kind, close) = packet(&mut ipc).await;
    assert_eq!(kind, CLOSE);
    assert_eq!(serde_json::from_slice::<serde_json::Value>(&close).unwrap()["reason"], "x".repeat(123));
    assert_eq!(packet(&mut ipc).await, (END, vec![]));
    task.await.unwrap();
    server.await.unwrap();
}

#[tokio::test]
async fn cli_connector_lifecycle_preserves_multiple_turns_on_one_socket() {
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let origin = format!("http://{}", listener.local_addr().unwrap());
    let server = tokio::spawn(async move {
        let (stream, _) = listener.accept().await.unwrap();
        let mut ws = tokio_tungstenite::accept_async_with_config(stream, Some(websocket_config(MAX_BODY))).await.unwrap();
        for value in ["first turn", "second turn", "third turn"] {
            assert_eq!(ws.next().await.unwrap().unwrap(), Message::Text(value.into()));
            ws.send(Message::Text(value.into())).await.unwrap();
        }
        ws.send(Message::Close(None)).await.unwrap();
        let _ = ws.next().await;
        assert!(timeout(TEST_TIMEOUT, listener.accept()).await.is_err());
    });
    let (mut ipc, task) = transaction_fixture(origin, MAX_BODY, vec![]).await;
    assert_eq!(packet(&mut ipc).await.0, HANDSHAKE);
    for (index, value) in ["first turn", "second turn", "third turn"].iter().enumerate() {
        let sequence = index as u64 + 1;
        send(&mut ipc, SEND, &message(sequence, 1, value.as_bytes())).await;
        assert_eq!(packet(&mut ipc).await, (WRITE_ACK, sequence.to_be_bytes().to_vec()));
        assert_eq!(packet(&mut ipc).await, (MESSAGE, message(sequence, 1, value.as_bytes())));
    }
    assert_eq!(packet(&mut ipc).await.0, CLOSE);
    assert_eq!(packet(&mut ipc).await, (END, vec![]));
    task.await.unwrap();
    server.await.unwrap();
}

#[tokio::test]
async fn close_metadata_is_not_limited_by_application_message_size() {
    let (mut ipc, mut commands, _, task) = input_fixture(true, 1).await;
    send(&mut ipc, SEND_CLOSE, br#"{"code":1000,"reason":"normal closure"}"#).await;
    match commands.recv().await.unwrap() {
        Command::Close(Some(frame)) => {
            assert_eq!(u16::from(frame.code), 1000);
            assert_eq!(frame.reason.as_str(), "normal closure");
        }
        _ => panic!("expected valid close"),
    }
    send(&mut ipc, CANCEL, &[]).await;
    assert_eq!(task.await.unwrap(), Err("cancelled"));
}

#[tokio::test]
async fn close_validates_wire_codes_utf8_byte_length_and_missing_code() {
    for code in [999, 1004, 1005, 1006, 1015, 2000, 5000] {
        let (mut ipc, _, _, task) = input_fixture(true, MAX_BODY).await;
        let close = serde_json::to_vec(&serde_json::json!({"code": code, "reason": ""})).unwrap();
        send(&mut ipc, SEND_CLOSE, &close).await;
        assert_eq!(task.await.unwrap(), Err("ws_close_invalid"), "code {code}");
    }
    for close in [
        serde_json::json!({"code": 1000, "reason": "é".repeat(62)}),
        serde_json::json!({"code": null, "reason": "missing status"}),
        serde_json::json!({"code": 1000, "reason": "", "unknown": true}),
    ] {
        let (mut ipc, _, _, task) = input_fixture(true, MAX_BODY).await;
        send(&mut ipc, SEND_CLOSE, &serde_json::to_vec(&close).unwrap()).await;
        assert_eq!(task.await.unwrap(), Err("ws_close_invalid"));
    }
}

#[tokio::test]
async fn valid_private_close_code_and_empty_close_are_accepted() {
    for close in [serde_json::json!({"code": 4001, "reason": "application"}), serde_json::json!({"code": null, "reason": ""})] {
        let (mut ipc, mut commands, _, task) = input_fixture(true, MAX_BODY).await;
        send(&mut ipc, SEND_CLOSE, &serde_json::to_vec(&close).unwrap()).await;
        assert!(matches!(commands.recv().await, Some(Command::Close(_))));
        send(&mut ipc, CANCEL, &[]).await;
        assert_eq!(task.await.unwrap(), Err("cancelled"));
    }
}

#[tokio::test]
async fn invalid_text_kind_and_sequence_are_rejected_before_network_send() {
    for (payload, error) in [
        (message(1, 1, &[255]), "ws_text_utf8"),
        (message(1, 3, &[]), "ws_message_kind"),
        (message(0, 1, &[]), "ws_sequence"),
        (message(2, 1, &[]), "ws_sequence"),
    ] {
        let (mut ipc, mut commands, _, task) = input_fixture(true, 16).await;
        send(&mut ipc, SEND, &payload).await;
        assert_eq!(task.await.unwrap(), Err(error));
        assert!(commands.recv().await.is_none());
    }
}

#[tokio::test]
async fn duplicate_sequence_is_rejected_after_first_command_ack() {
    let (mut ipc, mut commands, busy, task) = input_fixture(true, 16).await;
    send(&mut ipc, SEND, &message(1, 1, b"first")).await;
    assert!(matches!(commands.recv().await, Some(Command::Send(1, _))));
    busy.store(false, Ordering::Release);
    send(&mut ipc, SEND, &message(1, 1, b"duplicate")).await;
    assert_eq!(task.await.unwrap(), Err("ws_sequence"));
}

#[tokio::test]
async fn second_command_without_ack_is_rejected_even_after_dequeue() {
    let (mut ipc, mut commands, _, task) = input_fixture(true, 16).await;
    send(&mut ipc, SEND, &message(1, 1, b"first")).await;
    assert!(matches!(commands.recv().await, Some(Command::Send(1, _))));
    send(&mut ipc, SEND, &message(2, 1, b"second")).await;
    assert_eq!(task.await.unwrap(), Err("ws_command_invalid"));
}

#[tokio::test]
async fn partial_ipc_header_and_payload_have_a_single_command_deadline() {
    for prefix in [vec![SEND, 0], vec![SEND, 0, 0, 0, 10, 0]] {
        let (mut ipc, _, _, task) = input_fixture(true, 16).await;
        ipc.write_all(&prefix).await.unwrap();
        assert_eq!(timeout(Duration::from_secs(1), task).await.unwrap().unwrap(), Err("ws_ipc_read_timeout"));
    }
}

#[tokio::test]
async fn between_turns_idle_does_not_use_command_deadline() {
    let (mut ipc, mut commands, _, task) = input_fixture(true, 16).await;
    tokio::time::sleep(TEST_TIMEOUT * 2).await;
    send(&mut ipc, SEND, &message(1, 2, b"ready")).await;
    assert!(matches!(commands.recv().await, Some(Command::Send(1, _))));
    send(&mut ipc, CANCEL, &[]).await;
    assert_eq!(task.await.unwrap(), Err("cancelled"));
}

#[tokio::test]
async fn oversized_command_is_rejected_without_waiting_for_payload() {
    for (kind, length) in [(SEND, 11), (SEND, 8), (SEND_CLOSE, MAX_HEAD + 1), (CANCEL, 1)] {
        let (mut ipc, _, _, task) = input_fixture(true, 1).await;
        ipc.write_u8(kind).await.unwrap();
        ipc.write_u32(length as u32).await.unwrap();
        assert_eq!(task.await.unwrap(), Err("ws_command_invalid"));
    }
}

#[tokio::test]
async fn cancel_works_before_acceptance_but_send_does_not() {
    for (kind, data, error) in [(CANCEL, vec![], "cancelled"), (SEND, message(1, 1, b"x"), "ws_before_handshake")] {
        let (mut ipc, _, _, task) = input_fixture(false, 16).await;
        send(&mut ipc, kind, &data).await;
        assert_eq!(task.await.unwrap(), Err(error));
    }
}

#[tokio::test]
async fn ack_releases_input_before_publication_without_interleaving_packets() {
    let (mut client, mut server) = tokio::io::duplex(8);
    let (packets, receiver) = mpsc::channel(1);
    let busy = Arc::new(AtomicBool::new(true));
    let output_busy = busy.clone();
    let task = tokio::spawn(async move {
        output(&mut server, &mut false, receiver, output_busy, Duration::from_secs(1)).await
    });
    emit(&packets, WRITE_ACK, 1_u64.to_be_bytes().to_vec()).await.unwrap();
    assert_eq!(client.read_u8().await.unwrap(), WRITE_ACK);
    assert!(!busy.load(Ordering::Acquire));
    assert_eq!(client.read_u32().await.unwrap(), 8);
    assert_eq!(client.read_u64().await.unwrap(), 1);
    emit(&packets, END, vec![]).await.unwrap();
    assert_eq!(packet(&mut client).await, (END, vec![]));
    assert_eq!(task.await.unwrap(), Ok(()));
}

#[tokio::test]
async fn blocked_ipc_output_times_out_without_appending_error_to_partial_frame() {
    let (_client, mut server) = tokio::io::duplex(8);
    let mut in_frame = false;
    assert_eq!(write_packet(&mut server, &mut in_frame, MESSAGE, &[0; 64], TEST_TIMEOUT).await, Err("ws_ipc_write_timeout"));
    assert!(in_frame);
}

struct StalledConnection { blocked_write: bool }

impl Stream for StalledConnection {
    type Item = std::result::Result<Message, WsError>;
    fn poll_next(self: Pin<&mut Self>, _: &mut Context<'_>) -> Poll<Option<Self::Item>> { Poll::Pending }
}

impl Sink<Message> for StalledConnection {
    type Error = WsError;
    fn poll_ready(self: Pin<&mut Self>, _: &mut Context<'_>) -> Poll<std::result::Result<(), Self::Error>> { Poll::Ready(Ok(())) }
    fn start_send(self: Pin<&mut Self>, _: Message) -> std::result::Result<(), Self::Error> { Ok(()) }
    fn poll_flush(self: Pin<&mut Self>, _: &mut Context<'_>) -> Poll<std::result::Result<(), Self::Error>> {
        if self.blocked_write { Poll::Pending } else { Poll::Ready(Ok(())) }
    }
    fn poll_close(self: Pin<&mut Self>, context: &mut Context<'_>) -> Poll<std::result::Result<(), Self::Error>> { self.poll_flush(context) }
}

#[tokio::test]
async fn blocked_upstream_write_times_out_without_false_ack() {
    let (commands, receiver) = mpsc::channel(1);
    let (packets, mut events) = mpsc::channel(1);
    commands.send(Command::Send(1, Message::Text("request".into()))).await.unwrap();
    assert_eq!(pump(StalledConnection { blocked_write: true }, receiver, packets, TEST_TIMEOUT, 16).await, Err("ws_write_timeout"));
    assert!(events.recv().await.is_none());
}

// Unlike a network scheduling stress test, this source makes a complete
// message and the following peer failure ready in the same pump poll.
struct MessageThenFailure { state: u8, await_send: bool, sent: bool }

impl Stream for MessageThenFailure {
    type Item = std::result::Result<Message, WsError>;
    fn poll_next(mut self: Pin<&mut Self>, _: &mut Context<'_>) -> Poll<Option<Self::Item>> {
        if self.await_send && !self.sent { return Poll::Pending; }
        let result = match self.state {
            0 => Some(Ok(Message::Text("complete-terminal-event".into()))),
            1 => Some(Err(WsError::ConnectionClosed)),
            _ => None,
        };
        self.state += 1;
        Poll::Ready(result)
    }
}

impl Sink<Message> for MessageThenFailure {
    type Error = WsError;
    fn poll_ready(self: Pin<&mut Self>, _: &mut Context<'_>) -> Poll<std::result::Result<(), Self::Error>> { Poll::Ready(Ok(())) }
    fn start_send(mut self: Pin<&mut Self>, _: Message) -> std::result::Result<(), Self::Error> { self.sent = true; Ok(()) }
    fn poll_flush(self: Pin<&mut Self>, _: &mut Context<'_>) -> Poll<std::result::Result<(), Self::Error>> { Poll::Ready(Ok(())) }
    fn poll_close(self: Pin<&mut Self>, _: &mut Context<'_>) -> Poll<std::result::Result<(), Self::Error>> { Poll::Ready(Ok(())) }
}

#[derive(Default)]
struct RecordedIPC(Vec<u8>);

impl AsyncWrite for RecordedIPC {
    fn poll_write(mut self: Pin<&mut Self>, _: &mut Context<'_>, bytes: &[u8]) -> Poll<std::io::Result<usize>> {
        self.0.extend_from_slice(bytes);
        Poll::Ready(Ok(bytes.len()))
    }
    fn poll_flush(self: Pin<&mut Self>, _: &mut Context<'_>) -> Poll<std::io::Result<()>> { Poll::Ready(Ok(())) }
    fn poll_shutdown(self: Pin<&mut Self>, _: &mut Context<'_>) -> Poll<std::io::Result<()>> { Poll::Ready(Ok(())) }
}

fn recorded_packets(raw: &[u8]) -> Vec<(u8, Vec<u8>)> {
    let mut result = vec![];
    let mut offset = 0;
    while offset < raw.len() {
        assert!(raw.len() - offset >= 5, "partial IPC frame header");
        let length = u32::from_be_bytes(raw[offset + 1..offset + 5].try_into().unwrap()) as usize;
        assert!(raw.len() - offset - 5 >= length, "partial IPC frame body");
        result.push((raw[offset], raw[offset + 5..offset + 5 + length].to_vec()));
        offset += 5 + length;
    }
    result
}

#[tokio::test]
async fn relay_peer_failure_drains_preceding_message_before_error() {
    for await_send in [false, true] {
        let (commands, receiver) = mpsc::channel(1);
        if await_send { commands.send(Command::Send(1, Message::Text("request".into()))).await.unwrap(); }
        let mut writer = RecordedIPC::default();
        let mut in_frame = false;
        let result = relay(MessageThenFailure { state: 0, await_send, sent: false }, receiver, &mut writer, &mut in_frame, Arc::new(AtomicBool::new(await_send)), TEST_TIMEOUT, 1024).await;
        assert_eq!(result, Ok(()), "peer failure must be an ordered IPC event, not a cancellation of queued output");
        let packets = recorded_packets(&writer.0);
        let expected = if await_send { vec![WRITE_ACK, MESSAGE, ERROR, END] } else { vec![MESSAGE, ERROR, END] };
        assert_eq!(packets.iter().map(|packet| packet.0).collect::<Vec<_>>(), expected);
        let message = &packets[usize::from(await_send)].1;
        assert_eq!(&message[..8], &1_u64.to_be_bytes());
        assert_eq!(&message[8..], b"\x01complete-terminal-event");
        let error: serde_json::Value = serde_json::from_slice(&packets[usize::from(await_send) + 1].1).unwrap();
        assert_eq!(error["code"], "ws_connection_closed");
        assert_eq!(error["phase"], "message");
        assert_eq!(error["accepted"], true);
        assert!(!in_frame);
        drop(commands);
    }
}

struct FailedIPC;

impl AsyncWrite for FailedIPC {
    fn poll_write(self: Pin<&mut Self>, _: &mut Context<'_>, _: &[u8]) -> Poll<std::io::Result<usize>> {
        Poll::Ready(Err(std::io::ErrorKind::BrokenPipe.into()))
    }
    fn poll_flush(self: Pin<&mut Self>, _: &mut Context<'_>) -> Poll<std::io::Result<()>> { Poll::Ready(Ok(())) }
    fn poll_shutdown(self: Pin<&mut Self>, _: &mut Context<'_>) -> Poll<std::io::Result<()>> { Poll::Ready(Ok(())) }
}

#[tokio::test]
async fn relay_failed_output_cancels_pending_upstream_without_waiting() {
    let (commands, receiver) = mpsc::channel(1);
    commands.send(Command::Send(1, Message::Text("request".into()))).await.unwrap();
    let mut writer = FailedIPC;
    let mut in_frame = false;
    let result = timeout(Duration::from_secs(1), relay(StalledConnection { blocked_write: false }, receiver, &mut writer, &mut in_frame, Arc::new(AtomicBool::new(true)), Duration::from_secs(60), 1024)).await.unwrap();
    assert_eq!(result, Err("ws_ipc_write"));
    assert!(in_frame, "a partial or failed frame must suppress an appended outer error");
    drop(commands);
}

#[tokio::test]
async fn unacknowledged_close_has_a_deadline_without_turn_idle_timeout() {
    let (commands, receiver) = mpsc::channel(1);
    let (packets, mut events) = mpsc::channel(1);
    commands.send(Command::Close(None)).await.unwrap();
    assert_eq!(pump(StalledConnection { blocked_write: false }, receiver, packets, TEST_TIMEOUT, 16).await, Err("ws_close_timeout"));
    assert!(events.recv().await.is_none());
}

#[tokio::test]
async fn cancellation_interrupts_full_ipc_output_queue() {
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let origin = format!("http://{}", listener.local_addr().unwrap());
    let (ready, wait_ready) = tokio::sync::oneshot::channel();
    let server = tokio::spawn(async move {
        let (stream, _) = listener.accept().await.unwrap();
        let mut ws = tokio_tungstenite::accept_async_with_config(stream, Some(websocket_config(MAX_BODY))).await.unwrap();
        ws.send(Message::Binary(vec![1; 64 * 1024].into())).await.unwrap();
        let _ = ready.send(());
        let _ = ws.next().await;
    });
    let (mut ipc, task) = transaction_fixture(origin, MAX_BODY, vec![]).await;
    assert_eq!(packet(&mut ipc).await.0, HANDSHAKE);
    wait_ready.await.unwrap();
    send(&mut ipc, CANCEL, &[]).await;
    timeout(Duration::from_secs(1), task).await.unwrap().unwrap();
    timeout(Duration::from_secs(1), server).await.unwrap().unwrap();
}

#[tokio::test]
async fn invalid_switching_protocols_is_never_published_as_accepted() {
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let origin = format!("http://{}", listener.local_addr().unwrap());
    let server = tokio::spawn(async move {
        let (mut stream, _) = listener.accept().await.unwrap();
        let mut head = vec![];
        while !head.ends_with(b"\r\n\r\n") { head.push(stream.read_u8().await.unwrap()); }
        stream.write_all(b"HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: invalid\r\n\r\n").await.unwrap();
    });
    let (mut ipc, task) = transaction_fixture(origin, MAX_BODY, vec![]).await;
    let (kind, error) = packet(&mut ipc).await;
    assert_eq!(kind, ERROR);
    let error: serde_json::Value = serde_json::from_slice(&error).unwrap();
    assert_eq!(error["code"], "ws_protocol");
    assert_eq!(error["accepted"], false);
    assert_eq!(error["phase"], "handshake");
    task.await.unwrap();
    server.await.unwrap();
}

#[tokio::test]
async fn caller_cannot_override_owned_websocket_handshake_headers() {
    for name in ["sec-websocket-key", "sec-websocket-version", "sec-websocket-accept", "sec-websocket-extensions"] {
        let (mut ipc, task) = transaction_fixture("http://127.0.0.1:1".to_owned(), 16, vec![(name, "override")]).await;
        let (kind, error) = packet(&mut ipc).await;
        assert_eq!(kind, ERROR);
        let error: serde_json::Value = serde_json::from_slice(&error).unwrap();
        assert_eq!(error["code"], "ws_owned_header");
        assert_eq!(error["accepted"], false);
        task.await.unwrap();
    }
}

// Independently reviewed real-network EOF, cancellation and backpressure regressions.
#[tokio::test]
async fn independent_ack_and_large_message_drain_before_transport_error() {
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let origin = format!("http://{}", listener.local_addr().unwrap());
    let (done, observed_close) = tokio::sync::oneshot::channel();
    let server = tokio::spawn(async move {
        let (stream, _) = listener.accept().await.unwrap();
        let mut ws = tokio_tungstenite::accept_async_with_config(stream, Some(websocket_config(MAX_BODY))).await.unwrap();
        assert_eq!(ws.next().await.unwrap().unwrap(), Message::Text("original request".into()));
        ws.send(Message::Binary(vec![0x61; 64 * 1024].into())).await.unwrap();
        drop(ws); // Abrupt EOF after a complete message, no close handshake.
        let _ = done.send(());
    });
    let (mut ipc, task) = transaction_fixture(origin, MAX_BODY, vec![]).await;
    assert_eq!(packet(&mut ipc).await.0, HANDSHAKE);
    send(&mut ipc, SEND, &message(1, 1, b"original request")).await;
    // Hold the IPC reader until the upstream has ended. Its 4-KiB buffer is
    // much smaller than the message, so drain must preserve backpressure.
    observed_close.await.unwrap();
    assert_eq!(packet(&mut ipc).await, (WRITE_ACK, 1_u64.to_be_bytes().to_vec()));
    assert_eq!(packet(&mut ipc).await, (MESSAGE, message(1, 2, &vec![0x61; 64 * 1024])));
    let (kind, data) = packet(&mut ipc).await;
    assert_eq!(kind, ERROR);
    let error: serde_json::Value = serde_json::from_slice(&data).unwrap();
    assert_eq!(error["accepted"], true);
    assert_eq!(error["phase"], "message");
    assert!(matches!(error["code"].as_str(), Some("ws_protocol" | "ws_unexpected_eof" | "ws_connection_closed" | "ws_transport")));
    assert_eq!(packet(&mut ipc).await, (END, vec![]));
    timeout(Duration::from_secs(1), task).await.unwrap().unwrap();
    server.await.unwrap();
}

#[tokio::test]
async fn independent_cancel_interrupts_eof_drain_without_trailing_error() {
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let origin = format!("http://{}", listener.local_addr().unwrap());
    let (done, observed_close) = tokio::sync::oneshot::channel();
    let server = tokio::spawn(async move {
        let (stream, _) = listener.accept().await.unwrap();
        let mut ws = tokio_tungstenite::accept_async_with_config(stream, Some(websocket_config(MAX_BODY))).await.unwrap();
        ws.send(Message::Binary(vec![0x61; 64 * 1024].into())).await.unwrap();
        drop(ws);
        let _ = done.send(());
    });
    let (mut ipc, task) = transaction_fixture(origin, MAX_BODY, vec![]).await;
    assert_eq!(packet(&mut ipc).await.0, HANDSHAKE);
    observed_close.await.unwrap();
    assert_eq!(ipc.read_u8().await.unwrap(), MESSAGE);
    assert_eq!(ipc.read_u32().await.unwrap(), 64 * 1024 + 9);
    assert_eq!(ipc.read_u64().await.unwrap(), 1);
    assert_eq!(ipc.read_u8().await.unwrap(), 2);
    assert_eq!(ipc.read_u8().await.unwrap(), 0x61);
    send(&mut ipc, CANCEL, &[]).await;
    timeout(Duration::from_secs(1), task).await.unwrap().unwrap();
    let mut tail = Vec::new();
    timeout(Duration::from_secs(1), ipc.read_to_end(&mut tail)).await.unwrap().unwrap();
    assert!(tail.len() < 64 * 1024 - 1);
    assert!(tail.iter().all(|byte| *byte == 0x61), "cancel appended an IPC error/end into a partial message");
    server.await.unwrap();
}

#[tokio::test]
async fn independent_blocked_eof_drain_keeps_write_deadline_and_partial_frame() {
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let origin = format!("http://{}", listener.local_addr().unwrap());
    let server = tokio::spawn(async move {
        let (stream, _) = listener.accept().await.unwrap();
        let mut ws = tokio_tungstenite::accept_async_with_config(stream, Some(websocket_config(MAX_BODY))).await.unwrap();
        ws.send(Message::Binary(vec![0x62; 64 * 1024].into())).await.unwrap();
        drop(ws);
    });
    let (mut ipc, task) = transaction_fixture(origin, MAX_BODY, vec![]).await;
    assert_eq!(packet(&mut ipc).await.0, HANDSHAKE);
    assert_eq!(ipc.read_u8().await.unwrap(), MESSAGE);
    assert_eq!(ipc.read_u32().await.unwrap(), 64 * 1024 + 9);
    assert_eq!(ipc.read_u64().await.unwrap(), 1);
    assert_eq!(ipc.read_u8().await.unwrap(), 2);
    assert_eq!(ipc.read_u8().await.unwrap(), 0x62);
    // Stop consuming the payload. The fixed two-second transaction-fixture
    // write timeout must also bound drain after the remote has already failed.
    timeout(Duration::from_secs(3), task).await.unwrap().unwrap();
    let mut tail = Vec::new();
    timeout(Duration::from_secs(1), ipc.read_to_end(&mut tail)).await.unwrap().unwrap();
    assert!(tail.len() < 64 * 1024 - 1);
    assert!(tail.iter().all(|byte| *byte == 0x62), "write failure appended an error/end into a partial message");
    server.await.unwrap();
}
