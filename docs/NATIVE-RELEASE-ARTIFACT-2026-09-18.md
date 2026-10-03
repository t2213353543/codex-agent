# Native Release Artifact

This artifact was built and validated locally. It has not been deployed, and
no real-account or public-model testing was performed for this release step.

## Package

- Package: `.local/native-releases/0.155.0-linux-x86_64-ffbfae2a3af7.tar.gz`.
- Package size: 5,714,793 bytes.
- Package SHA256: `9611fac32443b44c68c4b9106e817ba10261a7c884e3ec734b34a2e7802673f7`.
- Manifest SHA256: `ced8aa98672c206b12990b9d72f50f8b5cd21e0dae889220f965abbbf7014f5f`.
- Binary SHA256: `ffbfae2a3af7a452e30df333f06908836fc9f55d4a0e168ed545fcd83d6a2d1c`.
- Binary size: 14,290,944 bytes, compared with 128,408,416 bytes for the previous debug artifact.

The package contains the executable, per-file hash manifest, exact source
provenance, official lock graph, WS adapter patch, license/notice, build and
test evidence, ELF inspection, runtime capabilities and deployment environment
examples. All 19 manifest-covered files were verified from the completed tar
archive; the executable archive mode is 0755. No signature is claimed.

## Build And Runtime Evidence

Run: `.local/native-local-build/20260918T045203Z-214aa719/`.

Source SHA256: `05ff7dd889c252f32ae1c8a302abe8b98abf1d10c60af6385ac1053f71faab25`.
This is the same frozen source as the strict-lifecycle debug build, not a source
rewrite for release. Cargo used the existing release profile: thin LTO,
codegen-units=4 and stripped symbols. The container stayed at 2 CPU and 3 GiB;
there was no OOM, resource escalation or profile relaxation.

`cargo build --release --locked` completed, followed by four real WS socket
tests under `cargo test --release`, then 25 HTTP/UDS tests against the release
executable with zero skips. Lock verification covers 265 packages, 30 unchanged
upstream Rust files and one explicitly recorded route-adapter source patch.
The observed ready record identifies CLI source 0.155.0,
`websocket_message_v1`, default `cli_request`, 32 admitted IPC tasks and the
32 MiB request-body limit.

`file` identified a stripped x86-64 static PIE ELF. `readelf` found no PT_INTERP
and no DT_NEEDED entries. A static PIE can still contain a dynamic section;
that alone is not a shared-library dependency. The compiler host was confirmed
as `x86_64-unknown-linux-musl`.

A separate read-only, network-disabled container with a rootfs containing only
this executable, no libc files and no dynamic loader completed startup, emitted
the expected ready record and exited normally on stdin EOF. Its temporary
container and imported image were removed. This confirms loader independence,
not a claim that production CentOS, every Linux kernel, CA store or target
network configuration was exercised. Native trust roots remain deployment
environment inputs.

## Rerun

```powershell
python tools/native_transport_local.py start --new --cache-from 20260918T042849Z-171e64e6 --profile release
```

Poll the returned handle with `status --run`, collect only after terminal
success, then run `tools/native_transport_release_smoke.py --run` and
`tools/native_transport_release.py --run`. The runner defaults to debug for
existing callers, records profile-specific artifact paths, and compares stale
binary hashes only between the same profile. Release binaries still require
their own final Go/official-CLI integration evidence; debug results do not
automatically certify the packaged digest.
