# Architecture

Elastic GCS deploys a [Globus Connect Server v5](https://docs.globus.org/globus-connect-server/) endpoint on AWS with automatic scaling from near-zero idle cost to multi-node high-throughput data transfer, then back to idle when transfers complete.

## DTN slots and three tiers

A deployment has `max_dtns` DTN **slots**. A slot is a persistent GCS node
identity (pre-created by the control node), an Elastic IP, and, when it isn't
COLD, an EC2 instance. Each slot is in one of three tiers:

```
COLD (no instance)         WARM (stopped)              HOT (running)
  No EC2 cost               EBS + Elastic IP only        Full compute cost
  ~1 min to set up          ~1.5 min to serving          0 s (already serving)
       │                         │                              │
       │  Launch from AMI        │  Start + node enable         │  In endpoint DNS
       │  + node setup           │  (boot unit)                 │
       ▼                         ▼                              ▼
   ┌─────────┐   revive     ┌─────────┐    start        ┌─────────┐
   │  COLD   │ ──────────►  │   HOT   │ ◄────────────── │  WARM   │
   └─────────┘              └─────────┘ ──────────────► └─────────┘
        ▲                                drain + stop        │
        │                                                    │
        └──────────── evict after cold_ttl (beyond warm_slots)
```

**Cold path (revive):** the controller launches an instance from the DTN AMI
in the slot's subnet, attaches the slot's Elastic IP, swaps the instance to the
setup profile, runs `node setup --import-node` with the slot's identity, swaps
back to the runtime profile, and revokes setup-role sessions. Measured: 47 s
from launch to node setup done; node setup itself ~18 s.

**Warm path (start):** the controller starts a stopped slot; its boot unit runs
`node enable` and waits for the slot's IP in endpoint DNS. Measured: ~95 s from
desired-dtns rising to healthy and in DNS.

**Hot path:** the slot is already serving — 0 s additional latency.

**Scale-in (drain):** the controller runs `node disable` on the slot (it leaves
endpoint DNS, so new sessions go elsewhere), waits for its GridFTP data
connections to reach 0 (or `drain_timeout`), then stops it. In-flight sessions
finish on the draining node (see [GridFTP restarts](#gridftp-restarts)).

## Components

```
┌────────────────────────────────────────────────────────────────┐
│                     AWS Region                                  │
│                                                                 │
│  ┌───────────────┐    ┌──────────────────────────────────────┐  │
│  │ Control Node  │    │  DTN slots (owned by the controller)  │  │
│  │ (t3a.large)   │    │  ┌──────────┐  ┌──────────┐          │  │
│  │               │    │  │ slot 1   │  │ slot 2   │  ...     │  │
│  │ • GCS endpoint│    │  │ HOT/WARM │  │  COLD    │          │  │
│  │ • Node idents │    │  │ + EIP    │  │          │          │  │
│  │ • GCS manager │    │  └──────────┘  └──────────┘          │  │
│  └───────┬───────┘    └──────────────────────────────────────┘  │
│          │                                                      │
│  ┌───────▼──────────────────────────────────────────────────┐  │
│  │  Lambda Functions                                         │  │
│  │  • demand_monitor  — sets desired-dtns from Transfer      │  │
│  │  • slot_controller — durable function: converges slots    │  │
│  │                      to desired-dtns                      │  │
│  └──────────────────────────────────────────────────────────┘  │
│                                                                 │
│  ┌───────────────────┐  ┌──────────────────────────────────┐   │
│  │ SSM Parameter     │  │ Secrets Manager                  │   │
│  │ • control-node-id │  │ • deployment-key                 │   │
│  │ • endpoint-id     │  │ • node-slot-1..N                 │   │
│  │ • desired-dtns    │  │ • globus-client-id/secret        │   │
│  │ • controller-lock │  └──────────────────────────────────┘   │
│  └───────────────────┘                                          │
└────────────────────────────────────────────────────────────────┘
```

### Control Node

A single `t3a.large` EC2 instance (x86-64 required — GCS 5.4 has no aarch64
packages) with its own Elastic IP. Responsible for:

- Hosting the GCS endpoint (Globus Manager API, data channel)
- Pre-creating DTN node identities (`globus-connect-server node create --export-node`), one per slot
- Storing node identity JSON files in Secrets Manager
- Running GCS CLI commands on behalf of `aws-elastic-gcs-ctl` via SSM Run Command

It enables itself on boot and disables itself on shutdown
(`elastic-gcs-control.service`). `scripts/control_node_power.sh` stops it between
sessions.

### DTN slots

Data Transfer Nodes handle the transfer I/O. Settings live in `regions.yaml`
(see [Configuration](configuration.md)):

- `max_dtns`: number of slots (default 4)
- `warm_slots`: WARM slots kept after `cold_ttl` (cost-profile default)
- `dtn_instance` and `instance_types`: EC2 types in fallback order (default c8in.2xlarge; see [Instance Sizing](instance-sizing.md))

CloudFormation creates only the launch template, the roles and instance
profiles, and the controller. Instances and Elastic IPs belong to the
controller, tagged `elastic-gcs:pool=slots`, so a deploy never resizes or
replaces them.

### Node identity delivery

Each slot has a pre-created GCS node identity (UUID + encryption key) in Secrets
Manager at `elastic-gcs/{region}/{deployment}/node-slot-{N}`. A DTN's runtime
role can't read secrets. For the one-time `node setup`, the controller swaps the
instance to a setup profile that can, runs setup over SSM, swaps back, and
revokes every setup-role session issued before the swap-back (verified live:
copied credentials stop working about 5 s later).

### Lambda Functions

| Function | Trigger | Action |
|----------|---------|--------|
| `demand_monitor` | EventBridge (1 min) | Polls Globus Transfer for the endpoint's ACTIVE tasks, publishes `ElasticGCS/ActiveTransfers`; raises `desired-dtns` while transfers are active and sets it to 0 after `scale_in_idle` |
| `slot_controller` | EventBridge (1 min) and `desired-dtns` changes | Lambda durable function (via its `live` alias). Holds `controller-lock`; observes slots (instances, SSM health, endpoint DNS, GCS services, data connections), plans actions with `internal/slots`, and executes them |

### Controller rules

`internal/slots.Plan` applies, in order: finish in-flight actions (stop drained
slots); health (restart an unhealthy slot, rebuild after `max_restarts`, rebuild
an abandoned revive); scale out (WARM slots first, one COLD revive at a time);
scale in (one slot at a time, fewest data connections; never the last DTN unless
the control node is serving — it starts the control node first); the control
node's place in the data path (below); evict WARM slots beyond `warm_slots`
after `cold_ttl`.

**The control node leaves the data path while DTNs serve (#146).** Globus
spreads a task's sessions across every enabled node in endpoint DNS with no
regard to capacity, so a t3a.large control node next to c8in DTNs would carry an
equal share of every task. Once a DTN has been healthy for
`control_demote_after` (default 2 minutes) and DTNs are wanted, the controller
runs a local `node disable` on the control node; its in-flight sessions finish
(`KillMode=process`, #141) and new ones go to the DTNs. It is enabled again at
once when no DTN is healthy, and before the last DTN drains. A control node
stopped on purpose (`control_node_power.sh stop`) is left stopped until the
last-DTN guard needs it.

A slot is healthy when it is running, its SSM agent is online, its IP is in
endpoint DNS, and `gcs_manager` and `globus-gridftp-server` are active.

## Key Design Decisions

### Slots with Elastic IPs

Each slot keeps one Elastic IP for its lifetime, so a slot's node identity is
always registered at the same address: a WARM start needs no `node update
--ip-address`, and the endpoint never has a stale IP for a slot. An Elastic IP
costs ~$3.60/month while allocated; evicting a slot releases it.

### Pre-Created Node Identities

`globus-connect-server node create --export-node` generates a node UUID and encryption key without starting services. This file is stored in Secrets Manager and injected at setup. Benefits:

- AMIs are fully generic (no per-slot baking required)
- Node identity survives instance replacement — same UUID, new EC2 instance
- `node setup --import-node` (~18 s) is faster than full `node create` + `node setup`

### Controller instead of an ASG and warm pool

The first design used an Auto Scaling group with a warm pool and lifecycle
hooks. It was replaced (#65, removed in #133) because the ASG couldn't express
what a GCS endpoint needs: per-slot identities and addresses, draining by data
connections, never stopping the last serving node, one-at-a-time identity
delivery, and deploys that never touch running DTNs.

### GridFTP restarts

GCS restarts `globus-gridftp-server` on every node whenever a node joins or
leaves the endpoint, and on `node disable`. With the unit's default
`KillMode=control-group` that killed every in-flight session. The DTN AMI and
the control node install a `KillMode=process` drop-in
(`scripts/configure_gridftp.sh`, #141): only the listener restarts, and
sessions finish in their own processes.

## Observed Timings (us-west-2, c8in.2xlarge, November 2026)

| Phase | Time |
|-------|------|
| Cold revive: launch → node setup done | ~47 s |
| Cold revive: `node setup --import-node` | ~18 s |
| Warm start: desired-dtns 0→1 → healthy and in DNS | ~95 s |
| Demand: task submitted → slot launched (cold) | ~36 s |
| Scale-in: drain (0 data connections) → stopped | ~60 s |
| DNS propagation after `node enable/disable` | up to 2 min |

## Metrics

| Metric | Namespace | Description |
|--------|-----------|-------------|
| `ActiveTransfers` | `ElasticGCS` | Active Globus tasks touching this endpoint |
| `TransferThroughput` | `ElasticGCS` | Sum of effective bytes/s over ACTIVE tasks |
| `QueuedTransfers` | `ElasticGCS` | INACTIVE (suspended) tasks; observability only |

Alarms, the operator alert topic and the dashboard aren't built for slots yet
(#144).

## Zero nodes

Globus Transfer rejects a task submission outright when the endpoint has no
serving node (`502 ExternalError … Error validating login`), so no task exists
for demand to see. With `control_node: always` the control node accepts
submissions; `on_demand` and `none` need an explicit wake first (#130).

## Cost Model

| State | Components | Approx. monthly cost |
|-------|-----------|---------------------|
| Fully idle | Control node (t3a.large) + EIP | ~$59/mo |
| Idle + 2 WARM slots | Above + 2× stopped DTN EBS (20 GiB gp3) + 2× EIP | ~$70/mo |
| 1 DTN always on | Above + 1× c8in.2xlarge on-demand | ~$467/mo |
| Elastic, 1 DTN 40 h/month | Idle + 2 WARM slots + 40 h × $0.544 | ~$92/mo |

Internet egress ($50–90/TB) dominates the cost of any transfer; see
[Instance Sizing](instance-sizing.md). WARM slots trade ~$5/month each (EBS +
Elastic IP) for a faster first transfer than a COLD revive.
