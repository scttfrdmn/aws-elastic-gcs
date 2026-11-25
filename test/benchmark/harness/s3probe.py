#!/usr/bin/env python3
"""P0 S3 control (#151): single-object and aggregate GET/PUT with the AWS CRT client.

Runs on a DTN (study.py ships it and installs awscrt in a venv there), using
the instance role. Keeps `--inflight` requests outstanding until `--count`
objects are done and prints one JSON line: aggregate Gbps over the wall time,
and per-object Gbps. GETs read existing synthetic objects of `--size` under
`--prefix` and write them to /dev/null. PUTs stream a repeating 64 MiB random
buffer (no disk in the path) to `--prefix`<i>; the caller deletes them.

usage: s3probe.py --op get|put --bucket B --prefix P --size BYTES --inflight N --count M [--region R]
"""
import argparse
import io
import json
import os
import subprocess
import sys
import threading
import time


class RepeatStream(io.RawIOBase):
    """`length` bytes cycling over one random buffer."""

    def __init__(self, buf, length):
        self.buf, self.left, self.pos = buf, length, 0

    def readable(self):
        return True

    def readinto(self, b):
        n = min(len(b), self.left)
        done = 0
        while done < n:
            k = min(n - done, len(self.buf) - self.pos)
            b[done:done + k] = self.buf[self.pos:self.pos + k]
            done += k
            self.pos = (self.pos + k) % len(self.buf)
        self.left -= n
        return n


def list_keys(bucket, prefix, size, region, want):
    out = subprocess.run(["aws", "s3api", "list-objects-v2", "--bucket", bucket, "--prefix", prefix, "--region", region,
                          "--query", f"Contents[?Size==`{size}`].Key", "--output", "text"],
                         capture_output=True, text=True, check=True).stdout.split()
    keys = [k for k in out if k != "None"]
    if not keys:
        sys.exit(f"no objects of {size} B under s3://{bucket}/{prefix}")
    return [keys[i % len(keys)] for i in range(want)]


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--op", choices=("get", "put"), required=True)
    ap.add_argument("--bucket", required=True)
    ap.add_argument("--prefix", required=True)
    ap.add_argument("--size", type=int, required=True)
    ap.add_argument("--inflight", type=int, default=1)
    ap.add_argument("--count", type=int, default=16)
    ap.add_argument("--region", default="us-west-2")
    ap.add_argument("--part-size", type=int, default=8 * 1024 * 1024)
    ap.add_argument("--throughput-gbps", type=float, default=100.0)
    a = ap.parse_args()

    from awscrt import auth, http, io as crtio, s3  # noqa: PLC0415

    elg = crtio.EventLoopGroup()
    boot = crtio.ClientBootstrap(elg, crtio.DefaultHostResolver(elg))
    creds = auth.AwsCredentialsProvider.new_default_chain(boot)
    signing = s3.create_default_s3_signing_config(region=a.region, credential_provider=creds)
    client = s3.S3Client(bootstrap=boot, region=a.region, signing_config=signing,
                         throughput_target_gbps=a.throughput_gbps, part_size=a.part_size)
    host = f"{a.bucket}.s3.{a.region}.amazonaws.com"
    keys = (list_keys(a.bucket, a.prefix, a.size, a.region, a.count) if a.op == "get"
            else [f"{a.prefix}{i:06d}" for i in range(a.count)])
    buf = os.urandom(64 * 1024 * 1024) if a.op == "put" else None

    sem = threading.Semaphore(a.inflight)
    lock = threading.Lock()
    per, errors = [], []
    live = []

    def start(key):
        sem.acquire()
        t0 = time.time()
        if a.op == "get":
            req = http.HttpRequest("GET", "/" + key, http.HttpHeaders([("Host", host)]))
            kw = {"type": s3.S3RequestType.GET_OBJECT, "recv_filepath": "/dev/null"}
        else:
            hdr = http.HttpHeaders([("Host", host), ("Content-Length", str(a.size)),
                                    ("Content-Type", "application/octet-stream")])
            req = http.HttpRequest("PUT", "/" + key, hdr, RepeatStream(buf, a.size))
            kw = {"type": s3.S3RequestType.PUT_OBJECT}

        def done(error=None, status_code=None, **_):
            with lock:
                if error or (status_code and status_code >= 300):
                    errors.append({"key": key, "status": status_code, "error": repr(error)[:200]})
                else:
                    per.append(time.time() - t0)
            sem.release()

        live.append(client.make_request(request=req, on_done=done, **kw))

    t0 = time.time()
    for k in keys:
        start(k)
    for mr in live:
        mr.finished_future.result()
    wall = time.time() - t0
    per.sort()
    med = per[len(per) // 2] if per else None
    print(json.dumps({"op": a.op, "size": a.size, "inflight": a.inflight, "count": a.count, "ok": len(per),
                      "errors": len(errors), "first_errors": errors[:5], "wall_s": round(wall, 3),
                      "gbps": round(8 * a.size * len(per) / wall / 1e9, 3) if wall else None,
                      "per_object_s_median": round(med, 3) if med else None,
                      "per_object_gbps_median": round(8 * a.size / med / 1e9, 3) if med else None,
                      "part_size": a.part_size, "throughput_target_gbps": a.throughput_gbps}))


if __name__ == "__main__":
    main()
