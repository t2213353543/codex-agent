//! Message IPC over the official Codex connector and exact Tungstenite forks.
//! There is no reconnect, JSON payload parsing, or model-level acknowledgement.

use std::sync::Arc;
use std::sync::atomic::{AtomicBool, Ordering};
use std::time::Duration;

use base64::Engine;
use futures::{Sink, SinkExt, Stream, StreamExt};
use http::{HeaderMap, Method};
use serde::Deserialize;
use tokio::io::{AsyncRead, AsyncReadExt, AsyncWrite, AsyncWriteExt};
use tokio::sync::mpsc;
use tokio::time::{Instant, sleep_until, timeout};
use tokio_tungstenite::tungstenite::{Error as WsError, Message};
use tokio_tungstenite::tungstenite::client::IntoClientRequest;
use tokio_tungstenite::tungstenite::protocol::{CloseFrame, WebSocketConfig};
use tungstenite::extensions::ExtensionsConfig;
use tungstenite::extensions::compression::deflate::DeflateConfig;

use crate::{BASE64, MAX_BODY, MAX_HEAD, RequestHead, Result, State, deadline};

const HANDSHAKE: u8 = 65;
const MESSAGE: u8 = 66;
const CLOSE: u8 = 67;
const ERROR: u8 = 68;
const END: u8 = 69;
const WRITE_ACK: u8 = 70;
const SEND: u8 = 82;
const SEND_CLOSE: u8 = 83;
const CANCEL: u8 = 84;
const MAX_TAIL: usize = 16 * 1024;

#[path = "websocket_v2.rs"]
mod v2;

struct Packet { kind: u8, data: Vec<u8> }
enum Command { Send(u64, Message), Close(Option<CloseFrame>) }

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct CloseHead { code: Option<u16>, reason: String }

fn encoded_headers(headers: &HeaderMap) -> Vec<(String, String)> {
    headers.iter().map(|(key, value)| (key.as_str().to_owned(), BASE64.encode(value.as_bytes()))).collect()
}

fn metadata(value: serde_json::Value) -> Result<Vec<u8>> {
    let data = serde_json::to_vec(&value).map_err(|_| "ws_metadata")?;
    if data.len() > MAX_HEAD { return Err("ws_metadata_limit"); }
    Ok(data)
}

async fn write_packet<W: AsyncWrite + Unpin>(writer: &mut W, in_frame: &mut bool, kind: u8, data: &[u8], duration: Duration) -> Result<()> {
    let limit = if kind == MESSAGE { MAX_BODY + 9 } else { MAX_HEAD };
    if data.len() > limit { return Err("ws_frame_limit"); }
    *in_frame = true;
    timeout(duration, async {
        writer.write_u8(kind).await?;
        writer.write_u32(data.len() as u32).await?;
        writer.write_all(data).await
    }).await.map_err(|_| "ws_ipc_write_timeout")?.map_err(|_| "ws_ipc_write")?;
    *in_frame = false;
    Ok(())
}

async fn input<R: AsyncRead + Unpin>(reader: &mut R, commands: mpsc::Sender<Command>, accepted: Arc<AtomicBool>, busy: Arc<AtomicBool>, limit: usize, duration: Duration) -> Result<()> {
    let mut next_sequence = 1_u64;
    loop {
        let kind = reader.read_u8().await.map_err(|_| "cancelled")?;
        // An idle IPC connection is valid between turns. Once a command starts,
        // its remaining header and payload must arrive within one bounded wait.
        timeout(duration, async {
            let length = reader.read_u32().await.map_err(|_| "ws_ipc_read")? as usize;
            if kind == CANCEL && length == 0 { return Err("cancelled"); }
            if !accepted.load(Ordering::Acquire) { return Err("ws_before_handshake"); }
            let valid_length = match kind {
                SEND => (9..=limit + 9).contains(&length),
                // Close metadata is independent of the application-message limit.
                SEND_CLOSE => length <= MAX_HEAD,
                _ => false,
            };
            if !valid_length
                || busy.swap(true, Ordering::AcqRel)
            { return Err("ws_command_invalid"); }
            let mut data = vec![0; length];
            reader.read_exact(&mut data).await.map_err(|_| "ws_ipc_read")?;
            let command = if kind == SEND {
                if data.len() < 9 { return Err("ws_command_invalid"); }
                let sequence = u64::from_be_bytes(data[..8].try_into().map_err(|_| "ws_sequence")?);
                if sequence != next_sequence { return Err("ws_sequence"); }
                next_sequence = next_sequence.checked_add(1).ok_or("ws_sequence")?;
                let message = match data[8] {
                    1 => Message::Text(String::from_utf8(data[9..].to_vec()).map_err(|_| "ws_text_utf8")?.into()),
                    2 => Message::Binary(data[9..].to_vec().into()),
                    _ => return Err("ws_message_kind"),
                };
                Command::Send(sequence, message)
            } else {
                let close: CloseHead = serde_json::from_slice(&data).map_err(|_| "ws_close_invalid")?;
                if close.reason.len() > 123 || close.code.is_none() && !close.reason.is_empty() { return Err("ws_close_invalid"); }
                let frame = match close.code {
                    None => None,
                    Some(code) => {
                        let code = tungstenite::protocol::frame::coding::CloseCode::from(code);
                        if !code.is_allowed() { return Err("ws_close_invalid"); }
                        Some(CloseFrame { code, reason: close.reason.into() })
                    }
                };
                Command::Close(frame)
            };
            commands.try_send(command).map_err(|_| "ws_command_capacity")?;
            Ok(())
        }).await.map_err(|_| "ws_ipc_read_timeout")??;
    }
}

#[cfg(test)]
async fn output<W: AsyncWrite + Unpin>(writer: &mut W, in_frame: &mut bool, packets: mpsc::Receiver<Packet>, busy: Arc<AtomicBool>, duration: Duration) -> Result<()> {
    output_with_mode(writer, in_frame, packets, busy, duration, false).await
}

async fn output_with_mode<W: AsyncWrite + Unpin>(writer: &mut W, in_frame: &mut bool, mut packets: mpsc::Receiver<Packet>, busy: Arc<AtomicBool>, duration: Duration, chunked: bool) -> Result<()> {
    while let Some(packet) = packets.recv().await {
        // Clear before publishing ACK bytes: a fast peer can send its next
        // command as soon as those bytes reach the kernel, on another core.
        if packet.kind == WRITE_ACK { busy.store(false, Ordering::Release); }
        if chunked && packet.kind == MESSAGE {
            v2::write_message(writer, in_frame, &packet.data, duration).await?;
        } else {
            write_packet(writer, in_frame, packet.kind, &packet.data, duration).await?;
        }
        if packet.kind == END { return Ok(()); }
    }
    Err("ws_output_incomplete")
}

async fn emit(packets: &mpsc::Sender<Packet>, kind: u8, data: Vec<u8>) -> Result<()> {
    packets.send(Packet { kind, data }).await.map_err(|_| "ws_ipc_write")
}

fn classify(error: WsError) -> &'static str {
    match error {
        WsError::Capacity(_) => "ws_message_limit",
        WsError::Protocol(_) | WsError::Utf8(_) => "ws_protocol",
        WsError::ConnectionClosed | WsError::AlreadyClosed => "ws_connection_closed",
        WsError::Io(_) | WsError::Tls(_) => "ws_transport",
        _ => "ws_transport",
    }
}

async fn pump<C>(mut connection: C, mut commands: mpsc::Receiver<Command>, packets: mpsc::Sender<Packet>, duration: Duration, limit: usize) -> Result<()>
where
    C: Stream<Item = std::result::Result<Message, WsError>> + Sink<Message, Error = WsError> + Unpin,
{
    let mut sequence = 0_u64;
    let mut close_deadline = None;
    loop {
        tokio::select! {
            command = commands.recv() => {
                match command.ok_or("cancelled")? {
                    Command::Send(sequence, message) => {
                        timeout(duration, connection.send(message)).await.map_err(|_| "ws_write_timeout")?.map_err(classify)?;
                        emit(&packets, WRITE_ACK, sequence.to_be_bytes().to_vec()).await?;
                    }
                    Command::Close(frame) => {
                        timeout(duration, connection.send(Message::Close(frame))).await.map_err(|_| "ws_write_timeout")?.map_err(classify)?;
                        // A peer that never answers our close must not retain an
                        // IPC transaction indefinitely, even with no turn idle timeout.
                        close_deadline = Some(Instant::now() + duration);
                    }
                }
            }
            _ = async {
                match close_deadline {
                    Some(deadline) => sleep_until(deadline).await,
                    None => futures::future::pending::<()>().await,
                }
            } => return Err("ws_close_timeout"),
            incoming = connection.next() => {
                let message = incoming.ok_or("ws_unexpected_eof")?.map_err(classify)?;
                match message {
                    Message::Ping(payload) => {
                        timeout(duration, connection.send(Message::Pong(payload))).await.map_err(|_| "ws_write_timeout")?.map_err(classify)?;
                    }
                    Message::Pong(_) => {},
                    Message::Text(_) | Message::Binary(_) => {
                        let kind = if message.is_text() { 1 } else { 2 };
                        let bytes = message.into_data();
                        if bytes.len() > limit { return Err("ws_message_limit"); }
                        sequence = sequence.checked_add(1).ok_or("ws_sequence")?;
                        let capacity = bytes.len().checked_add(9).ok_or("ws_message_limit")?;
                        let mut data = Vec::new();
                        data.try_reserve_exact(capacity).map_err(|_| "ws_message_allocation")?;
                        data.extend_from_slice(&sequence.to_be_bytes());
                        data.push(kind);
                        data.extend_from_slice(&bytes);
                        emit(&packets, MESSAGE, data).await?;
                    }
                    Message::Close(frame) => {
                        let (code, reason) = match frame { Some(frame) => (Some(u16::from(frame.code)), frame.reason.to_string()), None => (None, String::new()) };
                        // Tungstenite queued the peer-close reply while reading.
                        let _ = timeout(duration, connection.flush()).await;
                        emit(&packets, CLOSE, metadata(serde_json::json!({"code":code,"reason":reason}))?).await?;
                        emit(&packets, END, Vec::new()).await?;
                        return Ok(());
                    }
                    Message::Frame(_) => return Err("ws_raw_frame"),
                }
            }
        }
    }
}

#[cfg(test)]
async fn relay<C, W>(connection: C, commands: mpsc::Receiver<Command>, writer: &mut W, in_frame: &mut bool, busy: Arc<AtomicBool>, duration: Duration, limit: usize) -> Result<()>
where
    C: Stream<Item = std::result::Result<Message, WsError>> + Sink<Message, Error = WsError> + Unpin,
    W: AsyncWrite + Unpin,
{
    relay_with_mode(connection, commands, writer, in_frame, busy, duration, limit, false).await
}

async fn relay_with_mode<C, W>(connection: C, commands: mpsc::Receiver<Command>, writer: &mut W, in_frame: &mut bool, busy: Arc<AtomicBool>, duration: Duration, limit: usize, chunked: bool) -> Result<()>
where
    C: Stream<Item = std::result::Result<Message, WsError>> + Sink<Message, Error = WsError> + Unpin,
    W: AsyncWrite + Unpin,
{
    let (packets, output_rx) = mpsc::channel(1);
    // A complete message can precede an abrupt peer close in the same poll.
    // A pump error must not cancel output that still owns preceding ACK/data.
    // Queue the failure behind those packets. Output failure still short-circuits
    // the producer, and transaction's outer select still handles caller cancel.
    let receive = async {
        if let Err(code) = pump(connection, commands, packets.clone(), duration, limit).await {
            emit(&packets, ERROR, metadata(serde_json::json!({"phase":"message","code":code,"accepted":true}))?).await?;
            emit(&packets, END, Vec::new()).await?;
        }
        Ok(())
    };
    tokio::try_join!(receive, output_with_mode(writer, in_frame, output_rx, busy, duration, chunked)).map(|_| ())
}

#[cfg(test)]
fn websocket_config(limit: usize) -> WebSocketConfig {
    websocket_config_with_profile(limit, false)
}

fn websocket_config_with_profile(limit: usize, disable_compression: bool) -> WebSocketConfig {
    let mut extensions = ExtensionsConfig::default();
    if !disable_compression { extensions.permessage_deflate = Some(DeflateConfig::default()); }
    let mut config = WebSocketConfig::default();
    config.extensions = extensions;
    config.max_message_size = if limit == usize::MAX { None } else { Some(limit) };
    if limit == usize::MAX { config.max_frame_size = None; }
    // Keep the CLI fork's frame bound. A wire frame can be larger than its
    // decompressed message, and control frames are not application messages.
    config
}

fn connector_for_transaction(state: &State) -> Result<codex_websocket_client::WebSocketConnector> {
    match state.http_client_lifecycle {
        crate::HttpClientLifecycle::CliRequest => {
            // responses_websocket::connect_websocket constructs a connector for
            // each new socket. Its TLS config (and session cache) must not leak
            // across independent handshakes in the default CLI lifecycle.
            let factory = codex_http_client::HttpClientFactory::new(codex_http_client::OutboundProxyPolicy::ReqwestDefault);
            codex_websocket_client::WebSocketConnector::new(&factory).map_err(|_| "ws_transport")
        }
        crate::HttpClientLifecycle::Pooled => Ok(state.websocket.clone()),
    }
}

pub(crate) async fn transaction<R: AsyncRead + Unpin, W: AsyncWrite + Unpin>(state: &State, head: &RequestHead, method: Method, mut url: reqwest::Url, headers: HeaderMap, reader: &mut R, writer: &mut W) {
    let requested_limit = head.max_message_bytes.unwrap_or(MAX_BODY);
    let chunked = head.operation.as_deref() == Some("websocket_message_v2");
    let limit = if head.unbounded_payload { usize::MAX } else { requested_limit.min(MAX_BODY) };
    let write_ms = head.write_timeout_ms.unwrap_or(60_000);
    let accepted = Arc::new(AtomicBool::new(false));
    let busy = Arc::new(AtomicBool::new(false));
    let duration = Duration::from_millis(write_ms.min(60_000).max(1));
    let mut in_frame = false;
    let (commands, command_rx) = mpsc::channel(1);
    let workflow = async {
        if method != Method::GET || head.body_length != 0 || chunked != head.unbounded_payload
            || (!head.unbounded_payload && (requested_limit == 0 || requested_limit > MAX_BODY)) || write_ms == 0 || write_ms > 60_000 {
            return Err("ws_request_invalid");
        }
        let scheme = match url.scheme() {
            "https" => "wss",
            "http" => "ws",
            _ => return Err("ws_request_invalid"),
        };
        url.set_scheme(scheme).map_err(|_| "ws_request_invalid")?;
        let mut request = url.as_str().into_client_request().map_err(|_| "ws_request_invalid")?;
        for (name, value) in &headers {
            if matches!(name.as_str(), "sec-websocket-key" | "sec-websocket-version" | "sec-websocket-accept" | "sec-websocket-extensions") {
                return Err("ws_owned_header");
            }
            request.headers_mut().append(name, value.clone());
        }
        let config = websocket_config_with_profile(limit, head.disable_websocket_compression);
        let connector = connector_for_transaction(state)?;
        let outcome = deadline(state.headers_timeout, connector.connect_with_explicit_route(request, config, state.proxy.as_deref()), "ws_handshake_timeout").await?;
        let (connection, response) = match outcome {
            Ok(value) => value,
            Err(WsError::Http(response)) => {
                let tail = response.body().as_deref().unwrap_or_default();
                let data = metadata(serde_json::json!({"status":response.status().as_u16(),"headers":encoded_headers(response.headers()),"body_scope":"handshake_tail","body_complete":null,"body_base64":BASE64.encode(&tail[..tail.len().min(MAX_TAIL)]),"tail_truncated":tail.len()>MAX_TAIL}))?;
                write_packet(writer, &mut in_frame, HANDSHAKE, &data, duration).await?;
                write_packet(writer, &mut in_frame, END, &[], duration).await?;
                return Ok(());
            }
            Err(error) => return Err(classify(error)),
        };
        let data = metadata(serde_json::json!({"status":response.status().as_u16(),"headers":encoded_headers(response.headers()),"body_scope":"handshake_tail","body_complete":null,"body_base64":"","tail_truncated":false}))?;
        accepted.store(true, Ordering::Release);
        write_packet(writer, &mut in_frame, HANDSHAKE, &data, duration).await?;
        relay_with_mode(connection, command_rx, writer, &mut in_frame, busy.clone(), duration, limit, chunked).await
    };
    // An explicitly supplied IPC budget covers connection establishment and the
    // established socket, just like HTTP's total budget. No budget is installed
    // by default: interactive sockets remain open until caller cancellation.
    let operation = async {
        tokio::select! {
            result = async {
                if chunked { v2::input(reader, commands, accepted.clone(), busy.clone(), duration).await }
                else { input(reader, commands, accepted.clone(), busy.clone(), limit, duration).await }
            } => result,
            result = workflow => result,
        }
    };
    let result = match deadline(head.total_timeout_ms.map(Duration::from_millis), operation, "total_timeout").await {
        Ok(result) => result,
        Err(code) => Err(code),
    };
    if let Err(code) = result {
        if code != "cancelled" && !in_frame {
            let data = metadata(serde_json::json!({"phase":if accepted.load(Ordering::Acquire){"message"}else{"handshake"},"code":code,"accepted":accepted.load(Ordering::Acquire)}));
            if let Ok(data) = data {
                let _ = write_packet(writer, &mut in_frame, ERROR, &data, Duration::from_millis(100)).await;
            }
        }
    }
}

#[cfg(test)]
#[path = "websocket_tests.rs"]
mod regression_tests;

#[cfg(test)]
mod tests {
    use super::*;
    use tokio::io::DuplexStream;
    use tokio::net::TcpListener;

    async fn fixture(origin: String, limit: usize) -> (DuplexStream, tokio::task::JoinHandle<()>) {
        let config: crate::Config = serde_json::from_value(serde_json::json!({"protocol":1,"listen":"127.0.0.1:0","upstream":origin,"ipc_token":"a".repeat(64),"headers_timeout_ms":2000,"idle_timeout_ms":0})).unwrap();
        let state = crate::build_state(&config).unwrap();
        let head: RequestHead = serde_json::from_value(serde_json::json!({"protocol":1,"ipc_token":"a".repeat(64),"method":"GET","path":"/responses","headers":[],"body_length":0,"operation":"websocket_message_v1","max_message_bytes":limit,"write_timeout_ms":2000})).unwrap();
        let (client, server) = tokio::io::duplex(4096);
        let task = tokio::spawn(async move {
            let (method, url, headers) = crate::request_parts(&head, &state).unwrap();
            let (mut read, mut write) = tokio::io::split(server);
            transaction(&state, &head, method, url, headers, &mut read, &mut write).await;
        });
        (client, task)
    }

    async fn packet(stream: &mut DuplexStream) -> (u8, Vec<u8>) {
        timeout(Duration::from_secs(5), async {
            let kind = stream.read_u8().await.unwrap();
            let len = stream.read_u32().await.unwrap();
            assert!(len <= MAX_BODY as u32 + 9);
            let mut data = vec![0; len as usize];
            stream.read_exact(&mut data).await.unwrap();
            (kind, data)
        }).await.unwrap()
    }

    async fn send(stream: &mut DuplexStream, kind: u8, data: &[u8]) {
        stream.write_u8(kind).await.unwrap();
        stream.write_u32(data.len() as u32).await.unwrap();
        stream.write_all(data).await.unwrap();
    }

    fn server_config() -> WebSocketConfig {
        let mut config = WebSocketConfig::default();
        config.extensions.permessage_deflate = Some(DeflateConfig::default());
        config
    }

    #[tokio::test]
    async fn messages_ack_ping_and_close_use_real_websocket() {
        let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let origin = format!("http://{}", listener.local_addr().unwrap());
        let server = tokio::spawn(async move {
            let (stream, _) = listener.accept().await.unwrap();
            let mut ws = tokio_tungstenite::accept_hdr_async_with_config(stream, |request: &tungstenite::handshake::server::Request, response| {
                assert_eq!(request.headers().get("sec-websocket-extensions").unwrap(), "permessage-deflate; client_max_window_bits");
                Ok(response)
            }, Some(server_config())).await.unwrap();
            assert_eq!(ws.next().await.unwrap().unwrap(), Message::Text("synthetic text".into()));
            ws.send(Message::Ping(vec![1, 2, 3].into())).await.unwrap();
            assert_eq!(ws.next().await.unwrap().unwrap(), Message::Pong(vec![1, 2, 3].into()));
            ws.send(Message::Binary(vec![0, 255, 42].into())).await.unwrap();
            ws.send(Message::Close(Some(CloseFrame { code: 1008.into(), reason: "synthetic close".into() }))).await.unwrap();
            let _ = ws.next().await;
        });
        let (mut ipc, task) = fixture(origin, MAX_BODY).await;
        let (kind, data) = packet(&mut ipc).await;
        assert_eq!(kind, HANDSHAKE);
        assert_eq!(serde_json::from_slice::<serde_json::Value>(&data).unwrap()["status"], 101);
        let mut data = 1_u64.to_be_bytes().to_vec();
        data.push(1);
        data.extend_from_slice(b"synthetic text");
        send(&mut ipc, SEND, &data).await;
        let (kind, data) = packet(&mut ipc).await;
        assert_eq!((kind, data), (WRITE_ACK, 1_u64.to_be_bytes().to_vec()));
        let (kind, data) = packet(&mut ipc).await;
        assert_eq!(kind, MESSAGE);
        assert_eq!(&data[..8], &1_u64.to_be_bytes());
        assert_eq!(&data[8..], &[2, 0, 255, 42]);
        let (kind, data) = packet(&mut ipc).await;
        assert_eq!(kind, CLOSE);
        assert_eq!(serde_json::from_slice::<serde_json::Value>(&data).unwrap()["code"], 1008);
        assert_eq!(packet(&mut ipc).await, (END, vec![]));
        task.await.unwrap();
        server.await.unwrap();
    }

    #[tokio::test]
    async fn rejection_preserves_headers_and_unknown_tail_completeness() {
        let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let origin = format!("http://{}", listener.local_addr().unwrap());
        let server = tokio::spawn(async move {
            let (mut stream, _) = listener.accept().await.unwrap();
            let mut request = Vec::new();
            loop {
                request.push(stream.read_u8().await.unwrap());
                if request.ends_with(b"\r\n\r\n") { break; }
            }
            stream.write_all(b"HTTP/1.1 401 Unauthorized\r\nContent-Length: 20\r\nX-Repeat: one\r\nX-Repeat: two\r\nRetry-After: 7\r\n\r\npartial").await.unwrap();
        });
        let (mut ipc, task) = fixture(origin, MAX_BODY).await;
        let (kind, data) = packet(&mut ipc).await;
        assert_eq!(kind, HANDSHAKE);
        let data: serde_json::Value = serde_json::from_slice(&data).unwrap();
        assert_eq!(data["status"], 401);
        assert!(data["body_complete"].is_null());
        assert_eq!(data["body_scope"], "handshake_tail");
        let repeated = data["headers"].as_array().unwrap().iter().filter(|pair| pair[0] == "x-repeat").count();
        assert_eq!(repeated, 2);
        assert_eq!(packet(&mut ipc).await, (END, vec![]));
        task.await.unwrap();
        server.await.unwrap();
    }

    #[tokio::test]
    async fn cancellation_interrupts_a_pending_handshake() {
        let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let origin = format!("http://{}", listener.local_addr().unwrap());
        let (mut ipc, task) = fixture(origin, MAX_BODY).await;
        let (mut stream, _) = listener.accept().await.unwrap();
        send(&mut ipc, CANCEL, &[]).await;
        timeout(Duration::from_secs(1), task).await.unwrap().unwrap();
        let mut bytes = Vec::new();
        timeout(Duration::from_secs(1), stream.read_to_end(&mut bytes)).await.unwrap().unwrap();
        assert_eq!(ipc.read_u8().await.unwrap_err().kind(), std::io::ErrorKind::UnexpectedEof);
    }

    #[tokio::test]
    async fn message_limit_emits_typed_accepted_error_without_reconnect() {
        let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let origin = format!("http://{}", listener.local_addr().unwrap());
        let server = tokio::spawn(async move {
            let (stream, _) = listener.accept().await.unwrap();
            let mut ws = tokio_tungstenite::accept_async_with_config(stream, Some(server_config())).await.unwrap();
            ws.send(Message::Binary(vec![0; 1025].into())).await.unwrap();
            assert!(timeout(Duration::from_millis(100), listener.accept()).await.is_err());
        });
        let (mut ipc, task) = fixture(origin, 1024).await;
        assert_eq!(packet(&mut ipc).await.0, HANDSHAKE);
        let (kind, data) = packet(&mut ipc).await;
        assert_eq!(kind, ERROR);
        let data: serde_json::Value = serde_json::from_slice(&data).unwrap();
        assert_eq!(data["code"], "ws_message_limit");
        assert_eq!(data["accepted"], true);
        task.await.unwrap();
        server.await.unwrap();
    }
}
