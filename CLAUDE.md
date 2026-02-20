# CLAUDE.md — Elastic Globus Connect Server on AWS

## Project Overview

This project implements an elastic, auto-scaling Globus Connect Server (GCS) v5 deployment on AWS using CDK in Go. The system scales from a minimal control node (~$12/month idle) to multiple high-throughput Data Transfer Nodes (DTNs) on demand, contracting back when transfers complete.

The primary use case is fronting the **AWS Open Data Registry** with Globus endpoints, enabling any researcher with a Globus account to discover and transfer public datasets. The architecture deploys to multiple AWS regions to colocate with Open Data buckets.

**Owner:** Scott Friedman (github.com/scttfrdmn)
**Target conference:** GlobusWorld 2026 (Chicago Apr 21-22, Raleigh Jun 2-3)

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
│   └── elastic-gcs-ctl/
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

## Implementation Phases

### Phase 1: Single Node Baseline (START HERE)
**Goal:** Get GCS running on one EC2 instance with S3 connector fronting an Open Data bucket.

1. Create CDK stack for: VPC, security group (TCP 443 + 50000-51000), single EC2 instance (t4g.medium)
2. UserData script installs GCS, runs `endpoint setup`, `node setup`
3. Configure S3 storage gateway pointing at a public Open Data bucket
4. Create a mapped collection
5. **Validation:** Browse and transfer files via Globus web app

**Open Data bucket for initial test:** `1000genomes` in us-east-1 (well-known, large, genomics audience)

**IAM considerations for Open Data:**
- Public buckets: read-only access, no special IAM beyond standard S3 GetObject
- Requester-pays buckets: need s3:GetObject + s3:GetBucketRequestPayment
- Create an IAM role for the GCS instance with scoped S3 read permissions

### Phase 2: Multi-Node + Node Lifecycle Validation
**Goal:** Prove that node create → import → disable → stop → resume → enable works.

1. On control node, run `node create --export-node` to pre-create a DTN identity
2. Launch second EC2 instance, run `node setup --import-node`
3. Verify both nodes in `node list`, both active
4. Run a transfer, check which node(s) handle it
5. `node disable` on DTN → stop instance → resume → `node enable`
6. Verify DTN rejoins and serves transfers
7. **Measure and record timing of each step**

### Phase 3: ASG + Warm Pool Automation
**Goal:** Automate the scale-out/in lifecycle.

1. Build DTN AMI with GCS pre-installed (use EC2 Image Builder or Packer)
2. Pre-create node identities, store in Secrets Manager
3. CDK stack: ASG with warm pool, EIP allocation per DTN slot
4. Lambda lifecycle hooks: disable before stop, enable after resume
5. CloudWatch alarm or Globus webhook trigger for scale-out
6. Cache eviction Lambda (terminate warm instances after 72h)

### Phase 4: Multi-Region Open Data
**Goal:** Deploy to multiple regions to colocate with Open Data.

1. Parameterize CDK stacks by region
2. Create `config/open_data_buckets.yaml` mapping buckets to regions
3. Deploy to us-east-1, us-west-2, eu-west-1 (start with these)
4. Optionally: build a routing layer / Globus Flow that directs users to the nearest endpoint

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

### Why t4g.medium for Control Node
GCS requires 8GB RAM minimum. t4g.medium provides 4 vCPUs + 8GB at ~$0.0168/hr (~$12/month) on Graviton. It's the cheapest instance meeting the GCS memory requirement. The control node does minimal work — it just needs to stay registered and respond to GCS Manager API calls.

### DTN Instance Type Strategy
For S3 connector workloads, the bottleneck is network throughput, not CPU or memory. Recommended DTN types:
- **Light transfers:** m6i.xlarge (up to 12.5 Gbps, ~$0.192/hr)
- **Heavy transfers:** m6in.xlarge (up to 30 Gbps, ~$0.292/hr)  
- **Maximum throughput:** c6in.2xlarge (up to 40 Gbps, ~$0.432/hr)

The DTN instance type should be configurable per-region in `config/regions.yaml`.

## Environment Variables and Secrets

### Secrets Manager Keys
- `elastic-gcs/<region>/deployment-key` — The deployment-key.json
- `elastic-gcs/<region>/node-slot-<N>` — Pre-created node identity JSON files
- `elastic-gcs/<region>/globus-client-id` — Globus client ID
- `elastic-gcs/<region>/globus-client-secret` — Globus client secret

### SSM Parameters
- `/elastic-gcs/<region>/endpoint-id` — Globus endpoint UUID
- `/elastic-gcs/<region>/control-node-id` — Control node EC2 instance ID
- `/elastic-gcs/<region>/dtn-ami-id` — Current DTN AMI ID

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

### Integration Tests
- **Node lifecycle:** Programmatically run through disable → stop → resume → enable, assert node status
- **Transfer end-to-end:** Submit transfer via globus-go-sdk, verify completion
- **Scale-out latency:** Trigger scale-out, measure wall-clock time to first successful transfer on new DTN

### Benchmarks (for GlobusWorld presentation)
- Time each phase: instance resume, GCS service start, node enable, DNS propagation, first transfer byte
- Compare cold vs warm vs hot paths
- Measure throughput with 1, 2, 3 DTNs active (parallel transfers)
- Cost comparison: elastic vs static for realistic academic workload patterns

## Important Notes

1. **GCS version consistency:** All nodes in an endpoint MUST run the same GCS version. The AMI build process must ensure this. Rolling updates need the documented disable → upgrade → enable sequence.

2. **DNS propagation:** After `node enable` or `node disable`, there may be "intermittent errors for up to two minutes while DNS changes propagate." The orchestration should account for this — don't declare a node ready until DNS confirms its IP.

3. **Globus subscription required:** Multi-node endpoints, guest collections, and the S3 connector all require a managed endpoint under a Globus subscription.

4. **8GB RAM minimum:** Don't try to use t4g.small (2GB) or t4g.micro. GCS requires 8GB. t4g.medium (8GB) is the floor.

5. **Data channel ports:** Security groups must allow TCP 50000-51000 inbound and outbound for GridFTP data channels, plus TCP 443 for the control channel.

6. **Open Data bucket permissions:** Test access patterns carefully. Some Open Data buckets are fully public, some are requester-pays. The IAM role needs to handle both.

7. **`--export-node` / `--import-node` docs say "Contact Globus Support if you think you need this."** This is worth doing — reach out to Globus support to confirm this is a supported pattern for elastic deployments. Given Scott's connections with the GlobusWorld organizers, this conversation could also feed into the conference talk.
