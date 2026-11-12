# Operator Guide

This guide covers the full lifecycle of an Elastic GCS deployment: from first deploy to daily operations to teardown. It assumes you have met the prerequisites in [README.md](../README.md).

---

## 1. First Deploy

### 1.1 Bootstrap CDK (once per AWS account/region)

```bash
npx cdk bootstrap aws://{ACCOUNT_ID}/{REGION}
```

### 1.2 Configure

Edit `config/regions.yaml` for your deployment:

```yaml
regions:
  us-east-1:
    dtnInstanceType: c8in.2xlarge   # or m8in.xlarge / c8in.4xlarge; see instance-sizing.md
    maxDTNs: 4
    warmPoolSize: 2
    useSpot: false
    cacheEvictionTTL: 72
    cooldownMinutes: 15
```

See [Configuration Reference](configuration.md) for all parameters.

### 1.3 Store Globus Credentials

Create a Globus automation client (confidential app) and grant it Administrator role on your endpoint. Then store credentials:

```bash
REGION=us-east-1

aws secretsmanager create-secret \
  --region $REGION \
  --name elastic-gcs/$REGION/globus-client-id \
  --secret-string "<your-client-id>"

aws secretsmanager create-secret \
  --region $REGION \
  --name elastic-gcs/$REGION/globus-client-secret \
  --secret-string "<your-client-secret>"
```

### 1.4 Deploy Control Node

```bash
make deploy-control REGION=us-east-1
```

This deploys: VPC, security group (TCP 443 + 50000–51000), control node EC2 instance.

The control node UserData (~10 min) will:
1. Install GCS from Globus package repos
2. Run `globus-connect-server endpoint setup` — creates `deployment-key.json`, stores in Secrets Manager
3. Pre-create N node identities (`node create --export-node`), store in Secrets Manager as `node-slot-1` through `node-slot-N`
4. Configure S3 storage gateway and mapped collection (if configured)
5. Write control node instance ID and endpoint UUID to SSM Parameters

**Validation:**
```bash
./bin/aws-elastic-gcs-ctl status --region us-east-1
```
Expected: Control Node `● running`, Endpoint UUID present.

### 1.5 Deploy DTN Pool

```bash
make deploy-dtn REGION=us-east-1
```

This deploys: ASG + warm pool, Lambda functions, EventBridge rules, CloudWatch alarms and dashboard.

**Validation:**
```bash
./bin/aws-elastic-gcs-ctl status --region us-east-1
```
Expected: DTN Pool shows `warm=2` (or your configured `warmPoolSize`) with `Warmed:Stopped` instances.

---

## 2. Day-1 Validation

Run through these checks after first deploy to confirm end-to-end function.

### Scale up and watch
```bash
./bin/aws-elastic-gcs-ctl scale up --watch --region us-east-1
```
Expected: `InService=1` within ~60 seconds.

### Confirm GCS node active
```bash
./bin/aws-elastic-gcs-ctl status --region us-east-1
```
Expected: one GCS node shows `● active` (the DTN), one shows `● active` (control node).

### Run a test transfer
```bash
./bin/aws-elastic-gcs-ctl transfer test --region us-east-1
```
This shells out to the `globus` CLI to transfer a small file from the 1000 Genomes collection to Globus Tutorial Collection 2. Requires `globus` CLI to be installed and logged in.

Expected: `Status: SUCCEEDED`.

### Check version drift
```bash
./bin/aws-elastic-gcs-ctl nodes version-check --region us-east-1
```
Expected: `GCSVersionDrift = 0` and all nodes showing the same version string.

### Check logs
```bash
./bin/aws-elastic-gcs-ctl logs lifecycle --since 30m --region us-east-1
```
Expected: structured JSON log lines from the scale-out lifecycle hook.

### Scale back down
```bash
./bin/aws-elastic-gcs-ctl scale down --watch --region us-east-1
```
Expected: `InService=0` within ~30 seconds.

---

## 3. Day-2 Operations

### Monitoring

The CloudWatch dashboard `ElasticGCS-{region}` provides:
- Active transfer count and DTN pool size (last 3h)
- Scale event counts and lifecycle Lambda P99 latency (last 24h)
- GCS version drift value and Lambda error counts

Alarms:
- `elastic-gcs-scaleout-{region}` — fires when `ActiveTransfers > 0` for 5 min
- `elastic-gcs-scalein-{region}` — fires when `ActiveTransfers == 0` for 15 min
- `elastic-gcs-version-drift-{region}` — fires when nodes run different GCS versions

**Note:** The scalein alarm fires at idle (desired=0) as a false positive. The scalein Lambda is a no-op when desired is already 0.

### Manual Scaling

```bash
# Scale to a specific count
./bin/aws-elastic-gcs-ctl scale up --to 2 --watch --region us-east-1
./bin/aws-elastic-gcs-ctl scale down --to 0 --watch --region us-east-1
```

### Viewing Logs

```bash
# Pretty-print last 1h of lifecycle Lambda logs
./bin/aws-elastic-gcs-ctl logs lifecycle --since 1h --region us-east-1

# Stream live logs (polls every 2s)
./bin/aws-elastic-gcs-ctl logs demand-monitor --tail --region us-east-1

# Raw JSON (pipe to jq)
./bin/aws-elastic-gcs-ctl logs scaleout --since 2h --json --region us-east-1 | jq .
```

Available Lambda names: `lifecycle`, `scaleout`, `scalein`, `demand-monitor`, `cache-eviction`, `version-check`.

### GCS Version Upgrade

All nodes in a GCS endpoint **must run the same GCS version**. To upgrade:

1. **Scale to zero** (avoid disabling active transfers):
   ```bash
   ./bin/aws-elastic-gcs-ctl scale down --to 0 --watch --region us-east-1
   ```

2. **Rebuild the DTN AMI** with the new GCS version:
   ```bash
   ./bin/aws-elastic-gcs-ctl build-ami --region us-east-1
   # or: bash scripts/build_ami.sh us-east-1
   ```

3. **Upgrade the control node** via SSM or SSH:
   ```bash
   sudo dnf update globus-connect-server
   sudo globus-connect-server node disable
   sudo globus-connect-server node enable
   ```

4. **Trigger a rolling instance refresh** to replace warm pool instances with the new AMI:
   ```bash
   ./bin/aws-elastic-gcs-ctl refresh --region us-east-1
   ```

5. **Verify**:
   ```bash
   ./bin/aws-elastic-gcs-ctl nodes version-check --region us-east-1
   ```
   Expected: `GCSVersionDrift = 0` with the new version on all nodes.

### Evicting and Refreshing Warm Pool Instances

The cache eviction Lambda automatically terminates warm pool instances stopped longer than `cacheEvictionTTL` hours (default: 72h). To manually evict an instance:

```bash
# Terminate the instance directly — ASG will replace it
aws autoscaling terminate-instance-in-auto-scaling-group \
  --instance-id i-xxxx \
  --no-should-decrement-desired-capacity \
  --region us-east-1
```

### Cleaning Up Stale GCS Nodes

Orphaned GCS nodes (registered on the endpoint but no longer backed by an EC2 instance) appear as `○ inactive` in `status` with no corresponding warm pool instance.

```bash
# List all nodes
./bin/aws-elastic-gcs-ctl nodes list --region us-east-1

# Delete by 8-char prefix or full UUID
./bin/aws-elastic-gcs-ctl nodes delete <node-id-prefix> --region us-east-1
```

The cache eviction Lambda also sweeps orphaned nodes automatically (when all warm pool instances carry the `elastic-gcs:gcs-node-id` tag).

### Regenerating Node Slot Identities

If you replace the control node (e.g. instance failure), the new control node will regenerate node identities for any slots where Secrets Manager doesn't already have a valid identity. Warm pool instances that already booted retain their `node-id` file at `/var/lib/elastic-gcs/node-id` and will continue using their existing identity after resume.

To force regeneration of all slot identities:
```bash
# On the control node via SSM or SSH (run as root)
for i in $(seq 1 $TOTAL_SLOTS); do
  globus-connect-server node create \
    --deployment-key /etc/globus-connect-server/deployment-key.json \
    --export-node /tmp/node-slot-$i.json
  aws secretsmanager put-secret-value \
    --secret-id elastic-gcs/$REGION/node-slot-$i \
    --secret-string "$(cat /tmp/node-slot-$i.json)"
done
```

Then terminate and replace all warm pool instances so they pick up the new identities.

---

## 4. Teardown / Destroy

Follow these steps in order to avoid leaving orphaned GCS nodes or AWS resources.

### Step 1: Scale to zero
```bash
./bin/aws-elastic-gcs-ctl scale down --to 0 --watch --region us-east-1
```

### Step 2: Remove all GCS nodes (except control node)
```bash
./bin/aws-elastic-gcs-ctl nodes list --region us-east-1
./bin/aws-elastic-gcs-ctl nodes delete <dtn-node-id> --region us-east-1
# repeat for each non-control DTN node
```

### Step 3: Clean up the GCS endpoint (on the control node)
SSH or SSM onto the control node and run:
```bash
# Disable and clean up the control node itself
sudo globus-connect-server node cleanup

# Optional: delete the endpoint entirely
# globus-connect-server endpoint delete
```

### Step 4: Destroy CDK stacks
```bash
REGION=us-east-1
CDK_DEFAULT_ACCOUNT=$(aws sts get-caller-identity --query Account --output text) \
  AWS_PROFILE=aws \
  npx cdk destroy "ElasticGCS-DTNPool-$REGION" "ElasticGCS-Control-$REGION" "ElasticGCS-Network-$REGION" \
  --region $REGION --force
```

### Step 5: Clean up Secrets Manager (optional)
```bash
REGION=us-east-1
for secret in deployment-key globus-client-id globus-client-secret $(seq -f "node-slot-%.0f" 1 6); do
  aws secretsmanager delete-secret \
    --secret-id elastic-gcs/$REGION/$secret \
    --force-delete-without-recovery \
    --region $REGION
done
```

### Step 6: Verify
```bash
# Should return no results
aws ec2 describe-instances \
  --filters "Name=tag:aws:cloudformation:stack-name,Values=ElasticGCS-*" \
  --query 'Reservations[].Instances[].InstanceId' \
  --region us-east-1 --output text
```

At this point AWS cost = $0 for this deployment (no running EC2, no allocated EIPs).

---

## Troubleshooting

### DTN fails to come InService

1. Check lifecycle Lambda logs:
   ```bash
   ./bin/aws-elastic-gcs-ctl logs lifecycle --since 30m --region us-east-1
   ```
2. Check the instance boot log via SSM:
   ```bash
   aws ssm send-command --instance-ids i-xxx \
     --document-name AWS-RunShellScript \
     --parameters 'commands=["journalctl -u elastic-gcs-boot --no-pager | tail -50"]'
   ```
3. Common causes:
   - Secrets Manager missing `node-slot-N` — control node didn't finish setup
   - GCS lock file owned by root — `chown gcsweb:gcsweb /var/lib/globus-connect-server/gcs-manager/locks/subscription_pause`
   - SSM slot lock already held by a terminated instance — check `/elastic-gcs/{region}/slot-lock/{N}` parameters and delete stale ones

### `aws-elastic-gcs-ctl status` shows stale GCS nodes

Run `./bin/aws-elastic-gcs-ctl nodes delete <node-id>`. If that fails, the GCS CLI on the control node may need auth — confirm `elastic-gcs/{region}/globus-client-id` and `globus-client-secret` are present in Secrets Manager.

### `scalein ● ALARM` when desired=0

Expected behavior. The scalein alarm fires whenever `ActiveTransfers == 0` for 15 minutes, including when there are no DTNs running. The scalein Lambda is a no-op when desired capacity is already 0. This is cosmetically noisy but functionally correct.

### GCS version drift detected

1. Find which nodes are out of sync:
   ```bash
   ./bin/aws-elastic-gcs-ctl nodes version-check --region us-east-1
   ```
2. Follow the GCS Version Upgrade procedure in Day-2 Operations.
3. Do not run transfers while nodes are on different GCS versions — Globus does not support mixed-version endpoints.

### Control node IP changed (EIP detached)

If the control node EIP was lost, the GCS endpoint's DNS still points to the old IP.

On the control node:
```bash
NEW_IP=$(curl -s http://169.254.169.254/latest/meta-data/public-ipv4)
echo "127.0.0.1 $(cat /var/lib/globus-connect-server/info.json | jq -r .domain_name)" >> /etc/hosts
sudo globus-connect-server node update $(hostname) --ip-address $NEW_IP --enable
sed -i '/globus/d' /etc/hosts
```
