# CLAUDE.md — Elastic Globus Connect Server on AWS

## Project Overview

This project implements an elastic, auto-scaling Globus Connect Server (GCS) v5 deployment on AWS using CDK in Go. The system scales from a minimal control node (~$6/month stopped, ~$55/month running) to multiple high-throughput Data Transfer Nodes (DTNs) on demand, contracting back when transfers complete.

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
- **Milestones** (minor releases only while the project has no users):
  - v0.12: operational & security fixes
  - v0.13: refactor, test coverage & hardening
  - v0.14: slot controller replacing the ASG + warm pool (#65)
- **Releases:** tags and GitHub Releases match `CHANGELOG.md`
- **Site:** https://scttfrdmn.github.io/aws-elastic-gcs/ (built from `docs/` on every push to `main`)

## Key Repositories

- **This repo:** CDK infrastructure + orchestration Lambda functions + operational tooling
- **github.com/scttfrdmn/globus-go-sdk** (v3 recommended for production, v4 for new projects): Go SDK for Globus platform services — Auth, Transfer, Groups, Search, Flows, Compute
- **github.com/scttfrdmn/aws-globus-connector**: High-performance S3 connector for GCS with multipart upload, transfer acceleration, all storage tiers
- **github.com/scttfrdmn/globus-gcs-s3-deployment**: Existing shell-based GCS S3 deployment scripts (reference implementation)

## Technology Stack

- **Language:** Go
- **Infrastructure:** AWS CDK v2 (Go bindings)
- **AWS Services:** EC2, Lambda (durable functions), CloudWatch, Secrets Manager, S3, Elastic IPs, Systems Manager (SSM), EventBridge, IAM
- **Globus:** GCS v5.4+, S3 Connector, Globus Flows (for transfer event webhooks)
- **Globus SDK:** github.com/scttfrdmn/globus-go-sdk/v3

## Architecture

### DTN Slots and Three Tiers

A deployment has `max_dtns` DTN slots. A slot is a pre-created node identity,
an Elastic IP and (unless COLD) an instance, all owned by the slot controller.

```
COLD (no instance)         WARM (stopped)              HOT (running)
  No EC2 cost               EBS + Elastic IP            Full compute cost
  ~2.5 min to serving       ~1.5 min to serving         0s (already serving)
       │                         │                            │
       │  Launch from AMI        │  Start instance            │  Already active
       │  + node setup           │  + node enable (boot unit) │
       │  (--import-node)        │                            │
       ▼                         ▼                            ▼
   ┌─────────┐   revive     ┌─────────┐    start       ┌─────────┐
   │  COLD   │─────────────►│   HOT   │◄───────────────│  WARM   │
   └─────────┘              └─────────┘───────────────►└─────────┘
        ▲                                 drain + stop      │
        └──────────── evict after cold_ttl (beyond warm_slots)
```

### Component Layout

```
┌──────────────────────────────────────────────────────────────┐
│                     AWS Region (e.g. us-east-1)              │
│                                                              │
│  ┌──────────────┐     ┌──────────────────────────────────┐   │
│  │ Control Node │     │  DTN slots (controller-owned)    │   │
│  │ (t3a.large)  │     │  ┌─────────┐  ┌─────────┐       │   │
│  │              │     │  │ slot 1  │  │ slot 2  │ ...   │   │
│  │ • GCS endpoint     │  │HOT/WARM │  │  COLD   │       │   │
│  │ • Node identity    │  │ + EIP   │  │         │       │   │
│  │   pre-creation     │  └─────────┘  └─────────┘       │   │
│  └──────┬───────┘     └──────────────────────────────────┘   │
│         │                                                    │
│  ┌──────▼──────────┐  ┌──────────────────────────────────┐   │
│  │   Lambda        │  │  S3 (Open Data Buckets)          │   │
│  │ • demand_monitor│  │  • 1000genomes                   │   │
│  │ • slot_controller  │  • noaa-nexrad-level2            │   │
│  │   (durable)     │  │  • openneuro.org                 │   │
│  └─────────────────┘  └──────────────────────────────────┘   │
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
aws-elastic-gcs/
├── cdk.go, cdk.json                   # CDK app: one control + one DTN pool stack per (region, deployment)
├── mkdocs.yml                         # Project site (docs/ → GitHub Pages)
├── stacks/
│   ├── control_node.go                # VPC, security group, control node EC2 + EIP, boot unit
│   ├── dtn_pool.go, slot_pool.go      # DTN stack: launch template, slot roles/profiles,
│   │                                  # slot controller + triggers; dtn_lambdas.go: demand monitor
│   └── iam_scope.go                   # IAM statements confined to one deployment
├── lambda/                            # slot_controller (durable), demand_monitor; version_check,
│                                      # waker, idle_shutdown (not deployed yet: #144, #130)
├── internal/
│   ├── slots/                         # Pure reconciler: Plan over HOT/WARM/COLD slots
│   ├── slotctl/                       # Controller: observe slots, execute actions, lock
│   ├── identity/                      # One-time node identity delivery (profile swap + revoke)
│   ├── names/                         # All names/paths/tags from (region, deployment)
│   ├── gcsid/                         # UUID validation before IDs reach shell commands
│   └── logger/                        # Structured logging
├── cmd/aws-elastic-gcs-ctl/           # Operator CLI (status, nodes, collections, deprovision, ...)
├── config/                            # regions.yaml, open_data_buckets.yaml (+ examples)
├── scripts/
│   ├── userdata/                      # control_node_init.sh, control_node_boot.sh (node enable
│   │                                  # on boot / disable on shutdown, also used by slots),
│   │                                  # elastic-gcs-control.service
│   ├── build_ami.sh                   # DTN AMI per GCS version → SSM parameter
│   ├── configure_tls.sh, configure_gridftp.sh  # shared by the DTN AMI and control node
│   ├── control_node_power.sh          # stop/start/install the control node between sessions
│   ├── instance_sizing.py, throughput_model.py
├── docs/                              # Guides, performance study, blog, talk outline (the site)
└── test/
    ├── integration/                   # Live tests: slot scale out/in, transfers
    └── benchmark/                     # timing tests, harness/ (two-deployment campaign), results/
```

## Implementation Status

Status lives on GitHub (issues, milestones, project board); see `CHANGELOG.md`
for what each release contains. Stable facts worth knowing:
- **Multiple deployments per region** (`regions.yaml` `deployment:`). The
  November 2026 benchmarks ran two deployments, `src` and `dst`, in us-west-2.
- **The slot controller** (#65; the ASG and warm pool were removed in #133)
  is a Lambda durable function, invoked through its `live` alias every minute
  and on `desired-dtns` changes. `demand_monitor` writes `desired-dtns`. Slot
  instances and Elastic IPs are tagged `elastic-gcs:pool=slots` and belong to
  the controller, not CloudFormation.
- **Slot health** = running, SSM online, IP in endpoint DNS, and `gcs_manager`
  plus `globus-gridftp-server` active. Revives run one at a time.
- **GridFTP restarts.** GCS restarts GridFTP on every node at each node join or
  leave; the `KillMode=process` drop-in (`configure_gridftp.sh`, #141) keeps
  in-flight sessions alive, so drains are graceful.
- **Zero nodes.** Transfer rejects submissions to an endpoint with no serving
  node, so demand can't wake an endpoint from zero (#130).
- **The control node** enables itself on boot and disables itself on shutdown
  (`elastic-gcs-control.service`). `scripts/control_node_power.sh` stops it
  between sessions.
- **Performance** (`docs/performance/`): a single c8in.2xlarge DTN sustains
  ~9.2 Gbps, and the peer endpoint's `network_use` setting usually sets a task's
  speed.

## Key Design Decisions

### Why an Elastic IP per slot
Each slot keeps one Elastic IP for its lifetime, so its node identity is always
registered at the same address: a WARM start needs no `node update
--ip-address`, and DTNs never need the endpoint-admin client secret (#42). An
EIP costs ~$3.60/month while allocated; evicting a slot releases it. The first
design used SSM slot locks and no EIPs (#15), which forced the IP update on
every resume. The control node also has an EIP.

### Why Pre-Created Node Identities
`node create --export-node` generates the node ID and encryption key without deploying services. This decouples identity provisioning from instance lifecycle. Benefits:
- AMIs can be generic (no baked-in identity)
- Node identity files stored in Secrets Manager, delivered once at node setup
  (the controller swaps to a setup profile, then back, and revokes setup
  sessions; the runtime role can't read secrets)
- Failed instances can be replaced without creating new identities
- One identity per slot

### Why a slot controller instead of an ASG warm pool
The project first used an ASG with a warm pool and lifecycle hooks. It was
replaced (#65) and removed (#133) because the ASG couldn't express what a GCS
endpoint needs: per-slot identities and addresses, draining by GridFTP data
connections, never stopping the last serving node, one-at-a-time identity
delivery, and deploys that never resize or replace running DTNs. The
controller's rules are a pure, table-tested function (`internal/slots.Plan`).

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
- `/elastic-gcs/<region>/<deployment>/desired-dtns` — DTNs the slot controller converges to (written by demand_monitor)
- `/elastic-gcs/<region>/<deployment>/last-active` — Last time demand_monitor saw an active transfer
- `/elastic-gcs/<region>/<deployment>/controller-lock` — One slot-controller execution at a time
- `/elastic-gcs/<region>/<deployment>/endpoint-domain` — Endpoint DNS name, for health checks
- `/elastic-gcs/<region>/dtn-ami/gcs-<version>` — DTN AMI for a GCS version (region-wide, shared by deployments)

### CDK Context / Config
See `docs/configuration.md`. In short:
- `regions.yaml`: deployments, instance types, `max_dtns`, and the slot and
  cost settings (`cost_profile`, `warm_slots`, `cold_ttl`, `drain_timeout`,
  `scale_in_idle`, `unhealthy_after`, `max_restarts`, `instance_types`,
  `control_node`)
- `open_data_buckets.yaml`: Open Data bucket list per region
- Globus endpoint owner and subscription: **required per deployment** in
  `regions.yaml` (`globus_endpoint_owner`, `globus_subscription_id`; #162). The
  maintainer's values live in the local, gitignored `regions.yaml`, not in
  `cdk.json`
- `cdk.json` context: GCS version, default `max_dtns`

## Testing Strategy

### Validated
- Slot controller on src and dst (#132): cold revive ~47 s to node setup done,
  warm start ~95 s to in DNS, drain-then-stop under load with 0 task faults
- End-to-end transfers, and the November 2026 two-deployment benchmark campaign
  (#58): `docs/performance/`, `test/benchmark/results/2026-11-us-west-2/`

### Integration and benchmark tests
- `test/integration/`: slot scale out/in, transfer (live AWS + Globus)
- `test/benchmark/`: warm/hot/cold scale-out timing on slots; `harness/` drives
  the two-deployment campaign

## Important Notes

1. **GCS version consistency:** All nodes in an endpoint MUST run the same GCS version. The AMI build process must ensure this. Rolling updates need the documented disable → upgrade → enable sequence.

2. **DNS propagation:** After `node enable` or `node disable`, there may be "intermittent errors for up to two minutes while DNS changes propagate." The orchestration should account for this — don't declare a node ready until DNS confirms its IP.

3. **Globus subscription required:** Multi-node endpoints, guest collections, and the S3 connector all require a managed endpoint under a Globus subscription.

4. **8GB RAM minimum, x86-64 only:** GCS requires 8GB and publishes no aarch64 EL9 packages, so no Graviton (t4g.medium also has only 4GB). t3a.large is the floor for the control node.

5. **Data channel ports:** Security groups must allow TCP 50000-51000 inbound and outbound for GridFTP data channels, plus TCP 443 for the control channel.

6. **Open Data bucket permissions:** Test access patterns carefully. Some Open Data buckets are fully public, some are requester-pays. The IAM role needs to handle both.

7. **`--export-node` / `--import-node` docs say "Contact Globus Support if you think you need this."** This is worth doing — reach out to Globus support to confirm this is a supported pattern for elastic deployments. Given Scott's connections with the GlobusWorld organizers, this conversation could also feed into the conference talk.
