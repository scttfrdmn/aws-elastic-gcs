# Throughput model

A small model that predicts a Globus task's steady-state throughput and wall
time between two endpoints, calibrated on the November 2026 benchmarks.
Calculator: [`scripts/throughput_model.py`](../../scripts/throughput_model.py).

## The model

A task's steady-state throughput is the smallest of four limits:

```
C  = min(our max concurrency, peer max concurrency, 128)       negotiated per task
r  = file_MB / (0.9 s + file_MB / 75 MB/s)                     per-process rate
T  = min( C × r,                                               processes
          our_DTNs × our_node_Gbps,                            our capacity
          peer_DTNs × peer_node_Gbps,                          peer capacity
          files × r )                                          fewer files than processes
wall ≈ 20 s + 10 ms × files + bytes / T
```

| Constant | Value | From |
|---|---|---|
| Single-file stream | 75 MB/s | 111 GB file, any instance size (T5) |
| Per-file overhead | 0.9 s | fits ~0.29 Gbps/process on 59 MB files at concurrency 8, 32 and 128 |
| Per-node capacity | c8in.2xlarge 9.2, c8in.xlarge 6.25, m8azn.xlarge 5.0 Gbps | T1d, T2a, T2b |
| `normal` / `aggressive` concurrency | 4 / 8 | negotiated on the tasks' STARTED events at 0 ms RTT |
| Transfer concurrency cap | 128 | `endpoint update` rejects 256 |

What it leaves out:
- Globus re-tunes concurrency for high-latency paths (a `normal` peer got 8 at
  +80 ms) and mid-task (`aggressive` went from 8 to 24). The model uses the
  0 ms preset.
- S3 write throttling for tiny objects.
- Faults and their retry backoff.
- The c8in.4xlarge figure in the calculator is extrapolated, not measured.

## Back-test

| Run | Setup | Measured wall | Model wall | Measured steady | Model steady |
|---|---|---|---|---|---|
| T1d | 1+1 c8in.2xlarge, 32, 198 GB / 3,381 files | 251.5 s | 232 s | 9.2 | 8.9 |
| T3a | 2+2, 64, 373 GB | 258.2 s | 256 s | 18.1 | 17.5 |
| T3b | 4+4, 128, 659 GB | 302.7 s | 312 s | 37.2 | 32.8 |
| T2b | c8in.xlarge src, 32, 198 GB | 318.6 s | 308 s | 6.25 | 6.25 |
| P1a | peer `normal`, 35 GB / 504 files | 242.0 s | 255 s | — | 1.22 |
| P1b | peer `normal`, our 4 DTNs | 246.6 s | 255 s | — | 1.22 |
| P2 | peer `aggressive` (re-tuned mid-task, 2 faults) | 176.3 s | 140 s | — | 2.43 |
| P3a | tuned peer, 35 GB | 58.5 s | 54 s | — | 9.7 |

Wall time is within ~8% except P2. Steady state is within ~12%.

## Using it

```bash
# A researcher at a campus endpoint left at defaults, pulling 2 TB of 50 MB files:
scripts/throughput_model.py --gb 2000 --files 40000 --peer normal --our-dtns 4
#   binding_limit: concurrency x per-process; steady ~1.0 Gbps, ~4.5 h; useful_dtns_per_side 1

# The same pull with the campus endpoint at custom 64:
scripts/throughput_model.py --gb 2000 --files 40000 --peer-conc 64 --our-dtns 4 --peer-dtns 2
#   binding_limit: concurrency x per-process; steady ~16 Gbps, ~23 min; useful_dtns_per_side 2
```

`useful_dtns_per_side` is the number of our DTNs a single such task can keep
busy. Size the pool from the sum over concurrent tasks.
