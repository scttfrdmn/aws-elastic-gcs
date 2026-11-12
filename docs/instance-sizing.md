# Instance Sizing

How to choose DTN and control node instance types. Figures are on-demand Linux
prices and EC2 network specs pulled from the AWS APIs on 2026-11-12. Re-run
[`scripts/instance_sizing.py`](../scripts/instance_sizing.py) for current
numbers in any region:

```bash
AWS_PROFILE=aws scripts/instance_sizing.py --region us-east-1
AWS_PROFILE=aws scripts/instance_sizing.py --region eu-west-1 --types m8in.xlarge,c8in.2xlarge,c8in.4xlarge
```

## Recommendation

| Role | Type | vCPU / GiB | Sustained internet throughput | us-east-1 $/hr | Use when |
|------|------|-----------|-------------------------------|----------------|----------|
| **DTN (default)** | `c8in.2xlarge` | 8 / 16 | 12.5 Gbps | $0.544 | Most deployments. Matches 10 GbE campus DTNs. |
| DTN (light) | `m8in.xlarge` | 4 / 16 | 6.25 Gbps | $0.334 | Small datasets, or to cap per-node cost. |
| DTN (heavy) | `c8in.4xlarge` | 16 / 32 | 25 Gbps | $1.089 | Destinations that can absorb >10 Gbps (100 GbE DTNs). |
| Control node | `t3a.large` | 2 / 8 | (not a data mover, see below) | $0.0752 | Default. ~$55/month on-demand. |

Avoid:

- **`m6i.xlarge`** (the previous default; now a previous-generation type). Its
  baseline is 1.56 Gbps. It bursts higher for 5–60 minutes, but internet
  traffic is capped at 5 Gbps even while bursting. Sustained, it moves 1 TB in
  ~85 minutes; `c8in.2xlarge` does it in ~11.
- **`m6in`, `c6in`, and any other type with fewer than 32 vCPUs outside the
  8th-gen `*8in` families.** Their "up to 30/40 Gbps" ratings don't apply to
  internet traffic, which is capped at 5 Gbps. `c6in.2xlarge` costs 83% as much as
  `c8in.2xlarge` for 40% of the internet throughput.
- **`c8ine` / `m8ine`.** Same internet throughput as `c8in` / `m8in` at a ~25%
  higher price. Their advantage is packet rate, which GridFTP doesn't need.
- **`c8in.xlarge`.** Cheapest per Gbps, but only 8 GiB, the GCS minimum, which
  Globus says is not a production size.

## Why throughput, not $/hr, decides

**The internet gateway limit is the bottleneck.** Globus data channels to remote
endpoints leave through the VPC internet gateway. EC2 limits multi-flow IGW
traffic ([docs](https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/ec2-instance-network-bandwidth.html)):

| Instance | IGW limit |
|----------|-----------|
| < 32 vCPUs | 5 Gbps |
| ≥ 32 vCPUs | 50% of instance bandwidth |
| `c8in`, `c8ine`, `m8in`, `m8ine`, `m8idn`, `r8in`, `r8idn` | Full baseline bandwidth |

S3 reads go through the S3 gateway endpoint and aren't IGW-limited, so for
S3 → remote transfers the IGW figure is what a DTN can sustain. The `*8in`
families are the only types under 32 vCPUs that exceed 5 Gbps to the internet.

**Bigger DTNs cost nothing extra while idle.** A stopped warm-pool DTN costs only
its 20 GiB gp3 root volume (~$1.60/month), whatever the instance type. Instance
type only affects cost while a DTN is running, and a faster DTN finishes sooner.

**Egress dwarfs instance cost.** Per TB moved to the internet:

| Cost | Per TB |
|------|--------|
| Data transfer out, us-east-1 (first 10 TB/month → over 150 TB/month) | $90 → $50 |
| `c8in.2xlarge` instance time | $0.10 |
| `m6i.xlarge` instance time (sustained) | $0.27 |

Instance choice is 0.1–0.5% of the cost of a transfer. Optimize for throughput
and transfer time. Check whether your institution qualifies for the AWS Global
Data Egress Waiver before quoting costs.

**Spot isn't worth it for DTNs.** `c8in.2xlarge` Spot was $0.25–0.27/hr
(about 52% off), but Spot disables the warm pool, so every scale-out takes the
2–3 minute cold path. That saves ~$0.05/TB on a ~$50–90/TB transfer.

**Graviton isn't worth it for DTNs.** GCS has no official arm64 support (arm64
packages exist only for Ubuntu 24.04/26.04). Graviton types under 32 vCPUs are
also capped at 5 Gbps to the internet. Graviton is only cheaper per Gbps at 32+
vCPUs (`c8gn.8xlarge`, 50 Gbps, ~13% less than `c8in.8xlarge`).

## Regional availability

`c8in.2xlarge` is offered in us-east-1, us-east-2, us-west-2, eu-west-1,
eu-central-1 and ap-northeast-1. `m8in` is not offered in eu-central-1. Prices
in eu-west-1 are ~13% higher than in us-east-1.

## Scale-out threshold

`throughput_scaleout_mbps` should sit below what one DTN sustains, or the
multi-DTN alarm never fires:

| DTN type | Sustained | Suggested threshold (~70%) |
|----------|-----------|----------------------------|
| `m8in.xlarge` | 781 MB/s | 550 |
| `c8in.2xlarge` | 1,562 MB/s | 1100 |
| `c8in.4xlarge` | 3,125 MB/s | 2200 |

The old 500 MB/s default was above what `m6i.xlarge` sustains (195 MB/s), so
under steady load it only fired while burst credits lasted. The alarm adds at
most one DTN per OK→ALARM transition (#26).

## Control node

The control node runs the GCS Manager and must have at least 8 GiB of RAM.
Cheapest x86-64 options:

| Type | vCPU / GiB | Baseline network | $/month |
|------|-----------|------------------|---------|
| `t3a.large` | 2 / 8 (burstable CPU) | 0.51 Gbps | $54.90 |
| `r7a.medium` | 1 / 8 | 0.39 Gbps | $55.54 |
| `r8a.medium` | 1 / 8 | 0.52 Gbps | $58.31 |

`t3a.large` is fine: the GCS Manager is mostly idle. Because it runs 24/7, a
1-year Compute Savings Plan is worth buying for it.

**The control node is itself an active GCS node.** If Globus routes data channels
to it while DTNs are up, those flows run at ≤0.5 Gbps. Verify this in the
benchmark. If it does carry data, disable the control node as a data node while
DTNs are active and re-enable it on scale-in to zero.

## Still to measure

These figures are the AWS limits. GCS-level throughput still needs a benchmark
on a deployed endpoint (`make benchmark`):

1. **CPU headroom.** S3 connector HTTPS plus GridFTP data-channel encryption at
   12.5 Gbps on 8 vCPUs, and 25 Gbps on 16.
2. **Control-node data path.** Whether the control node carries data channels
   (see above).
3. **Multi-node distribution.** Whether one large task spreads across DTNs, or
   only concurrent tasks do.
4. **Real destinations.** Throughput to a typical campus endpoint versus the
   per-DTN limit.
