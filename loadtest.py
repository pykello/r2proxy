#!/usr/bin/env python3
"""Put N objects of a given size through r2proxy and measure total duration.

Usage:
  export AWS_ACCESS_KEY_ID=<proxy access key>
  export AWS_SECRET_ACCESS_KEY=<proxy secret key>
  python3 loadtest.py [--endpoint URL] [--bucket test] [--count 1000]
                      [--size-mb 1] [--concurrency 32] [--prefix load/] [--cleanup]
"""
import argparse, os, sys, time, threading
from concurrent.futures import ThreadPoolExecutor, as_completed
import boto3
from botocore.config import Config

ap = argparse.ArgumentParser()
ap.add_argument("--endpoint", default=os.getenv("R2PROXY_ENDPOINT_URL", "http://144.76.134.100:8080"))
ap.add_argument("--bucket", default="test")
ap.add_argument("--count", type=int, default=1000)
ap.add_argument("--size-mb", type=float, default=1.0)
ap.add_argument("--concurrency", type=int, default=32)
ap.add_argument("--prefix", default="load/")
ap.add_argument("--cleanup", action="store_true", help="delete the objects afterward")
args = ap.parse_args()

payload = os.urandom(int(args.size_mb * 1024 * 1024))
total_bytes = len(payload) * args.count

# One shared client; boto3 clients are thread-safe. Size the pool to concurrency.
s3 = boto3.client(
    "s3",
    endpoint_url=args.endpoint,
    region_name="auto",
    config=Config(
        s3={"addressing_style": "path"},
        max_pool_connections=args.concurrency,
        retries={"max_attempts": 3, "mode": "standard"},
    ),
)

done = 0
fail = 0
lock = threading.Lock()

def put(i):
    global done, fail
    key = f"{args.prefix}obj-{i:05d}.bin"
    try:
        s3.put_object(Bucket=args.bucket, Key=key, Body=payload)
        ok = True
    except Exception as e:
        ok = False
        err = e
    with lock:
        if ok:
            done += 1
        else:
            fail += 1
        n = done + fail
        if n % 50 == 0 or n == args.count:
            print(f"\r  {n}/{args.count}  ok={done} fail={fail}", end="", flush=True)
    if not ok and fail <= 5:
        print(f"\n  error on {key}: {err}", file=sys.stderr)
    return key

print(f"PUT {args.count} x {args.size_mb} MiB ({total_bytes/1e6:.0f} MB total) "
      f"-> {args.endpoint}  bucket={args.bucket}  concurrency={args.concurrency}")

t0 = time.perf_counter()
with ThreadPoolExecutor(max_workers=args.concurrency) as ex:
    futs = [ex.submit(put, i) for i in range(args.count)]
    for _ in as_completed(futs):
        pass
dt = time.perf_counter() - t0

print()
print("─" * 56)
print(f"  objects        : {done} ok, {fail} failed")
print(f"  total uploaded : {done*len(payload)/1e6:.1f} MB")
print(f"  total duration : {dt:.2f} s")
if dt > 0 and done:
    print(f"  throughput     : {done/dt:.1f} obj/s   {done*len(payload)/1e6/dt:.1f} MB/s")
    print(f"  avg per object : {dt/done*1000:.1f} ms (wall/obj at c={args.concurrency})")
print("─" * 56)

if args.cleanup and done:
    print("cleaning up...", end="", flush=True)
    keys = [{"Key": f"{args.prefix}obj-{i:05d}.bin"} for i in range(args.count)]
    for j in range(0, len(keys), 1000):
        s3.delete_objects(Bucket=args.bucket, Delete={"Objects": keys[j:j+1000], "Quiet": True})
    print(" done")
