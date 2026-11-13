#!/usr/bin/env python3
"""Estimate Globus task throughput between two endpoints (Elastic GCS calibration).

Model (calibrated on the 2026-11 us-west-2 benchmarks; see docs/performance/throughput-model.md):

  concurrency  C = min(our max, peer max, 128)         -- negotiated per task by Globus
  per-process  r = file_mb / (overhead_s_per_file + file_mb / 75)   [MB/s, 75 MB/s = one-file stream]
  node limit   N_min * node_gbps                        -- the side with less capacity
  steady       T = min(C * r, our_dtns * our_node_gbps, peer_dtns * peer_node_gbps)
  wall         = 20 s + 10 ms/file + bytes / T            -- listing/ramp + checksum tail grow with file count

Back-test (docs/performance/throughput-model.md): steady state within ~12% and wall time
within ~20% across the tuned runs; e.g. one c8in.2xlarge per side at concurrency 32 on
198 GB of 59 MB files -> 8.9 Gbps steady (measured 9.2), ~232 s wall (measured 251.5 s).

usage:
  throughput_model.py --gb 198 --files 3381 --our-conc 64 --peer-conc 4 --our-dtns 4
  throughput_model.py --gb 35 --files 504 --peer normal
"""
import argparse

PRESETS = {"normal": 4, "aggressive": 8}  # Globus network_use presets, observed max concurrency (0 ms RTT)
NODE_GBPS = {"c8in.2xlarge": 9.2, "c8in.xlarge": 6.25, "m8azn.xlarge": 5.0, "c8in.4xlarge": 18.4}  # 4xlarge: extrapolated
STREAM_MB_S = 75.0       # single-file ceiling of the stock S3 connector (111 GB file, T5)
FILE_OVERHEAD_S = 0.9    # per-file cost (control commands, S3 requests, checksum); fits 0.29 Gbps/process at 59 MB
TASK_OVERHEAD_S = 20.0   # fixed per-task cost (submission, first connections)
TASK_OVERHEAD_PER_FILE_S = 0.010  # listing + checksum tail scale with file count (fit: 504-13,125 files)
TASK_CONC_CAP = 128      # Transfer rejects endpoint max concurrency above 128


def estimate(gb, files, our_conc, peer_conc, our_dtns, peer_dtns, our_node, peer_node):
    c = min(our_conc, peer_conc, TASK_CONC_CAP)
    mean_mb = gb * 1000 / max(files, 1)
    per_proc_mb_s = mean_mb / (FILE_OVERHEAD_S + mean_mb / STREAM_MB_S)
    proc_gbps = c * per_proc_mb_s * 8 / 1000
    limits = {
        "concurrency x per-process": proc_gbps,
        "our DTNs": our_dtns * NODE_GBPS[our_node],
        "peer DTNs": peer_dtns * NODE_GBPS[peer_node],
        "files in flight": files * per_proc_mb_s * 8 / 1000,  # fewer files than processes
    }
    binding = min(limits, key=limits.get)
    steady = limits[binding]
    wall_s = TASK_OVERHEAD_S + TASK_OVERHEAD_PER_FILE_S * files + gb * 8 / steady
    return {"negotiated_concurrency": c, "mean_file_mb": round(mean_mb, 1),
            "per_process_gbps": round(per_proc_mb_s * 8 / 1000, 3),
            "limits_gbps": {k: round(v, 2) for k, v in limits.items()},
            "binding_limit": binding, "steady_gbps": round(steady, 2),
            "wall_s": round(wall_s), "wall_gbps": round(gb * 8 / wall_s, 2),
            "useful_dtns_per_side": max(1, round(c * per_proc_mb_s * 8 / 1000 / NODE_GBPS[our_node] + 0.49))}


def main():
    ap = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    ap.add_argument("--gb", type=float, required=True, help="task size in GB")
    ap.add_argument("--files", type=int, required=True, help="number of files in the task")
    ap.add_argument("--our-conc", type=int, default=64, help="our endpoint max concurrency")
    ap.add_argument("--peer-conc", type=int, help="peer endpoint max concurrency")
    ap.add_argument("--peer", choices=sorted(PRESETS), help="peer network_use preset instead of --peer-conc")
    ap.add_argument("--our-dtns", type=int, default=1)
    ap.add_argument("--peer-dtns", type=int, default=1)
    ap.add_argument("--our-node", default="c8in.2xlarge", choices=sorted(NODE_GBPS))
    ap.add_argument("--peer-node", default="c8in.2xlarge", choices=sorted(NODE_GBPS))
    a = ap.parse_args()
    peer_conc = PRESETS[a.peer] if a.peer else (a.peer_conc or a.our_conc)
    r = estimate(a.gb, a.files, a.our_conc, peer_conc, a.our_dtns, a.peer_dtns, a.our_node, a.peer_node)
    for k, v in r.items():
        print(f"{k:24} {v}")


if __name__ == "__main__":
    main()
