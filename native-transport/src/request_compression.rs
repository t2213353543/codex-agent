//! Preserve the gateway-prepared JSON bytes while using the official encoder.

use bytes::Bytes;
use codex_http_client::{EncodedJsonBody, Request, RequestBody, RequestCompression};
use http::{HeaderMap, Method};
use serde::Deserialize;
use serde_json::value::RawValue;

use crate::{MAX_BODY, Result};

#[derive(Clone, Copy, Default, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "snake_case")]
pub(crate) enum RequestCompressionMode {
    #[default]
    None,
    Zstd,
}

#[cfg(test)]
fn prepare(method: Method, url: &str, headers: HeaderMap, body: Bytes, mode: RequestCompressionMode) -> Result<(HeaderMap, Bytes)> {
    prepare_with_policy(method, url, headers, body, mode, false)
}

pub(crate) fn prepare_with_policy(method: Method, url: &str, mut headers: HeaderMap, body: Bytes, mode: RequestCompressionMode, unbounded_payload: bool) -> Result<(HeaderMap, Bytes)> {
    if !unbounded_payload && body.len() > MAX_BODY { return Err("request_limit"); }
    if mode == RequestCompressionMode::None { return Ok((headers, body)); }
    if headers.contains_key(http::header::CONTENT_ENCODING) { return Err("request_compression_encoding_conflict"); }
    let value: &RawValue = serde_json::from_slice(&body).map_err(|_| "request_compression_json")?;
    let encoded = EncodedJsonBody::encode(value).map_err(|_| "request_compression_json")?;
    // RawValue keeps internal ordering, duplicate keys, escapes, and number
    // spelling, but its parser trims outer whitespace. Never silently do so.
    if encoded.as_bytes() != body.as_ref() { return Err("request_compression_json_noncanonical"); }
    headers.remove(http::header::CONTENT_LENGTH);
    let mut request = Request::new(method, url.to_owned()).with_compression(RequestCompression::Zstd);
    request.headers = headers;
    request.body = Some(RequestBody::EncodedJson(encoded));
    let prepared = request.prepare_body_for_send().map_err(|_| "request_compression_failed")?;
    let body = prepared.body.ok_or("request_compression_failed")?;
    if !unbounded_payload && body.len() > MAX_BODY + 1024 * 1024 { return Err("request_compression_limit"); }
    Ok((prepared.headers, body))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn negotiated_large_json_preserves_all_bytes_through_zstd() {
        let mut raw = Vec::with_capacity(33 * 1024 * 1024 + 2);
        raw.push(b'"');
        raw.resize(33 * 1024 * 1024 + 1, b'x');
        raw.push(b'"');
        let body = Bytes::from(raw);
        for mode in [RequestCompressionMode::None, RequestCompressionMode::Zstd] {
            let (_, prepared) = prepare_with_policy(Method::POST, "http://fixture.invalid/responses", HeaderMap::new(), body.clone(), mode, true).unwrap();
            if mode == RequestCompressionMode::None { assert_eq!(prepared, body); }
            else { assert_eq!(zstd::stream::decode_all(std::io::Cursor::new(prepared)).unwrap(), body); }
        }
    }

    #[test]
    fn preserves_raw_json_and_matches_official_level_three_exactly() {
        let body = Bytes::from_static(br#"{"z": 1e+03, "duplicate":1,"duplicate":2,"escaped":"\u0061","tools":[],"input":[{"content":"fixture"}]}"#);
        let mut headers = HeaderMap::new();
        headers.insert(http::header::CONTENT_LENGTH, body.len().into());
        headers.insert(http::header::CONTENT_TYPE, "application/json; charset=utf-8".parse().unwrap());
        let (first_headers, first) = prepare(Method::POST, "http://fixture.invalid/responses", headers.clone(), body.clone(), RequestCompressionMode::Zstd).unwrap();
        let (second_headers, second) = prepare(Method::POST, "http://fixture.invalid/responses", headers, body.clone(), RequestCompressionMode::Zstd).unwrap();
        assert_eq!(first, Bytes::from(zstd::stream::encode_all(std::io::Cursor::new(body.as_ref()), 3).unwrap()));
        assert_eq!(zstd::stream::decode_all(std::io::Cursor::new(&first)).unwrap(), body);
        assert_eq!(first, second);
        assert_eq!(first_headers, second_headers);
        assert_eq!(first_headers[http::header::CONTENT_ENCODING], "zstd");
        assert_eq!(first_headers[http::header::CONTENT_TYPE], "application/json; charset=utf-8");
        assert!(!first_headers.contains_key(http::header::CONTENT_LENGTH));
    }

    #[test]
    fn none_keeps_preencoded_bytes_and_headers_opaque() {
        let body = Bytes::from_static(b"\x28\xb5\x2f\xfdsynthetic-preencoded");
        let mut headers = HeaderMap::new();
        headers.insert(http::header::CONTENT_ENCODING, "zstd".parse().unwrap());
        let (actual_headers, actual) = prepare(Method::POST, "http://fixture.invalid/responses", headers.clone(), body.clone(), RequestCompressionMode::None).unwrap();
        assert_eq!(actual_headers, headers);
        assert_eq!(actual, body);
    }

    #[test]
    fn invalid_or_ambiguous_compression_is_rejected_without_rewriting() {
        let body = Bytes::from_static(b"{}");
        let mut headers = HeaderMap::new();
        headers.insert(http::header::CONTENT_ENCODING, "zstd".parse().unwrap());
        assert_eq!(prepare(Method::POST, "http://fixture.invalid", headers, body, RequestCompressionMode::Zstd).unwrap_err(), "request_compression_encoding_conflict");
        for (body, expected) in [(b"invalid".as_slice(), "request_compression_json"), (b" {}\n".as_slice(), "request_compression_json_noncanonical")] {
            assert_eq!(prepare(Method::POST, "http://fixture.invalid", HeaderMap::new(), Bytes::copy_from_slice(body), RequestCompressionMode::Zstd).unwrap_err(), expected);
        }
        assert_eq!(prepare(Method::POST, "http://fixture.invalid", HeaderMap::new(), Bytes::from(vec![b' '; MAX_BODY + 1]), RequestCompressionMode::Zstd).unwrap_err(), "request_limit");
    }
}
