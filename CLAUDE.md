# CLAUDE.md — Elastic Globus Connect Server on AWS

## Project Overview

This project implements an elastic, auto-scaling Globus Connect Server (GCS) v5 deployment on AWS using CDK in Go. The system scales from a minimal control node (~$12/month idle) to multiple high-throughput Data Transfer Nodes (DTNs) on demand, contracting back when transfers complete.

The primary use case is fronting the **AWS Open Data Registry** with Globus endpoints, enabling any researcher with a Globus account to discover and transfer public datasets. The architecture deploys to multiple AWS regions to colocate with Open Data buckets.

**Owner:** Scott Friedman (github.com/scttfrdmn)
**Target conference:** GlobusWorld 2026 (Chicago Apr 21-22, Raleigh Jun 2-3)

**Globus endpoint owner identity:** `scofri@globusid.org`
**Globus subscription ID:** `1e8d2d5b-d29f-46a1-b152-059a7c3c9971`
(Always use `scofri@globusid.org` when running `globus-connect-server login` — NOT `scttfrdmn@globusid.org`)

## Project Tracking

All work is tracked in GitHub:
- **Project board:** https://github.com/users/scttfrdmn/projects/27
- **Issues:** https://github.com/scttfrdmn/aws-elastic-gcs/issues
- **Milestones:** v0.2 (scaling fixes), v0.3 (cost/reliability), v0.4 (observability/multi-region)

| Milestone | Focus | Key Issues |
|-----------|-------|------------|
| v0.2 | Correct automatic scaling | #1 SNS fix, #2 MaxDTNs config, #3 demand monitor, #4 scale-in guard, #5 cache eviction TTL, #6 warm pool CDK |
| v0.3 | Cost & reliability | #7 Spot instances, #8 t4g.medium control node, #9 stale node cleanup, #10 S3 VPC endpoint |
| v0.4 | Observability & multi-region | #11 open_data.go, #12 CloudWatch dashboard, #13 structured logging, #14 version drift detection |

## Key Repositories

- **This repo:** CDK infrastructure + orchestration Lambda functions + operational tooling
- **github.com/scttfrdmn/globus-go-sdk** (v3 recommended for production, v4 for new projects): Go SDK for Globus platform services — Auth, Transfer, Groups, Search, Flows, Compute
- **github.com/scttfrdmn/aws-globus-connector**: High-performance S3 connector for GCS with multipart upload, transfer acceleration, all storage tiers
- **github.com/scttfrdmn/globus-gcs-s3-deployment**: Existing shell-based GCS S3 deployment scripts (reference implementation)

## Technology Stack

- **Language:** Go
- **Infrastructure:** AWS CDK v2 (Go bindings)
- **AWS Services:** EC2, Auto Scaling (with warm pools), Lambda, CloudWatch, Secrets Manager, S3, Elastic IPs, Systems Manager (SSM), EventBridge
- **Globus:** GCS v5.4+, S3 Connector, Globus Flows (for transfer event webhooks)
- **Globus SDK:** github.com/scttfrdmn/globus-go-sdk/v3

## Architecture

### Three-Tier Caching Model

```
COLD (terminated)          WARM (stopped)              HOT (running)
  No EC2 cost               EBS-only cost               Full compute cost
  2-3 min scale-out         30-60s scale-out             0s (already serving)
       │                         │                            │
       │  Launch from AMI        │  Resume instance           │  Already active
       │  + node setup           │  + node enable             │
       │  + import-node          │                            │
       ▼                         ▼                            ▼
   ┌─────────┐  stop+hold   ┌─────────┐  transfer    ┌─────────┐
   │  COLD   │ ──────────►  │  WARM   │ ──────────►  │   HOT   │
   └─────────┘              └─────────┘              └─────────┘
                                 ▲          cool-down     │
                                 │          (15-30 min)   │
                                 └────────────────────────┘
                            cache eviction
                            (72h timeout) ──► TERMINATE
```

### Component Layout

```
┌──────────────────────────────────────────────────────────────┐
│                     AWS Region (e.g. us-east-1)              │
│                                                              │
│  ┌──────────────┐     ┌──────────────────────────────────┐   │
│  │ Control Node │     │  ASG + Warm Pool                 │   │
│  │ (t4g.medium) │     │  ┌─────────┐  ┌─────────┐       │   │
│  │              │     │  │  DTN 1   │  │  DTN 2   │ ...  │   │
│  │ • GCS endpoint     │  │(stopped) │  │(stopped) │      │   │
│  │ • Node identity    │  │ + EIP    │  │ + EIP    │      │   │
│  │   pre-creation     │  └─────────┘  └─────────┘       │   │
│  │ • Orchestration    │                                  │   │
│  └──────┬───────┘     └──────────────────────────────────┘   │
│         │                                                    │
│  ┌──────▼───────┐     ┌──────────────────────────────────┐   │
│  │   Lambda     │     │  S3 (Open Data Buckets)          │   │
│  │ • Scale-out  │     │  • 1000genomes                   │   │
│  │ • Scale-in   │     │  • noaa-nexrad-level2            │   │
│  │ • Lifecycle  │     │  • openneuro.org                 │   │
│  └──────────────┘     └──────────────────────────────────┘   │
│                                                              │
└──────────────────────────────────────────────────────────────┘
```

## GCS v5 CLI Reference (Critical Commands)

All GCS CLI commands must run as root on the target node. The `deployment-key.json` is the master secret — guard it carefully.

### Endpoint Setup (one-time, on control node)
```bash
globus-connect-server endpoint setup "Elastic GCS - Open Data" \
  --organization "Your Org" \
  --owner owner@globus.org
# Produces: deployment-key.json (KEEP THIS SAFE)
```

### Node Identity Pre-creation (on control node, for each DTN slot)
```bash
# Pre-create node identity — outputs a JSON file with node ID + encryption key
globus-connect-server node create \
  --deployment-key deployment-key.json \
  --export-node /path/to/node-slot-N.json
```
This is the key enabler for elastic scaling. Node identities can be created ahead of time and stored in Secrets Manager. Each DTN slot gets a unique pre-created identity.

### Node Setup (on each DTN, first boot)
```bash
# Import the pre-created identity to join the endpoint
globus-connect-server node setup \
  --deployment-key deployment-key.json \
  --import-node /path/to/node-slot-N.json \
  --ip-address <PUBLIC_IP>
```
**Note:** `node setup` "may take several minutes to complete" per docs. This is the cold path.

### Node Enable/Disable (fast path for warm nodes)
```bash
# Disable before stopping instance (removes from DNS, stops serving)
sudo globus-connect-server node disable

# Enable after resuming instance (adds to DNS, resumes serving)
sudo globus-connect-server node enable
```
These commands run as root, no Globus identity auth needed. After enable, DNS propagation can take up to 2 minutes.

### Node Update (for IP changes if not using EIPs)
```bash
globus-connect-server node update NODE_ID \
  --ip-address <NEW_PUBLIC_IP> \
  --enable
```

### Node Lifecycle Warning
**CRITICAL:** "Failure to disable a node with `node update` or clean it up with `node cleanup` before shutting down a DTN can make your endpoint unreliable." Always disable before stop/terminate.

### Node Cleanup (permanent removal)
```bash
# Removes node from endpoint, disables all services, deletes local config
sudo globus-connect-server node cleanup
```
Use cleanup only for permanent removal. DNS changes take a few minutes to propagate after cleanup.

### Useful Diagnostic Commands
```bash
globus-connect-server node list          # Show all nodes with IPs and status
globus-connect-server node show          # Show this node's details
globus-connect-server self-diagnostic    # Check service health
```

### S3 Storage Gateway
```bash
globus-connect-server storage-gateway create s3 \
  "AWS Open Data Gateway" \
  --domain globusid.org \
  --s3-endpoint https://s3.<region>.amazonaws.com \
  --bucket <bucket-name> \
  --s3-unauthenticated   # for public buckets; use --s3-user-credential for requester-pays
```

### GCS Minimum Requirements
- **RAM:** 8GB minimum
- **Ports:** TCP 443 (control), TCP 50000-51000 (data channels)
- **OS:** RHEL 8/9, Rocky, Alma, Ubuntu, Fedora (x86-64)
- **Software version:** All nodes in an endpoint MUST run the same GCS version

## Project Structure

```
elastic-gcs/
├── CLAUDE.md                          # This file
├── README.md                          # Project README
├── go.mod
├── go.sum
├── cdk.json                           # CDK app configuration
├── cdk.go                             # CDK app entrypoint
│
├── stacks/
│   ├── network.go                     # VPC, subnets, security groups
│   ├── control_node.go                # Control node EC2 + GCS endpoint setup
│   ├── dtn_pool.go                    # ASG + warm pool + EIPs for DTNs
│   ├── orchestration.go               # Lambda functions + EventBridge rules
│   └── open_data.go                   # S3 storage gateway configs per Open Data bucket
│
├── lambda/
│   ├── scaleout/
│   │   └── main.go                    # Handles scale-out triggers
│   ├── scalein/
│   │   └── main.go                    # Handles scale-in + lifecycle hooks
│   ├── lifecycle/
│   │   └── main.go                    # ASG lifecycle hook handler (disable before stop)
│   └── cache_eviction/
│       └── main.go                    # Terminates warm pool instances past TTL
│
├── scripts/
│   ├── userdata/
│   │   ├── control_node_init.sh       # Control node bootstrap (install GCS, endpoint setup)
│   │   ├── dtn_first_boot.sh          # DTN first boot (node setup --import-node)
│   │   └── dtn_resume.sh              # DTN resume from stopped (node enable)
│   ├── build_ami.sh                   # Packer or EC2 Image Builder script for DTN AMI
│   └── create_node_identities.sh      # Pre-create N node identities on control node
│
├── config/
│   ├── open_data_buckets.yaml         # Registry of Open Data buckets per region
│   └── regions.yaml                   # Target regions and instance type preferences
│
├── cmd/
│   └── aws-elastic-gcs-ctl/
│       └── main.go                    # CLI tool for manual operations (check status, force scale, etc.)
│
├── internal/
│   ├── gcs/
│   │   └── client.go                  # Wrapper for GCS CLI operations via SSM Run Command
│   ├── scaling/
│   │   └── policy.go                  # Scale-out/in decision logic
│   └── metrics/
│       └── collector.go               # Collect and publish custom CloudWatch metrics
│
└── test/
    ├── integration/
    │   ├── node_lifecycle_test.go      # Test: disable → stop → resume → enable
    │   ├── transfer_test.go            # Test: end-to-end transfer via elastic endpoint
    │   └── scaling_test.go             # Test: scale-out latency measurement
    └── benchmark/
        └── timing_test.go             # Benchmark: measure each phase of scale-out
```

## Implementation Status

### v0.1 — Completed ✅
Single-region deployment with control node, ASG + warm pool, Lambda lifecycle hooks, EIP-based node slot assignment, boot/shutdown scripts with GCS node disable/enable, lock file fix, and validated end-to-end Globus transfer against the 1000 Genomes Open Data bucket.

**What works:**
- Control node boots, runs GCS endpoint setup, pre-creates node identities in Secrets Manager
- DTN warm pool (2 stopped instances) with EIP slot assignment via tags
- Scale-out: DTN resumes in ~60s, `node enable` succeeds, transfer completes
- Scale-in: lifecycle hook calls `node disable` before instance stops
- Lock file fix: `chown gcsweb:gcsweb subscription_pause` after each root GCS CLI call

**Known gaps → see v0.2 issues:**
- Scale-out alarm never fires (demand monitor Lambda missing, `ActiveTransfers` metric not published)
- Scale-in alarm fires immediately after scale-out (no active transfer guard)
- `MaxDTNs` config doesn't exist; ASG max capped at `warmPoolSize + 1 = 3`
- Warm pool `MinSize=2` reset on every `cdk deploy` (no CDK custom resource)

### v0.2 — Correct Automatic Scaling
See GitHub milestone [v0.2](https://github.com/scttfrdmn/aws-elastic-gcs/milestone/1) and issues [#1–#6](https://github.com/scttfrdmn/aws-elastic-gcs/issues).

### v0.3 — Cost & Reliability
See GitHub milestone [v0.3](https://github.com/scttfrdmn/aws-elastic-gcs/milestone/2) and issues [#7–#10](https://github.com/scttfrdmn/aws-elastic-gcs/issues).

### v0.4 — Observability & Multi-Region
See GitHub milestone [v0.4](https://github.com/scttfrdmn/aws-elastic-gcs/milestone/3) and issues [#11–#14](https://github.com/scttfrdmn/aws-elastic-gcs/issues).

## Key Design Decisions

### Why EIPs for DTNs
EC2 instances get new public IPs after stop/start. GCS nodes are registered with specific IPs in DNS. Without EIPs, every resume requires `node update --ip-address` which adds latency and complexity. EIPs are free when attached to a running instance and $3.60/month when not. For 2-3 warm DTN slots, this is negligible.

### Why Pre-Created Node Identities
`node create --export-node` generates the node ID and encryption key without deploying services. This decouples identity provisioning from instance lifecycle. Benefits:
- AMIs can be generic (no baked-in identity)
- Node identity files stored in Secrets Manager, injected at boot
- Failed instances can be replaced without creating new identities
- The node slot concept maps cleanly to ASG desired capacity

### Why ASG Warm Pool (Not Custom Stop/Start)
ASG warm pools are the native AWS mechanism for exactly this pattern. Benefits over custom EC2 stop/start:
- Built-in lifecycle hooks for pre-stop and post-resume
- `InstanceReusePolicy` automatically routes scale-in to stopped instead of terminated
- `MaxGroupPreparedCapacity` caps warm pool size
- Health checks and replacement are automatic
- No custom state machine needed

### Why t3a.large for Control Node
GCS requires 8GB RAM minimum and publishes no aarch64 RPMs, so Graviton is out for the Rocky Linux AMI. t3a.large (2 vCPU, 8GB, x86-64) at $0.0752/hr (~$55/month) is the cheapest x86-64 type meeting the memory requirement. The control node does minimal work — it just needs to stay registered and respond to GCS Manager API calls. See `docs/instance-sizing.md`.

### DTN Instance Type Strategy
The bottleneck is sustained internet-gateway throughput: most types under 32 vCPUs are capped at 5 Gbps to the internet regardless of their "up to" rating; the `*8in` families get their full baseline. See `docs/instance-sizing.md` and `scripts/instance_sizing.py`. Recommended DTN types (us-east-1, Nov 2026):
- **Light:** m8in.xlarge (6.25 Gbps sustained, $0.334/hr)
- **Default:** c8in.2xlarge (12.5 Gbps sustained, $0.544/hr)
- **Heavy:** c8in.4xlarge (25 Gbps sustained, $1.089/hr)

The DTN instance type should be configurable per-region in `config/regions.yaml`.

## Environment Variables and Secrets

### Secrets Manager Keys
- `elastic-gcs/<region>/<deployment>/deployment-key` — The deployment-key.json
- `elastic-gcs/<region>/<deployment>/node-slot-<N>` — Pre-created node identity JSON files
- `elastic-gcs/<region>/<deployment>/globus-client-id` — Globus client ID
- `elastic-gcs/<region>/<deployment>/globus-client-secret` — Globus client secret

### SSM Parameters
- `/elastic-gcs/<region>/<deployment>/endpoint-id` — Globus endpoint UUID
- `/elastic-gcs/<region>/<deployment>/control-node-id` — Control node EC2 instance ID
- `/elastic-gcs/<region>/<deployment>/dtn-ami-id` — Current DTN AMI ID

### CDK Context / Config
- Target regions
- Open Data bucket list per region
- Globus subscription ID
- Globus endpoint owner identity
- DTN instance types per region
- Warm pool size (max prepared capacity)
- Cool-down period (minutes)
- Cache eviction TTL (hours)

## Testing Strategy

### Validated (v0.1)
- Node lifecycle (disable → stop → resume → enable) via warm pool scale events
- End-to-end transfer: 1000 Genomes → Globus Tutorial Collection 1 (15KB file, 3s, SUCCEEDED)
- Scale-out latency: ~60s DTN InService, ~30s node enable → first transfer

### Integration Tests (planned)
- Programmatic node lifecycle via globus-go-sdk assertions
- Transfer throughput at 1, 2, 3 concurrent DTNs
- Scale-out latency cold vs warm vs hot path timing

### Benchmarks (GlobusWorld 2026 presentation)
- Instance resume timing per instance type
- GCS service start + node enable + DNS propagation breakdown
- Throughput comparison: elastic vs static for bursty academic workload
- Cost model: elastic vs always-on for various utilization patterns

## Important Notes

1. **GCS version consistency:** All nodes in an endpoint MUST run the same GCS version. The AMI build process must ensure this. Rolling updates need the documented disable → upgrade → enable sequence.

2. **DNS propagation:** After `node enable` or `node disable`, there may be "intermittent errors for up to two minutes while DNS changes propagate." The orchestration should account for this — don't declare a node ready until DNS confirms its IP.

3. **Globus subscription required:** Multi-node endpoints, guest collections, and the S3 connector all require a managed endpoint under a Globus subscription.

4. **8GB RAM minimum:** Don't try to use t4g.small (2GB) or t4g.micro. GCS requires 8GB. t4g.medium (8GB) is the floor.

5. **Data channel ports:** Security groups must allow TCP 50000-51000 inbound and outbound for GridFTP data channels, plus TCP 443 for the control channel.

6. **Open Data bucket permissions:** Test access patterns carefully. Some Open Data buckets are fully public, some are requester-pays. The IAM role needs to handle both.

7. **`--export-node` / `--import-node` docs say "Contact Globus Support if you think you need this."** This is worth doing — reach out to Globus support to confirm this is a supported pattern for elastic deployments. Given Scott's connections with the GlobusWorld organizers, this conversation could also feed into the conference talk.
