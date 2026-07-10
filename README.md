# r2proxy

A man-in-the-middle proxy for Cloudflare R2 (and any S3-compatible endpoint),
built for **testing**. Your program talks to r2proxy instead of R2; r2proxy
records statistics, optionally injects errors/latency, and otherwise transparently
re-signs and forwards every request to the real backend.

- **Transparent MITM** — full request/response visibility; streams large objects.
- **Statistics** — per-op / per-bucket / per-status counters, bytes, latency
  percentiles, live request feed.
- **Error injection** — filter by bucket / key / op, probability, any status
  (`500`, `503`, `429`, …) with a real S3 error body, plus latency injection.
- **Multi-tenant + secure** — each user gets isolated proxy credentials, upstream
  target, stats, rules, and a scoped console token. Tenants can't see or use each
  other's credentials or data.
- **UI + CLI** — a web console and a `r2proxy` CLI, both over the admin API.
- **Trivial deploy** — one static Go binary (UI embedded), or Docker, or one
  command to an Ubicloud VM.

## How it works

S3 requests are SigV4-signed against the *host*, so a plain TCP forward would fail
signature validation at R2. Instead r2proxy **terminates** each request,
**verifies** the caller's signature against their tenant's proxy secret (real
authentication), then **re-signs** with that tenant's real R2 credentials toward
the R2 endpoint:

```
your app ──SigV4(proxy creds)──▶ r2proxy ──re-SigV4(real R2 creds)──▶ R2
                                    │
                         stats · error injection · isolation
```

Point your S3 client at the proxy with **path-style** addressing and the tenant's
proxy access key + secret. r2proxy does the rest.

## Quick start (local)

```bash
make build      # -> dist/r2proxy  (single binary, UI embedded)

# Run with a bootstrap "default" tenant created from your R2 creds:
R2PROXY_ENDPOINT=https://ACCOUNT.r2.cloudflarestorage.com \
R2PROXY_ACCESS_KEY=... R2PROXY_SECRET_KEY=... \
./dist/r2proxy serve
```

On first run it prints a **global admin token** and the default tenant's
**proxy access key / secret / tenant token** (shown once). Then:

```bash
# Point any S3 client at the proxy, path-style, with the PROXY creds:
export AWS_ACCESS_KEY_ID=<proxy access key>
export AWS_SECRET_ACCESS_KEY=<proxy secret key>
export AWS_DEFAULT_REGION=auto
aws s3 cp ./file s3://test/file --endpoint-url http://localhost:8080
```

Open the console at **http://localhost:8081** and log in with the admin token (or a
tenant token). Ports: **8080** = S3 data-plane, **8081** = admin API + console.

## CLI

The CLI talks to the admin API. Configure it with env:

```bash
export R2PROXY_ADMIN=http://HOST:8081
export R2PROXY_TOKEN=<admin or tenant token>
# export R2PROXY_TENANT=<tenant id>   # only when using the global admin token

r2proxy stats                 # tenant statistics
r2proxy tail                  # live request feed
r2proxy rules list
r2proxy rules add --op GetObject --bucket 'test' --status 503 --code SlowDown --prob 0.3
r2proxy rules add --op '*' --delay 500          # inject 500ms latency on everything
r2proxy rules toggle <id> | rm <id> | clear
r2proxy tenant list                              # (superuser token)
r2proxy tenant add --name team-b --endpoint https://... --access-key ... --secret-key ...
r2proxy tenant rm <id>
```

## Error injection

A rule matches on `op` (comma list, blank = any), `bucket` glob, and `key` glob,
fires with `probability` (0–1), and then injects an S3 error (`status` + `code` +
`message`) and/or `delay_ms` of latency. `count` limits how many times it fires
(`-1` = unlimited). Rules are evaluated top-to-bottom; the first error rule that
fires wins. Manage them in the console or via `r2proxy rules`.

## Multi-tenancy & security

- **Data plane:** every request must be SigV4-signed with the tenant's **proxy
  secret**. r2proxy recomputes and compares the signature, so possessing only a
  proxy *access key* (which is not secret) is not enough — you can't use another
  tenant's upstream. Unknown key → `InvalidAccessKeyId`; bad signature →
  `SignatureDoesNotMatch`; stale clock → `RequestTimeTooSkewed`.
- **Control plane:** the console/API require a bearer token. A **tenant token**
  sees and manages only that tenant's stats and rules. The **global admin token**
  manages tenants. Upstream and proxy **secrets are never returned** by the API —
  they are shown exactly once, at tenant creation.
- **Optional bucket allowlist** per tenant.
- State (tenants, tokens, rules) is persisted to `--config` (default
  `r2proxy.json`, mode `0600`); live stats are in-memory and reset on restart.

Create an isolated tenant per user:

```bash
r2proxy tenant add --name alice \
  --endpoint https://ACCOUNT.r2.cloudflarestorage.com \
  --access-key <alice R2 key> --secret-key <alice R2 secret>
# -> returns alice's proxy access key, proxy secret, and tenant token (once)
```

## Deploy

### Ubicloud (one command)

```bash
cp deploy.env.example deploy.env      # fill in UBI_TOKEN + R2 creds
./deploy.sh up                        # provision VM + firewall, build, install, start
./deploy.sh info                      # print URLs + first-run credentials
./deploy.sh push                      # rebuild the binary + restart (redeploy)
./deploy.sh logs                      # tail service logs
./deploy.sh destroy                   # tear it all down
```

`deploy.sh` builds a static binary, creates a private subnet + firewall (data-plane
`8080` open — it's authenticated; SSH + admin `8081` lockable to your IP via
`LOCK_ADMIN_TO_MY_IP=true`), and installs a `systemd` service. Redeploy to a
**fresh** server by changing `DEPLOY_NAME`/`DEPLOY_LOCATION` and running `up` again.

### Docker (anywhere)

```bash
docker compose up -d          # reads R2PROXY_* from your env / .env
# data persists in the r2proxy-data volume
```

### Anywhere else

It's a single static binary with no runtime dependencies — copy `dist/r2proxy`,
run `r2proxy serve` behind a `systemd` unit (see the one `deploy.sh` installs).

## Commands

```
r2proxy serve    [--listen :8080] [--admin-listen :8081] [--config r2proxy.json]
                 [--admin-token T] [--endpoint URL --access-key K --secret-key S --region auto]
r2proxy stats    [--json]
r2proxy tail
r2proxy rules    list | add [...] | rm <id> | toggle <id> | clear
r2proxy tenant   list | add [...] | rm <id>
r2proxy version
```

## Notes

- Configure clients for **path-style** addressing (boto3:
  `s3={'addressing_style':'path'}`; aws-sdk: `forcePathStyle: true`). The AWS CLI
  works as-is.
- `aws-chunked` streaming uploads are decoded before re-signing, so the AWS CLI
  and all SDKs upload correctly.
- Talk to the proxy over plain HTTP (it re-signs to R2 over HTTPS). Put it behind a
  TLS terminator if you need HTTPS on the client side.
