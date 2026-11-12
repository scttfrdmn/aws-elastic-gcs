# Elastic GCS on AWS

Auto-scaling [Globus Connect Server v5](https://docs.globus.org/globus-connect-server/) infrastructure for AWS. Scales from a single control node (~$40/month idle) to multiple high-throughput Data Transfer Nodes in 30–60 seconds, then contracts back when transfers complete.

**Any Globus subscriber with data in S3 can use this.** The primary use case driving development is fronting the [AWS Open Data Registry](https://registry.opendata.aws/), but the infrastructure is generic.

## Why

| | Always-on (2 DTNs) | Elastic GCS |
|-|-------------------|-------------|
| Idle cost | ~$400/mo | ~$4–56/mo |
| Scale-out (warm) | — | ~60s |
| Scale-out (cold) | — | ~3–5 min |
| Max throughput | fixed | scales with demand |

### Cost tiers

| Scenario | Monthly idle cost |
|----------|-------------------|
| Control node on 24/7, warm pool intact | ~$58 |
| Control node idles off after 1h, warm pool intact | ~$12 |
| Fully idle, warm pool intact (control node stopped) | ~$3.50 |
| Fully idle, warm pool evicted after 72h | ~$1.40 |

See [docs/ephemeral-control-node.md](docs/ephemeral-control-node.md) for the full cost breakdown and idle-shutdown configuration.

## How It Works

GCS nodes are pre-registered with unique identities stored in AWS Secrets Manager. DTN instances stay stopped in an ASG warm pool, paying only for EBS storage. When Globus transfer demand is detected, a Lambda resumes a stopped instance, runs `node enable`, and the DTN joins the endpoint within 60 seconds.

When transfers finish, a lifecycle hook runs `node disable` before the instance stops back to the warm pool.

```
COLD (terminated)          WARM (stopped)              HOT (running)
  No EC2 cost               EBS-only cost               Full compute cost
  3–5 min scale-out         ~60s scale-out              0s (already serving)
       │                         │                            │
       │  Launch from AMI        │  Resume instance           │  Already active
       │  + node setup           │  + node enable             │
       ▼                         ▼                            ▼
   ┌─────────┐              ┌─────────┐              ┌─────────┐
   │  COLD   │──stop+hold──►│  WARM   │──transfer───►│   HOT   │
   └─────────┘              └─────────┘              └─────────┘
                                 ▲          cool-down     │
                                 │          (15–30 min)   │
                                 └────────────────────────┘
                            cache eviction
                            (72h timeout) ──► TERMINATE
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

# 2. Configure region
cp config/regions.example.yaml config/regions.yaml
# Edit config/regions.yaml — set your region, instance type, pool sizes

# 3. Store Globus credentials
aws secretsmanager create-secret \
  --name elastic-gcs/us-east-1/globus-client-id \
  --secret-string "<your-automation-client-id>"
aws secretsmanager create-secret \
  --name elastic-gcs/us-east-1/globus-client-secret \
  --secret-string "<your-automation-client-secret>"

# 4. Deploy
make deploy-control REGION=us-east-1  # ~10 min (GCS endpoint setup)
make deploy-dtn REGION=us-east-1      # ~3 min (ASG, Lambdas, dashboard)

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

DTN Pool       desired=0    in-service=0    warm=2
  i-09df2757f113b5448       slot=1    Warmed:Stopped          node=12ba8822
  i-0b4cde7d88b82ece1       slot=3    Warmed:Stopped          node=030674af

GCS Nodes
  ○  030674af                        inactive
  ○  12ba8822                        inactive
  ●  26ce140b                        active

Active Transfers (5 min):  0
Alarms:  scaleout ○ OK  scalein ● ALARM  version-drift ○ OK
```

Commands:

| Command | Description |
|---------|-------------|
| `status` | System overview: control node, DTN pool, GCS nodes, metrics, alarms |
| `wake [--scale-out]` | Start the control node (and optionally scale out a DTN) |
| `scale up [--to N] [--watch]` | Scale out the DTN pool |
| `scale down [--to N] [--watch]` | Scale in the DTN pool |
| `nodes list` | List all GCS nodes with status |
| `nodes delete <id>` | Delete a GCS node registration (accepts 8-char prefix) |
| `nodes version-check` | Invoke version drift Lambda, show per-instance versions |
| `collections list` | List storage gateways and mapped collections |
| `collections setup --config config/open_data_buckets.yaml` | Idempotently create gateways and collections |
| `logs <lambda> [--since 1h] [--tail] [--json]` | Stream or fetch Lambda logs |
| `metrics [--since 1h]` | ActiveTransfers sparkline + Lambda invocation counts |
| `refresh [--force]` | Trigger ASG instance refresh (rolling DTN AMI update) |
| `transfer test` | Submit a test transfer via the `globus` CLI |
| `build-ami` | Build a new DTN AMI (wraps `scripts/build_ami.sh`) |
| `deprovision [--confirm]` | Dry-run or execute full teardown (Globus nodes, CFN stacks, secrets) |
| `version` | Print tool version |

## Project Structure

```
elastic-gcs/
├── cdk.go / cdk.json         CDK app entrypoint and context
├── stacks/                   CDK stacks (network, control node, DTN pool)
├── lambda/                   Lambda function source (Go)
│   ├── scaleout/
│   ├── scalein/
│   ├── lifecycle/
│   ├── cache_eviction/
│   ├── demand_monitor/
│   └── version_check/
├── scripts/userdata/         EC2 boot/shutdown scripts
│   ├── control_node_init.sh
│   ├── dtn_boot.sh
│   └── dtn_shutdown.sh
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
- [Ephemeral Control Node](docs/ephemeral-control-node.md) — idle-shutdown, wake mechanisms, Globus Flow integration, cost model
- [GlobusWorld Demo Runbook](docs/demo-runbook.md) — live demo script with timing notes and contingency paths

## Related Projects

- [globus-go-sdk](https://github.com/scttfrdmn/globus-go-sdk) — Go SDK for Globus services (Auth, Transfer, Flows, Search)
- [aws-globus-connector](https://github.com/scttfrdmn/aws-globus-connector) — High-performance S3 connector for GCS
- [globus-gcs-s3-deployment](https://github.com/scttfrdmn/globus-gcs-s3-deployment) — Reference shell-based GCS S3 deployment

## License

Apache-2.0
