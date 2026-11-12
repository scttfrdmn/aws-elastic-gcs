#!/usr/bin/env python3
"""Rank EC2 instance types for Elastic GCS DTNs by sustained throughput per dollar.

Uses live data from the AWS APIs (read-only): DescribeInstanceTypes for vCPU,
memory and baseline/peak network bandwidth, and the Price List API for
on-demand Linux pricing in the target region.

The number that matters for a DTN is *sustained internet throughput*: Globus
data channels to remote endpoints leave through the VPC internet gateway, and
EC2 limits multi-flow IGW traffic by instance type
(https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/ec2-instance-network-bandwidth.html):

  - fewer than 32 vCPUs:        5 Gbps
  - 32 vCPUs or more:           50% of the instance bandwidth
  - IGW_BASELINE_FAMILIES:      the instance's baseline bandwidth

"Up to" bandwidth is a burst that lasts 5-60 minutes, so sustained figures use
the baseline. S3 reads go through the S3 gateway endpoint and are not IGW
limited, so the IGW figure is the bottleneck for S3 -> remote transfers.

Usage:
  AWS_PROFILE=aws scripts/instance_sizing.py --region us-east-1
  scripts/instance_sizing.py --region us-west-2 --arch arm64 --min-mem-gib 8
  scripts/instance_sizing.py --types c8in.2xlarge,m8in.xlarge,m6i.xlarge
  scripts/instance_sizing.py --arch any --fleet-gbps 50       # horizontal: N-node fleets
  scripts/instance_sizing.py --arch any --cpu amd --fleet-gbps 50
"""
import argparse
import json
import math
import subprocess
import sys

# Families whose IGW traffic is limited to baseline bandwidth rather than the
# 5 Gbps / 50% rule. Keep in sync with the EC2 network bandwidth docs.
IGW_BASELINE_FAMILIES = {"c8in", "c8ine", "m8in", "m8ine", "m8idn", "r8in", "r8idn"}

# Accelerated and HPC families: priced for GPUs/fabric a DTN can't use, and
# often without stop/start (warm pool) support.
EXCLUDED_PREFIXES = ("g", "p", "inf", "trn", "dl", "f", "vt", "hpc")

TIERS = [(1, 5), (5, 6.25), (6.25, 12.5), (12.5, 25), (25, 50), (50, 10_000)]


def aws(*args):
    out = subprocess.run(["aws", *args, "--output", "json"], check=True,
                         capture_output=True, text=True).stdout
    return json.loads(out)


def instance_types(region, arch):
    archs = "x86_64,arm64" if arch == "any" else arch
    data = aws("ec2", "describe-instance-types", "--region", region,
               "--filters", "Name=current-generation,Values=true",
               f"Name=processor-info.supported-architecture,Values={archs}",
               "Name=bare-metal,Values=false",
               "--query", "InstanceTypes[].{t:InstanceType,v:VCpuInfo.DefaultVCpus,"
               "m:MemoryInfo.SizeInMiB,b:NetworkInfo.NetworkCards[0].BaselineBandwidthInGbps,"
               "p:NetworkInfo.NetworkCards[0].PeakBandwidthInGbps,"
               "burst:BurstablePerformanceSupported,cpu:ProcessorInfo.Manufacturer}")
    return {x["t"]: x for x in data}


def on_demand_prices(region):
    # The Price List API is only served from a few regions; us-east-1 works for all.
    data = aws("pricing", "get-products", "--region", "us-east-1",
               "--service-code", "AmazonEC2", "--filters",
               f"Type=TERM_MATCH,Field=regionCode,Value={region}",
               "Type=TERM_MATCH,Field=operatingSystem,Value=Linux",
               "Type=TERM_MATCH,Field=tenancy,Value=Shared",
               "Type=TERM_MATCH,Field=preInstalledSw,Value=NA",
               "Type=TERM_MATCH,Field=capacitystatus,Value=Used",
               "Type=TERM_MATCH,Field=licenseModel,Value=No License required")
    prices = {}
    for item in data["PriceList"]:
        p = json.loads(item)
        t = p["product"]["attributes"].get("instanceType")
        for term in p["terms"].get("OnDemand", {}).values():
            for dim in term["priceDimensions"].values():
                usd = float(dim["pricePerUnit"]["USD"])
                if usd > 0:
                    prices[t] = usd
    return prices


def igw_gbps(family, vcpu, baseline, bandwidth):
    """Multi-flow IGW limit when the instance is pushing `bandwidth` Gbps."""
    if family in IGW_BASELINE_FAMILIES:
        return baseline
    return min(bandwidth, 5.0) if vcpu < 32 else bandwidth * 0.5


CPU_VENDORS = {"Intel": "intel", "AMD": "amd", "AWS": "graviton"}

# Per-node costs that scale with DTN count rather than size: a public IPv4
# address per running DTN, and a 20 GiB gp3 root volume per warm-pool slot.
IPV4_USD_HR = 0.005
WARM_SLOT_USD_MONTH = 20 * 0.08


def rows(types, prices, min_mem_gib, cpu="any"):
    out = []
    for t, x in types.items():
        if t not in prices or x["b"] is None or x["m"] < min_mem_gib * 1024:
            continue
        fam = t.split(".")[0]
        if fam.startswith(EXCLUDED_PREFIXES):
            continue
        vendor = CPU_VENDORS.get(x.get("cpu"), x.get("cpu") or "?")
        if cpu != "any" and vendor != cpu:
            continue
        base, peak = x["b"], x["p"] or x["b"]
        sustained = igw_gbps(fam, x["v"], base, base)
        hr = prices[t]
        tb_per_hr = sustained / 8 * 3600 / 1000
        out.append({
            "type": t, "cpu": vendor, "vcpu": x["v"], "mem_gib": x["m"] // 1024,
            "baseline_gbps": base, "peak_gbps": peak,
            "internet_sustained_gbps": sustained,
            "internet_burst_gbps": igw_gbps(fam, x["v"], base, peak),
            "usd_hr": hr, "usd_per_gbps_hr": hr / sustained,
            "instance_usd_per_tb": hr / tb_per_hr,
            "hours_per_tb": 1 / tb_per_hr,
            "burstable_cpu": bool(x.get("burst")),
        })
    return out


def table(rs, title):
    print(f"\n== {title}")
    print(f"{'type':16}{'cpu':9}{'vCPU':>5}{'GiB':>5}{'base':>7}{'peak':>7}{'net sus':>9}"
          f"{'net bst':>9}{'$/hr':>8}{'$/Gbps-h':>10}{'$/TB':>7}{'min/TB':>8}")
    for r in rs:
        print(f"{r['type']:16}{r['cpu']:9}{r['vcpu']:>5}{r['mem_gib']:>5}{r['baseline_gbps']:>7.2f}"
              f"{r['peak_gbps']:>7.1f}{r['internet_sustained_gbps']:>9.2f}"
              f"{r['internet_burst_gbps']:>9.1f}{r['usd_hr']:>8.3f}"
              f"{r['usd_per_gbps_hr']:>10.3f}{r['instance_usd_per_tb']:>7.3f}"
              f"{r['hours_per_tb'] * 60:>8.0f}")


def fleet(rs, target, top, types=None):
    """Horizontal scaling view: N identical DTNs to reach `target` Gbps."""
    out = []
    for r in rs:
        if r["burstable_cpu"] or (types and r["type"] not in types.split(",")):
            continue
        n = math.ceil(target / r["internet_sustained_gbps"])
        hr = n * (r["usd_hr"] + IPV4_USD_HR)
        out.append((hr, n, r))
    out.sort(key=lambda x: x[0])
    if not types:
        out = out[:top]
    print(f"\n== fleets reaching {target:g} Gbps sustained internet throughput")
    print(f"{'type':16}{'cpu':9}{'GiB':>5}{'node Gbps':>10}{'nodes':>6}{'fleet Gbps':>11}"
          f"{'fleet $/hr':>11}{'$/Gbps-h':>9}{'warm $/mo':>10}")
    for hr, n, r in out:
        print(f"{r['type']:16}{r['cpu']:9}{r['mem_gib']:>5}{r['internet_sustained_gbps']:>10.2f}"
              f"{n:>6}{n * r['internet_sustained_gbps']:>11.1f}{hr:>11.3f}{hr / target:>9.3f}"
              f"{n * WARM_SLOT_USD_MONTH:>10.2f}")


def main():
    ap = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    ap.add_argument("--region", default="us-east-1")
    ap.add_argument("--arch", default="x86_64", choices=["x86_64", "arm64", "any"],
                    help="x86_64 (Intel/AMD) is the only architecture GCS officially supports")
    ap.add_argument("--cpu", default="any", choices=["any", "intel", "amd", "graviton"])
    ap.add_argument("--min-mem-gib", type=int, default=16,
                    help="GCS minimum is 8 GiB, which Globus says is not a production size")
    ap.add_argument("--top", type=int, default=5)
    ap.add_argument("--types", help="comma-separated types to show regardless of rank")
    ap.add_argument("--json", action="store_true", help="emit all rows as JSON")
    ap.add_argument("--fleet-gbps", type=float,
                    help="price the cheapest N-node fleet of each type reaching this "
                         "aggregate sustained internet throughput")
    a = ap.parse_args()

    rs = rows(instance_types(a.region, a.arch), on_demand_prices(a.region), a.min_mem_gib, a.cpu)
    if a.json:
        json.dump(sorted(rs, key=lambda r: r["usd_per_gbps_hr"]), sys.stdout, indent=1)
        return
    print(f"region={a.region} arch={a.arch} cpu={a.cpu} min_mem={a.min_mem_gib}GiB  "
          "(net = multi-flow internet-gateway Gbps; $/TB = instance cost only)")
    if a.fleet_gbps:
        fleet(rs, a.fleet_gbps, a.top, a.types)
        return
    if a.types:
        want = set(a.types.split(","))
        table(sorted((r for r in rs if r["type"] in want),
                     key=lambda r: r["internet_sustained_gbps"]), "selected types")
        missing = want - {r["type"] for r in rs}
        if missing:
            print(f"(not shown: {', '.join(sorted(missing))} — unavailable, below "
                  "--min-mem-gib, or not current generation)")
        return
    for lo, hi in TIERS:
        tier = [r for r in rs if lo <= r["internet_sustained_gbps"] < hi
                and not r["burstable_cpu"]]
        if tier:
            table(sorted(tier, key=lambda r: r["usd_per_gbps_hr"])[:a.top],
                  f"sustained internet {lo:g}-{hi:g} Gbps, cheapest per Gbps")


if __name__ == "__main__":
    main()
