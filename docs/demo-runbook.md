# GlobusWorld 2026 — Demo Runbook

**Session:** Elastic GCS on AWS — Scale-on-Demand Open Data Endpoints
**Venues:** Chicago (Apr 21–22) · Raleigh (Jun 2–3)
**Presenter:** Scott Friedman
**Total demo time:** ≤ 8 minutes

---

## Pre-demo checklist (≥ 30 min before session)

- [ ] `aws sso login --profile aws` — ensure AWS credentials are fresh
- [ ] `globus whoami` — confirm authenticated as `scofri@globusid.org`
- [ ] `elastic-gcs-ctl status --region us-east-1` — check baseline state (see expected output below)
- [ ] Warm pool has ≥ 1 `Warmed:Stopped` instance — if evicted, run `make deploy-dtn REGION=us-east-1` to restore
- [ ] Terminal font ≥ 16pt, dark background, 120-column width
- [ ] Browser tab open to CloudWatch dashboard for `elastic-gcs-us-east-1`
- [ ] Browser tab open to `https://app.globus.org/file-manager`

### Expected baseline state

```
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

> **Note:** The control node is shown running here. If `idle_shutdown` is enabled, it may
> be stopped — run `elastic-gcs-ctl wake` ~40s before the demo starts, or start the demo
> from step 2 with "we're about to wake the endpoint."

---

## Demo script

### Step 1 — Show the idle state (~45 seconds of talk)

```bash
elastic-gcs-ctl status --region us-east-1
```

**Say:** *"This is a live Globus Connect Server endpoint fronting the AWS 1000 Genomes
Open Data bucket. Right now it's idle — the control node is running, but both DTN slots
are stopped in the warm pool, paying only for EBS storage. At this scale, idle cost is
about $4–12 per month."*

**Point out:** DTN Pool `in-service=0`, both nodes `inactive`, `Active Transfers: 0`.

---

### Step 2 — Wake the endpoint and trigger scale-out (~60 seconds)

```bash
elastic-gcs-ctl wake --scale-out --region us-east-1
```

**Say:** *"Watch what happens when I request a DTN. The waker Lambda starts the
control node — already running in this case — and sets the ASG desired capacity to 1.
The warm-pool instance starts resuming immediately."*

**Expected output:**
```
Invoking waker Lambda (elastic-gcs-waker-us-east-1)...

Control node ready (2s, already running)
  Warm pool populated — DTN scale-out will take ~60s
  Scale-out triggered (desired=1)
```

> **If control node was stopped:** output shows `~40s` for control node boot, then
> `Scale-out triggered`. Total warm path time is ~100s.

---

### Step 3 — Watch the DTN come InService (~60 seconds of live polling)

```bash
watch -n 5 elastic-gcs-ctl status --region us-east-1
```

**Say:** *"The instance is resuming from the warm pool. On cold boot from AMI this would
take 3–5 minutes. From the warm pool it's about 60 seconds — the OS is already booted,
the GCS agent is pre-installed, we just run `node enable` and update DNS."*

**Watch for:** `in-service=1`, then `● active` on the DTN node.

**Quit watch when DTN node shows active**, then:

```bash
# Ctrl-C out of watch
elastic-gcs-ctl status --region us-east-1
```

---

### Step 4 — Submit a test transfer (~30 seconds)

```bash
elastic-gcs-ctl transfer test --region us-east-1
```

**Say:** *"Let's do a real transfer. This submits a file from the 1000 Genomes collection
to a Globus Tutorial endpoint using the endpoint owner's credentials."*

**Expected output:**
```
Submitting test transfer...
  source:  28daf55e-...: /1000genomes/release/20130502/20140625_related_individuals.txt
  dest:    31ce9ba0-...: /~/elastic-gcs-test.txt

  task_id: <uuid>
  Waiting for SUCCEEDED...
  ✓ SUCCEEDED  1168 bytes  ~13s
```

---

### Step 5 — Show CloudWatch dashboard (~45 seconds of talk)

Switch to browser: CloudWatch dashboard `elastic-gcs-us-east-1`.

**Point out:**
- `ActiveTransfers` spike at transfer time
- ASG scale-out event in the `DTN Pool` widget
- Lambda invocation for `demand_monitor` and `lifecycle`

**Say:** *"The demand monitor publishes ActiveTransfers every minute. If it sees INACTIVE
tasks — transfers queued against a stopped endpoint — it auto-wakes the system. You can
submit a transfer before the endpoint is up and the system wakes itself."*

---

### Step 6 — Scale back in (~45 seconds)

```bash
elastic-gcs-ctl scale down --to 0 --watch --region us-east-1
```

**Say:** *"Now scale-in. The lifecycle hook calls `node disable` before the instance stops
— GCS removes it from DNS, transfers drain, then the instance goes back to the warm pool.
No data left on the instance; the node identity is preserved in Secrets Manager for next time."*

**Expected output:**
```
Setting desired=0...
Waiting for warm pool...
  [30s] i-09df2757... Warmed:Stopped  ✓
Done (1m 08s)
```

---

### Step 7 — Cost story (~30 seconds of talk)

```bash
elastic-gcs-ctl status --region us-east-1
```

**Say:** *"Back to idle. Two warm DTNs sitting stopped, costing a few dollars a month in EBS.
The control node is running — if I enable idle shutdown, it stops after 30 minutes of
inactivity too, dropping to ~$3.50/month total idle cost. Compare that to keeping two
m6i.xlarge DTNs always on: $400/month. For a dataset that researchers access occasionally
— workshop days, grant deadlines — elastic is the right model."*

**Point to slide:** cost comparison table from `docs/ephemeral-control-node.md`.

---

## Timing summary

| Step | Expected time | Notes |
|------|--------------|-------|
| 1 — Show idle state | 45s talk | No commands running |
| 2 — Wake + scale-out | 10s (CLI returns) | ~60s happening in background |
| 3 — DTN InService | 50–70s | Watch loop |
| 4 — Transfer | 15–30s | SUCCEEDED |
| 5 — CloudWatch | 45s | Browser tab |
| 6 — Scale-in | 60–90s | Watch loop |
| 7 — Cost story | 30s talk | |
| **Total** | **~5.5–7 min** | |

---

## Contingency paths

### Control node is stopped (not running)

Run `elastic-gcs-ctl wake --scale-out` **5 minutes before** the session starts.
Or demonstrate the wake from stopped as the opening step — it takes ~40s and is
a good story in itself.

### Warm pool evicted (all DTNs terminated, >72h idle)

The scale-out will launch from AMI — ~3–5 minutes, not 60s. Options:

1. If you have time: let it run, narrate the cold boot, show the contrast
2. If tight on time: skip steps 2–4, use pre-recorded [terminal cast] of warm path
3. Prevention: run `elastic-gcs-ctl wake` once in the 72h before the demo

### Globus auth expired

```bash
globus login
# authenticate as scofri@globusid.org
```

The `transfer test` subcommand requires an active `globus` CLI session.

### Transfer fails with "endpoint not found" or 404

The endpoint ID is stored in SSM. Check:
```bash
aws ssm get-parameter --name /elastic-gcs/us-east-1/endpoint-id --query Parameter.Value --output text
```
If this doesn't match `09c97a5c-63e2-40a2-bcc9-caf96c0de230`, update the constant in
`cmd/elastic-gcs-ctl/main.go` and rebuild.

### DTN node doesn't activate within 3 minutes

Check boot logs via SSM:
```bash
aws ssm send-command \
  --instance-ids <DTN_INSTANCE_ID> \
  --document-name AWS-RunShellScript \
  --parameters 'commands=["journalctl -u elastic-gcs-boot --no-pager -n 50"]' \
  --output text --query 'Command.CommandId'
# then: aws ssm get-command-invocation --command-id <id> --instance-id <DTN_INSTANCE_ID>
```

Common causes: slot-lock conflict (another instance holds the slot), Secrets Manager
permission error, GCS version mismatch. See [docs/operator-guide.md](operator-guide.md).

### CloudWatch dashboard not showing data

The custom metrics namespace is `elastic-gcs`. If no data appears:
- `demand_monitor` Lambda runs every minute — check its logs: `elastic-gcs-ctl logs demand_monitor --since 5m`
- Confirm `ENDPOINT_ID` env var on the Lambda matches the active endpoint

---

## Post-demo cleanup

```bash
# Scale to 0 if not already done
elastic-gcs-ctl scale down --to 0 --region us-east-1

# Verify warm pool restored
elastic-gcs-ctl status --region us-east-1
```

Leave the control node running (it will auto-shutdown after 30 min if idle shutdown is enabled).

---

## Benchmark numbers (for slides)

From `make benchmark REGION=us-east-1 TIER=warm` (5 iterations, 2026-02-26):

| Phase | Mean | P95 |
|-------|------|-----|
| EC2 resume (T0→T1) | ~38s | ~51s |
| SSM online (T1→T2) | ~5s | ~8s |
| node enable + DNS (T2→T3) | ~14s | ~17s |
| first transfer (T3→T4) | ~13s | ~20s |
| **Total** | **~70s** | **~90s** |

Cold path (launch from AMI): ~3m 30s mean.
Hot path (DTN already InService): ~13s mean.

Update these numbers from `test/benchmark/results/` after running benchmarks closer to the conference.
