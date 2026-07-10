# r2proxy

A man-in-the-middle proxy for Cloudflare R2 (and any S3-compatible endpoint),
built for **testing**. Your program talks to r2proxy instead of R2; r2proxy
records statistics, optionally injects errors/latency, and otherwise
transparently re-signs and forwards every request to the real backend.

- **Transparent MITM** — full request/response visibility; streams large objects.
- **Statistics** — per-op / per-status counters, bytes, latency percentiles,
  live request feed.
- **Error injection** — filter by key / op, probability, any status (`500`,
  `503`, `429`, …) with a **byte-exact R2 error body** + `Retry-After`, plus
  latency injection.
- **UI + CLI** — a web console and a `r2proxy` CLI, both over the admin API.
- **Simple auth** — one proxy access key + secret for S3 clients, one admin
  token for the console.
- **Trivial deploy** — one static Go binary (UI embedded), or Docker, or one
  command to an Ubicloud VM.

## How it works

S3 requests are SigV4-signed against the *host*, so a naive TCP forward fails
signature validation at R2. r2proxy instead **terminates** each request,
**verifies** the caller's signature against the proxy secret, then **re-signs**
with the real R2 credentials toward the R2 endpoint:

```
your app ──SigV4(proxy creds)──▶ r2proxy ──re-SigV4(real R2 creds)──▶ R2
                                    │
                            stats · error injection
```

Point your S3 client at the proxy with **path-style** addressing, using the
proxy access key + secret. r2proxy does the rest.

## Quick start

```bash
make build      # -> dist/r2proxy  (single binary, UI embedded)

R2PROXY_ENDPOINT=https://ACCOUNT.r2.cloudflarestorage.com \
R2PROXY_ACCESS_KEY=<r2 key> R2PROXY_SECRET_KEY=<r2 secret> \
./dist/r2proxy serve
```

On first run it prints (and persists) the **proxy access key + secret** your S3
client uses, and the **admin token** for the console:

```
┌─ r2proxy ready ─────────────────────────────────────────
│ proxy endpoint    http://localhost:8080
│ proxy access key  0123456789abcdef0123456789abcdef
│ proxy secret key  fedcba9876543210fedcba9876543210…  (64 hex chars)
│ upstream          https://ACCOUNT.r2.cloudflarestorage.com (auto)
│ console           http://localhost:8081
│ admin token       0f0e0d0c0b0a09080706050403020100f0e0d0c0b0a09080
└─────────────────────────────────────────────────────────
```

Then point any S3 client at the proxy, path-style, with the **proxy** creds:

```bash
export AWS_ACCESS_KEY_ID=<proxy access key>
export AWS_SECRET_ACCESS_KEY=<proxy secret key>
export AWS_DEFAULT_REGION=auto
aws s3 cp ./file s3://test/file --endpoint-url http://localhost:8080
```

Open the console at **http://localhost:8081** and log in with the admin token.
Ports: **8080** = S3 data-plane, **8081** = admin API + console.

## CLI

The CLI talks to the admin API:

```bash
export R2PROXY_ADMIN=http://HOST:8081
export R2PROXY_TOKEN=<admin token>

r2proxy stats                 # statistics
r2proxy tail                  # live request feed
r2proxy rules list
r2proxy rules add --op GetObject --key 'photos/*' --status 503 --prob 0.3
r2proxy rules add --op '*' --delay 500          # inject 500ms latency on everything
r2proxy rules add --op GetObject --status 429   # R2's exact same-object throttle
r2proxy rules toggle <id> | rm <id> | clear
```

## Load testing

`loadtest.py` uploads many objects through the proxy concurrently and reports
total duration + throughput (needs `boto3`):

```bash
export AWS_ACCESS_KEY_ID=<proxy access key>
export AWS_SECRET_ACCESS_KEY=<proxy secret key>
python3 loadtest.py --endpoint http://HOST:8080 --count 1000 --size-mb 1 --concurrency 32 --cleanup
```

Flags: `--endpoint --bucket --count --size-mb --concurrency --prefix --cleanup`.
Afterward, `r2proxy stats` shows the recorded PutObjects, bytes, and latency
percentiles.

## Error injection

A rule matches on `op` (comma list; blank = any) and `key` glob, fires with
`probability` (0–1), then injects an S3 error (`status` + `code` + `message` +
`retry_after`) and/or `delay_ms` of latency. Rules are evaluated top-to-bottom;
the first error rule that fires wins. `code`, `message`, and `retry_after` are
optional — left blank, r2proxy fills in **R2-accurate defaults** (e.g. a bare
`--status 429` reproduces R2's real 159-byte `ServiceUnavailable` /
`Retry-After: 5` throttle byte-for-byte). Manage rules in the console or via
`r2proxy rules`.

## Security

- **Data plane:** requests must be SigV4-signed with the proxy secret. r2proxy
  recomputes and compares the signature, so the proxy is not an open relay to
  your R2. Unknown key → `InvalidAccessKeyId`; bad signature →
  `SignatureDoesNotMatch`.
- **Control plane:** the console/API require the admin token (bearer).
- State (proxy creds, admin token, rules) persists to `--config` (default
  `r2proxy.json`, mode `0600`); live stats are in-memory and reset on restart.

## Deploy

### Ubicloud (one command)

```bash
cp deploy.env.example deploy.env      # fill in UBI_TOKEN + R2 creds
./deploy.sh up                        # provision VM + firewall, build, install, start
./deploy.sh info                      # print URLs + credentials
./deploy.sh push                      # rebuild the binary + restart (redeploy)
./deploy.sh destroy                   # tear it all down
```

### Docker (anywhere)

```bash
docker compose up -d          # reads R2PROXY_* from your env / .env
```

### Anywhere else

It's a single static binary with no runtime dependencies — copy `dist/r2proxy`,
run `r2proxy serve` behind a `systemd` unit (see the one `deploy.sh` installs).

## Notes

- Configure clients for **path-style** addressing (boto3:
  `s3={'addressing_style':'path'}`; aws-sdk: `forcePathStyle: true`). The AWS CLI
  works as-is.
- `aws-chunked` streaming uploads are decoded before re-signing, so the AWS CLI
  and all SDKs upload correctly.
- Talk to the proxy over plain HTTP (it re-signs to R2 over HTTPS). Put it behind
  a TLS terminator if you need HTTPS on the client side.
