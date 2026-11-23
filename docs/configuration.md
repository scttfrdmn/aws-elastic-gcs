# Configuration Reference

Per-deployment settings live in `config/regions.yaml` (copy
`config/regions.example.yaml`); deployment-wide defaults and Globus identity
settings live in `cdk.json` context. `config.Resolve` applies the defaults and
validates everything, so the control and DTN pool stacks always agree (#49).

## `config/regions.yaml`

Each entry is one deployment (an independent Globus endpoint):

```yaml
regions:
  - name: us-west-2
    deployment: src             # 1-12 lowercase letters/digits; default "default"
    control_instance: t3a.large
    dtn_instance: c8in.2xlarge
    max_dtns: 8
    # cost_profile: balanced    # or minimal
    # warm_slots: 2
```

| Setting | Default | Description |
|---|---|---|
| `name` | — | AWS region |
| `deployment` | `default` | Deployment name; see [Multiple deployments per region](#multiple-deployments-per-region) |
| `control_instance` | `t3a.large` | Control node type. GCS needs 8 GB and x86-64 |
| `dtn_instance` | `c8in.2xlarge` | DTN type. Sustained internet-gateway throughput is the bottleneck; see [Instance Sizing](instance-sizing.md) |
| `globus_endpoint_owner` | **required**, no default | Globus username that owns the endpoint (e.g. `you@globusid.org`). Also settable site-wide with `-c globus:endpointOwner=...` |
| `globus_subscription_id` | **required**, no default | Your Globus subscription's ID; multi-node endpoints and the S3 connector need one. Also `-c globus:subscriptionId=...` |
| `globus_endpoint_name` | `Elastic GCS <region>-<deployment>` | Endpoint display name |
| `globus_organization` | `Elastic GCS` | Organization shown on the endpoint |
| `max_dtns` | context `elastic-gcs:maxDTNs`, else `4` | Number of DTN slots (1..`max_dtns`), and so the most DTNs serving at once. The control node pre-creates one node identity per slot. At most **63**: Globus allows 64 node keys per endpoint, and the control node holds one |
| `throughput_scaleout_mbps` | ~70% of `dtn_instance`'s sustained throughput | Throughput one DTN is expected to carry. demand_monitor asks for ceil(total ACTIVE throughput / this) DTNs |
| `key_pair_name` | `""` | EC2 key pair for the control node and DTNs |
| `network_use` | derived from `max_dtns` | See [Network use](#network-use-regionsyaml-network_use) |

### Slot controller and cost (#65)

Each DTN slot is a persistent node identity, Elastic IP and (unless COLD) EC2
instance, in one of three tiers:

- **hot:** running and serving;
- **warm:** stopped, keeps its address, resumes in about 1.5 minutes;
- **cold:** no instance; reviving it launches from the AMI and runs `node setup` (about 1 minute).

| Setting | Default | Description |
|---|---|---|
| `pool` | `slots` | Optional. `slots` is the only pool since the ASG pool was removed (#133); any other value is an error |
| `cost_profile` | `balanced` | Preset for the idle-cost settings below. Any setting given explicitly wins |
| `warm_slots` | profile (`balanced`: 2, `minimal`: 0), at most `max_dtns` | Slots kept warm when idle. The rest go cold after `cold_ttl`. Each warm slot costs about $3.65/month (Elastic IP) plus about $1.60 (EBS) |
| `cold_ttl` | profile (`balanced`: 72h, `minimal`: 24h) | How long an extra warm slot stays stopped before going cold. The legacy `elastic-gcs:cacheEvictionHours` context value also sets it |
| `drain_timeout` | `10m` | Scale-in: maximum wait for a disabled node's GridFTP data connections before stopping it. Sessions finish on the draining node (#141); Globus retries anything still in flight on other nodes |
| `scale_in_idle` | `15m` | No active transfers for this long scales the pool to 0 |
| `unhealthy_after` | `5m` | A running slot that isn't healthy (SSM online, in endpoint DNS, GCS services active) this long is restarted |
| `control_demote_after` | `2m` | Once a DTN has been healthy this long, take the control node out of the data path (local `node disable`); it serves again as soon as no DTN is healthy. `off` keeps it serving alongside DTNs. Globus balances sessions across nodes without regard to capacity (#146) |
| `max_restarts` | `2` | Restarts before an unhealthy slot is rebuilt (cold, then revived) |
| `instance_types` | `[dtn_instance]` | DTN types in fallback order, used when EC2 reports no capacity for the first. `dtn_instance` is always first |
| `control_node` | profile (`balanced`: `always`, `minimal`: `none`) | `always`, `on_demand` or `none` (#130). Transfer rejects submissions to an endpoint with zero serving nodes, so `on_demand` and `none` need an explicit wake before the first transfer |
| `dashboard` | profile (`balanced`: `true`, `minimal`: `false`) | Not built for slots yet (#144) |

**Cost profiles:**

- **`balanced`:** two warm slots and an always-on control node. The first transfer is served by the control node at once, and DTNs join within about 1.5 minutes.
- **`minimal`:** scales to about $0–1/month idle. The first transfer after an idle period waits for a cold revival, and the endpoint shows as offline until then.

**Removed settings.** These belonged to the Auto Scaling group pool and are
rejected with a pointer to their replacement (#133): `warm_pool_size` (use
`warm_slots`), `use_spot` and `spot_max_price` (Spot isn't supported on slots
yet), `idle_shutdown_minutes` and `min_uptime_minutes` (see `control_node`,
#130).

### Network use (`regions.yaml` `network_use`)

| Field | Default | Description |
|---|---|---|
| `preferred_concurrency` | `min(max, 32 × max_dtns)` | Files in flight Globus prefers per task |
| `max_concurrency` | `min(128, 64 × max_dtns)` | Upper limit; Transfer rejects more than 128 |
| `preferred_parallelism` | `8` | TCP streams per file |
| `max_parallelism` | `16` | |

A task's concurrency is the lower of the two endpoints' maximums, so these only
raise the ceiling; the peer's settings still apply. The control node's first
setup applies them once the service account has its role. For existing
endpoints, use `aws-elastic-gcs-ctl endpoint network-use --apply`. See
[operator tuning](performance/operator-guide.md).

## CDK context (`cdk.json`)

| Context key | Default | Description |
|---|---|---|
| `globus:subscriptionId` | — | Globus subscription the endpoint is managed under |
| `globus:endpointOwner` | — | Endpoint owner identity |
| `globus:endpointDisplayName` | — | Display name; the deployment ID is appended |
| `globus:organizationName` | — | Endpoint organization |
| `globus:gcsVersion` | `5.4.101` | Exact GCS version on every node; also keys the DTN AMI |
| `elastic-gcs:maxDTNs` | `4` | Default `max_dtns` for deployments that don't set it |
| `elastic-gcs:cacheEvictionHours` | — | Legacy: default `cold_ttl` in hours |
| `elastic-gcs:throughputScaleOutMBps` | — | Overrides `throughput_scaleout_mbps` for every deployment |
| `adminCIDR` | `""` | Opens SSH (22) to this CIDR on the control node and DTNs. Empty means no SSH ingress; use SSM Session Manager (`aws ssm start-session --target <instance-id>`) |
| `elastic-gcs:operatorEmail` | `""` | Operator alert email. The slots stack has no alert topic yet (#144) |

### DTN Instance Type Selection

| Use case | Recommended type | Sustained internet throughput | us-east-1 on-demand |
|----------|-----------------|-------------------------------|---------------------|
| Light | `m8in.xlarge` | 6.25 Gbps | $0.334/hr |
| Default | `c8in.2xlarge` | 12.5 Gbps | $0.544/hr |
| Heavy | `c8in.4xlarge` | 25 Gbps | $1.089/hr |

Most other types under 32 vCPUs are capped at 5 Gbps to the internet whatever
their "up to" rating. See [Instance Sizing](instance-sizing.md) for the
analysis and `scripts/instance_sizing.py` to recompute for your region.

## Secrets Manager

All secrets are under the prefix `elastic-gcs/{region}/{deployment}/` (see [Multiple deployments per region](#multiple-deployments-per-region)).

| Key | Description |
|-----|-------------|
| `elastic-gcs/{region}/{deployment}/deployment-key` | GCS deployment key JSON (`deployment-key.json`). Generated by `globus-connect-server endpoint setup`. Guard carefully — losing this requires Globus support to recover. |
| `elastic-gcs/{region}/{deployment}/node-slot-{N}` | Pre-created GCS node identity JSON for slot N. Generated by `globus-connect-server node create --export-node`. Only the setup role can read it, and only during a slot's node setup |
| `elastic-gcs/{region}/{deployment}/globus-client-id` | Globus automation client ID. Used by Lambdas and `aws-elastic-gcs-ctl` to authenticate GCS CLI commands. |
| `elastic-gcs/{region}/{deployment}/globus-client-secret` | Globus automation client secret. |

### Creating a Globus Automation Client

The automation client authenticates Lambda functions and CLI commands to the GCS Manager API without requiring interactive login.

1. Register a client at [app.globus.org/settings/developers](https://app.globus.org/settings/developers)
2. Create a client credential (not a native app — use "Confidential App" or "Service Account")
3. Grant the client the `Administrator` role on your endpoint:
   ```bash
   globus-connect-server endpoint role create \
     --role administrator \
     --principal "client:{your-client-id}"
   ```
4. Store the client ID and secret in Secrets Manager (see above)

## SSM Parameters

| Path | Description |
|------|-------------|
| `/elastic-gcs/{region}/{deployment}/control-node-id` | EC2 instance ID of the control node. Written by control node UserData. |
| `/elastic-gcs/{region}/{deployment}/endpoint-id` | Globus endpoint UUID. Written by control node UserData after `endpoint setup`. |
| `/elastic-gcs/{region}/{deployment}/desired-dtns` | DTNs the slot controller converges to. Written by demand_monitor (and `aws-elastic-gcs-ctl scale`) |
| `/elastic-gcs/{region}/{deployment}/last-active` | Last time demand_monitor saw an active transfer; `scale_in_idle` counts from it |
| `/elastic-gcs/{region}/{deployment}/controller-lock` | Held by the running slot-controller execution, so only one runs at a time |
| `/elastic-gcs/{region}/{deployment}/endpoint-domain` | The endpoint's DNS name, recorded at the first slot setup; used for health checks |
| `/elastic-gcs/{region}/dtn-ami/gcs-{version}` | DTN AMI for a GCS version, shared by every deployment in the region. Written by `build-ami`; resolved by the DTN pool stack at deploy time. |

## Lambda Environment Variables

These are set by CDK and are not typically modified directly, but are documented here for reference. Every Lambda also gets `DEPLOYMENT` and `SSM_LOG_GROUP`.

### demand_monitor
| Variable | Description |
|----------|-------------|
| `SECRETS_PREFIX` | Secrets Manager prefix for the Globus client credentials |
| `ENDPOINT_ID_PARAM` | SSM parameter path for Globus endpoint UUID |
| `METRIC_NAMESPACE` | CloudWatch namespace (`ElasticGCS`) |
| `MAX_DTNS` | Upper bound for desired-dtns |
| `THROUGHPUT_THRESHOLD_BPS` | Throughput one DTN is expected to carry |
| `SCALE_IN_IDLE` | No active transfers this long sets desired-dtns to 0 |

### slot_controller
| Variable | Description |
|----------|-------------|
| `MAX_DTNS`, `WARM_SLOTS`, `DRAIN_TIMEOUT`, `COLD_TTL`, `UNHEALTHY_AFTER`, `MAX_RESTARTS` | The resolved slot settings above |
| `LAUNCH_TEMPLATE_ID` | DTN launch template |
| `INSTANCE_TYPES` | DTN types in fallback order |
| `SUBNET_IDS` | Public subnets; slot N uses subnet (N-1) mod count |
| `RUNTIME_PROFILE`, `SETUP_PROFILE`, `SETUP_ROLE` | Instance profiles and the setup role whose sessions are revoked after node setup |

## Multiple deployments per region

Several independent Elastic GCS endpoints can run in one region and account,
for example a source and a destination for benchmarking, or staging and
production. Each `regions.yaml` entry is one deployment, identified by
`(name, deployment)`. `deployment` is 1–12 lowercase letters or digits,
starting with a letter, and defaults to `default`.

Every name is derived from the deployment (`internal/names`). For deployment
`src` in `us-west-2`:

| Resource | Name |
|----------|------|
| Stacks | `ElasticGCS-Control-us-west-2-src`, `ElasticGCS-DTNPool-us-west-2-src` |
| DTN launch template | `elastic-gcs-dtn-us-west-2-src` |
| DTN instance profiles and roles | `elastic-gcs-dtn-runtime-us-west-2-src`, `elastic-gcs-dtn-setup-us-west-2-src` |
| Lambdas, EventBridge rules | `elastic-gcs-<kind>-us-west-2-src` |
| Secrets | `elastic-gcs/us-west-2/src/...` |
| SSM parameters | `/elastic-gcs/us-west-2/src/...` |
| Globus endpoint display name | `<globus:endpointDisplayName> (us-west-2-src)` |
| Metrics | namespace `ElasticGCS`, dimensions `Region` + `Deployment` |

IAM policies are scoped to the deployment's own secrets and parameters, so one
deployment's instances can't read another's. Every resource is tagged
`elastic-gcs:deployment=<region>-<deployment>`, which can be activated as a
cost allocation tag to track spend per deployment. Slot instances and Elastic
IPs also carry `elastic-gcs:pool=slots` and `elastic-gcs:slot=<N>`.

The DTN AMI is shared: it is named only by GCS version, so every deployment in
the region running that version uses it. `make ami REGION=us-west-2
DEPLOYMENT=src` builds it using `src`'s networking.

Buckets in `open_data_buckets.yaml` apply to every deployment in their region
unless they list `deployments: [src]`.

The CLI, Makefile and tests take the deployment as `--deployment`,
`DEPLOYMENT=` and `ELASTIC_GCS_DEPLOYMENT` respectively.

## S3 Bucket Configuration

S3 buckets are added as GCS storage gateways on the control node. There is no CDK configuration for this — storage gateways are created by `aws-elastic-gcs-ctl collections setup` or manually via the GCS CLI.

### Public Buckets (no auth required)
```bash
globus-connect-server storage-gateway create s3 "My Gateway" \
  --domain your-domain.org \
  --s3-endpoint https://s3.us-east-1.amazonaws.com \
  --s3-unauthenticated
```

### Private Buckets (IAM credentials)
Add S3 read permissions to the control node IAM role, then omit `--s3-unauthenticated`.

### Requester-Pays Buckets
The control node IAM role must have `s3:GetBucketRequestPayment` in addition to `s3:GetObject`. Pass `--s3-requester-pays` to the storage gateway create command (if supported by your GCS version) or configure the S3 connector to pass the requester-pays header.

### Multi-Key Buckets (s3_allow_multi_keys)
When `s3_allow_multi_keys: true` is set on the storage gateway, the first path component is the bucket name:
- **Correct:** `/my-bucket/path/to/file.txt`
- **Wrong:** `/path/to/file.txt` (treated as bucket name)

This is the default for gateways created by this project to allow multiple buckets under one gateway.
