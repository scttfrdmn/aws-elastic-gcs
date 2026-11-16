#!/usr/bin/env python3
"""P0 of the envelope study (#151): instance and S3 reference, no Globus.

Runs on the src P0 instance (same VPC as the src DTNs; reads the synthetic
bucket anonymously through the src S3 VPC endpoint, like the stock GCS S3
connector does). Measures, for this instance type:

  net:     iperf3 to the dst P0 instance over public IPs (the path Globus data
           takes), at increasing flow counts, several iperf3 processes in
           parallel (each is single-threaded);
  get1:    S3 GET with ONE connection per object (curl), N objects in flight;
           the suspected per-file behaviour of the stock connector;
  getcrt:  S3 GET with the AWS CRT client (parallel ranged GETs per object);
  putcrt:  S3 PUT with the CRT client (parallel multipart), streamed from
           /dev/zero (no disk in the path);
  sustained: 180 s runs of the network and of single-connection GETs in 10 s
           bins, so burst-credit decay on "up to" instance types is visible.

Multi-process CRT tests give each process a target bandwidth of its share of
the NIC (2 x peak / n), so n processes don't each size buffers for 100 Gb/s.

One JSON line per measurement to --out. Stdlib only.

usage: p0_reference.py --label c8in.2xlarge --peer <dst-ip> --bucket <synth>
                       [--out /tmp/p0.jsonl] [--reps 3]
"""
import argparse, concurrent.futures as cf, json, os, subprocess, time

GIB = 1 << 30


def sh(cmd, **kw):
    return subprocess.run(cmd, shell=True, capture_output=True, text=True, **kw)


def emit(out, rec):
    rec["t"] = time.time()
    with open(out, "a") as f:
        f.write(json.dumps(rec) + "\n")
    print(json.dumps(rec), flush=True)


def net(peer, flows, procs, secs=20):
    per = max(1, flows // procs)
    omit = 1 if secs <= 5 else 3
    ps = [subprocess.Popen(f"iperf3 -c {peer} -p {5201 + k} -P {per} -t {secs} -O {omit} -J",
                           shell=True, stdout=subprocess.PIPE, text=True) for k in range(procs)]
    bps = 0.0
    for p in ps:
        out, _ = p.communicate()
        try:
            bps += json.loads(out)["end"]["sum_received"]["bits_per_second"]
        except Exception:
            pass
    return bps / 1e9, per * procs


def parallel(n, fn, keys):
    t0 = time.time()
    with cf.ThreadPoolExecutor(max_workers=n) as ex:
        results = list(ex.map(fn, keys))
    wall = time.time() - t0
    return results, wall


def curl_get(bucket):
    def f(key):
        r = sh(f"curl -s -o /dev/null -w '%{{size_download}} %{{time_total}} %{{http_code}}' "
               f"https://{bucket}.s3.us-west-2.amazonaws.com/{key}")
        b, t, code = r.stdout.split()
        return int(b), float(t), code
    return f


def crt_env(target_gbps):
    cfg = f"/tmp/aws-crt-{target_gbps}.cfg"
    with open(cfg, "w") as f:
        f.write(f"[default]\ns3 =\n  preferred_transfer_client = crt\n  target_bandwidth = {target_gbps}Gb/s\n")
    return f"AWS_CONFIG_FILE={cfg} "


def crt_get(bucket, env=""):
    def f(key):
        t0 = time.time()
        r = sh(f"{env}aws s3 cp s3://{bucket}/{key} - --only-show-errors > /dev/null")
        return r.returncode, time.time() - t0
    return f


def crt_put(bucket, size, prefix, env=""):
    def f(i):
        t0 = time.time()
        r = sh(f"head -c {size} /dev/zero | {env}aws s3 cp - s3://{bucket}/{prefix}/{i} "
               f"--expected-size {size} --only-show-errors")
        return r.returncode, time.time() - t0
    return f


def net_sustained(peer, procs, per, secs=180):
    ps = [subprocess.Popen(f"iperf3 -c {peer} -p {5201 + k} -P {per} -t {secs} -i 10 -J",
                           shell=True, stdout=subprocess.PIPE, text=True) for k in range(procs)]
    bins = {}
    for p in ps:
        out, _ = p.communicate()
        try:
            for i, iv in enumerate(json.loads(out)["intervals"]):
                bins[i] = bins.get(i, 0.0) + iv["sum"]["bits_per_second"] / 1e9
        except Exception:
            pass
    return [round(bins[i], 2) for i in sorted(bins)]


def get1_sustained(bucket, n, keys, secs=180, binw=10):
    """n workers fetch objects back to back for secs; bytes binned by completion time."""
    t0 = time.time()
    done = []  # (t_end, bytes)
    def worker(w):
        i = w
        while time.time() - t0 < secs:
            b, t, code = curl_get(bucket)(keys[i % len(keys)])
            if code == "200":
                done.append((time.time() - t0, b))
            i += n
    with cf.ThreadPoolExecutor(max_workers=n) as ex:
        list(ex.map(worker, range(n)))
    nb = int(secs // binw)
    bins = [0] * nb
    for t, b in done:
        k = int(t // binw)
        if k < nb:
            bins[k] += b
    return [round(x * 8 / binw / 1e9, 2) for x in bins]


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--label", required=True)
    ap.add_argument("--peer", required=True)
    ap.add_argument("--bucket", required=True)
    ap.add_argument("--out", default="/tmp/p0.jsonl")
    ap.add_argument("--reps", type=int, default=3)
    a = ap.parse_args()
    vcpu = os.cpu_count() or 8
    big = vcpu >= 32
    base = {"label": a.label, "vcpu": vcpu}

    peak = 0.0
    # Network: flows, iperf3 processes.
    # Public-IP traffic is billed as regional transfer (~$0.02/GB round trip):
    # fixed-rate large types get short 5 s tests and no sustained run; burstable
    # ("up to") types get the full grid plus a sustained run to see credit decay.
    burstable = vcpu < 32
    grid = [(1, 1), (8, 1), (32, 4), (128, 8)] if burstable else [(1, 1), (256, 16), (512, 32)]
    secs = 20 if burstable else 5
    for flows, procs in grid:
        g, f = net(a.peer, flows, procs, secs)
        peak = max(peak, g)
        emit(a.out, {**base, "test": "net", "flows": f, "procs": procs, "gbps": round(g, 3)})
    if burstable:
        emit(a.out, {**base, "test": "net_sustained", "flows": 128, "procs": 8, "bin_s": 10,
                     "gbps_bins": net_sustained(a.peer, 8, 16, secs=150)})

    keys_1g = [f"p0/1g/{i:03d}" for i in range(128)]
    keys_64m = [f"p0/64m/{i:03d}" for i in range(128)]

    # Single connection per object (stock-connector-like), 1 GiB and 64 MiB objects.
    for size, keys, ns in [(GIB, keys_1g, [1, 8, 32, 128] + ([512] if big else [])),
                           (64 << 20, keys_64m, [1, 32, 128] + ([512] if big else []))]:
        for n in ns:
            reps = a.reps if n == 1 else 1
            for r in range(reps):
                ks = [keys[i % len(keys)] for i in range(max(n, 1) * (2 if n <= 8 else 1))]
                res, wall = parallel(n, curl_get(a.bucket), ks)
                ok = [x for x in res if x[2] == "200"]
                tot = sum(x[0] for x in ok)
                per = [x[0] * 8 / x[1] / 1e9 for x in ok if x[1] > 0]
                emit(a.out, {**base, "test": "get1", "object": size, "inflight": n, "rep": r,
                             "objects": len(ks), "errors": len(res) - len(ok),
                             "agg_gbps": round(tot * 8 / wall / 1e9, 3),
                             "per_object_gbps_mean": round(sum(per) / len(per), 3) if per else None})

    # CRT client (parallel ranged GET per object).
    for key, size in [("p0/seed/4G", 4 * GIB), ("p0/1g/000", GIB)]:
        for r in range(a.reps):
            rc, secs = crt_get(a.bucket)(key)
            emit(a.out, {**base, "test": "getcrt", "object": size, "inflight": 1, "rep": r,
                         "rc": rc, "gbps": round(size * 8 / secs / 1e9, 3)})
    for n in [8, 32] + ([64, 128] if big else []):
        ks = [keys_1g[i % 128] for i in range(n * 2)]
        res, wall = parallel(n, crt_get(a.bucket, crt_env(max(2, round(2 * max(peak, 10) / n)))), ks)
        emit(a.out, {**base, "test": "getcrt", "object": GIB, "inflight": n, "objects": len(ks),
                     "errors": sum(1 for x in res if x[0] != 0),
                     "agg_gbps": round(len(ks) * GIB * 8 / wall / 1e9, 3)})

    # CRT PUT, streamed (no disk).
    prefix = f"p0/put/{a.label}"
    for n in [1, 8, 32] + ([64] if big else []):
        reps = a.reps if n == 1 else 1
        for r in range(reps):
            env = crt_env(max(2, round(2 * max(peak, 10) / n))) if n > 1 else ""
            res, wall = parallel(n, crt_put(a.bucket, GIB, f"{prefix}/{n}-{r}", env), range(n))
            emit(a.out, {**base, "test": "putcrt", "object": GIB, "inflight": n, "rep": r,
                         "errors": sum(1 for x in res if x[0] != 0),
                         "agg_gbps": round(n * GIB * 8 / wall / 1e9, 3)})
    sh(f"aws s3 rm s3://{a.bucket}/{prefix}/ --recursive --only-show-errors")
    for n in [128] + ([512] if big else []):
        emit(a.out, {**base, "test": "get1_sustained", "object": GIB, "inflight": n, "bin_s": 10,
                     "gbps_bins": get1_sustained(a.bucket, n, keys_1g)})
    emit(a.out, {**base, "test": "done"})


if __name__ == "__main__":
    main()
