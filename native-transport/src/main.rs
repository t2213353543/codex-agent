//! Native HTTP and WebSocket transport. Each authenticated IPC connection owns
//! one request; dropping it cancels only that request.

use std::io::{Read, Write};
use std::net::SocketAddr;
use std::sync::Arc;
use std::time::Duration;

use base64::Engine;
use base64::engine::general_purpose::STANDARD as BASE64;
use bytes::Bytes;
use codex_http_client::{HttpClient, build_reqwest_client_with_custom_ca};
use futures::StreamExt;
use http::{HeaderMap, HeaderName, HeaderValue, Method};
use serde::{Deserialize, Serialize};
use tokio::io::{AsyncRead, AsyncReadExt, AsyncWrite, AsyncWriteExt};
use tokio::net::TcpListener;
use tokio::sync::oneshot;
use tokio::task::JoinSet;
use tokio::time::timeout;

mod websocket;
mod request_compression;

#[cfg(test)]
mod http_tests;

const PROTOCOL: u32 = 1;
const MAX_HEAD: usize = 64 * 1024;
const MAX_BODY: usize = 32 * 1024 * 1024;
const MAX_FRAME: usize = 64 * 1024;
const IPC_TIMEOUT: Duration = Duration::from_secs(30);
const ERROR_WRITE_TIMEOUT: Duration = Duration::from_millis(100);
const HEAD: u8 = 1;
const DATA: u8 = 2;
const END: u8 = 3;
const ERROR: u8 = 4;

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Config {
    protocol: u32,
    listen: Option<SocketAddr>,
    unix_socket: Option<String>,
    upstream: String,
    ipc_token: String,
    proxy: Option<String>,
    #[serde(default = "default_headers_ms")]
    headers_timeout_ms: u64,
    #[serde(default = "default_idle_ms")]
    idle_timeout_ms: u64,
    #[serde(default)]
    http_client_lifecycle: HttpClientLifecycle,
}

#[derive(Clone, Copy, Default, Deserialize, Serialize, PartialEq, Eq)]
#[serde(rename_all = "snake_case")]
enum HttpClientLifecycle {
    #[default]
    CliRequest,
    Pooled,
}

fn default_headers_ms() -> u64 { 90_000 }
fn default_idle_ms() -> u64 { 300_000 }

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct RequestHead {
    protocol: u32,
    ipc_token: String,
    method: String,
    path: String,
    // Base64 retains non-UTF8 header bytes and repeated fields.
    headers: Vec<(String, String)>,
    body_length: usize,
    #[serde(default)]
    unbounded_payload: bool,
    #[serde(default)]
    disable_websocket_compression: bool,
    #[serde(default)]
    total_timeout_ms: Option<u64>,
    #[serde(default)]
    operation: Option<String>,
    #[serde(default)]
    max_message_bytes: Option<usize>,
    #[serde(default)]
    write_timeout_ms: Option<u64>,
    #[serde(default)]
    request_compression: request_compression::RequestCompressionMode,
}

#[derive(Serialize)]
struct ResponseHead {
    status: u16,
    version: &'static str,
    headers: Vec<(String, String)>,
}

struct State {
    client: Option<HttpClient>,
    origin: reqwest::Url,
    ipc_token: String,
    headers_timeout: Option<Duration>,
    idle_timeout: Option<Duration>,
    proxy: Option<String>,
    websocket: codex_websocket_client::WebSocketConnector,
    http_client_lifecycle: HttpClientLifecycle,
}

// Error codes are deliberately finite and never contain URLs, headers, payloads
// or a library's Debug/Display string, including during TLS/proxy failures.
type Result<T> = std::result::Result<T, &'static str>;

trait IpcStream: AsyncRead + AsyncWrite + Unpin + Send {}
impl<T: AsyncRead + AsyncWrite + Unpin + Send> IpcStream for T {}

enum Listener {
    Tcp(TcpListener),
    #[cfg(unix)]
    Unix(tokio::net::UnixListener, unix_socket::SocketGuard),
}

impl Listener {
    async fn bind(config: &Config) -> Result<Self> {
        if let Some(path) = &config.unix_socket {
            #[cfg(unix)]
            {
                let (listener, guard) = unix_socket::bind(path)?;
                return Ok(Self::Unix(listener, guard));
            }
            #[cfg(not(unix))]
            {
                let _ = path;
                return Err("unix_unsupported");
            }
        }
        let address = config.listen.ok_or("config_invalid")?;
        Ok(Self::Tcp(TcpListener::bind(address).await.map_err(|_| "listen_failed")?))
    }

    fn ready(&self) -> Result<serde_json::Value> {
        let mut ready = serde_json::json!({
            "protocol": PROTOCOL,
            "cli_source": "0.155.0",
            "helper": env!("CARGO_PKG_VERSION"),
            "max_body": MAX_BODY,
            // Admission is performed by the gateway per account. A route may
            // carry several accounts, so the helper must not cap their total.
            "max_active": 0,
            "capabilities": ["websocket_message_v1", "request_zstd_v1", "unbounded_payload_v1", "websocket_message_v2", "websocket_no_compression_v1"],
        });
        match self {
            Self::Tcp(listener) => {
                ready["transport"] = "tcp".into();
                ready["listen"] = listener.local_addr().map_err(|_| "listen_failed")?.to_string().into();
            }
            #[cfg(unix)]
            Self::Unix(_, guard) => {
                ready["transport"] = "unix".into();
                ready["unix_socket"] = guard.path().into();
            }
        }
        Ok(ready)
    }

    async fn accept(&self) -> Result<Option<Box<dyn IpcStream>>> {
        match self {
            Self::Tcp(listener) => {
                let (stream, peer) = listener.accept().await.map_err(|_| "accept_failed")?;
                if !peer.ip().is_loopback() { return Ok(None); }
                // This is the local IPC hop, not the upstream HTTP/TLS socket.
                // Small HEAD/DATA/END writes must not wait on delayed ACKs.
                stream.set_nodelay(true).map_err(|_| "ipc_socket")?;
                Ok(Some(Box::new(stream)))
            }
            #[cfg(unix)]
            Self::Unix(listener, guard) => {
                let (stream, _) = listener.accept().await.map_err(|_| "accept_failed")?;
                if !stream.peer_cred().is_ok_and(|peer| peer.uid() == guard.uid()) {
                    return Ok(None);
                }
                Ok(Some(Box::new(stream)))
            }
        }
    }
}

#[cfg(unix)]
mod unix_socket {
    use std::fs;
    use std::os::unix::fs::{FileTypeExt, MetadataExt, PermissionsExt};
    use std::path::{Component, Path};
    use tokio::net::{UnixListener, UnixStream};
    use super::Result;

    pub struct SocketGuard {
        path: String,
        device: u64,
        inode: u64,
        uid: u32,
    }

    impl SocketGuard {
        pub fn path(&self) -> &str { &self.path }
        pub fn uid(&self) -> u32 { self.uid }
    }

    impl Drop for SocketGuard {
        fn drop(&mut self) {
            // Never remove a path replaced by another process or a symlink.
            if let Ok(metadata) = fs::symlink_metadata(&self.path) {
                if metadata.file_type().is_socket()
                    && metadata.dev() == self.device && metadata.ino() == self.inode
                {
                    let _ = fs::remove_file(&self.path);
                }
            }
        }
    }

    pub fn bind(value: &str) -> Result<(UnixListener, SocketGuard)> {
        let path = Path::new(value);
        if !path.is_absolute() || path.file_name().is_none()
            || path.components().any(|part| matches!(part, Component::ParentDir | Component::CurDir))
        { return Err("unix_path_invalid"); }
        let parent = path.parent().ok_or("unix_path_invalid")?;
        // Resolve all ancestors too; a private-looking final directory reached
        // through a symlink is not an accepted service IPC directory.
        if fs::canonicalize(parent).map_err(|_| "unix_parent_invalid")? != parent {
            return Err("unix_parent_invalid");
        }
        let parent_metadata = fs::symlink_metadata(parent).map_err(|_| "unix_parent_invalid")?;
        let (probe, _) = UnixStream::pair().map_err(|_| "unix_identity")?;
        let uid = probe.peer_cred().map_err(|_| "unix_identity")?.uid();
        if !parent_metadata.is_dir() || parent_metadata.file_type().is_symlink()
            || parent_metadata.permissions().mode() & 0o7777 != 0o700
            || parent_metadata.uid() != uid
        { return Err("unix_parent_invalid"); }
        match fs::symlink_metadata(path) {
            Ok(_) => return Err("unix_path_exists"),
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => {},
            Err(_) => return Err("unix_path_invalid"),
        }
        let listener = UnixListener::bind(path).map_err(|_| "listen_failed")?;
        let metadata = fs::symlink_metadata(path).map_err(|_| "unix_metadata")?;
        let guard = SocketGuard { path: value.to_owned(), device: metadata.dev(), inode: metadata.ino(), uid };
        if !metadata.file_type().is_socket() || metadata.uid() != uid {
            return Err("unix_metadata");
        }
        fs::set_permissions(path, fs::Permissions::from_mode(0o600)).map_err(|_| "unix_permissions")?;
        Ok((listener, guard))
    }
}

fn optional_timeout(milliseconds: u64) -> Option<Duration> {
    (milliseconds != 0).then(|| Duration::from_millis(milliseconds))
}

async fn deadline<F: std::future::Future>(duration: Option<Duration>, operation: F, code: &'static str) -> Result<F::Output> {
    match duration {
        Some(duration) => timeout(duration, operation).await.map_err(|_| code),
        None => Ok(operation.await),
    }
}

fn read_config() -> Result<Config> {
    let mut input = std::io::stdin().lock();
    let mut length = [0_u8; 4];
    input.read_exact(&mut length).map_err(|_| "config_read")?;
    let length = u32::from_be_bytes(length) as usize;
    if length == 0 || length > MAX_HEAD { return Err("config_limit"); }
    let mut bytes = vec![0; length];
    input.read_exact(&mut bytes).map_err(|_| "config_read")?;
    let config: Config = serde_json::from_slice(&bytes).map_err(|_| "config_json")?;
    if config.protocol != PROTOCOL
        || config.listen.is_some() == config.unix_socket.is_some()
        || config.listen.is_some_and(|address| !address.ip().is_loopback())
        || config.ipc_token.len() != 64
        || !config.ipc_token.bytes().all(|b| b.is_ascii_hexdigit())
        || config.headers_timeout_ms > 3_600_000
        || config.idle_timeout_ms > 3_600_000
    { return Err("config_invalid"); }
    Ok(config)
}

fn build_state(config: &Config) -> Result<State> {
    let origin = reqwest::Url::parse(&config.upstream).map_err(|_| "upstream_invalid")?;
    if !matches!(origin.scheme(), "http" | "https")
        || origin.host_str().is_none()
        || !origin.username().is_empty()
        || origin.password().is_some()
        || origin.path() != "/"
        || origin.query().is_some()
        || origin.fragment().is_some()
    { return Err("upstream_invalid"); }

    let client = build_http_client(config.proxy.as_deref(), optional_timeout(config.headers_timeout_ms))?;
    let factory = codex_http_client::HttpClientFactory::new(codex_http_client::OutboundProxyPolicy::ReqwestDefault);
    let websocket = codex_websocket_client::WebSocketConnector::new(&factory).map_err(|_| "tls_or_client_config")?;
    Ok(State {
        client: if config.http_client_lifecycle == HttpClientLifecycle::Pooled { client } else { None },
        origin,
        ipc_token: config.ipc_token.clone(),
        headers_timeout: optional_timeout(config.headers_timeout_ms),
        idle_timeout: optional_timeout(config.idle_timeout_ms),
        proxy: config.proxy.clone(),
        websocket,
        http_client_lifecycle: config.http_client_lifecycle,
    })
}

fn build_http_client(proxy: Option<&str>, header_timeout: Option<Duration>) -> Result<Option<HttpClient>> {
    // Preserve the official default native TLS selection and CA switching.
    // Gateway policy owns retries and redirects, so the adapter disables both.
    let mut builder = reqwest::Client::builder()
        .no_proxy()
        .redirect(reqwest::redirect::Policy::none())
        .retry(reqwest::retry::never())
        .no_gzip().no_brotli().no_deflate().no_zstd();
    if let Some(duration) = header_timeout {
        builder = builder.connect_timeout(duration);
    }
    let mut http_supported = true;
    if let Some(proxy) = proxy {
        let parsed = reqwest::Url::parse(proxy).map_err(|_| "proxy_invalid")?;
        // No SOCKS feature is added to the official HTTP feature set silently.
        if !matches!(parsed.scheme(), "http" | "https" | "socks5" | "socks5h") || parsed.host_str().is_none() {
            return Err("proxy_unsupported");
        }
        if parsed.path() != "/" || parsed.query().is_some() || parsed.fragment().is_some() {
            return Err("proxy_invalid");
        }
        if matches!(parsed.scheme(), "socks5" | "socks5h") {
            http_supported = false;
        } else {
            builder = builder.proxy(reqwest::Proxy::all(proxy).map_err(|_| "proxy_invalid")?);
        }
    }
    builder = codex_http_client::with_chatgpt_cloudflare_cookie_store(builder);
    let client = if http_supported {
        Some(HttpClient::new_without_request_logging(build_reqwest_client_with_custom_ca(builder).map_err(|_| "tls_or_client_config")?))
    } else { None };
    Ok(client)
}

fn token_matches(expected: &str, supplied: &str) -> bool {
    if supplied.len() != expected.len() { return false; }
    expected.bytes().zip(supplied.bytes()).fold(0_u8, |n, (a, b)| n | (a ^ b)) == 0
}

fn request_parts(head: &RequestHead, state: &State) -> Result<(Method, reqwest::Url, HeaderMap)> {
    if head.protocol != PROTOCOL || !token_matches(&state.ipc_token, &head.ipc_token) {
        return Err("ipc_auth");
    }
    if (!head.unbounded_payload && head.body_length > MAX_BODY) || head.headers.len() > 128 {
        return Err("request_limit");
    }
    if !head.path.starts_with('/') || head.path.starts_with("//")
        || head.path.contains('#') || head.path.contains('\\')
        || head.path.bytes().any(|byte| !(0x21..=0x7e).contains(&byte))
    { return Err("path_invalid"); }
    let url = reqwest::Url::parse(&format!("{}{}", state.origin.as_str().trim_end_matches('/'), head.path))
        .map_err(|_| "path_invalid")?;
    if url.origin() != state.origin.origin() { return Err("path_invalid"); }
    // Do not silently rewrite dot segments, whitespace, or URL escaping. The
    // caller supplies an already prepared origin-form request target.
    let target = match url.query() {
        Some(query) => format!("{}?{}", url.path(), query),
        None => url.path().to_owned(),
    };
    if target != head.path { return Err("path_invalid"); }
    let method = Method::from_bytes(head.method.as_bytes()).map_err(|_| "method_invalid")?;
    if method == Method::CONNECT || method == Method::TRACE { return Err("method_unsupported"); }
    if head.total_timeout_ms.is_some_and(|value| value == 0 || value > 86_400_000) {
        return Err("timeout_invalid");
    }
    let mut headers = HeaderMap::new();
    for (name, value) in &head.headers {
        let name = HeaderName::from_bytes(name.as_bytes()).map_err(|_| "header_invalid")?;
        let raw = BASE64.decode(value).map_err(|_| "header_invalid")?;
        let mut value = HeaderValue::from_bytes(&raw).map_err(|_| "header_invalid")?;
        match name.as_str() {
            // Callers supply end-to-end headers; transport framing is derived
            // from the actual raw body. Reject conflicts, never rewrite them.
            "host" | "connection" | "transfer-encoding" | "upgrade" | "trailer" | "te"
            | "proxy-authorization" | "proxy-authenticate" | "proxy-connection" | "keep-alive" => return Err("header_hop_by_hop"),
            "content-length" => {
                // usize::parse accepts a leading '+', which HTTP's 1*DIGIT
                // grammar does not. Reject ambiguous framing before reqwest.
                let length = value.to_str().ok()
                    .filter(|s| !s.is_empty() && s.bytes().all(|byte| byte.is_ascii_digit()))
                    .and_then(|s| s.parse::<usize>().ok());
                if length != Some(head.body_length) || headers.contains_key(&name) {
                    return Err("content_length_invalid");
                }
            }
            "authorization" | "cookie" => value.set_sensitive(true),
            _ => {}
        }
        headers.append(name, value);
    }
    if head.request_compression != request_compression::RequestCompressionMode::None
        && headers.contains_key(http::header::CONTENT_ENCODING)
    { return Err("request_compression_encoding_conflict"); }
    Ok((method, url, headers))
}

async fn read_head<R: AsyncRead + Unpin>(reader: &mut R) -> Result<RequestHead> {
    let length = reader.read_u32().await.map_err(|_| "ipc_read")? as usize;
    if length == 0 || length > MAX_HEAD { return Err("request_limit"); }
    let mut encoded = vec![0; length];
    reader.read_exact(&mut encoded).await.map_err(|_| "ipc_read")?;
    serde_json::from_slice(&encoded).map_err(|_| "request_json")
}

async fn frame<W: AsyncWrite + Unpin>(writer: &mut W, in_frame: &mut bool, kind: u8, data: &[u8]) -> Result<()> {
    if data.len() > MAX_HEAD { return Err("response_header_limit"); }
    *in_frame = true;
    writer.write_u8(kind).await.map_err(|_| "ipc_write")?;
    writer.write_u32(data.len() as u32).await.map_err(|_| "ipc_write")?;
    writer.write_all(data).await.map_err(|_| "ipc_write")?;
    *in_frame = false;
    Ok(())
}

fn classify_error(error: reqwest::Error) -> &'static str {
    if error.is_timeout() { "upstream_timeout" }
    else if error.is_connect() { "upstream_connect" }
    else if error.is_body() || error.is_decode() { "upstream_body" }
    else { "upstream_transport" }
}

async fn exchange<W: AsyncWrite + Unpin>(
    state: &State,
    method: Method,
    url: reqwest::Url,
    headers: HeaderMap,
    body: Bytes,
    compression: request_compression::RequestCompressionMode,
    unbounded_payload: bool,
    writer: &mut W,
    in_frame: &mut bool,
) -> Result<()> {
    let (headers, body) = request_compression::prepare_with_policy(method.clone(), url.as_str(), headers, body, compression, unbounded_payload)?;
    // Official ModelClient::build_api_transport creates a fresh HTTP client
    // for each Responses call. Keep pooling an explicit performance policy.
    let request_client = if state.http_client_lifecycle == HttpClientLifecycle::CliRequest {
        build_http_client(state.proxy.as_deref(), state.headers_timeout)?
    } else { None };
    let client = if state.http_client_lifecycle == HttpClientLifecycle::CliRequest {
        request_client.as_ref()
    } else { state.client.as_ref() }.ok_or("proxy_unsupported")?;
    let response = deadline(
        state.headers_timeout,
        client.request(method, url).headers(headers).body(body).send(),
        "headers_timeout",
    ).await?.map_err(classify_error)?;
    // Do not use error_for_status or ReqwestTransport's error-body aggregation.
    let header = ResponseHead {
        status: response.status().as_u16(),
        version: match response.version() {
            http::Version::HTTP_09 => "HTTP/0.9",
            http::Version::HTTP_10 => "HTTP/1.0",
            http::Version::HTTP_11 => "HTTP/1.1",
            http::Version::HTTP_2 => "HTTP/2",
            http::Version::HTTP_3 => "HTTP/3",
            _ => "unknown",
        },
        headers: response.headers().iter()
            .map(|(name, value)| (name.as_str().to_owned(), BASE64.encode(value.as_bytes())))
            .collect(),
    };
    let encoded = serde_json::to_vec(&header).map_err(|_| "response_header_encode")?;
    frame(writer, in_frame, HEAD, &encoded).await?;
    if response.status() == http::StatusCode::SWITCHING_PROTOCOLS {
        return Err("upgrade_unsupported");
    }
    let mut stream = response.bytes_stream();
    loop {
        // The idle clock only runs while awaiting upstream data. It is stopped
        // throughout IPC writes, so downstream backpressure isn't upstream idle.
        let next = deadline(state.idle_timeout, stream.next(), "idle_timeout").await?;
        let Some(chunk) = next else { break; };
        let chunk = chunk.map_err(classify_error)?;
        for part in chunk.chunks(MAX_FRAME) {
            frame(writer, in_frame, DATA, part).await?;
        }
    }
    frame(writer, in_frame, END, &[]).await
}

async fn transaction<S: IpcStream>(stream: S, state: Arc<State>) {
    let (mut reader, mut writer) = tokio::io::split(stream);
    let mut in_frame = false;
    let result = async {
        let head = timeout(IPC_TIMEOUT, read_head(&mut reader)).await.map_err(|_| "ipc_timeout")??;
        let (method, url, headers) = request_parts(&head, &state)?;
        match head.operation.as_deref() {
            Some("websocket_message_v1") | Some("websocket_message_v2") => {
                if head.request_compression != request_compression::RequestCompressionMode::None {
                    return Err("request_compression_operation");
                }
                websocket::transaction(&state, &head, method, url, headers, &mut reader, &mut writer).await;
                return Ok(());
            }
            None | Some("http") => {},
            _ => return Err("operation_unsupported"),
        }
        let operation = async {
            // A caller-controlled length must never reach Vec's panicking
            // allocation path, even when the policy has no payload ceiling.
            let mut body = Vec::new();
            body.try_reserve_exact(head.body_length).map_err(|_| "request_allocation")?;
            body.resize(head.body_length, 0);
            timeout(IPC_TIMEOUT, reader.read_exact(&mut body)).await
                .map_err(|_| "ipc_timeout")?.map_err(|_| "ipc_read")?;
            let operation = exchange(&state, method, url, headers, Bytes::from(body), head.request_compression, head.unbounded_payload, &mut writer, &mut in_frame);
            // After body_length bytes the reverse direction is only a cancellation
            // channel. Any byte, EOF or read error cancels, including while blocked
            // in an IPC DATA write. Clients must not half-close before END.
            let mut cancel = [0; 1];
            tokio::select! {
                biased;
                _ = reader.read(&mut cancel) => Err("cancelled"),
                result = operation => result,
            }
        };
        // A total budget covers IPC body upload too; never reset it once the
        // upload finishes. Header decoding retains its separate IPC deadline.
        deadline(head.total_timeout_ms.map(Duration::from_millis), operation, "total_timeout").await?
    }.await;
    if let Err(code) = result {
        // Cancellation or a total timeout can interrupt any frame. Never append
        // an ERROR frame into that frame's unfinished payload.
        if !in_frame && code != "cancelled" && code != "ipc_write" {
            let _ = timeout(ERROR_WRITE_TIMEOUT, frame(&mut writer, &mut in_frame, ERROR, code.as_bytes())).await;
        }
    }
    let _ = writer.shutdown().await;
}

async fn run() -> Result<()> {
    let config = read_config()?;
    let state = Arc::new(build_state(&config)?);
    let listener = Listener::bind(&config).await?;
    // The only stdout record: safe process metadata, no route or credentials.
    let mut ready = listener.ready()?;
    ready["http_client_lifecycle"] = serde_json::to_value(config.http_client_lifecycle).map_err(|_| "config_invalid")?;
    println!("{}", ready);
    std::io::stdout().flush().map_err(|_| "ready_write")?;

    let (stop_tx, mut stop_rx) = oneshot::channel();
    // A std thread, rather than Tokio's blocking stdin adapter, lets runtime
    // shutdown finish without waiting for a blocked standard-input read.
    std::thread::spawn(move || {
        let mut byte = [0; 1];
        let _ = std::io::stdin().read(&mut byte);
        let _ = stop_tx.send(());
    });
    let mut tasks = JoinSet::new();
    let mut accept_retry_at = tokio::time::Instant::now();
    loop {
        tokio::select! {
            biased;
            _ = &mut stop_rx => break,
            _ = tasks.join_next(), if !tasks.is_empty() => {},
            accepted = async {
                tokio::time::sleep_until(accept_retry_at).await;
                listener.accept().await
            } => {
                let stream = match accepted {
                    Ok(stream) => stream,
                    Err(_) => {
                        // An aborted connection or temporary descriptor pressure
                        // must not kill unrelated requests. Backoff stays inside
                        // select so parent EOF and task cleanup remain responsive.
                        accept_retry_at = tokio::time::Instant::now() + Duration::from_millis(100);
                        continue;
                    }
                };
                let Some(stream) = stream else { continue; };
                let state = state.clone();
                tasks.spawn(async move {
                    transaction(stream, state).await;
                });
            }
        }
    }
    tasks.shutdown().await;
    Ok(())
}

#[tokio::main]
async fn main() {
    std::panic::set_hook(Box::new(|_| eprintln!("native-transport: internal panic")));
    if let Err(code) = run().await {
        eprintln!("native-transport: {code}");
        std::process::exit(1);
    }
}
