//! HTTP adapter boundaries independent of the gateway and external endpoints.

use super::*;

fn state() -> Arc<State> {
    Arc::new(build_state(&Config {
        protocol: PROTOCOL,
        listen: Some("127.0.0.1:0".parse().unwrap()),
        unix_socket: None,
        upstream: "http://127.0.0.1:1".to_owned(),
        ipc_token: "a".repeat(64),
        proxy: None,
        headers_timeout_ms: 100,
        idle_timeout_ms: 100,
        http_client_lifecycle: HttpClientLifecycle::Pooled,
    }).unwrap())
}

fn head(body_length: usize) -> RequestHead {
    serde_json::from_value(serde_json::json!({
        "protocol": PROTOCOL, "ipc_token": "a".repeat(64),
        "method": "POST", "path": "/responses", "headers": [],
        "body_length": body_length,
    })).unwrap()
}

fn header(head: &mut RequestHead, name: &str, value: &[u8]) {
    head.headers.push((name.to_owned(), BASE64.encode(value)));
}

#[tokio::test]
async fn unbounded_payload_requires_explicit_opt_in_and_checks_allocation() {
    let state = state();
    let mut request = head(33 * 1024 * 1024);
    assert_eq!(request_parts(&request, &state).unwrap_err(), "request_limit");
    request.unbounded_payload = true;
    assert!(request_parts(&request, &state).is_ok());
    let (mut peer, backend) = tokio::io::duplex(4096);
    let task = tokio::spawn(transaction(backend, state));
    let encoded = serde_json::to_vec(&serde_json::json!({
        "protocol":1,"ipc_token":"a".repeat(64),"method":"POST","path":"/responses","headers":[],
        "body_length":usize::MAX,"unbounded_payload":true
    })).unwrap();
    peer.write_u32(encoded.len() as u32).await.unwrap();
    peer.write_all(&encoded).await.unwrap();
    assert_eq!(peer.read_u8().await.unwrap(), ERROR);
    let length = peer.read_u32().await.unwrap() as usize;
    let mut output = vec![0; length];
    peer.read_exact(&mut output).await.unwrap();
    assert_eq!(output, b"request_allocation");
    task.await.unwrap();
}

#[tokio::test]
async fn content_length_requires_decimal_digits_and_exact_single_length() {
    let state = state();
    for invalid in [b"+3".as_slice(), b"-3", b"3,3", b"3 ", b" 3", b"", b"4", b"184467440737095516160"] {
        let mut head = head(3);
        header(&mut head, "Content-Length", invalid);
        assert_eq!(request_parts(&head, &state).unwrap_err(), "content_length_invalid");
    }
    for valid in [b"3".as_slice(), b"003"] {
        let mut head = head(3);
        header(&mut head, "Content-Length", valid);
        assert_eq!(request_parts(&head, &state).unwrap().2["content-length"].as_bytes(), valid);
        header(&mut head, "content-length", valid);
        assert_eq!(request_parts(&head, &state).unwrap_err(), "content_length_invalid");
    }
}

#[tokio::test]
async fn duplicate_binary_headers_and_sensitive_flags_survive_validation() {
    let state = state();
    let mut head = head(0);
    header(&mut head, "X-Repeat", b"first");
    header(&mut head, "x-repeat", b"\x80\xff");
    header(&mut head, "Authorization", b"Bearer synthetic");
    header(&mut head, "Cookie", b"synthetic=1");
    let (_, _, headers) = request_parts(&head, &state).unwrap();
    assert_eq!(headers.get_all("x-repeat").iter().map(|value| value.as_bytes()).collect::<Vec<_>>(), vec![b"first".as_slice(), b"\x80\xff"]);
    assert!(headers["authorization"].is_sensitive());
    assert!(headers["cookie"].is_sensitive());
}

#[tokio::test]
async fn hop_headers_and_injected_header_values_are_rejected() {
    let state = state();
    for name in ["Host", "Connection", "Transfer-Encoding", "Upgrade", "Trailer", "TE", "Proxy-Authorization", "Proxy-Authenticate", "Proxy-Connection", "Keep-Alive"] {
        let mut head = head(0);
        header(&mut head, name, b"synthetic");
        assert_eq!(request_parts(&head, &state).unwrap_err(), "header_hop_by_hop");
    }
    for value in [b"x\r\ninjected: yes".as_slice(), b"x\0y", b"x\ny"] {
        let mut head = head(0);
        header(&mut head, "x-test", value);
        assert_eq!(request_parts(&head, &state).unwrap_err(), "header_invalid");
    }
}

#[tokio::test]
async fn prepared_paths_preserve_query_and_reject_url_normalization() {
    let state = state();
    for path in ["/responses?first=%2f&duplicate=a&duplicate=b&empty=", "/%2F/%25?", "/a//b"] {
        let mut head = head(0);
        head.path = path.to_owned();
        let (_, url, _) = request_parts(&head, &state).unwrap();
        assert_eq!(&url.as_str()[state.origin.as_str().trim_end_matches('/').len()..], path);
    }
    for path in ["//outside.invalid/x", "http://outside.invalid", "/a/../b", "/a/%2e%2e/b", "/a\\b", "/a#secret", "/a b", "/a\tb", "/é"] {
        let mut head = head(0);
        head.path = path.to_owned();
        assert_eq!(request_parts(&head, &state).unwrap_err(), "path_invalid");
    }
}

#[tokio::test]
async fn body_header_auth_and_timeout_limits_fail_before_network() {
    let state = state();
    let mut request = head(MAX_BODY);
    assert!(request_parts(&request, &state).is_ok());
    request.body_length += 1;
    assert_eq!(request_parts(&request, &state).unwrap_err(), "request_limit");
    let mut request = head(0);
    request.headers = vec![("x-test".to_owned(), BASE64.encode(b"a")); 128];
    assert!(request_parts(&request, &state).is_ok());
    header(&mut request, "x-test", b"a");
    assert_eq!(request_parts(&request, &state).unwrap_err(), "request_limit");
    for value in [0, 86_400_001, u64::MAX] {
        let mut request = head(0);
        request.total_timeout_ms = Some(value);
        assert_eq!(request_parts(&request, &state).unwrap_err(), "timeout_invalid");
    }
    let mut request = head(0);
    request.ipc_token.replace_range(63..64, "b");
    assert_eq!(request_parts(&request, &state).unwrap_err(), "ipc_auth");
}

#[tokio::test]
async fn head_framing_rejects_zero_oversize_truncation_and_unknown_fields() {
    for length in [0, (MAX_HEAD + 1) as u32] {
        let bytes = length.to_be_bytes();
        assert_eq!(read_head(&mut bytes.as_slice()).await.err(), Some("request_limit"));
    }
    let bytes = [0, 0, 0, 10, b'{'];
    assert_eq!(read_head(&mut bytes.as_slice()).await.err(), Some("ipc_read"));
    let encoded = br#"{"protocol":1,"ipc_token":"a","method":"POST","path":"/","headers":[],"body_length":0,"unexpected":true}"#;
    let mut bytes = (encoded.len() as u32).to_be_bytes().to_vec();
    bytes.extend_from_slice(encoded);
    assert_eq!(read_head(&mut bytes.as_slice()).await.err(), Some("request_json"));
}

#[tokio::test]
async fn total_timeout_includes_stalled_ipc_body_upload() {
    let (mut client, server) = tokio::io::duplex(4096);
    let state = state();
    let task = tokio::spawn(transaction(server, state));
    let encoded = serde_json::to_vec(&serde_json::json!({
        "protocol": PROTOCOL, "ipc_token": "a".repeat(64), "method": "POST",
        "path": "/responses", "headers": [], "body_length": 1024,
        "total_timeout_ms": 30,
    })).unwrap();
    client.write_u32(encoded.len() as u32).await.unwrap();
    client.write_all(&encoded).await.unwrap();
    client.write_all(b"incomplete").await.unwrap();
    let mut output = Vec::new();
    timeout(Duration::from_millis(500), client.read_to_end(&mut output)).await.unwrap().unwrap();
    assert_eq!(output[0], ERROR);
    assert_eq!(u32::from_be_bytes(output[1..5].try_into().unwrap()) as usize, output.len() - 5);
    assert_eq!(&output[5..], b"total_timeout");
    task.await.unwrap();
}

#[tokio::test]
async fn interrupted_frame_retains_partial_marker_and_oversize_writes_nothing() {
    let (mut writer, mut reader) = tokio::io::duplex(8);
    let mut in_frame = false;
    assert_eq!(frame(&mut writer, &mut in_frame, HEAD, &vec![0; MAX_HEAD + 1]).await, Err("response_header_limit"));
    assert!(!in_frame);
    assert!(timeout(Duration::from_millis(10), frame(&mut writer, &mut in_frame, DATA, &[7; 32])).await.is_err());
    assert!(in_frame);
    let mut prefix = [0; 8];
    reader.read_exact(&mut prefix).await.unwrap();
    assert_eq!(&prefix[..5], &[DATA, 0, 0, 0, 32]);
    assert_eq!(&prefix[5..], &[7; 3]);
}
