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
  - name: us-east-1
    deployment: default
    dtn_instance: c8in.2xlarge   # or m8in.xlarge / c8in.4xlarge; see instance-sizing.md
    max_dtns: 4                  # DTN slots
    # cost_profile: balanced     # 2 warm slots, control node always on
```

See [Configuration Reference](configuration.md) for all parameters.

### 1.3 Store Globus Credentials

Create a Globus automation client (confidential app) and grant it Administrator role on your endpoint. Then store credentials:

```bash
REGION=us-east-1
DEPLOYMENT=default   # the regions.yaml `deployment` name

aws secretsmanager create-secret \
  --region $REGION \
  --name elastic-gcs/$REGION/$DEPLOYMENT/globus-client-id \
  --secret-string "<your-client-id>"

aws secretsmanager create-secret \
  --region $REGION \
  --name elastic-gcs/$REGION/$DEPLOYMENT/globus-client-secret \
  --secret-string "<your-client-secret>"
```

### 1.4 Deploy Control Node

```bash
make deploy-control REGION=us-east-1 DEPLOYMENT=default
```

This deploys: VPC, security group (TCP 443 + 50000–51000), control node EC2 instance.

The control node UserData (~10 min) will:
1. Install GCS from Globus package repos
2. Run `globus-connect-server endpoint setup` — creates `deployment-key.json`, stores in Secrets Manager
3. Pre-create N node identities (`node create --export-node`), store in Secrets Manager as `node-slot-1` through `node-slot-N`
4. Configure S3 storage gateway and mapped collection (if configured)
5. Write control node instance ID and endpoint UUID to SSM Parameters
6. Install `elastic-gcs-control.service`, which enables the node on every boot and disables it on shutdown

The deployment key goes to Secrets Manager immediately after `endpoint setup`.
It exists on disk only in a 0700 directory that is removed when setup ends.

**Owner step (required, once per endpoint).** The service account can't be given
a role until the endpoint has a subscription, and only the owner can attach one
(#66). The setup log (`/var/log/elastic-gcs-setup.log`) prints these commands
with the IDs filled in:

```bash
globus login --gcs <endpoint-id>          # as the endpoint owner
globus gcs endpoint set-subscription-id <endpoint-id> <subscription-id>
globus gcs endpoint role create <endpoint-id> administrator <service-account-client-id>
```

Until then, collections setup, the demand monitor and scaling don't work.

**Validation:**
```bash
./bin/aws-elastic-gcs-ctl status --region us-east-1
```
Expected: Control Node `● running`, Endpoint UUID present.

### 1.5 Deploy DTN Pool

```bash
make deploy-dtn REGION=us-east-1 DEPLOYMENT=default
```

This deploys: the DTN launch template, runtime and setup roles and instance profiles, the slot controller (a Lambda durable function, with its `live` alias), demand_monitor, and their EventBridge rules. No DTN instances are created: every slot starts COLD, and the controller creates instances and Elastic IPs as demand arrives.

**Validation:**
```bash
./bin/aws-elastic-gcs-ctl status --region us-east-1
```
Expected: DTN Slots shows `desired=0  serving=0  warm=0` and no slot instances.

---

## 2. Day-1 Validation

Run through these checks after first deploy to confirm end-to-end function.

### Scale up and watch
```bash
./bin/aws-elastic-gcs-ctl scale up --watch --region us-east-1
```
Expected: `serving=1` within ~2.5 minutes for the first (cold) revive, ~1.5 minutes from a warm slot. `scale` sets `desired-dtns` and refreshes `last-active`, so demand_monitor holds the value for `scale_in_idle` (default 15 minutes).

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

### Check logs
```bash
./bin/aws-elastic-gcs-ctl logs slot-controller --since 30m --region us-east-1
```
The controller's step results (each revive, start, drain and stop) are also in its durable execution history:
```bash
aws lambda list-durable-executions-by-function --function-name elastic-gcs-slot-controller-us-east-1-default --region us-east-1
aws lambda get-durable-execution-history --durable-execution-arn <arn> --include-execution-data --region us-east-1
```

### Scale back down
```bash
./bin/aws-elastic-gcs-ctl scale down --to 0 --watch --region us-east-1
```
Expected: the slot drains (`node disable`, data connections to 0) and stops within about a minute. The controller never stops the last DTN unless the control node is serving.

---

## 3. Day-2 Operations

### Monitoring

`aws-elastic-gcs-ctl status` and `metrics` show the slots, desired-dtns and the
`ElasticGCS/ActiveTransfers` metric. The slots stack doesn't build alarms, an
operator alert topic, a dashboard or the GCS version-drift check yet (#144).

### Manual Scaling

```bash
# Scale to a specific count
./bin/aws-elastic-gcs-ctl scale up --to 2 --watch --region us-east-1
./bin/aws-elastic-gcs-ctl scale down --to 0 --watch --region us-east-1
```

### Viewing Logs

```bash
# Pretty-print last 1h of slot controller logs
./bin/aws-elastic-gcs-ctl logs slot-controller --since 1h --region us-east-1

# Stream live logs (polls every 2s)
./bin/aws-elastic-gcs-ctl logs demand-monitor --tail --region us-east-1

# Raw JSON (pipe to jq)
./bin/aws-elastic-gcs-ctl logs demand-monitor --since 2h --json --region us-east-1 | jq .
```

Deployed Lambdas: `slot-controller`, `demand-monitor`. (`version-check`, `waker` and `idle-shutdown` return with #144 and #130.)

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

4. **Point the DTN stack at the new AMI and replace the slots' instances.** Set `globus:gcsVersion`, run `make deploy-dtn`, then terminate the stopped slot instances (see below) so each slot revives from the new AMI.

5. **Verify** every node reports the new version:
   ```bash
   ./bin/aws-elastic-gcs-ctl nodes list --region us-east-1
   ```

### Evicting a slot

The controller evicts WARM slots beyond `warm_slots` after `cold_ttl` (default
72h for `balanced`). To evict a slot by hand, for example to pick up a new AMI,
terminate its stopped instance. The slot goes COLD and keeps its Elastic IP;
the next scale-out revives it from the AMI (#131 adds `slots evict`):

```bash
aws ec2 describe-instances --region us-east-1 \
  --filters Name=tag:elastic-gcs:deployment,Values=us-east-1-default Name=tag:elastic-gcs:pool,Values=slots \
  --query 'Reservations[].Instances[].[InstanceId,State.Name,Tags[?Key==`elastic-gcs:slot`]|[0].Value]' --output text
aws ec2 terminate-instances --instance-ids i-xxxx --region us-east-1
```

### Cleaning Up Stale GCS Nodes

A WARM or COLD slot's node shows as `○ inactive` in `status`; that's expected, and the slot reuses it. A node is orphaned only if it isn't one of the slots' identities (for example after regenerating identities).

```bash
# List all nodes
./bin/aws-elastic-gcs-ctl nodes list --region us-east-1

# Delete by 8-char prefix or full UUID
./bin/aws-elastic-gcs-ctl nodes delete <node-id-prefix> --region us-east-1
```

### Regenerating Node Slot Identities

If you replace the control node (e.g. instance failure), the new control node will regenerate node identities for any slots where Secrets Manager doesn't already have a valid identity. Slot instances that already ran `node setup` keep their identity across stop and start.

To force regeneration of all slot identities:
```bash
# On the control node via SSM or SSH (run as root)
for i in $(seq 1 $TOTAL_SLOTS); do
  globus-connect-server node create \
    --deployment-key /etc/globus-connect-server/deployment-key.json \
    --export-node /tmp/node-slot-$i.json
  aws secretsmanager put-secret-value \
    --secret-id elastic-gcs/$REGION/$DEPLOYMENT/node-slot-$i \
    --secret-string "$(cat /tmp/node-slot-$i.json)"
done
```

Then terminate every slot instance (see [Evicting a slot](#evicting-a-slot)) so each slot revives with its new identity.

---

## 4. Teardown / Destroy

`./bin/aws-elastic-gcs-ctl deprovision --region us-east-1` does all of this in
order (dry run without `--confirm`). Slot instances and Elastic IPs belong to
the slot controller, not to CloudFormation, so stack deletion alone leaves them
behind, and they hold the control stack's security group. By hand, follow these
steps in order.

### Step 1: Scale to zero and remove the slots
```bash
./bin/aws-elastic-gcs-ctl scale down --to 0 --watch --region us-east-1
# Terminate every slot instance, then release the slots' Elastic IPs:
aws ec2 describe-instances --region us-east-1 \
  --filters Name=tag:elastic-gcs:deployment,Values=us-east-1-default Name=tag:elastic-gcs:pool,Values=slots \
  --query 'Reservations[].Instances[].InstanceId' --output text
aws ec2 describe-addresses --region us-east-1 \
  --filters Name=tag:elastic-gcs:deployment,Values=us-east-1-default Name=tag:elastic-gcs:pool,Values=slots \
  --query 'Addresses[].AllocationId' --output text
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
  npx cdk destroy "ElasticGCS-DTNPool-$REGION-$DEPLOYMENT" "ElasticGCS-Control-$REGION-$DEPLOYMENT" "ElasticGCS-Network-$REGION" \
  --region $REGION --force
```

### Step 5: Clean up Secrets Manager (optional)
```bash
REGION=us-east-1
for secret in deployment-key globus-client-id globus-client-secret $(seq -f "node-slot-%.0f" 1 6); do
  aws secretsmanager delete-secret \
    --secret-id elastic-gcs/$REGION/$DEPLOYMENT/$secret \
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

### A slot doesn't become healthy

1. Check the controller: its logs, and the step results in its durable execution history (see [Check logs](#check-logs)). A failed action is recorded as `error: ...` and re-planned the next round.
2. Check the slot's node unit via SSM:
   ```bash
   aws ssm send-command --instance-ids i-xxx \
     --document-name AWS-RunShellScript \
     --parameters 'commands=["journalctl -u elastic-gcs-node --no-pager | tail -50"]'
   ```
3. Common causes:
   - Secrets Manager missing `node-slot-N` — control node didn't finish setup
   - GCS lock file owned by root — `chown gcsweb:gcsweb /var/lib/globus-connect-server/gcs-manager/locks/subscription_pause`
   - A stale `controller-lock` from an execution that was stopped by hand — the next execution takes it over after it goes stale, or delete `/elastic-gcs/{region}/{deployment}/controller-lock`
   - A revive whose execution died leaves the slot `reviving`; the controller rebuilds it after `unhealthy_after`

### `aws-elastic-gcs-ctl status` shows stale GCS nodes

Run `./bin/aws-elastic-gcs-ctl nodes delete <node-id>`. If that fails, the GCS CLI on the control node may need auth — confirm `elastic-gcs/{region}/{deployment}/globus-client-id` and `globus-client-secret` are present in Secrets Manager.

### Transfers to the endpoint fail with "Error validating login"

The endpoint has no serving node (control node stopped and no HOT slot).
Transfer rejects the submission outright, so demand can't wake the endpoint
(#130). Start the control node (`scripts/control_node_power.sh <region>
<deployment> start`) or raise desired-dtns.

### GCS version drift

1. Find which nodes are out of sync: `./bin/aws-elastic-gcs-ctl nodes list --region us-east-1`
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
