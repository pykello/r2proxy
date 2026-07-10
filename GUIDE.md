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
- [6. Statistics](#6-statistics)
- [7. Error injection](#7-error-injection)
- [8. Matching R2 errors exactly (429 verified)](#8-matching-r2-errors-exactly-429-verified)
- [9. Multi-tenancy & security](#9-multi-tenancy--security)
- [10. Web console](#10-web-console)
- [11. CLI reference](#11-cli-reference)
- [12. Admin API reference](#12-admin-api-reference)
- [13. Deployment](#13-deployment)
- [14. Operating the live instance](#14-operating-the-live-instance)
- [15. Configuration reference](#15-configuration-reference)
- [16. Troubleshooting](#16-troubleshooting)

---

## 1. What it does

| Capability | Detail |
|---|---|
| **Transparent MITM** | Full visibility of every request/response; streams large objects without buffering. |
| **Statistics** | Per-op / per-bucket / per-status counters, bytes in/out, latency p50/p90/p99, live request feed. |
| **Error injection** | Filter by bucket / key / op; probability; any status (`500`, `503`, `429`, …) with a real S3 error body + `Retry-After`; latency injection; fire-count limits. |
| **Multi-tenant + secure** | Each user gets isolated proxy credentials, upstream target, stats, rules, and a scoped console token. Tenants cannot see or use each other's credentials or data. |
| **UI + CLI** | A web console and an `r2proxy` CLI, both over the admin API. |
| **Trivial deploy** | One static Go binary (UI embedded, no runtime deps); or Docker; or one command to an Ubicloud VM. |

Ports: **8080** = S3 data-plane proxy · **8081** = admin API + web console.

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
                          stats · error injection · isolation
```

1. **Authenticate** — parse the caller's access key, look up the tenant, and
   **recompute and compare their SigV4 signature** against the tenant's proxy
   secret. This is real authentication, not a key-id match.
2. **Classify** — derive `(op, bucket, key)` from method + path + query.
3. **Inject** — consult the tenant's rules; maybe short-circuit with an error or
   add latency.
4. **Re-sign & forward** — strip the caller's auth headers, sign fresh with the
   tenant's real R2 credentials (payload `UNSIGNED-PAYLOAD`, so bodies stream),
   send to R2, and relay the response back verbatim.

`aws-chunked` streaming uploads (used by the AWS CLI for large PUTs) are decoded
back to the raw object bytes before re-signing, so all clients upload correctly.

---

## 3. Install & build

Requirements: Go 1.22+ (build only). The binary itself has **no runtime
dependencies** and embeds the web UI.

```bash
make build      # -> dist/r2proxy         (host binary)
make static     # -> dist/r2proxy         (static linux/amd64)
make docker     # -> r2proxy:latest image
```

---

## 4. Running the proxy

Bootstrap a "default" tenant from your R2 credentials on first run:

```bash
R2PROXY_ENDPOINT=https://ACCOUNT.r2.cloudflarestorage.com \
R2PROXY_ACCESS_KEY=<r2 access key id> \
R2PROXY_SECRET_KEY=<r2 secret access key> \
./dist/r2proxy serve
```

First-run output (store these — secrets are shown once):

```
generated global admin token (store it): 8437....
┌─ tenant credentials (shown once) ───────────────────────
│ tenant id        t_e045b3...
│ proxy access key b4733a9b180c...
│ proxy secret key 32b8c49395ed...
│ tenant token     6a85b23c9a7e...
│ upstream         https://ACCOUNT.r2.cloudflarestorage.com
└─────────────────────────────────────────────────────────
proxy (data-plane) listening on 0.0.0.0:8080
admin API + UI listening on http://0.0.0.0:8081
```

State (tenants, tokens, rules) persists to `--config` (default `r2proxy.json`,
mode `0600`). Live statistics are in-memory and reset on restart.

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

## 6. Statistics

```bash
export R2PROXY_ADMIN=http://HOST:8081
export R2PROXY_TOKEN=<tenant or admin token>
r2proxy stats            # summary
r2proxy stats --json     # raw
r2proxy tail             # live request feed
```

Collected per tenant: total requests, req/s, in-flight, injected count, error
count, bytes in/out, latency p50/p90/p99/avg, breakdowns by op / bucket / status,
and a ring buffer of the last 200 requests (shown live in the console).

### Load testing (`loadtest.py`)

`loadtest.py` PUTs many objects through the proxy concurrently and reports total
duration and throughput — useful for exercising the stats and seeing real
latency percentiles. It needs `boto3` (`pip install boto3`).

```bash
export AWS_ACCESS_KEY_ID=<proxy access key>
export AWS_SECRET_ACCESS_KEY=<proxy secret key>
# 1000 objects of 1 MiB (~1 GB) at concurrency 32, then delete them:
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
| `--prefix` | `load/` | key prefix (`load/obj-00001.bin`, …) |
| `--cleanup` | off | delete the objects afterward |

Output reports objects ok/failed, total MB, wall-clock duration, `obj/s`, `MB/s`,
and average wall-time per object. Concurrency matters: serial uploads pay the
full proxy→R2 round-trip per object, so raise `--concurrency` for throughput.
After a run, `r2proxy stats` shows the recorded PutObjects, bytes, and latency.

---

## 7. Error injection

A rule matches on `op` (comma list; blank = any), `bucket` glob, and `key` glob;
fires with `probability` (0–1); then injects an S3 error (`status` + `code` +
`message` + `retry_after`) and/or `delay_ms` of latency. `count` limits how many
times it fires (`-1` = unlimited). Rules are evaluated top-to-bottom; the first
**error** rule that fires wins (latency-only rules stack and continue).

```bash
# 30% of GetObjects on bucket "test" fail with R2's real 429 throttle:
r2proxy rules add --op GetObject --bucket test --status 429 --prob 0.3

# Inject 500ms of latency on everything:
r2proxy rules add --op '*' --delay 500

# Fail the next 3 PUTs with 503, then stop:
r2proxy rules add --op PutObject --status 503 --count 3

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
`TooManyRequests`) with that specific message and `Retry-After: 5`. A bare
`--status 429` reproduces all of it automatically; r2proxy's injected 429 is a
159-byte, byte-identical match, and SDKs parse it exactly as they parse R2's
(`Code=ServiceUnavailable`, `HTTP 429`, `Retry-After=5`).

R2-accurate defaults filled in when you omit fields:

| status | code | message | Retry-After |
|---|---|---|---|
| 429 | `ServiceUnavailable` | Reduce your concurrent request rate for the same object. | 5 |
| 503 | `ServiceUnavailable` | Please reduce your request rate. | 1 |
| 500 | `InternalError` | We encountered an internal error. Please try again. | — |
| 504 | `GatewayTimeout` | The gateway timed out. | — |
| 404 | `NoSuchKey` | The specified key does not exist. | — |
| 403 | `AccessDenied` | Access Denied. | — |

Override any of them with `--code`, `--message`, `--retry-after`. **Real** R2
throttles (when you actually exceed R2's limits through the proxy) pass through
untouched, with R2's own headers.

> Reproduce R2's real 429 yourself: PUT the same key from ~48 threads in a tight
> loop; R2 starts returning `429 ServiceUnavailable / Retry-After: 5` within a
> few hundred requests.

---

## 9. Multi-tenancy & security

**Data plane.** Every request must be SigV4-signed with the tenant's **proxy
secret**. r2proxy recomputes and compares the signature (constant-time), so
holding only a proxy *access key* (not secret) is useless — you cannot use
another tenant's upstream. Failure modes mirror S3:

- unknown access key → `403 InvalidAccessKeyId`
- bad signature → `403 SignatureDoesNotMatch`
- stale clock (>15 min skew) → `403 RequestTimeTooSkewed`

**Control plane.** The console/API require a bearer token:

- a **tenant token** sees and manages only that tenant's stats and rules;
- the **global admin token** manages tenants.

Upstream and proxy **secrets are never returned** by the API — they are shown
exactly once, at tenant creation. A tenant token passing `?tenant=<other>` is
ignored; it only ever resolves to its own tenant.

**Per-tenant bucket allowlist** (optional) restricts which buckets a tenant may
touch (`403 AccessDenied` otherwise).

Create an isolated tenant per user:

```bash
export R2PROXY_ADMIN=http://HOST:8081
export R2PROXY_TOKEN=<global admin token>
r2proxy tenant add --name alice \
  --endpoint https://ACCOUNT.r2.cloudflarestorage.com \
  --access-key <alice's R2 key> --secret-key <alice's R2 secret> \
  --buckets alice-bucket        # optional allowlist
# -> prints alice's proxy access key, proxy secret, and tenant token (once)
```

Hand Alice her proxy access key + secret (for her S3 client) and her tenant token
(for the console). She never sees other tenants, their creds, or their traffic.

**Network posture.** The data-plane port (`8080`) is safe to expose — it's
authenticated by SigV4. Restrict the admin port (`8081`) to trusted IPs; the
deploy script can lock it to your IP (`LOCK_ADMIN_TO_MY_IP=true`).

---

## 10. Web console

Open `http://HOST:8081` and log in with a token.

- **Tenant token** → that tenant's dashboard only: connection info, stats cards,
  by-op/by-status tables, live request feed, and the error-injection rule editor
  (with error presets, including the verified 429).
- **Global admin token** → everything above plus a **Tenants** panel: create
  tenants (secrets shown once), list them with their proxy access key + tenant
  token, switch which tenant you're viewing, and delete tenants.

The UI polls every 2s. Everything it does is available over the API/CLI too.

---

## 11. CLI reference

The CLI talks to the admin API. Configure with env:

```bash
export R2PROXY_ADMIN=http://HOST:8081     # default http://127.0.0.1:8081
export R2PROXY_TOKEN=<admin or tenant token>
export R2PROXY_TENANT=<tenant id>          # only with the global admin token
```

```
r2proxy serve   [flags]                    run proxy + admin API + console
r2proxy stats   [--json]                   tenant statistics
r2proxy tail                               live request feed
r2proxy rules   list
r2proxy rules   add --op O --bucket B --key K --prob P \
                    --status S --code C --message M --retry-after N \
                    --delay MS --count N
r2proxy rules   toggle <id> | rm <id> | clear
r2proxy tenant  list                       (superuser)
r2proxy tenant  add --name N --endpoint U --access-key K --secret-key S \
                    --region auto --buckets csv
r2proxy tenant  rm <id>
r2proxy version
```

---

## 12. Admin API reference

Bearer-token auth (`Authorization: Bearer <token>`, or `?token=`, or
`X-Admin-Token`). Tenant-scoped routes use the token's tenant; a superuser
selects one with `?tenant=<id>`.

| Method & path | Scope | Purpose |
|---|---|---|
| `GET /api/serverinfo` | public | version, proxy port |
| `GET /api/me` | any | who am I (super / tenant) |
| `GET /api/templates` | any | error presets |
| `GET /api/tenants` | super | list tenants (no secrets) |
| `POST /api/tenants` | super | create tenant (secrets once) |
| `DELETE /api/tenants/{id}` | super | delete tenant |
| `GET /api/info` | scoped | tenant connection info (no secrets) |
| `GET /api/stats` | scoped | statistics snapshot |
| `POST /api/stats/reset` | scoped | reset statistics |
| `GET /api/recent` | scoped | recent requests (newest first) |
| `GET /api/rules` | scoped | list rules |
| `POST /api/rules` | scoped | add rule |
| `DELETE /api/rules` | scoped | clear rules |
| `DELETE /api/rules/{id}` | scoped | delete rule |
| `POST /api/rules/{id}/toggle` | scoped | enable/disable rule |

---

## 13. Deployment

### Ubicloud (one command)

```bash
cp deploy.env.example deploy.env      # fill in UBI_TOKEN + R2 creds + settings
./deploy.sh up                        # provision VM + firewall, build, install, start
./deploy.sh info                      # print URLs + first-run credentials
./deploy.sh push                      # rebuild binary + swap in + restart (redeploy)
./deploy.sh logs                      # tail service logs
./deploy.sh ssh                       # shell on the VM
./deploy.sh destroy                   # tear down VM + subnet + firewall
```

`deploy.sh` builds a static binary, creates a private subnet + firewall (opens
`8080` to the world — it's authenticated; opens SSH + `8081` to your IP when
`LOCK_ADMIN_TO_MY_IP=true`), uploads the binary, and installs a `systemd`
service (`Restart=always`, `CAP_NET_BIND_SERVICE`). **Redeploy to a fresh
server** by changing `DEPLOY_NAME` / `DEPLOY_LOCATION` and running `up` again —
each name is an independent instance.

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

## 14. Operating the live instance

A test instance is currently deployed on Ubicloud:

- **Proxy (point your app here):** `http://144.76.134.100:8080`
- **Console + API:** `http://144.76.134.100:8081`
- Credentials are in `./deploy.sh info` (read from the VM's service logs).

```bash
./deploy.sh info        # URLs + admin token + default tenant creds
./deploy.sh logs        # follow logs
./deploy.sh push        # ship a new build
./deploy.sh destroy     # stop billing when done
```

> The VM bills until `./deploy.sh destroy`. The admin port is currently open;
> set `LOCK_ADMIN_TO_MY_IP=true` in `deploy.env` and re-run `./deploy.sh up` to
> restrict it (the console token still protects it either way).

---

## 15. Configuration reference

`serve` flags (each has an env fallback):

| Flag | Env | Default | Purpose |
|---|---|---|---|
| `--listen` | `R2PROXY_LISTEN` | `0.0.0.0:8080` | data-plane address |
| `--admin-listen` | `R2PROXY_ADMIN_LISTEN` | `0.0.0.0:8081` | admin API + console |
| `--config` | `R2PROXY_CONFIG` | `r2proxy.json` | state file (0600) |
| `--admin-token` | `R2PROXY_ADMIN_TOKEN` | *(generated)* | global admin token |
| `--endpoint` | `R2PROXY_ENDPOINT` | — | bootstrap: upstream URL |
| `--access-key` | `R2PROXY_ACCESS_KEY` / `AWS_ACCESS_KEY_ID` | — | bootstrap: upstream key |
| `--secret-key` | `R2PROXY_SECRET_KEY` / `AWS_SECRET_ACCESS_KEY` | — | bootstrap: upstream secret |
| `--region` | `R2PROXY_REGION` | `auto` | bootstrap: region |

Bootstrap only fires on first run when no tenants exist. Afterwards, manage
tenants via the API/CLI/console.

---

## 16. Troubleshooting

| Symptom | Cause / fix |
|---|---|
| `SignatureDoesNotMatch` | Client is using the wrong proxy secret, or not path-style. Set `forcePathStyle`/`addressing_style: path` and the tenant's proxy secret. |
| `InvalidAccessKeyId` | Client's access key isn't a known tenant proxy key. Check `r2proxy tenant list`. |
| `RequestTimeTooSkewed` | Client clock is off by >15 min. Fix the clock. |
| Uploads fail only for large files | Ensure you're on a current build — `aws-chunked` streaming uploads are decoded before re-signing. |
| Bucket returns `AccessDenied` for everything | Tenant has a `bucket_allowlist` that excludes it. |
| Can't reach the console remotely | Admin port firewalled to your IP (`LOCK_ADMIN_TO_MY_IP`). Open it or use an SSH tunnel: `ssh -L 8081:localhost:8081 ...`. |
| Stats reset after redeploy | Expected — live stats are in-memory; tenants/tokens/rules persist on disk. |
