use super::*;
use tokio::io::DuplexStream;
use tokio::net::TcpListener;

async fn send(stream: &mut DuplexStream, kind: u8, data: &[u8]) {
    stream.write_u8(kind).await.unwrap();
    stream.write_u32(u32::try_from(data.len()).unwrap()).await.unwrap();
    stream.write_all(data).await.unwrap();
}

fn start(sequence: u64, kind: u8, total: u64) -> Vec<u8> {
    let mut out = sequence.to_be_bytes().to_vec();
    out.push(kind);
    out.extend_from_slice(&total.to_be_bytes());
    out
}

async fn packet(stream: &mut DuplexStream) -> (u8, Vec<u8>) {
    timeout(Duration::from_secs(10), async {
        let kind = stream.read_u8().await.unwrap();
        let size = stream.read_u32().await.unwrap() as usize;
        assert!(size <= CHUNK_SIZE);
        let mut bytes = vec![0; size];
        stream.read_exact(&mut bytes).await.unwrap();
        (kind, bytes)
    }).await.unwrap()
}

#[tokio::test]
async fn real_websocket_roundtrips_33_mib_text_and_binary_without_frame_or_message_cap() {
    for kind in [1_u8, 2] {
        let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let origin = format!("http://{}", listener.local_addr().unwrap());
        let server = tokio::spawn(async move {
            let (stream, _) = listener.accept().await.unwrap();
            let mut config = WebSocketConfig::default();
            config.max_message_size = None;
            config.max_frame_size = None;
            // No negotiated compression: the echo is an actual 33 MiB wire
            // frame, so this tests both tungstenite limits independently.
            let mut ws = tokio_tungstenite::accept_hdr_async_with_config(stream, |request: &tungstenite::handshake::server::Request, response| {
                assert!(!request.headers().contains_key("sec-websocket-extensions"));
                Ok(response)
            }, Some(config)).await.unwrap();
            let incoming = ws.next().await.unwrap().unwrap();
            assert_eq!(incoming.len(), 33 * 1024 * 1024);
            assert_eq!(incoming.is_text(), kind == 1);
            ws.send(incoming).await.unwrap();
            ws.send(Message::Close(None)).await.unwrap();
        });
        let config: crate::Config = serde_json::from_value(serde_json::json!({
            "protocol":1,"listen":"127.0.0.1:0","upstream":origin,"ipc_token":"a".repeat(64),"headers_timeout_ms":2000,"idle_timeout_ms":0
        })).unwrap();
        let state = crate::build_state(&config).unwrap();
        let head: RequestHead = serde_json::from_value(serde_json::json!({
            "protocol":1,"ipc_token":"a".repeat(64),"method":"GET","path":"/responses","headers":[],"body_length":0,
            "operation":"websocket_message_v2","unbounded_payload":true,"disable_websocket_compression":true,"write_timeout_ms":10000
        })).unwrap();
        let (mut ipc, backend) = tokio::io::duplex(64 * 1024);
        let task = tokio::spawn(async move {
            let (method, url, headers) = crate::request_parts(&head, &state).unwrap();
            let (mut read, mut write) = tokio::io::split(backend);
            super::super::transaction(&state, &head, method, url, headers, &mut read, &mut write).await;
        });
        assert_eq!(packet(&mut ipc).await.0, HANDSHAKE);
        let value = if kind == 1 { b'x' } else { 0xff };
        let payload = vec![value; 33 * 1024 * 1024];
        send(&mut ipc, SEND_START, &start(1, kind, payload.len() as u64)).await;
        for chunk in payload.chunks(CHUNK_SIZE) { send(&mut ipc, SEND_CHUNK, chunk).await; }
        send(&mut ipc, SEND_FINISH, &1_u64.to_be_bytes()).await;
        let mut received = Vec::new();
        let mut ack = false;
        loop {
            let (opcode, data) = packet(&mut ipc).await;
            match opcode {
                WRITE_ACK => { assert_eq!(data, 1_u64.to_be_bytes()); ack = true; }
                MESSAGE_START => assert_eq!(data, start(1, kind, payload.len() as u64)),
                MESSAGE_CHUNK => received.extend_from_slice(&data),
                MESSAGE_FINISH => { assert_eq!(data, 1_u64.to_be_bytes()); break; }
                _ => panic!("unexpected packet {opcode}"),
            }
        }
        assert!(ack);
        assert_eq!(received, payload);
        assert_eq!(packet(&mut ipc).await.0, CLOSE);
        assert_eq!(packet(&mut ipc).await.0, END);
        task.await.unwrap();
        server.await.unwrap();
    }
}

async fn invalid_case(packets: Vec<(u8, Vec<u8>)>, expected: &'static str) {
    let (mut peer, mut read) = tokio::io::duplex(4096);
    let (tx, mut rx) = mpsc::channel(1);
    let task = tokio::spawn(async move {
        input(&mut read, tx, Arc::new(AtomicBool::new(true)), Arc::new(AtomicBool::new(false)), Duration::from_secs(1)).await
    });
    for (kind, data) in packets { send(&mut peer, kind, &data).await; }
    assert_eq!(timeout(Duration::from_secs(2), task).await.unwrap().unwrap(), Err(expected));
    assert!(rx.try_recv().is_err());
}

#[tokio::test]
async fn malformed_chunks_never_publish_partial_messages() {
    invalid_case(vec![(SEND_START,start(1,1,3)),(SEND_CHUNK,b"ab".to_vec()),(SEND_FINISH,1_u64.to_be_bytes().to_vec())], "ws_command_invalid").await;
    invalid_case(vec![(SEND_START,start(1,1,1)),(SEND_CHUNK,b"ab".to_vec())], "ws_command_invalid").await;
    invalid_case(vec![(SEND_START,start(1,1,0)),(SEND_FINISH,2_u64.to_be_bytes().to_vec())], "ws_command_invalid").await;
    invalid_case(vec![(SEND_START,start(1,1,1)),(SEND_START,start(2,1,1))], "ws_command_invalid").await;
    invalid_case(vec![(SEND_START,start(1,1,1)),(SEND_CHUNK,vec![])], "ws_command_invalid").await;
    invalid_case(vec![(SEND_START,start(1,1,1)),(SEND_CHUNK,vec![255]),(SEND_FINISH,1_u64.to_be_bytes().to_vec())], "ws_text_utf8").await;
    invalid_case(vec![(SEND_START,start(1,1,u64::MAX))], "ws_message_limit").await;
}

#[tokio::test]
async fn cancel_during_more_than_u32_declared_length_does_not_allocate_or_truncate() {
    invalid_case(vec![(SEND_START,start(1,2,u32::MAX as u64 + 2)),(SEND_CHUNK,vec![42]),(CANCEL,vec![])], "cancelled").await;
}

#[test]
fn unbounded_wire_config_removes_both_tungstenite_limits() {
    let config = websocket_config(usize::MAX);
    assert_eq!(config.max_message_size, None);
    assert_eq!(config.max_frame_size, None);
    let legacy = websocket_config(MAX_BODY);
    assert_eq!(legacy.max_message_size, Some(MAX_BODY));
    assert!(legacy.max_frame_size.is_some());
}

#[test]
fn realtime_compression_profile_changes_only_the_selected_extension() {
    for limit in [MAX_BODY, usize::MAX] {
        let realtime = websocket_config_with_profile(limit, true);
        let responses = websocket_config_with_profile(limit, false);
        assert!(realtime.extensions.permessage_deflate.is_none());
        assert!(responses.extensions.permessage_deflate.is_some());
        assert_eq!(realtime.max_frame_size, responses.max_frame_size);
        assert_eq!(realtime.max_message_size, responses.max_message_size);
    }
}
