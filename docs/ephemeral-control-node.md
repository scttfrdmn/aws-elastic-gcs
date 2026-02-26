# Ephemeral Control Node

Elastic GCS can automatically stop the control node EC2 instance when no
transfers are active, reducing idle cost from ~$50/month to ~$4.50/month.
This document covers the availability trade-offs, configuration, wake
mechanisms, a sample Globus Flow, and what Globus could do to make this
fully seamless.

---

## How It Works

Two new Lambda functions manage the control node lifecycle:

**`elastic-gcs-waker`** — starts the control node on demand.
Exposed via an HTTPS Function URL callable by Globus Flows, curl, or the
`elastic-gcs-ctl wake` CLI. Optionally triggers DTN scale-out concurrently
so warm-pool instances begin resuming in parallel with the control node boot.

**`elastic-gcs-idle-shutdown`** — runs every 15 minutes via EventBridge.
Stops the control node only when all of the following are true:

1. Control node has been running for at least `min_uptime_minutes` (default 30)
2. ASG desired capacity is 0 (no DTNs requested)
3. No DTN instance is InService
4. All warm-pool instances are `Warmed:Stopped`
5. `ElasticGCS/ActiveTransfers` has been 0 for the full `idle_shutdown_minutes` window

When the control node stops, the warm-pool DTN instances remain stopped
(not terminated). The DTN EBS volumes persist. On the next wake, DTNs can
resume from the warm pool in ~60s rather than booting cold (~3–5 min).

Cache eviction still applies: warm-pool instances stopped for longer than
`cacheEvictionHours` (default 72h) are terminated by the existing
`cache_eviction` Lambda. This is the natural two-stage cost model:

| Idle duration | DTN state | DTN scale-out latency | Monthly idle cost |
|---|---|---|---|
| 0–72h | Warm stopped | ~60s (resume) | ~$4.50 |
| 72h+ | Terminated | 3–5min (cold boot) | ~$2.50 |

---

## ⚠️ Availability Warning

**The Globus endpoint becomes unreachable while the control node is stopped.**

The endpoint domain (`*.gaccess.io`) is Globus-managed DNS pointing at the
control node's Elastic IP. When the EC2 instance is stopped, the GCS Manager
is not running. Any transfer attempted during this window fails immediately
with "endpoint not responding" — Globus does not queue transfers for
unavailable endpoints.

**Do not enable idle shutdown for:**
- Publicly advertised, always-available endpoints
- Endpoints where users initiate transfers directly from the Globus web app
  without advance coordination

**Suitable for:**
- Operator-managed usage windows ("workshop at 2pm, wake at 1:55pm")
- Endpoints primarily accessed via Globus Flows (see Flow integration below)
- Teams that run `elastic-gcs-ctl wake` before starting work
- Scheduled/batch workflows where the transfer time is known in advance

---

## Configuration

In `config/regions.yaml`, set `idle_shutdown_minutes` to a non-zero value:

```yaml
regions:
  - name: us-east-1
    # ... other settings ...
    idle_shutdown_minutes: 30   # stop after 30 min idle; 0 = disabled (default)
    min_uptime_minutes: 30      # anti-thrash guard
```

Then `cdk deploy` to create the idle-shutdown Lambda and EventBridge rule.

The waker Lambda and its Function URL are **always created** regardless of
whether idle shutdown is enabled — useful even without auto-shutdown (e.g.
for manual wake after a planned outage, or Flows integration on a mostly-on
endpoint).

### Recommended values

| Usage pattern | `idle_shutdown_minutes` | Notes |
|---|---|---|
| Disabled | `0` (default) | Control node runs 24/7 |
| Workday hours | `60` | Stops after 1h idle; saves nights/weekends |
| On-demand only | `30` | Aggressive; ensure operators wake before use |
| Flows-only access | `30` | Flows handle wake transparently |

---

## Wake Mechanisms

### 1. `elastic-gcs-ctl wake` (operator CLI)

The simplest path. Uses your AWS credentials to invoke the waker Lambda
directly via the Lambda Invoke API — no bearer token needed.

```bash
# Start control node only (~40s)
elastic-gcs-ctl wake

# Start control node and trigger DTN scale-out simultaneously
# Total time to first transfer: ~100s (warm pool) or ~4min (cold)
elastic-gcs-ctl wake --scale-out
```

Example output:
```
Invoking waker Lambda (elastic-gcs-waker-us-east-1)...

Control node ready (41s, 41s)
  Warm pool populated — DTN scale-out will take ~60s
  Scale-out triggered (desired=1)
```

### 2. Lambda Function URL (Globus Flows, curl, scripts)

Every deployment outputs a `WakerFunctionUrl`. Calls require a Bearer token
stored in Secrets Manager at `elastic-gcs/{region}/waker-token`.

Retrieve the token:
```bash
aws secretsmanager get-secret-value \
  --secret-id elastic-gcs/us-east-1/waker-token \
  --query SecretString --output text
```

Wake via curl:
```bash
TOKEN=$(aws secretsmanager get-secret-value \
  --secret-id elastic-gcs/us-east-1/waker-token \
  --query SecretString --output text)

WAKER_URL=$(aws cloudformation describe-stacks \
  --stack-name ElasticGCS-DTNPool-us-east-1 \
  --query "Stacks[0].Outputs[?OutputKey=='WakerFunctionUrl'].OutputValue" \
  --output text)

curl -s -X POST "$WAKER_URL" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"scale_out": true}'
```

Response (blocks until control node is healthy, ~40s):
```json
{
  "status": "ready",
  "elapsed_seconds": 41,
  "warm_pool_populated": true,
  "scale_out_triggered": true
}
```

Set your HTTP client timeout to at least 90 seconds.

### 3. Scheduled wake (cron)

If your usage follows a predictable schedule, add an EventBridge rule
outside CDK:

```bash
# Wake Mon–Fri at 7:50 AM UTC
aws events put-rule \
  --name elastic-gcs-scheduled-wake \
  --schedule-expression "cron(50 7 ? * MON-FRI *)" \
  --state ENABLED

aws events put-targets \
  --rule elastic-gcs-scheduled-wake \
  --targets '[{"Id":"WakerLambda","Arn":"arn:aws:lambda:us-east-1:ACCOUNT:function:elastic-gcs-waker-us-east-1"}]'

# Grant EventBridge permission to invoke the Lambda
aws lambda add-permission \
  --function-name elastic-gcs-waker-us-east-1 \
  --statement-id scheduled-wake \
  --action lambda:InvokeFunction \
  --principal events.amazonaws.com \
  --source-arn arn:aws:events:us-east-1:ACCOUNT:rule/elastic-gcs-scheduled-wake
```

---

## Globus Flow Integration

The cleanest production pattern is a Globus Flow that wakes the endpoint
before submitting a transfer. Users submit the Flow rather than a direct
transfer; the wake step is transparent to them.

### Sample Flow Definition

Save as `flows/wake-and-transfer.json`:

```json
{
  "Comment": "Wake Elastic GCS endpoint then transfer files",
  "StartAt": "WakeEndpoint",
  "States": {
    "WakeEndpoint": {
      "Comment": "Start the control node. Blocks until GCS Manager is healthy (~40s).",
      "Type": "Action",
      "ActionUrl": "https://actions.automate.globus.org/http/run",
      "Parameters": {
        "url.$": "$.waker_url",
        "method": "POST",
        "headers": {
          "Authorization.$": "States.Format('Bearer {}', $.waker_token)",
          "Content-Type": "application/json"
        },
        "body": {
          "scale_out": true
        },
        "timeout": 90
      },
      "ResultPath": "$.wake_result",
      "Next": "WaitForDTN"
    },
    "WaitForDTN": {
      "Comment": "Allow time for the warm-pool DTN to finish enabling (~60s after wake).",
      "Type": "Wait",
      "Seconds": 60,
      "Next": "Transfer"
    },
    "Transfer": {
      "Comment": "Submit the Globus transfer task.",
      "Type": "Action",
      "ActionUrl": "https://actions.automate.globus.org/transfer/transfer",
      "Parameters": {
        "source_endpoint_id.$": "$.source_endpoint",
        "destination_endpoint_id.$": "$.dest_endpoint",
        "transfer_items.$": "$.transfer_items",
        "notify_on_succeeded": true,
        "notify_on_failed": true
      },
      "ResultPath": "$.transfer_result",
      "End": true
    }
  }
}
```

### Sample Flow Input

```json
{
  "waker_url": "https://<id>.lambda-url.us-east-1.on.aws/",
  "waker_token": "<value from elastic-gcs/us-east-1/waker-token>",
  "source_endpoint": "<source-collection-uuid>",
  "dest_endpoint": "<dest-collection-uuid>",
  "transfer_items": [
    {
      "source_path": "/1000genomes/release/20130502/",
      "destination_path": "/~/my-transfer/",
      "recursive": true
    }
  ]
}
```

### Registering the Flow

```bash
globus login
globus flows create \
  --title "Elastic GCS Wake and Transfer" \
  --definition flows/wake-and-transfer.json
```

Once registered, the Flow appears in the Globus web app under Flows.
Users can submit it from the web UI or the Globus CLI without knowing
anything about the wake mechanism.

### Security note on the waker token in Flows

The waker token is passed as a Flow input parameter. For production use:
- Restrict who can run the Flow via Globus Groups membership on the Flow ACL
- Consider short-lived tokens rotated via Secrets Manager rotation
- A future Globus Flows version may support run-time secrets natively,
  eliminating the need to pass the token in plaintext input

---

## What Globus Could Do to Make This Seamless

The current architecture requires a proactive wake step because the Globus
platform has no mechanism to intercept a transfer request before it reaches
the GCS Manager. The endpoint simply appears offline. Several platform
features would make ephemeral endpoints fully transparent to end users:

**1. Transfer queuing for endpoints in maintenance state**
If Globus queued transfer tasks submitted to an endpoint marked as
"ephemeral" and retried automatically when it came back online, the wake
step would be invisible to users. The endpoint owner would register a wake
callback URL that Globus calls before attempting to connect.

**2. Pre-transfer webhook / lifecycle hook**
A pre-transfer hook fired by the Transfer service before initiating a task
would allow the endpoint to wake on demand and signal "ready" before Globus
begins moving data. This mirrors how cold-tier cloud storage handles
rehydration for archive retrieval.

**3. Native Globus Flows "wake endpoint" action**
A first-party Flows action (`https://actions.automate.globus.org/gcs/wake`)
that knows how to wake a GCS endpoint would remove the need for a custom
HTTP action and waker Lambda entirely. The endpoint owner would register
a wake URL via the GCS API and Flows would call it automatically when the
endpoint is needed.

**4. GCS Manager lightweight standby mode**
If GCS could run in a minimal "standby" state on a small instance —
responding to Globus's health probes and accepting connection attempts while
the data-serving DTNs are stopped — the control node could stay registered
and responsive without incurring full compute cost. DTNs would still scale
on demand.

Items 2 and 3 are the most impactful and fit naturally within the existing
Flows and GCS architecture. This is worth raising with the Globus engineering
team, particularly in the context of cloud-native and cost-sensitive
deployments.

---

## Cost Model

| Scenario | Control node | DTNs | Monthly cost |
|---|---|---|---|
| Always-on (default) | t3a.large 24/7 | 2× warm stopped | ~$55 + $3 EBS = **~$58** |
| Idle shutdown, active 4h/day | t3a.large 4h/day | 2× warm stopped | ~$9 + $3 EBS = **~$12** |
| Idle shutdown, active 2h/month | t3a.large 2h/month | Evicted after 72h | ~$0.15 + $1 EBS = **~$1.50** |
| Fully idle, warm pool intact | Stopped | 2× warm stopped EBS | ~$0 + $3 EBS + $0.40 SM = **~$3.50** |
| Fully idle, evicted | Stopped | Terminated | ~$0 + $1 EBS + $0.40 SM = **~$1.40** |

EBS: control node 30 GB gp3 ≈ $2.40/mo; each DTN 20 GB gp3 ≈ $1.60/mo.
SM: Secrets Manager secrets at $0.40/mo each (7 node slots + deployment key + credentials + waker token).
