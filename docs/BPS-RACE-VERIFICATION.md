# PowerPoint BPS race verification

Run the adapter protocol, streaming, HTTP/WebSocket gateway and isolated Redis history tests under Linux's Go race detector:

```powershell
python tools/bps_race.py --with-redis
```

The runner uses the existing project integration SSH helper and compiler Docker image. It does not deploy an application, read model account credentials or configure a business database. It snapshots only project Go sources, migrations, `go.mod` and `go.sum`, vendors dependencies from the local module cache with network fetching disabled, and records every source file's SHA-256. The remote container mounts this source read-only and runs only `./internal/bps` and `./internal/server -run '^TestBPS'` with `-race -count=1`.

The default command without options runs with Docker networking disabled. `--with-redis` additionally runs the four Redis persistence tests against the existing isolated integration Redis on loopback port 26379. `--redis-only` selects just those four tests. Redis tests use random `codex-agent:integration:bps-test:` namespaces and delete only explicitly registered keys. The temporary credential environment file never enters the source archive or reports and is removed with the test container. Real model tests explicitly skip because their credential variables are unset.

Useful options:

```powershell
python tools/bps_race.py --prepare-only
python tools/bps_race.py
python tools/bps_race.py --redis-only
```

`--prepare-only` validates and freezes local source without connecting to the server. `--image` selects an already installed compiler image; the runner resolves it to an immutable image ID. `--timeout` bounds the whole remote run, between 60 and 1800 seconds.

Each invocation writes a timestamped directory under `.local/powerpoint-migration/race/` containing `source-manifest.json`, `go-test.jsonl`, and `report.json`. The report distinguishes tested source hashes from later workspace edits, real test passes from credential-gated skips, and functional failures from race detector warnings. A previously passing snapshot does not certify source changes made afterward. The original `.local/.../run_race.py` path remains a small compatibility launcher for older notes.
