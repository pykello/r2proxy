# r2proxy — Complete Guide

A man-in-the-middle proxy for Cloudflare R2 (and any S3-compatible endpoint),
built for **testing**. Your program points its S3 client at r2proxy instead of
R2; the proxy records statistics, optionally injects errors/latency, and
otherwise transparently re-signs and forwards every request to the real backend.

- [1. What it does](#1-what-it-does)
- [2. Architecture: why re-signing](#2-architecture-why-re-signing)
- [3. Install & build](#3-install--build)
- [4. Running the proxy](#4-running-the-proxy)
- [5. Pointing your client at it](#5-pointing-your-client-at-it)
- [6. Statistics & load testing](#6-statistics--load-testing)
- [7. Error injection](#7-error-injection)
- [8. Matching R2 errors exactly (429 verified)](#8-matching-r2-errors-exactly-429-verified)
- [9. Security](#9-security)
- [10. Web console](#10-web-console)
- [11. CLI reference](#11-cli-reference)
- [12. Admin API reference](#12-admin-api-reference)
- [13. Deployment](#13-deployment)
- [14. Configuration reference](#14-configuration-reference)
- [15. Troubleshooting](#15-troubleshooting)

---

## 1. What it does

| Capability | Detail |
|---|---|
| **Transparent MITM** | Full visibility of every request/response; streams large objects without buffering. |
| **Statistics** | Per-op / per-status counters, bytes in/out, latency p50/p90/p99, live request feed. |
| **Error injection** | Filter by key / op; probability; any status (`500`, `503`, `429`, …) with a byte-exact R2 error body + `Retry-After`; latency injection. |
| **Simple auth** | One proxy access key + secret for S3 clients; one admin token for the console. |
| **UI + CLI** | A web console and an `r2proxy` CLI, both over the admin API. |
| **Trivial deploy** | One static Go binary (UI embedded, no runtime deps); or Docker; or one command to an Ubicloud VM. |

Ports: **8080** = S3 data-plane proxy · **8081** = admin API + web console. It
proxies a **single** upstream target.

---

## 2. Architecture: why re-signing

S3 requests are SigV4-signed, and the signature covers the **Host** header. A
plain TCP forward would therefore fail signature validation at R2 (the client
signed for the proxy's host, not R2's). r2proxy instead **terminates** each
request and **re-originates** it:

```
                 SigV4(proxy creds)              re-SigV4(real R2 creds)
   your app  ───────────────────────▶  r2proxy  ───────────────────────▶  R2
                                          │
                          verify signature · classify op
                          statistics · error injection
```

1. **Authenticate** — check the caller's access key against the single proxy
   access key, then **recompute and compare their SigV4 signature** against the
   proxy secret. This proves possession of the secret; the proxy is not an open
   relay to your R2.
2. **Classify** — derive `(op, bucket, key)` from method + path + query.
3. **Inject** — consult the rules; maybe short-circuit with an error or add latency.
4. **Re-sign & forward** — strip the caller's auth headers, sign fresh with the
   real R2 credentials (payload `UNSIGNED-PAYLOAD`, so bodies stream), send to
   R2, and relay the response back verbatim.

`aws-chunked` streaming uploads (used by the AWS CLI for large PUTs) are decoded
back to the raw object bytes before re-signing, so all clients upload correctly.

---

## 3. Install & build

Requirements: Go 1.22+ (build only). The binary has **no runtime dependencies**
and embeds the web UI.

```bash
make build      # -> dist/r2proxy         (host binary)
make static     # -> dist/r2proxy         (static linux/amd64)
make docker     # -> r2proxy:latest image
```

---

## 4. Running the proxy

```bash
R2PROXY_ENDPOINT=https://ACCOUNT.r2.cloudflarestorage.com \
R2PROXY_ACCESS_KEY=<r2 access key id> \
R2PROXY_SECRET_KEY=<r2 secret access key> \
./dist/r2proxy serve
```

First-run output — the proxy credentials and admin token are generated and
persisted (shown on every start):

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

State (proxy creds, admin token, rules, upstream target) persists to `--config`
(default `r2proxy.json`, mode `0600`). Live statistics are in-memory and reset on
restart. On later runs you don't need the `--endpoint/--access-key/--secret-key`
flags — they're read from the config.

---

## 5. Pointing your client at it

Use **path-style** addressing, the proxy URL, and the **proxy** access key +
secret (not your real R2 keys). Region is `auto`.

### AWS CLI (works as-is)

```bash
export AWS_ACCESS_KEY_ID=<proxy access key>
export AWS_SECRET_ACCESS_KEY=<proxy secret key>
export AWS_DEFAULT_REGION=auto
aws s3 cp ./file s3://test/file --endpoint-url http://HOST:8080
aws s3 ls s3://test/ --endpoint-url http://HOST:8080
```

### boto3 (Python)

```python
import boto3
from botocore.config import Config
s3 = boto3.client(
    "s3",
    endpoint_url="http://HOST:8080",
    aws_access_key_id="<proxy access key>",
    aws_secret_access_key="<proxy secret key>",
    region_name="auto",
    config=Config(s3={"addressing_style": "path"}),
)
s3.put_object(Bucket="test", Key="k", Body=b"hi")
```

### AWS SDK for JS v3

```js
import { S3Client } from "@aws-sdk/client-s3";
const s3 = new S3Client({
  endpoint: "http://HOST:8080",
  region: "auto",
  forcePathStyle: true,
  credentials: { accessKeyId: "<proxy access key>", secretAccessKey: "<proxy secret key>" },
});
```

The proxy speaks plain HTTP (it re-signs to R2 over HTTPS). If you need HTTPS on
the client side, put a TLS terminator (Caddy/nginx) in front of `:8080`.

---

## 6. Statistics & load testing

```bash
export R2PROXY_ADMIN=http://HOST:8081
export R2PROXY_TOKEN=<admin token>
r2proxy stats            # summary
r2proxy stats --json     # raw
r2proxy tail             # live request feed
```

Collected: total requests, req/s, in-flight, injected count, error count, bytes
in/out, latency p50/p90/p99/avg, breakdowns by op and status, and a ring buffer
of the last 200 requests (shown live in the console).

### Load testing (`loadtest.py`)

`loadtest.py` PUTs many objects through the proxy concurrently and reports total
duration and throughput. Needs `boto3`.

```bash
export AWS_ACCESS_KEY_ID=<proxy access key>
export AWS_SECRET_ACCESS_KEY=<proxy secret key>
python3 loadtest.py --endpoint http://HOST:8080 \
  --count 1000 --size-mb 1 --concurrency 32 --cleanup
```

| Flag | Default | Meaning |
|---|---|---|
| `--endpoint` | `http://144.76.134.100:8080` | proxy data-plane URL |
| `--bucket` | `test` | target bucket |
| `--count` | `1000` | number of objects |
| `--size-mb` | `1` | size of each object (MiB) |
| `--concurrency` | `32` | parallel uploads (also sizes the connection pool) |
| `--prefix` | `load/` | key prefix |
| `--cleanup` | off | delete the objects afterward |

Output reports objects ok/failed, total MB, wall-clock duration, `obj/s`, `MB/s`,
and average wall-time per object. Raise `--concurrency` for throughput.

---

## 7. Error injection

A rule matches on `op` (comma list; blank = any) and `key` glob; fires with
`probability` (0–1); then injects an S3 error (`status` + `code` + `message` +
`retry_after`) and/or `delay_ms` of latency. Rules are evaluated top-to-bottom;
the first **error** rule that fires wins (latency-only rules stack and continue).

```bash
# 30% of GetObjects on keys under photos/ fail with 503:
r2proxy rules add --op GetObject --key 'photos/*' --status 503 --prob 0.3

# R2's real 429 same-object throttle (byte-exact):
r2proxy rules add --op GetObject --status 429

# Inject 500ms of latency on everything:
r2proxy rules add --op '*' --delay 500

r2proxy rules list
r2proxy rules toggle <id>
r2proxy rules rm <id>
r2proxy rules clear
```

Glob syntax is Go `path.Match` (`*`, `?`, `[abc]`). `code`, `message`, and
`retry_after` are optional — left blank, r2proxy fills in R2-accurate defaults
for the status (see next section).

---

## 8. Matching R2 errors exactly (429 verified)

Injected errors are written in R2's **exact** wire format so they're
indistinguishable from ones R2 actually returns: a single-line, minimal XML body
(`Code` + `Message` only — no `<Resource>`/`<RequestId>`), `Content-Type:
application/xml`, `Server: cloudflare`, and a `Retry-After` header for throttles.

The **429** case was reproduced against real R2 by hammering a single object
concurrently. R2 returns (verified byte-for-byte):

```
HTTP/1.1 429 Too Many Requests
Content-Type: application/xml
Retry-After: 5
Server: cloudflare
Content-Length: 159

<?xml version="1.0" encoding="UTF-8"?><Error><Code>ServiceUnavailable</Code><Message>Reduce your concurrent request rate for the same object.</Message></Error>
```

Note R2's 429 uses code **`ServiceUnavailable`** (not `SlowDown`/
`TooManyRequests`) with that message and `Retry-After: 5`. A bare `--status 429`
reproduces all of it; the injected 429 is a 159-byte, byte-identical match, and
SDKs parse it exactly as they parse R2's.

R2-accurate defaults filled in when you omit fields:

| status | code | message | Retry-After |
|---|---|---|---|
| 429 | `ServiceUnavailable` | Reduce your concurrent request rate for the same object. | 5 |
| 503 | `ServiceUnavailable` | Please reduce your request rate. | 1 |
| 500 | `InternalError` | We encountered an internal error. Please try again. | — |
| 504 | `GatewayTimeout` | The gateway timed out. | — |
| 404 | `NoSuchKey` | The specified key does not exist. | — |
| 403 | `AccessDenied` | Access Denied. | — |

Override any with `--code`, `--message`, `--retry-after`. **Real** R2 throttles
(when you actually exceed R2's limits through the proxy) pass through untouched
with R2's own headers.

> Reproduce R2's real 429 yourself: PUT the same key from ~48 threads in a tight
> loop; R2 starts returning `429 ServiceUnavailable / Retry-After: 5` within a
> few hundred requests.

---

## 9. Security

- **Data plane:** every request must be SigV4-signed with the proxy secret.
  r2proxy recomputes and compares the signature (constant-time), so holding only
  the proxy *access key* (not secret) is useless — the proxy won't relay to your
  R2. Failure modes mirror S3: unknown key → `403 InvalidAccessKeyId`; bad
  signature → `403 SignatureDoesNotMatch`.
- **Control plane:** the console/API require the single admin token (bearer, via
  `Authorization: Bearer`, `X-Admin-Token`, or `?token=`).
- The **real R2 credentials never leave the proxy** — clients present only the
  proxy creds; the proxy re-signs with the real keys.
- Secrets persist to `--config` (mode `0600`). Rotate by deleting the
  `proxy_access_key_id` / `proxy_secret_key` / `admin_token` fields and
  restarting (they regenerate), or set `--admin-token` to pin a known token.
- **Network posture:** the data-plane port (`8080`) is safe to expose — it's
  authenticated by SigV4. Restrict the admin port (`8081`) to trusted IPs; the
  deploy script can lock it to your IP (`LOCK_ADMIN_TO_MY_IP=true`).

---

## 10. Web console

Open `http://HOST:8081` and log in with the admin token. The dashboard shows:

- **Connection** — the proxy URL, access key, secret, target endpoint, region
  (copy-to-clipboard) so you can configure your client.
- **Statistics** — stat cards, by-op and by-status tables, live request feed.
- **Error injection** — the rule editor with presets (including the verified 429).

The UI polls every 2s. Everything it does is available over the API/CLI too.

---

## 11. CLI reference

The CLI talks to the admin API. Configure with env:

```bash
export R2PROXY_ADMIN=http://HOST:8081     # default http://127.0.0.1:8081
export R2PROXY_TOKEN=<admin token>
```

```
r2proxy serve   [flags]                    run proxy + admin API + console
r2proxy stats   [--json]                   statistics
r2proxy tail                               live request feed
r2proxy rules   list
r2proxy rules   add --op O --key K --prob P --status S \
                    --code C --message M --retry-after N --delay MS
r2proxy rules   toggle <id> | rm <id> | clear
r2proxy version
```

---

## 12. Admin API reference

Bearer-token auth (`Authorization: Bearer <token>`, or `?token=`, or
`X-Admin-Token`) — the single admin token. `/api/serverinfo` is public.

| Method & path | Purpose |
|---|---|
| `GET /api/serverinfo` | version, proxy port (public) |
| `GET /api/info` | connection info: proxy access key + secret, endpoint host, region |
| `GET /api/templates` | error presets |
| `GET /api/stats` | statistics snapshot |
| `POST /api/stats/reset` | reset statistics |
| `GET /api/recent` | recent requests (newest first) |
| `GET /api/rules` | list rules |
| `POST /api/rules` | add rule |
| `DELETE /api/rules` | clear rules |
| `DELETE /api/rules/{id}` | delete rule |
| `POST /api/rules/{id}/toggle` | enable/disable rule |

---

## 13. Deployment

### Ubicloud (one command)

```bash
cp deploy.env.example deploy.env      # fill in UBI_TOKEN + R2 creds + settings
./deploy.sh up                        # provision VM + firewall, build, install, start
./deploy.sh info                      # print URLs + credentials
./deploy.sh push                      # rebuild binary + swap in + restart (redeploy)
./deploy.sh logs                      # tail service logs
./deploy.sh ssh                       # shell on the VM
./deploy.sh destroy                   # tear down VM + subnet + firewall
```

`deploy.sh` builds a static binary, creates a private subnet + firewall (opens
`8080` to the world — it's authenticated; opens SSH + `8081` to your IP when
`LOCK_ADMIN_TO_MY_IP=true`), uploads the binary, and installs a `systemd`
service (`Restart=always`, `CAP_NET_BIND_SERVICE`). Redeploy to a **fresh**
server by changing `DEPLOY_NAME` / `DEPLOY_LOCATION` and running `up` again.

### Docker (anywhere)

```bash
docker compose up -d      # reads R2PROXY_* from your environment / .env
```

State persists in the `r2proxy-data` volume. The image is
`distroless/static:nonroot`.

### Bare binary + systemd (anywhere)

It's a single static binary — copy `dist/r2proxy`, drop in an env file and a unit
(exactly what `deploy.sh` installs), `systemctl enable --now r2proxy`.

---

## 14. Configuration reference

`serve` flags (each has an env fallback):

| Flag | Env | Default | Purpose |
|---|---|---|---|
| `--listen` | `R2PROXY_LISTEN` | `0.0.0.0:8080` | data-plane address |
| `--admin-listen` | `R2PROXY_ADMIN_LISTEN` | `0.0.0.0:8081` | admin API + console |
| `--config` | `R2PROXY_CONFIG` | `r2proxy.json` | state file (0600) |
| `--admin-token` | `R2PROXY_ADMIN_TOKEN` | *(generated)* | admin token |
| `--endpoint` | `R2PROXY_ENDPOINT` | — | upstream URL |
| `--access-key` | `R2PROXY_ACCESS_KEY` / `AWS_ACCESS_KEY_ID` | — | upstream key |
| `--secret-key` | `R2PROXY_SECRET_KEY` / `AWS_SECRET_ACCESS_KEY` | — | upstream secret |
| `--region` | `R2PROXY_REGION` | `auto` | region |

The upstream target is required on first run (via flags/env); afterwards it's
read from the config file. Flags/env override the stored values when present.

---

## 15. Troubleshooting

| Symptom | Cause / fix |
|---|---|
| `SignatureDoesNotMatch` | Client is using the wrong proxy secret, or not path-style. Set `forcePathStyle`/`addressing_style: path` and the proxy secret. |
| `InvalidAccessKeyId` | Client's access key isn't the proxy access key. Check the startup banner or `GET /api/info`. |
| Uploads fail only for large files | Ensure you're on a current build — `aws-chunked` streaming uploads are decoded before re-signing. |
| Can't reach the console remotely | Admin port firewalled to your IP (`LOCK_ADMIN_TO_MY_IP`). Open it or use an SSH tunnel: `ssh -L 8081:localhost:8081 ...`. |
| Stats reset after redeploy | Expected — live stats are in-memory; proxy creds/token/rules persist on disk. |
