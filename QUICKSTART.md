# r2proxy — Quick Start

An S3/R2 man-in-the-middle proxy for testing: your app talks to r2proxy instead
of R2, so you get request **stats** and on-demand **error/latency injection**.
It transparently re-signs every request to the real backend.

Ports: **8080** = S3 proxy · **8081** = console + admin API.

## 1. Build & run

```bash
make build

R2PROXY_ENDPOINT=https://ACCOUNT.r2.cloudflarestorage.com \
R2PROXY_ACCESS_KEY=<r2 key> R2PROXY_SECRET_KEY=<r2 secret> \
./dist/r2proxy serve
```

First run prints (and saves to `r2proxy.json`) the **proxy access key + secret**
for your S3 client and the **admin token** for the console.

## 2. Point your client at it

Use **path-style** addressing and the *proxy* creds (not your real R2 keys):

```bash
export AWS_ACCESS_KEY_ID=<proxy access key>
export AWS_SECRET_ACCESS_KEY=<proxy secret key>
export AWS_DEFAULT_REGION=auto
aws s3 cp ./file s3://test/file --endpoint-url http://localhost:8080
```

boto3: `config=Config(s3={"addressing_style":"path"})` · JS SDK: `forcePathStyle: true`.

## 3. Console & stats

Open <http://localhost:8081> and log in with the admin token. Or via CLI:

```bash
export R2PROXY_ADMIN=http://localhost:8081
export R2PROXY_TOKEN=<admin token>
r2proxy stats          # counters, bytes, latency percentiles
r2proxy tail           # live request feed
```

## 4. Inject errors

```bash
r2proxy rules add --op GetObject --status 503 --prob 0.3   # 30% of GETs fail 503
r2proxy rules add --op GetObject --status 429              # R2's exact same-object throttle
r2proxy rules add --op '*' --delay 500                     # 500ms latency on everything
r2proxy rules list | toggle <id> | rm <id> | clear
```

Leave `--code`/`--message`/`--retry-after` blank and r2proxy fills in
R2-accurate defaults for the status.

## 5. Deploy

```bash
cp deploy.env.example deploy.env      # fill UBI_TOKEN + R2 creds
./deploy.sh up                        # provision Ubicloud VM + build + start
./deploy.sh info | push | destroy     # creds | redeploy | tear down
```

Or `docker compose up -d`, or copy the static `dist/r2proxy` binary anywhere.

## Repoint to another R2/S3 backend

Edit `endpoint` / `upstream_access_key_id` / `upstream_secret_key` in
`r2proxy.json` (or pass `--endpoint`/`--access-key`/`--secret-key`), then restart
(`sudo systemctl restart r2proxy` on a deployed VM).

## Load testing

```bash
python3 loadtest.py --endpoint http://HOST:8080 --count 1000 --size-mb 1 --concurrency 32 --cleanup
```

Uploads N objects concurrently and reports duration + throughput (needs `boto3`).

## Good to know

- The proxy verifies your client's signature against the proxy secret, then
  re-signs to R2 — it is **not** an open relay.
- `aws-chunked` streaming uploads (AWS CLI large files) work out of the box.
- Talk to the proxy over plain HTTP; it re-signs to R2 over HTTPS. Front it with
  a TLS terminator if you need HTTPS client-side.
```
