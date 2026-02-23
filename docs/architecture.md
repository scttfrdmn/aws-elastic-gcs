# Architecture

Elastic GCS deploys a [Globus Connect Server v5](https://docs.globus.org/globus-connect-server/) endpoint on AWS with automatic scaling from near-zero idle cost to multi-node high-throughput data transfer, then back to idle when transfers complete.

## Three-Tier Caching Model

Instances exist in one of three tiers:

```
COLD (terminated)          WARM (stopped)              HOT (running)
  No EC2 cost               EBS + EIP cost only          Full compute cost
  2–3 min scale-out         30–60 s scale-out             0 s (already serving)
       │                         │                              │
       │  Launch from AMI        │  Resume + node enable        │  Already registered
       │  + node setup           │                              │
       ▼                         ▼                              ▼
   ┌─────────┐  stop+hold   ┌─────────┐  scale out      ┌─────────┐
   │  COLD   │ ──────────►  │  WARM   │ ─────────────►  │   HOT   │
   └─────────┘              └─────────┘                 └─────────┘
                                 ▲          cool-down         │
                                 │          (15–30 min)       │
                                 └───────────────────────────-┘
                            cache eviction
                            (72 h TTL) ──► TERMINATE
```

**Cold path** (new instance, never seen before): instance launches from DTN AMI, runs `node setup --import-node` with a pre-created node identity from Secrets Manager (~23 s), ASG marks it InService. Total: ~2–3 minutes.

**Warm path** (instance returning from stopped state): instance resumes, runs `node update --ip-address --enable` (~7 s), ASG marks it InService. Total: ~60 s wall-clock.

**Hot path**: instance is already InService and active — 0 s additional latency.

## Components

```
┌────────────────────────────────────────────────────────────────┐
│                     AWS Region                                  │
│                                                                 │
│  ┌───────────────┐    ┌──────────────────────────────────────┐  │
│  │ Control Node  │    │  ASG + Warm Pool                     │  │
│  │ (t3a.large)   │    │  ┌──────────┐  ┌──────────┐         │  │
│  │               │    │  │  DTN 1   │  │  DTN 2   │  ...    │  │
│  │ • GCS endpoint│    │  │ (stopped)│  │ (stopped)│         │  │
│  │ • Node idents │    │  └──────────┘  └──────────┘         │  │
│  │ • GCS manager │    └──────────────────────────────────────┘  │
│  └───────┬───────┘                                             │
│          │                                                      │
│  ┌───────▼──────────────────────────────────────────────────┐  │
│  │  Lambda Functions                                         │  │
│  │  • scaleout      — increments ASG desired                 │  │
│  │  • scalein       — decrements ASG desired when idle       │  │
│  │  • lifecycle     — node disable before instance stops     │  │
│  │  • cache_eviction — terminates warm pool instances > 72h  │  │
│  │  • demand_monitor — publishes ActiveTransfers metric       │  │
│  │  • version_check — detects GCS version drift every 6h     │  │
│  └──────────────────────────────────────────────────────────┘  │
│                                                                 │
│  ┌───────────────────┐  ┌──────────────────────────────────┐   │
│  │ SSM Parameter     │  │ Secrets Manager                  │   │
│  │ • control-node-id │  │ • deployment-key                 │   │
│  │ • endpoint-id     │  │ • node-slot-1..N                 │   │
│  │ • slot locks      │  │ • globus-client-id/secret        │   │
│  └───────────────────┘  └──────────────────────────────────┘   │
└────────────────────────────────────────────────────────────────┘
```

### Control Node

A single `t3a.large` EC2 instance (x86-64 required — GCS 5.4 has no aarch64 packages). Runs at all times. Responsible for:

- Hosting the GCS endpoint (Globus Manager API, data channel)
- Pre-creating DTN node identities (`globus-connect-server node create --export-node`)
- Storing node identity JSON files in Secrets Manager
- Running GCS CLI commands on behalf of `elastic-gcs-ctl` via SSM Run Command

### DTN Pool (ASG + Warm Pool)

Data Transfer Nodes handle actual transfer I/O. Configured via CDK context:

- `maxDTNs`: maximum InService DTN count (default: 4)
- `warmPoolSize`: number of pre-warmed stopped instances (default: 2)
- `dtnInstanceType`: EC2 instance type (default: m6i.xlarge)

AWS Auto Scaling warm pools keep `warmPoolSize` instances stopped (paying EBS + SSM only). On scale-out, ASG resumes a stopped instance rather than launching cold, cutting scale-out time from ~3 minutes to ~60 seconds.

### Node Slot Identity

Each DTN slot has a pre-created GCS node identity (UUID + encryption key) stored in Secrets Manager at `elastic-gcs/{region}/node-slot-{N}`. This decouples GCS registration from EC2 instance lifecycle:

- AMIs are generic — no baked-in identity
- Failed instances are replaced without creating orphaned GCS nodes
- Slot locking uses `ssm:PutParameter` (atomic, fails if key exists) to prevent two instances from claiming the same slot

Slot locks are held for the full instance lifetime and released by the lifecycle Lambda on permanent termination.

### Lambda Functions

| Function | Trigger | Action |
|----------|---------|--------|
| `scaleout` | SNS (CloudWatch alarm) | `SetDesiredCapacity += 1`, capped at `maxDTNs` |
| `scalein` | SNS (CloudWatch alarm) | `SetDesiredCapacity -= 1`, guarded by `ActiveTransfers == 0` |
| `lifecycle` | ASG lifecycle hook | SSM `node disable` before stop; releases slot lock on termination |
| `demand_monitor` | EventBridge (1 min) | Polls Globus Transfer API, publishes `ElasticGCS/ActiveTransfers` |
| `cache_eviction` | EventBridge (1 h) | Terminates warm pool instances stopped > 72 h; sweeps orphaned GCS nodes |
| `version_check` | EventBridge (6 h) | Runs `globus-connect-server --version` on all nodes via SSM, publishes `ElasticGCS/GCSVersionDrift` |

### Scaling Triggers

```
Globus Transfer API
        │  (polled every 1 min by demand_monitor)
        ▼
ElasticGCS/ActiveTransfers (CloudWatch metric)
        │
        ├──► > 0 for 5 min  ──► scaleout alarm ──► SNS ──► scaleout Lambda
        │
        └──► == 0 for 15 min ──► scalein alarm  ──► SNS ──► scalein Lambda
```

## Key Design Decisions

### SSM Slot Locking (replaced EIPs)

Originally DTN slots were assigned via Elastic IPs (one EIP per slot, discovered by tag). This cost ~$3.60/slot/month at idle and required EIP quota.

Current design: each DTN claims a slot by calling `aws ssm put-parameter --name /elastic-gcs/{region}/slot-lock/{N} --value {instance-id}` without `--overwrite`. SSM rejects duplicate writes atomically. Slot locks are held for the instance lifetime and released by the lifecycle Lambda on termination.

Node IP is fetched from IMDS at boot; `node update --ip-address` handles IP registration dynamically.

### Pre-Created Node Identities

`globus-connect-server node create --export-node` generates a node UUID and encryption key without starting services. This file is stored in Secrets Manager and injected at instance boot. Benefits:

- AMIs are fully generic (no per-slot baking required)
- Node identity survives instance replacement — same UUID, new EC2 instance
- `node setup --import-node` (~23 s) is faster than full `node create` + `node setup`

### Warm Pool over Custom Stop/Start

AWS Auto Scaling warm pools provide lifecycle hooks, health checks, and `InstanceReusePolicy` (scale-in goes to stopped rather than terminated) as native features. The alternative — managing EC2 stop/start with a custom state machine — would replicate this complexity without the operational benefits.

## Observed Timings (us-east-1, m6i.xlarge)

| Phase | Time |
|-------|------|
| Cold boot: instance launch → AMI boot | ~90 s |
| Cold boot: `node setup --import-node` | ~23 s |
| Warm resume: instance start | ~30 s |
| Warm resume: `node update --enable` | ~7 s |
| **Warm scale-out: desired=1 → InService** | **~60 s** |
| Scale-in: `node disable` via SSM | ~30 s |
| DNS propagation after `node enable/disable` | up to 2 min |

## Metrics and Alarms

| Metric / Alarm | Namespace | Description |
|----------------|-----------|-------------|
| `ActiveTransfers` | `ElasticGCS` | Active Globus tasks touching this endpoint |
| `GCSVersionDrift` | `ElasticGCS` | Distinct GCS versions across nodes minus 1 (0 = healthy) |
| `scaleout` alarm | CloudWatch | ActiveTransfers > 0 for 5 min |
| `scalein` alarm | CloudWatch | ActiveTransfers == 0 for 15 min |
| `version-drift` alarm | CloudWatch | GCSVersionDrift > 0 |

All alarms route through SNS. The `scalein` alarm fires at idle (desired=0) as a known false positive — it is functionally correct but cosmetically noisy; the scalein Lambda is a no-op when desired is already 0.

## Cost Model

| State | Components | Approx. monthly cost |
|-------|-----------|---------------------|
| Fully idle | Control node (t3a.large) | ~$40/mo |
| Idle + warm pool (2) | Above + 2× stopped DTN EBS (100 GB gp3) | ~$56/mo |
| 1 active DTN | Above + 1× m6i.xlarge on-demand | ~$196/mo |
| 1 active DTN (Spot) | Above + 1× m6i.xlarge Spot (~70% savings) | ~$112/mo |

Warm pool provides faster scale-out (30–60 s vs 2–3 min cold) at the cost of ~$16/mo for EBS storage. Whether to use warm pool vs cold-start is a latency/cost trade-off. Spot instances are incompatible with warm pools (AWS constraint) — choose one or the other.
