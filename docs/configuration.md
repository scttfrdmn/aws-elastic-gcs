# Configuration Reference

## CDK Context (`cdk.json`)

All parameters live under `context` in `cdk.json`. Region-specific values are nested under the region key.

```json
{
  "context": {
    "elastic-gcs:regions": ["us-east-1"],

    "elastic-gcs:us-east-1": {
      "dtnInstanceType":  "c8in.2xlarge",
      "maxDTNs":          4,
      "warmPoolSize":     2,
      "totalSlots":       6,
      "cooldownMinutes":  15,
      "cacheEvictionTTL": 72,
      "useSpot":          false,
      "spotMaxPrice":     ""
    }
  }
}
```

### Parameter Reference

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `dtnInstanceType` | string | `c8in.2xlarge` | EC2 instance type for DTN nodes. Sustained internet-gateway throughput is the bottleneck; see [Instance Sizing](instance-sizing.md). |
| `maxDTNs` | int | `4` | Maximum number of simultaneously InService DTN instances. Scale-out Lambda caps at this value. |
| `warmPoolSize` | int | `2` | Number of stopped instances maintained in the warm pool. Higher = faster scale-out, higher EBS cost. Set to 0 to disable warm pool (cold-start only). |
| `totalSlots` | int | `maxDTNs + warmPoolSize` | Total node slot identities to pre-create in Secrets Manager. Must be ≥ `maxDTNs + warmPoolSize`. |
| `cooldownMinutes` | int | `15` | Minutes of zero active transfers before scale-in fires. |
| `cacheEvictionTTL` | int | `72` | Hours a warm pool instance can remain stopped before the cache eviction Lambda terminates it. |
| `useSpot` | bool | `false` | Use Spot instances for DTNs. Saves ~60–80% on compute but disables warm pool (AWS constraint). Spot DTNs cold-start (2–3 min). |
| `spotMaxPrice` | string | `""` | Maximum Spot price per hour (e.g. `"0.10"`). Empty = up to on-demand price. |

### Slot controller and cost (`regions.yaml`, #65)

The slot controller replaces the Auto Scaling group and warm pool when `pool: slots` is set. Each slot is a persistent node identity, instance and Elastic IP, in one of three tiers:

- **hot:** running and serving;
- **warm:** stopped, keeps its address, resumes in about 1.5 minutes;
- **cold:** no instance and no address; reviving it runs `node setup` again.

| Setting | Default | Description |
|---|---|---|
| `pool` | `asg` | `asg` (Auto Scaling group + warm pool) or `slots` (slot controller) |
| `cost_profile` | `balanced` | Preset for the idle-cost settings below. Any setting given explicitly wins |
| `warm_slots` | profile (`balanced`: 2, `minimal`: 0) | Slots kept warm when idle. The rest go cold after `cold_ttl`. Each warm slot costs about $3.65/month (Elastic IP) plus about $1.60 (EBS) |
| `cold_ttl` | profile (`balanced`: 72h, `minimal`: 24h) | How long an extra warm slot stays stopped before going cold. The legacy `elastic-gcs:cacheEvictionHours` context value also sets it |
| `drain_timeout` | `10m` | Scale-in: maximum wait for a disabled node's GridFTP data connections before stopping it. Globus retries interrupted files on other nodes |
| `scale_in_idle` | `15m` | No active transfers for this long scales the pool to 0 |
| `unhealthy_after` | `5m` | A running slot that isn't active in GCS (or not answering SSM) this long is restarted |
| `max_restarts` | `2` | Restarts before an unhealthy slot is rebuilt (cold, then revived) |
| `instance_types` | `[dtn_instance]` | DTN types in fallback order, used when EC2 reports no capacity for the first. `dtn_instance` is always first |
| `control_node` | profile (`balanced`: `always`, `minimal`: `none`) | `always`, `on_demand` (started for management, stopped when idle) or `none` (management runs through an active DTN) |
| `dashboard` | profile (`balanced`: `true`, `minimal`: `false`) | The CloudWatch dashboard costs $3/month beyond the account's three free ones |

**Cost profiles:**

- **`balanced`:** two warm slots and an always-on control node. The first transfer is served by the control node at once, and DTNs join within about 1.5 minutes.
- **`minimal`:** scales to about $0–1/month idle. The trade-off is that the first transfer after an idle period waits for a cold revival (a few minutes of `node setup`), and the endpoint shows as offline until then.

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

### Access and alerts

| Context key | Default | Description |
|---|---|---|
| `adminCIDR` | `""` | Opens SSH (22) to this CIDR on the control node and DTNs. Empty means no SSH ingress; use SSM Session Manager (`aws ssm start-session --target <instance-id>`). |
| `elastic-gcs:operatorEmail` | `""` | Subscribes this address to the deployment's operator alert topic (`elastic-gcs-operator-<region>-<deployment>`), which receives the GCS version-drift alarm. Confirm the subscription from the email AWS sends. The topic ARN is a stack output either way. |

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
| `elastic-gcs/{region}/{deployment}/node-slot-{N}` | Pre-created GCS node identity JSON for slot N. Generated by `globus-connect-server node create --export-node`. |
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
| `/elastic-gcs/{region}/dtn-ami/gcs-{version}` | DTN AMI for a GCS version, shared by every deployment in the region. Written by `build-ami`; resolved by the DTN pool stack at deploy time. |
| `/elastic-gcs/{region}/{deployment}/slot-lock/{N}` | Slot lock for slot N. Value = EC2 instance ID of the current holder. Written atomically by DTN boot script; deleted by lifecycle Lambda on termination. |

## Lambda Environment Variables

These are set by CDK and are not typically modified directly, but are documented here for reference.

### scaleout / scalein
| Variable | Description |
|----------|-------------|
| `ASG_NAME` | Auto Scaling Group name |
| `MAX_DTNS` | Maximum InService DTN count |
| `REGION` | AWS region |

### lifecycle
| Variable | Description |
|----------|-------------|
| `ASG_NAME` | Auto Scaling Group name |
| `REGION` | AWS region |
| `CONTROL_NODE_ID_PARAM` | SSM parameter path for control node instance ID |

### demand_monitor
| Variable | Description |
|----------|-------------|
| `REGION` | AWS region |
| `ENDPOINT_ID_PARAM` | SSM parameter path for Globus endpoint UUID |
| `GLOBUS_CLIENT_ID_SECRET` | Secrets Manager key for Globus client ID |
| `GLOBUS_CLIENT_SECRET_SECRET` | Secrets Manager key for Globus client secret |

### cache_eviction
| Variable | Description |
|----------|-------------|
| `ASG_NAME` | Auto Scaling Group name |
| `REGION` | AWS region |
| `CACHE_TTL_HOURS` | Cache eviction TTL (hours) |
| `CONTROL_NODE_ID_PARAM` | SSM parameter path for control node instance ID |
| `TOTAL_SLOTS` | Total slot count (for orphan node sweep) |

### version_check
| Variable | Description |
|----------|-------------|
| `ASG_NAME` | Auto Scaling Group name |
| `REGION` | AWS region |
| `CONTROL_NODE_ID_PARAM` | SSM parameter path for control node instance ID |

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
| ASG, launch template, DTN role | `elastic-gcs-dtn-us-west-2-src` |
| Lambdas, alarms, SNS topics | `elastic-gcs-<kind>-us-west-2-src` |
| Secrets | `elastic-gcs/us-west-2/src/...` |
| SSM parameters | `/elastic-gcs/us-west-2/src/...` |
| Dashboard | `ElasticGCS-us-west-2-src` |
| Globus endpoint display name | `<globus:endpointDisplayName> (us-west-2-src)` |
| Metrics | namespace `ElasticGCS`, dimensions `Region` + `Deployment` |

IAM policies are scoped to the deployment's own secrets and parameters, so one
deployment's instances can't read another's. Every resource is tagged
`elastic-gcs:deployment=<region>-<deployment>`, which can be activated as a
cost allocation tag to track spend per deployment.

The DTN AMI is shared: it is named only by GCS version, so every deployment in
the region running that version uses it. `make ami REGION=us-west-2
DEPLOYMENT=src` builds it using `src`'s networking.

Buckets in `open_data_buckets.yaml` apply to every deployment in their region
unless they list `deployments: [src]`.

The CLI, Makefile and tests take the deployment as `--deployment`,
`DEPLOYMENT=` and `ELASTIC_GCS_DEPLOYMENT` respectively.

## config/regions.yaml

Per-region configuration file. Example:

```yaml
regions:
  us-east-1:
    dtnInstanceType: c8in.2xlarge
    maxDTNs: 4
    warmPoolSize: 2
    useSpot: false
    spotMaxPrice: ""
    cacheEvictionTTL: 72
    cooldownMinutes: 15
```

Values here are read by CDK context and override `cdk.json` defaults. At minimum, set the target region(s) and instance type.

## S3 Bucket Configuration

S3 buckets are added as GCS storage gateways on the control node. There is no CDK configuration for this — storage gateways are created by `control_node_init.sh` or manually via the GCS CLI.

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
