# GlobusWorld 2026 — Demo Runbook

**Session:** Elastic GCS on AWS — Scale-on-Demand Open Data Endpoints
**Venues:** Chicago (Apr 21–22) · Raleigh (Jun 2–3)
**Presenter:** Scott Friedman
**Total demo time:** ≤ 8 minutes

---

## Pre-demo checklist (≥ 30 min before session)

- [ ] `aws sso login --profile aws` — ensure AWS credentials are fresh
- [ ] `globus whoami` — confirm authenticated as `scofri@globusid.org`
- [ ] Control node running: `scripts/control_node_power.sh us-east-1 default start` (Transfer rejects submissions to an endpoint with no serving node, #130)
- [ ] `aws-elastic-gcs-ctl status --region us-east-1` — check baseline state (see expected output below)
- [ ] At least one WARM slot (a stopped slot instance). If every slot is COLD, run `aws-elastic-gcs-ctl scale up --to 1 --watch`, then `scale down --to 0 --watch`, once before the session
- [ ] Terminal font ≥ 16pt, dark background, 120-column width
- [ ] Browser tab open to `https://app.globus.org/file-manager`

### Expected baseline state

```
Elastic GCS — us-east-1-default
━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
Control Node   i-065c4e068da377b11     ● running       34.224.250.21
Endpoint       09c97a5c-63e2-40a2-bcc9-caf96c0de230

DTN Slots      desired=0    serving=0    warm=2
  slot 1    i-09df2757f113b5448   stopped         phase=-           52.0.0.11
  slot 2    i-0b4cde7d88b82ece1   stopped         phase=-           52.0.0.12

GCS Nodes
  ○  030674af                        inactive
  ○  12ba8822                        inactive
  ●  26ce140b                        active      (control)

Active Transfers (5 min):  0
```

---

## Demo script

### Step 1 — Show the idle state (~45 seconds of talk)

```bash
aws-elastic-gcs-ctl status --region us-east-1
```

**Say:** *"This is a live Globus Connect Server endpoint fronting the AWS 1000 Genomes
Open Data bucket. Right now it's idle — the control node is serving, and both DTN slots
are stopped, paying only for their disks and addresses: about $5 a month each."*

**Point out:** `serving=0`, both slot nodes `inactive`, `Active Transfers: 0`.

---

### Step 2 — Ask for a DTN (~10 seconds)

```bash
aws-elastic-gcs-ctl scale up --to 1 --region us-east-1
```

**Say:** *"In production the demand monitor does this when it sees active transfers.
Here I'm setting desired DTNs by hand. The slot controller — a Lambda durable function —
picks it up within seconds and starts a stopped slot."*

---

### Step 3 — Watch the slot start serving (~90 seconds of live polling)

```bash
watch -n 5 aws-elastic-gcs-ctl status --region us-east-1
```

**Say:** *"The slot keeps its node identity and its Elastic IP while stopped, so all it has
to do on boot is `node enable` and wait for its address in endpoint DNS. A cold slot —
no instance at all — takes about a minute longer: launch from the AMI, then a one-time
`node setup` with credentials it can only read for those few seconds."*

**Watch for:** the slot `running` with `phase=starting`, then `phase=-` and `● active` on its node.

---

### Step 4 — Submit a test transfer (~30 seconds)

```bash
aws-elastic-gcs-ctl transfer test --region us-east-1
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

### Step 5 — Show demand (~30 seconds)

```bash
aws-elastic-gcs-ctl metrics --since 30m --region us-east-1
```

**Point out:** the `ActiveTransfers` sparkline and the slot controller and demand
monitor invocations.

**Say:** *"The demand monitor polls Globus every minute and sets how many DTNs we need
from the aggregate throughput of active tasks. Scale-in waits until nothing has been
active for 15 minutes."*

---

### Step 6 — Scale back in (~60 seconds)

```bash
aws-elastic-gcs-ctl scale down --to 0 --watch --region us-east-1
```

**Say:** *"Now scale-in. The controller disables the node — GCS takes it out of DNS so new
work goes elsewhere — waits for its data connections to finish, then stops it. In-flight
sessions finish on the draining node. The identity and the address stay with the slot
for next time."*

---

### Step 7 — Cost story (~30 seconds of talk)

```bash
aws-elastic-gcs-ctl status --region us-east-1
```

**Say:** *"Back to idle. Two warm slots cost about $10 a month. Stopping the control node
between sessions brings the whole endpoint to about $16 a month. Compare that to keeping
two c8in.2xlarge DTNs always on: about $800/month. For a dataset that researchers access
occasionally — workshop days, grant deadlines — elastic is the right model."*

**Point to slide:** cost comparison table from `docs/architecture.md`.

---

## Timing summary

| Step | Expected time | Notes |
|------|--------------|-------|
| 1 — Show idle state | 45s talk | No commands running |
| 2 — Scale up | 5s (CLI returns) | |
| 3 — Slot serving | ~90s | Watch loop (warm slot) |
| 4 — Transfer | 15–30s | SUCCEEDED |
| 5 — Metrics | 30s | |
| 6 — Scale-in | ~60s | Watch loop |
| 7 — Cost story | 30s talk | |
| **Total** | **~5–6 min** | |

---

## Contingency paths

### Control node is stopped

Start it **5 minutes before** the session:
`scripts/control_node_power.sh us-east-1 default start` (about 2 minutes, until its
address is in endpoint DNS). With no serving node, Transfer rejects submissions.

### Every slot is COLD

The scale-out revives a slot from the AMI — about 2.5 minutes to serving, not 1.5.
Options:

1. If you have time: let it run, narrate the cold revive, show the contrast
2. If tight on time: skip steps 2–4, use a pre-recorded terminal cast of the warm path
3. Prevention: scale up and back down once before the session, which leaves a WARM slot

### Globus auth expired

```bash
globus login
# authenticate as scofri@globusid.org
```

The `transfer test` subcommand requires an active `globus` CLI session.

### Transfer fails with "endpoint not found" or 404

The endpoint ID is stored in SSM. Check:
```bash
aws ssm get-parameter --name /elastic-gcs/us-east-1/default/endpoint-id --query Parameter.Value --output text
```
If this doesn't match `09c97a5c-63e2-40a2-bcc9-caf96c0de230`, update the constant in
`cmd/aws-elastic-gcs-ctl/main.go` and rebuild.

### Slot doesn't start serving within 3 minutes

Check the controller and the slot's node unit:
```bash
aws-elastic-gcs-ctl logs slot-controller --since 10m --region us-east-1
aws ssm send-command \
  --instance-ids <SLOT_INSTANCE_ID> \
  --document-name AWS-RunShellScript \
  --parameters 'commands=["journalctl -u elastic-gcs-node --no-pager -n 50"]' \
  --output text --query 'Command.CommandId'
# then: aws ssm get-command-invocation --command-id <id> --instance-id <SLOT_INSTANCE_ID>
```

Common causes: Secrets Manager permission error, GCS version mismatch. See
[docs/operator-guide.md](operator-guide.md).

---

## Post-demo cleanup

```bash
# Scale to 0 if not already done
aws-elastic-gcs-ctl scale down --to 0 --region us-east-1

# Verify the slots are stopped again
aws-elastic-gcs-ctl status --region us-east-1
```

Stop the control node when you're done: `scripts/control_node_power.sh us-east-1 default stop`.

---

## Benchmark numbers (for slides)

Slot controller, us-west-2, c8in.2xlarge (November 2026, #132):

| Path | Time |
|------|------|
| Cold revive: launch → node setup done | ~47 s (node setup ~18 s) |
| Warm start: desired 0→1 → healthy and in DNS | ~95 s |
| Real demand: task submitted → cold slot healthy and in DNS | ~147 s |
| Drain under load → stopped, 0 task faults | ~3 min (sessions finished first) |

Earlier ASG warm-pool numbers (`make benchmark TIER=warm`, 2026-02-28, before the
slot controller):

| Phase | Min | Mean | P95 | Max |
|-------|-----|------|-----|-----|
| EC2 resume (T0→T1) | 7s | 11s | 13s | 13s |
| SSM online (T1→T2) | 12s | 12s | 13s | 13s |
| node enable + DNS (T2→T3) | 11s | 21s | 23s | 23s |
| first transfer (T3→T4) | 13s | 20s | 45s | 45s |
| **Total** | **50s** | **64s** | **90s** | **90s** |

Hot path (DTN already serving), 3 iterations:

| Phase | Min | Mean | P95 | Max |
|-------|-----|------|-----|-----|
| submitted → first bytes | 5s | 5s | 6s | 6s |
| first bytes → SUCCEEDED | 11s | 11s | 11s | 11s |
| **Total** | **16s** | **16s** | **17s** | **17s** |

`make benchmark TIER=warm|hot|cold` now drives the slot controller; re-run closer
to the conference to refresh the numbers. Raw JSON in `test/benchmark/results/`.
