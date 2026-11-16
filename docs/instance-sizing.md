# Instance Sizing

How to choose DTN and control node instance types. Figures are on-demand Linux
prices and EC2 network specs pulled from the AWS APIs on 2026-11-12. Re-run
[`scripts/instance_sizing.py`](../scripts/instance_sizing.py) for current
numbers in any region:

```bash
AWS_PROFILE=aws scripts/instance_sizing.py --region us-east-1                     # rank single nodes
AWS_PROFILE=aws scripts/instance_sizing.py --arch any --fleet-gbps 50              # rank N-node fleets
AWS_PROFILE=aws scripts/instance_sizing.py --arch arm64 --cpu graviton --min-mem-gib 8
AWS_PROFILE=aws scripts/instance_sizing.py --region eu-west-1 --types m8in.xlarge,c8in.2xlarge,c8in.4xlarge
```

DTNs scale out horizontally, so the instance type sets two things: the cost per
Gbps of the whole fleet, and the size of each scale-out step.

## Recommendation

| Role | Type | vCPU / GiB | Sustained internet throughput | us-east-1 $/hr | Use when |
|------|------|-----------|-------------------------------|----------------|----------|
| **DTN (default)** | `c8in.2xlarge` | 8 / 16 | 12.5 Gbps per node (**9.2 measured** with GCS at concurrency 32, 46% CPU) | $0.544 | Most deployments. One node matches a 10 GbE campus DTN; scale out in ~9 Gbps steps. |
| DTN (fine-grained) | `m8in.xlarge` / `c8in.xlarge` | 4 / 16 or 8 | 6.25 Gbps per node (c8in.xlarge **6.25 measured**, at 77% CPU) | $0.334 / $0.272 | Mostly small or bursty demand. c8in.xlarge has only 8 GiB and is near CPU saturation at its network limit. |
| Control node | `t3a.large` | 2 / 8 | (not a data mover, see below) | $0.0752 | Default. ~$55/month on-demand. |

**Measured (2026-11, [performance results](performance/methodology.md)):**
- The coarse `c8in.4xlarge` tier (previously "for very large single files") is
  dropped. One file moves at ~75 MB/s on any instance size, so a bigger node
  doesn't help.
- A single Globus task can't fan out beyond ~4 DTNs at Transfer's 128-process
  cap, so scale out with more nodes, not larger ones.
- AMD `m8azn.xlarge` measured exactly its 5 Gbps internet cap, at 36% CPU.

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
- **`c8in.xlarge`.** Same cost per Gbps as the other c8in sizes, but only 8 GiB,
  the GCS minimum, which Globus says is not a production size.
- **AMD (`c8a`, `m8a`, `m7a`, `c6a`, `m8azn`) for DTNs.** See below. AMD is fine
  for the control node; `t3a.large` is AMD EPYC.

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

**Bigger DTNs cost nothing extra while idle.** A WARM (stopped) slot costs only
its 20 GiB gp3 root volume (~$1.60/month) and its Elastic IP (~$3.65/month),
whatever the instance type. Instance
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
(about 52% off). That saves ~$0.05/TB on a ~$50–90/TB transfer, and a Spot
interruption ends every session on the node. The slot controller doesn't
support Spot yet.

## Intel vs AMD vs Graviton

Cheapest fleet from each CPU vendor reaching 50 Gbps sustained internet
throughput (us-east-1, on-demand, including $0.005/hr public IPv4 per node):

| Vendor | Best type | GiB | Gbps/node | Nodes | Fleet $/hr | $/Gbps-h | vs c8in |
|--------|-----------|-----|-----------|-------|-----------|----------|---------|
| Graviton | `m8gn.large` | 8 | 5 | 10 | $1.51 | $0.030 | −32% |
| Graviton | `r8gn.large` | 16 | 5 | 10 | $1.87 | $0.037 | −15% |
| Graviton | `c8gn.8xlarge` | 64 | 50 | 1 | $1.90 | $0.038 | −13% |
| **Intel** | **`c8in.2xlarge`** | 16 | 12.5 | 4 | $2.20 | $0.044 | — |
| Intel | `m6in.xlarge` | 16 | 5 | 10 | $2.84 | $0.057 | +29% |
| AMD | `m8azn.xlarge` | 16 | 5 | 10 | $4.18 | $0.084 | +90% |
| AMD | `c8a.2xlarge` | 16 | 3.75 | 14 | $6.11 | $0.122 | +178% |

**AMD loses for DTNs at every size.** No AMD family is exempt from the IGW cap,
and AMD network baselines are low: `c8a.2xlarge` sustains 3.75 Gbps, and only
`m8azn` reaches the 5 Gbps cap below 32 vCPUs. The cheapest AMD fleet costs
about 1.9× the c8in fleet per Gbps.

**Graviton is 13–32% cheaper per Gbps, but don't adopt it yet.**
- **Why it's cheaper:** small Graviton nodes are cheap enough that a fleet of
  5 Gbps-capped nodes beats c8in. At 32+ vCPUs, `c8gn` gets 50% of its 100 Gbps
  baseline.
- **Supported platform:** GCS officially supports only x86-64. arm64 packages
  exist only for Ubuntu 24.04/26.04, and the scripts are Rocky-only.
- **Memory:** the cheapest option, `m8gn.large`, is at the 8 GiB minimum.
- **CPU:** whether 2 vCPUs can handle S3 HTTPS plus GridFTP TLS at 5 Gbps is
  unmeasured.
- **Node count:** reaching 50 Gbps takes 10 nodes instead of 4 (see below).
- **Dollar value:** the saving is ~$0.016–0.03 per TB against $50–90/TB egress.

Revisit Graviton if Globus adds official arm64 support (`r8gn.large` is the
natural candidate).

## CPU: clock speed and physical cores

GCS work is CPU-heavy per process: S3 HTTPS, GridFTP data-channel TLS, and
integrity checksums. Each concurrency slot is a GridFTP process, so per-core
speed matters, not just core count. Experience suggests Globus favors fast
cores. The test plan (#58) checks this at equal network throughput.

| Type | CPU | vCPU | Physical cores | Sustained GHz | Internet Gbps | $/hr |
|------|-----|------|----------------|---------------|---------------|------|
| `m8in.xlarge` | Intel Xeon 6 | 4 | 2 (SMT) | 3.9 | 6.25 | $0.334 |
| `c8in.2xlarge` | Intel Xeon 6 | 8 | 4 (SMT) | 3.9 | 12.5 | $0.544 |
| `c8in.4xlarge` | Intel Xeon 6 | 16 | 8 (SMT) | 3.9 | 25 | $1.089 |
| `m8azn.xlarge` | AMD EPYC (8th gen) | 4 | 4 | 5.0 | 5 | $0.413 |
| `c8a.2xlarge` | AMD EPYC (8th gen) | 8 | 8 | 4.5 | 3.75 | $0.431 |
| `r8gn.large` | Graviton4 | 2 | 2 | 2.8 | 5 | $0.182 |
| `c8gn.2xlarge` | Graviton4 | 8 | 8 | 2.8 | 5 | $0.474 |
| `c9g.2xlarge` | Graviton (9th gen) | 8 | 8 | 3.3 | 4.25 | $0.348 |

Trade-offs:

- **Intel `*8in`:** the only types with uncapped internet throughput, but each
  vCPU is a hyperthread. `c8in.2xlarge` has 4 physical cores for 12.5 Gbps.
- **AMD 8th gen:** the fastest clocks (`m8azn` at 5.0 GHz), and every vCPU is a
  full core. But AMD types are network-capped (5 Gbps or less below 32 vCPUs,
  low baselines), so they only win if DTNs turn out to be CPU-bound below
  5 Gbps.
- **Graviton:** full cores but the slowest clocks (2.8–3.3 GHz). 9th-gen
  Graviton (`c9g`/`m9g`/`r9g`) has no network-optimized variant yet, so its
  baseline is 4.25 Gbps at 2xlarge.

If CPU-bound per node, the fix is horizontal: more nodes, each running fewer
concurrency processes.

## Multiple network interfaces

On EC2, bandwidth belongs to the **network card**, not to the network interface
(ENI). Every candidate type below the largest sizes has one network card.
Adding ENIs there splits the same bandwidth and the same 5 Gbps / baseline
internet limit. Only `c8in.96xlarge`, `m8in.96xlarge` and `c8gn.48xlarge`
have 2 cards (300 Gbps each). To get more interfaces' worth of throughput,
scale out instead.

## Horizontal scaling

Within the c8in family, cost per Gbps is the same at every size, and idle cost
per warm slot is the same for every type. So node size is a choice of scaling
step, not cost:

| Consideration | Smaller nodes (`m8in.xlarge`, 6.25 Gbps) | Larger nodes (`c8in.4xlarge`, 25 Gbps) |
|---------------|-------------------------------------------|-----------------------------------------|
| Over-provisioning between steps | Less | More (but ~$0.044/Gbps-h, so small) |
| Node failure / Spot loss | Smaller share of capacity | Larger share |
| Nodes, slots, warm instances per Gbps | More | Fewer |
| Node joins to reach a target (each restarts GridFTP listeners endpoint-wide, #141) | More | Fewer |
| Single-file throughput (one file = one node) | Lower | Higher |
| Per-task ceiling (concurrency × node Gbps) | Lower | Higher |

`c8in.2xlarge` is the balance point. One node saturates a typical 10 GbE
destination, and 4 nodes reach 50 Gbps.

Configure the fleet for its peak, not for one node:

- **`max_dtns`:** set to `ceil(peak aggregate Gbps / per-node Gbps)`. For example,
  50 Gbps of `c8in.2xlarge` → 4. The control node pre-creates one node identity
  per slot.
- **`warm_slots`:** WARM slots start in about 1.5 minutes; COLD slots revive
  one at a time (about 1 minute each, plus DNS). A warm slot costs ~$5/month
  (EBS + Elastic IP), so `warm_slots: max_dtns` (4 × ~$5 = ~$20/month) makes the
  whole fleet fast to reach, against the default 2.
- **Scale-out math:** demand_monitor sets desired-dtns to
  `ceil(aggregate throughput / throughput_scaleout_mbps)` directly (#26), and
  the controller starts every WARM slot needed at once.
- **Capacity resilience:** `instance_types` is a fallback list used when EC2
  reports no capacity for the first type. Pair equal-throughput types so the
  per-node scaling math still holds. For example, `c8in.2xlarge` first, then
  `m8in.2xlarge` (both 12.5 Gbps; m8in is 23% more expensive and used only if
  c8in capacity runs short).
- **Quota:** the default on-demand standard-instance quota (2,048 vCPUs in this
  account) allows ~250 `c8in.2xlarge`. It is not a constraint.
- **The ceiling is usually the other end.** Fan-out per task is capped by
  concurrency (see below), and aggregate throughput by what the destinations can
  absorb.

### Why horizontal scale suits S3

The S3 leg scales almost linearly with DTN count:

- **No bucket throughput cap.** S3 has no per-bucket throughput limit and no
  limit on the number of prefixes. Request rates are at least 5,500 GET/HEAD and
  3,500 PUT/COPY/POST/DELETE per second *per partitioned prefix*, and S3
  partitions automatically as load rises. AWS's guidance for high throughput is
  to aggregate across many connections and instances, which is what a DTN fleet
  does.
- **The S3 leg isn't IGW-capped.** S3 traffic uses the gateway endpoint, so the
  S3 side of each DTN gets full instance bandwidth (including burst), and the
  gateway endpoint is free. The internet leg stays the per-node bottleneck.
- **Request rate rarely binds.** It matters only for small objects or heavy
  writes. 5,500 GET/s of 5 MB objects is ~27 GB/s from a *single* prefix, far
  above a DTN fleet. Writes count every multipart part against the PUT limit.

How Globus spreads the work (from the [Globus FAQ](https://docs.globus.org/faq/transfer-sharing/)):

- **Within a task:** Globus load-balances across DTNs only when the task has
  **more than 10 files**. Files are spread over *concurrency* GridFTP processes,
  and those processes are distributed across nodes. A single file stays on one
  process and node; *parallelism* (multiple TCP streams) speeds it up but
  doesn't split it across nodes.
- **Across tasks:** concurrent tasks are round-robined across active nodes.
- **No prefix awareness:** Globus distributes *files to processes*, not prefixes.
  It doesn't shard by prefix, and doesn't need to unless a workload is
  request-rate-bound on one hot prefix. A large write into a new prefix can
  draw S3 503 Slow Down responses while S3 repartitions; the connector should
  retry these.
- **Per-task fan-out = concurrency.** Concurrency is the *minimum* of the two
  endpoints' MaximumConcurrency, so one task can use at most that many
  processes and nodes. Its ceiling is roughly `concurrency × per-node Gbps`, and
  the destination's settings can cap it. Nothing in this repo sets the
  endpoint's network-use (concurrency/parallelism) policy, so it runs on the
  GCS default. Set it so this endpoint is never the limiting side.
- **Many small nodes win for multi-file datasets and many users.** Most Open
  Data collections (NEXRAD, 1000 Genomes) are many-file. **Larger nodes win for
  single huge files and low-concurrency destinations.**

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

**The control node is itself an active GCS node, and it does carry data.**
Measured in #58: Globus gave it the same share of processes as a DTN. It ran at
75% CPU on 2 burstable vCPUs and became the bottleneck. Disable it as a data
node while DTNs are active, and re-enable it before the last DTN leaves; see the
[operator guide](performance/operator-guide.md). Never let the endpoint reach
zero active nodes.

## Measured results

The test matrix above was run in November 2026 (#58). Results, charts, guides
and the throughput model are in [docs/performance/](performance/README.md).
