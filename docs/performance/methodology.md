# Methodology and results

Campaign `2026-11-us-west-2`, run 2026-11-12/13 for #58.
- **Data:** raw per-run logs and node snapshots are in
  [`test/benchmark/results/2026-11-us-west-2/raw/`](../../test/benchmark/results/2026-11-us-west-2/raw/),
  the per-run table in `runs.json`, and curated figures in `summary.json`.
- **Harness:** [`test/benchmark/harness/`](../../test/benchmark/harness/).

## Topology

```
RODA public buckets (us-west-2)                       private bench bucket (us-west-2)
        │ S3 gateway endpoint                                  ▲ S3 gateway endpoint
        ▼                                                      │
  [src DTNs] ── GridFTP over public IPs / internet gateway ──▶ [dst DTNs]
  Elastic GCS deployment "src"                           Elastic GCS deployment "dst"
  + t3a.large control node                               + t3a.large control node
```

- **Two independent deployments in one region** (multi-deployment support, #57).
  `dst` plays the remote endpoint, so both sides are observable and controllable.
  Traffic between them uses public IPs, so EC2's internet-gateway limits apply as
  they would for a real remote user. It's billed at the same-region rate (~$0.02/GB
  round trip) rather than as internet egress.
- **GCS 5.4.101** on Rocky Linux 9 (x86-64), from the version-pinned DTN AMI.
- **`src` gateway:** anonymous S3 (`--s3-unauthenticated`) over four RODA buckets.
- **`dst` gateway:** `--s3-user-credential --admin-managed-credentials`. The `dst`
  admin registered an IAM key scoped to the bench bucket for the submitting
  identity.
- **Submitter:** the `src` service account, from the `src` control node, so Globus
  secrets never leave AWS. It uses globus-sdk 4.10 with dependent `data_access`
  scopes for both collections; service accounts need no interactive consent.
- **Control nodes:** taken out of the data path (`node update --disable`) after
  T1b, except where noted.
- **Scaling alarms:** actions disabled during runs, so DTN counts were set
  explicitly.

## Measurements

- **Wall throughput:** bytes ÷ (completion − submission), from the Transfer task.
  It includes per-task listing and ramp (~20 s + ~10 ms per file) and the checksum
  tail.
- **Steady state:** sum of DTN NIC `tx_bytes` rates over a 20–80 s mid-transfer
  window, sampled on every node in parallel over SSM (`netsnap.sh`, `rates.py`).
- **CPU:** `/proc/stat` busy share over the same window.
- **Negotiated settings:** concurrency and parallelism from each task's `STARTED`
  events.
- **Latency:** `tc netem` on the `dst` DTNs, applied only to traffic toward the
  `src` DTN IPs, so S3 writes are unaffected. Verified on live data connections
  with `ss -ti`: 128 connections at 50.2–50.8 ms under +50 ms.

## Datasets

| Name | Path | Files | Size |
|---|---|---|---|
| Sentinel tile, 2024 | `sentinel-cogs/sentinel-s2-l2a-cogs/10/T/ET/2024/` | 3,381 | 198 GB (mean 59 MB) |
| Sentinel tile, June–July | `…/10/T/ET/2024/{6,7}/` | 504 | 35 GB |
| Sentinel, 8 tiles 2024 | `…/10/T/{ET,ES,ER,EQ,DT,DS,DR,DQ}/2024/` | 29,296 | 1.24 TB |
| NREL WTK single file | `nrel-pds-wtk/Mid_Atlantic/2000/Mid_Atlantic_2000_10m.h5` | 1 | 111 GB |
| NASA POWER zarr | `nasa-power/merra2/temporal/power_merra2_annual_temporal_lst.zarr/` | 123,676 | 4.3 GB (mean 35 KB) |

## Results

### Network use, one c8in.2xlarge DTN per side (198 GB)

| Setting | Concurrency | Control node in path | Per-DTN steady | Wall | Run |
|---|---|---|---|---|---|
| normal | 4 | yes | 0.35 Gbps | cancelled | t1-c8in2x-1v1-sentinel |
| aggressive | 8 | yes | 1.3 Gbps | 2.13 Gbps (745 s) | t1b-aggressive |
| aggressive | 8 | no | 1.1 Gbps | cancelled | t1c-aggr-noctl |
| custom 32/64 | 32 | no | **9.2 Gbps** | **6.31 Gbps (252 s)** | t1d-custom32-noctl |

### DTN scaling, single task

| DTNs per side | Concurrency | Data | Wall | Steady (sum) | Faults |
|---|---|---|---|---|---|
| 1 | 32 | 198 GB | 6.3 Gbps | 9.2 Gbps | 0 |
| 2 | 64 | 373 GB | 11.5 Gbps | 18.1 Gbps | 0 |
| 4 | 128 | 659 GB | 17.4 Gbps | 37.2 Gbps | 0 |
| 8 | 128 | 1.24 TB | 13.0 Gbps | bursty, 2–4 Gbps per node | 2 |
| 8, **2 concurrent tasks** | 128 each | 1.24 TB | **25.0 Gbps aggregate** | — | 1 |

### Peer settings (35 GB; our side custom 32/64)

| Peer | Our DTNs | Tasks | Negotiated | Wall |
|---|---|---|---|---|
| normal | 1 | 1 | 4 × 8 | 1.15 Gbps |
| normal | 4 | 1 | 4 × 8 | 1.13 Gbps |
| aggressive | 4 | 1 | 8 × 8, then 24 × 8 | 1.58 Gbps |
| custom 32/64 | 4 | 1 | 32 × 8 | 4.77 Gbps |
| normal | 4 | 8 concurrent | 4 × 8 each | 5.03 Gbps aggregate (0.6–1.9 per task) |

### Round-trip time (35 GB)

| Peer | Added RTT | Parallelism | Negotiated | Wall |
|---|---|---|---|---|
| custom | 0 | 8 | 32 × 8 | 4.77 Gbps |
| custom | +20 ms | 8 | — | 5.25 Gbps |
| custom | +50 ms | 8 | — | 4.80 Gbps |
| custom | +50 ms | 1 | 32 × 1 | 5.24 Gbps |
| custom | +80 ms | 8 | 32 × 8 | 4.77 Gbps |
| normal | 0 | — | 4 × 8 | 1.13 Gbps |
| normal | +80 ms | — | **8** × 8 | 1.86 Gbps |

### Instance type (one src DTN; dst c8in.2xlarge; 198 GB; concurrency 32)

| Type | CPU | Steady | Binding limit | CPU used | vCPU per Gbps | Wall | $/h |
|---|---|---|---|---|---|---|---|
| c8in.xlarge | Intel Xeon 6, 2C/4T, 3.9 GHz | 6.25 Gbps | network baseline | 77% | 0.49 | 4.98 Gbps | 0.272 |
| m8azn.xlarge | AMD EPYC, 4C, 5.0 GHz | 5.0 Gbps | 5 Gbps IGW cap | 36% | 0.29 | 4.06 Gbps | 0.413 |
| c8in.2xlarge | Intel Xeon 6, 4C/8T, 3.9 GHz | 9.2 Gbps | concurrency / GCS per node | 46% | 0.40 | 6.31 Gbps | 0.544 |

Graviton wasn't tested: GCS publishes arm64 packages only for Ubuntu, and the
deployment is Rocky Linux.

### Data shape

| Shape | Concurrency | Result |
|---|---|---|
| 111 GB single file | — | ~0.6 Gbps (75 MB/s), 3% CPU; cancelled after 12 GB |
| 35 KB objects × 123,676 | 128 | ~50 files/s, then S3 503 `SlowDown` on dst; task stalled (`ENDPOINT_TOO_BUSY`); cancelled |
| 35 KB objects × 123,676 | 16 | ~175 files/s, 0 faults for ~10 min; throttling returned later as the prefix filled |
| 59 MB files | 8 / 32 / 128 | ~0.29 Gbps per process in every case |

### Checksum verification (198 GB, 4+2 DTNs, custom 64/128)

| Order | verify_checksum | Wall | Faults |
|---|---|---|---|
| 1 | on | 11.3 Gbps | 0 |
| 1 | off | 4.6 Gbps | 3 × `CONNECTION_RESET` (retry backoff; discarded) |
| 2 | off | **12.2 Gbps** | 0 |
| 2 | on | **11.8 Gbps** | 0 |

### Scale-out timing (warm pool, seconds from SetDesiredCapacity)

| Step | Before #72 (to InService / to GCS active) | After #72, DNS-gated |
|---|---|---|
| 0→1 | 50 / 77 | 99–105 / 111–148 |
| 1→2 | 39 / 50 | — |
| 2→4 | 45 / 88 | — |
| 1→4 | — | 121 / 132 |
| 4→8 | 54 / 129 | — |

After #72, InService means the node is in endpoint DNS (true readiness).

## Caveats

- **One region, one instance family,** on a well-provisioned path. Real remote
  peers add their own storage and network limits.
- **Few repeats.** Runs are mostly single; the checksum pair was repeated after a
  confounded first run.
- **Rates are sampled.** Steady state comes from 20–80 s windows, and the 8-DTN
  single-task run had no stable window.
- **Second ENI not tested.** EC2 bandwidth is per network card, and only 96xlarge
  (c8in/m8in) and 48xlarge (c8gn) sizes have two cards.

### Kernel network tuning (#145)

Run 2026-11-16 to decide whether the DTN AMI should ship ESnet fasterdata
tuning. This was an A/B on the same pair of c8in.2xlarge DTNs, one per side,
with the control nodes out of the data path.
- **Data:** Sentinel June–July, 504 files, 35 GB.
- **Peer:** `dst` set to `normal` network use, as a typical remote endpoint; it
  negotiated 4 × 8.
- **Path:** `tc netem` on the `src` DTN's egress toward the `dst` DTN only, so
  the data direction sees the delay and loss.
- **Kernel tuning:** applied live with `configure_network.sh`. The 0.1% pair
  flips only the sender's congestion control.
- **Measurement:** steady state is `src` DTN NIC transmit rate over a 180 s
  mid-transfer window.
- **Verification:** `ss -ti` on the data connections showed 32 sockets on `bbr`
  at 100.1–100.2 ms RTT.

| Run | Path | Sender | Steady | Task |
|---|---|---|---|---|
| A0 | none | defaults (`cubic`, 6 MB) | 1.05 Gbps | SUCCEEDED, 0 faults |
| B0 | none | tuned (`bbr`, fq, 32 MB) | 1.01 Gbps | SUCCEEDED, 0 faults |
| A1 | +100 ms, 0.01% loss | defaults | 1.03 Gbps | SUCCEEDED, 0 faults |
| B1 | +100 ms, 0.01% loss | tuned | 0.98 Gbps | SUCCEEDED, 0 faults |
| B2 | +100 ms, 0.1% loss | `bbr` | **0.97 Gbps** | SUCCEEDED, 0 faults |
| A2 | +100 ms, 0.1% loss | `cubic` | **0.16 Gbps** | cancelled after the window |

- **Clean or mildly lossy paths:** tuning is neutral. With 4 GridFTP processes
  the limit is about 0.26 Gbps per process on the S3 side. Each of the 32 streams
  needs only about 31 Mbps, which `cubic` sustains at 100 ms with 0.01% loss.
- **At 0.1% loss:** `cubic` falls to about 5 Mbps per stream, near the Mathis
  bound, and 6× below BBR. BBR holds the application-limited rate, so the AMI
  ships the tuning.

## Cost

About **$103** benchmark-related in us-west-2 through 2026-11-13 (Cost Explorer,
partial day). Of that, $77 was regional data transfer, $11 c8in instances,
$2.30 control nodes, and ~$12 storage, snapshots and requests.
