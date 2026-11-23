# Elastic GCS on AWS

Auto-scaling [Globus Connect Server v5](https://docs.globus.org/globus-connect-server/) infrastructure for AWS. Scales from a single control node (~$59/month running, a few dollars stopped) to multiple high-throughput Data Transfer Nodes in about 1.5–2.5 minutes (measured until the node is in endpoint DNS), then contracts back when transfers complete.

**Project site:** https://scttfrdmn.github.io/aws-elastic-gcs/ (guides, performance results, blog).

**Any Globus subscriber with data in S3 can use this.** The primary use case driving development is fronting the [AWS Open Data Registry](https://registry.opendata.aws/), but the infrastructure is generic.

## Why

| | Always-on (2 DTNs) | Elastic GCS |
|-|-------------------|-------------|
| Idle cost | ~$400/mo | ~$6–70/mo |
| Scale-out (warm slot) | — | ~1.5 min (in DNS) |
| Scale-out (cold slot) | — | ~2.5 min (in DNS) |
| Max throughput | fixed | scales with demand |

### Cost tiers

| Scenario | Monthly idle cost |
|----------|-------------------|
| Control node on 24/7, 2 warm slots (`cost_profile: balanced`) | ~$70 |
| Control node stopped between sessions, 2 warm slots | ~$16 |
| Control node stopped, no warm slots | ~$6 |

A warm slot costs about $5/month (EBS + Elastic IP). See
[docs/architecture.md](docs/architecture.md#cost-model) for the cost model and
[docs/configuration.md](docs/configuration.md) for the cost settings.

## Install the operator CLI

```bash
brew tap scttfrdmn/tap && brew trust scttfrdmn/tap     # Homebrew 7+ loads only trusted third-party taps
brew install --cask aws-elastic-gcs-ctl                 # macOS / Linux
scoop bucket add scttfrdmn https://github.com/scttfrdmn/scoop-bucket
scoop install aws-elastic-gcs-ctl                      # Windows
```

`.deb` and `.rpm` packages and archives are attached to each
[release](https://github.com/scttfrdmn/aws-elastic-gcs/releases). The
infrastructure itself is deployed from a clone of this repository (below).

## How It Works

A deployment has a fixed number of DTN **slots**. Each slot has a GCS node
identity pre-created by the control node and stored in AWS Secrets Manager, and
an Elastic IP. A **slot controller** (a Lambda durable function) converges the
slots to `desired-dtns`, which `demand_monitor` sets from active Globus
transfers. It starts stopped (WARM) slots first; their node enables itself on
boot and is serving, in endpoint DNS, in about 1.5 minutes. Otherwise it revives
a COLD slot: launch from the DTN AMI, then a one-time `node setup` with the
slot's identity.

When transfers finish, the controller drains each slot (`node disable`, wait for
its data connections to end) and stops it. Slots beyond `warm_slots` go COLD
after `cold_ttl`.

```
COLD (no instance)         WARM (stopped)              HOT (running)
  No EC2 cost               EBS + Elastic IP            Full compute cost
  ~2.5 min to serving       ~1.5 min to serving         0s (already serving)
       │                         │                            │
       │  Launch from AMI        │  Start instance            │  Already active
       │  + node setup           │  + node enable             │
       ▼                         ▼                            ▼
   ┌─────────┐   revive     ┌─────────┐    start       ┌─────────┐
   │  COLD   │─────────────►│   HOT   │◄───────────────│  WARM   │
   └─────────┘              └─────────┘───────────────►└─────────┘
        ▲                                 drain + stop      │
        └──────────── evict after cold_ttl (beyond warm_slots)
```

See [docs/architecture.md](docs/architecture.md) for full details.

## Requirements

- AWS account with CDK bootstrapped (`npx cdk bootstrap`)
- [Globus subscription](https://www.globus.org/subscriptions) (required for multi-node endpoints and S3 connector)
- Globus endpoint owner identity (a Globus ID, e.g. `you@globusid.org`)
- A Globus automation client (confidential app) with Administrator role on the endpoint
- Tools: `go 1.23+`, `node` (for CDK), `aws` CLI, `globus-connect-server` CLI, `globus` CLI

## Quick Start

```bash
# 1. Clone
git clone https://github.com/scttfrdmn/aws-elastic-gcs
cd aws-elastic-gcs

# 2. Configure your deployment
cp config/regions.example.yaml config/regions.yaml
# Edit config/regions.yaml: your region, DTN instance type, max_dtns, and the
# REQUIRED globus_endpoint_owner (you@globusid.org) and globus_subscription_id

# 3. Store Globus credentials (names are elastic-gcs/<region>/<deployment>/...)
aws secretsmanager create-secret \
  --name elastic-gcs/us-east-1/default/globus-client-id \
  --secret-string "<your-automation-client-id>"
# Read the secret from stdin (paste, then Ctrl-D) so it isn't in your shell
# history or the process list.
aws secretsmanager create-secret \
  --name elastic-gcs/us-east-1/default/globus-client-secret \
  --secret-string file:///dev/stdin

# 4. Deploy
make deploy-control REGION=us-east-1 DEPLOYMENT=default  # ~10 min (GCS endpoint setup)
make deploy-dtn REGION=us-east-1 DEPLOYMENT=default      # ~3 min (slot controller, launch template, Lambdas)

# 5. Validate
make build
./bin/aws-elastic-gcs-ctl status --region us-east-1
./bin/aws-elastic-gcs-ctl scale up --watch --region us-east-1
./bin/aws-elastic-gcs-ctl transfer test --region us-east-1
./bin/aws-elastic-gcs-ctl scale down --watch --region us-east-1
```

See [docs/operator-guide.md](docs/operator-guide.md) for the full deploy-through-destroy lifecycle.

## aws-elastic-gcs-ctl

The operator CLI provides a single-screen system overview and common operations:

```
$ ./bin/aws-elastic-gcs-ctl status --region us-east-1
Elastic GCS — us-east-1
━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
Control Node   i-065c4e068da377b11     ● running       34.224.250.21
Endpoint       09c97a5c-63e2-40a2-bcc9-caf96c0de230

DTN Slots      desired=0    serving=0    warm=2
  slot 1    i-09df2757f113b5448   stopped         phase=-           
  slot 2    i-0b4cde7d88b82ece1   stopped         phase=-           

GCS Nodes
  ○  030674af                        inactive
  ○  12ba8822                        inactive
  ●  26ce140b                        active

Active Transfers (5 min):  0
```

Commands:

| Command | Description |
|---------|-------------|
| `status` | System overview: control node, DTN slots, GCS nodes, active transfers |
| `wake [--scale-out]` | Start the control node, enable it and wait until it's in endpoint DNS (an endpoint with no serving node rejects transfers); `--scale-out` also asks for one DTN |
| `scale up [--to N] [--watch]` | Raise desired-dtns (held for `scale_in_idle`) |
| `scale down [--to N] [--watch]` | Lower desired-dtns |
| `nodes list` | List all GCS nodes with status |
| `nodes delete <id>` | Delete a GCS node registration (accepts 8-char prefix) |
| `nodes version-check [--expect V]` | GCS version on the control node and every running DTN (over SSM); exits non-zero on drift |
| `collections list` | List storage gateways and mapped collections |
| `collections setup --config config/open_data_buckets.yaml` | Idempotently create gateways and collections |
| `logs <lambda> [--since 1h] [--tail] [--json]` | Stream or fetch Lambda logs |
| `metrics [--since 1h]` | ActiveTransfers sparkline + Lambda invocation counts |
| `transfer test` | Submit a test transfer via the `globus` CLI |
| `build-ami` | Build a new DTN AMI (wraps `scripts/build_ami.sh`) |
| `deprovision [--confirm]` | Dry-run or execute full teardown (slot instances and Elastic IPs, Globus nodes, CFN stacks, secrets) |
| `version` | Print tool version |

## Project Structure

```
elastic-gcs/
├── cdk.go / cdk.json         CDK app entrypoint and context
├── stacks/                   CDK stacks (network, control node, DTN pool)
├── lambda/                   Lambda function source (Go)
│   ├── slot_controller/      durable function: converges DTN slots to desired-dtns
│   ├── demand_monitor/       sets desired-dtns from active Globus transfers
│   └── version_check/, waker/, idle_shutdown/   not deployed by the slots stack yet (#144, #130)
├── scripts/userdata/         EC2 boot/shutdown scripts
│   ├── control_node_init.sh
│   ├── control_node_boot.sh  node enable on boot, disable on shutdown (control node and slots)
│   └── elastic-gcs-control.service
├── internal/slots/           pure slot reconciler (Plan)
├── internal/slotctl/         controller: observe slots, execute actions
├── internal/identity/        one-time node identity delivery (profile swap + revoke)
├── internal/logger/          Structured JSON logging (slog wrapper)
├── cmd/aws-elastic-gcs-ctl/      Operator CLI
├── config/                   regions.yaml, open_data_buckets.yaml
└── docs/                     Architecture, operator guide, configuration
```

## Documentation

- [Architecture](docs/architecture.md) — three-tier model, components, design decisions, cost model
- [Operator Guide](docs/operator-guide.md) — full deploy-through-destroy lifecycle and day-2 operations
- [Configuration Reference](docs/configuration.md) — CDK context, Secrets Manager, SSM parameters
- [Instance Sizing](docs/instance-sizing.md) — DTN and control node types by sustained throughput and cost
- [Performance](docs/performance/README.md) — measured throughput, user and operator tuning guides, throughput model
- [Blog post draft](docs/blog/2026-11-its-not-aws.md) and [talk outline](docs/talks/its-not-aws-outline.md) — the performance story for wider audiences
- [Project site](https://scttfrdmn.github.io/aws-elastic-gcs/): built from `docs/` with `mkdocs.yml` and published on every push to `main` (preview locally: `pip install -r docs/requirements.txt && mkdocs serve`)
- [Ephemeral Control Node](docs/ephemeral-control-node.md) — idle-shutdown, wake mechanisms, Globus Flow integration, cost model
- [GlobusWorld Demo Runbook](docs/demo-runbook.md) — live demo script with timing notes and contingency paths

## Related Projects

- [globus-go-sdk](https://github.com/scttfrdmn/globus-go-sdk) — Go SDK for Globus services (Auth, Transfer, Flows, Search)
- [aws-globus-connector](https://github.com/scttfrdmn/aws-globus-connector) — S3 connector for GCS (dormant: the Globus connector protocol is not public)
- [globus-gcs-s3-deployment](https://github.com/scttfrdmn/globus-gcs-s3-deployment) — Reference shell-based GCS S3 deployment

## License

Apache-2.0
