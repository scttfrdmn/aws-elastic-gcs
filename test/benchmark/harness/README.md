# Benchmark harness

Tools for measuring Elastic GCS transfers between two deployments, `src` and
`dst`, in one region. The shell tools ran the November 2026 campaign
(`docs/performance/methodology.md`). The Python tools run the envelope study
of stock GCS 5.4 transfers to and from S3 (#151).

| Tool | What it does |
|---|---|
| `ssm_run.sh`, `gcs.sh` | Run a command on an instance over SSM; run the GCS CLI on a control node |
| `xfer_bg.sh`, `xfer_wait.sh`, `xfer_multi.sh`, `bench_transfer.py` | Submit and follow transfers from the src control node |
| `netem.sh` | Add delay and loss on one side's DTNs, toward the other side's DTNs only |
| `netsnap.sh`, `rates.py`, `scale.sh`, `pull_runs.sh`, `summarize.py`, `multi_summary.py` | November campaign helpers |
| `synth.py`, `datasets/*.yaml`, `posix_synth.sh` | Generate synthetic datasets in S3 or on a DTN volume |
| `study.py`, `plans/envelope.yaml` | Run a study plan: points, randomization, refinement, repeats, records |
| `steady.py`, `hstats.py` | Steady-state detection and statistics |
| `dtn_sampler.sh` | 1 s NIC, CPU, memory, retransmit and per-process samples on each node |
| `gridftp_log.py` | Parse, enable and collect GridFTP per-transfer records |
| `s3_access_logs.py` | Profile S3 server access logs per object and per point (P1b) |
| `s3probe.py` | P0 S3 control: CRT client GET/PUT, run on a DTN |
| `analyze.py` | Tables with CIs, model fits with validation, figures, attribution template |

Local requirements: Python 3, the AWS CLI and boto3 (live runs only),
matplotlib (figures only). The src control node needs only `globus_sdk` in
`/opt/elastic-gcs/bench/venv`, as before.

## Safety rules

1. **Secrets stay in AWS.** Commands sent over SSM fetch Globus client secrets
   from Secrets Manager on the instance, inside the command
   (`$(aws secretsmanager get-secret-value ...)`). Nothing in the harness
   reads, prints or stores a secret, and `hlib.assert_no_inline_secret`
   refuses a command that carries one. `bench.env` holds IDs only.
2. **Datasets are private.** Both buckets block public access. Transfers use
   credentialed collections, so no public-egress path exists.
3. **Delete datasets after each phase:** `synth.py delete datasets/<X>.yaml
   --bucket $SYNTH_BUCKET` (add `--seeds` after the last phase). Every point
   deletes what its transfer wrote, including incomplete multipart uploads,
   unless you pass `--keep-data`. Add a lifecycle rule on both buckets that
   aborts incomplete multipart uploads after one day, as a backstop.
4. **Know the cost before you run.** `synth.py plan` and `study.py plan` print
   counts, bytes and a dollar estimate offline. `study.py run` prints the
   estimate for the phase and asks for confirmation (skip it with `--yes`).
5. **Restore the pools afterwards.** The run disables the demand monitors and
   restores them when it ends. After the instance-size arm, run `study.py
   reset` so stopped slots go back to c8in.2xlarge.

## Prerequisites

- Both deployments up, with the control nodes out of the data path (#146) and
  `max_dtns` ≥ 8. For instance-type overrides, enough **stopped warm slots**:
  the override changes the type of stopped slot instances, and fresh launches
  still use the launch template's type. `study.py` checks the serving types
  before every point and fails the point if they don't match.
- `bench.env` filled in from `bench.env.example`:
  - the synth bucket and a src collection on it (credentialed S3 gateway);
  - the dst bench bucket behind `DST_COL`;
  - POSIX collections on each side's gp3 volume for P1;
  - S3 server access logging on both buckets to `S3_LOG_BUCKET` for P1b.
- **Write access for seeds.** The instance named in `synth.py create
  --instance` needs `s3:PutObject` on `$SYNTH_BUCKET/synth/_seeds/*`. P0-s3
  runs on a src DTN, whose runtime role needs `s3:GetObject` and
  `s3:ListBucket` on the synth bucket and `s3:PutObject` and
  `s3:DeleteObject` on `probe/*`. A bucket policy naming those roles avoids
  changing the stacks.
- `python3 study.py install` copies `bench_transfer.py` to the src control
  node. It is backward compatible with `xfer_bg.sh`.

## Workflow

```bash
cd test/benchmark/harness
# 1. Generate data (seeds are written in-region over SSM; fan-out is server-side)
python3 synth.py plan datasets/*.yaml                                   # offline
python3 synth.py create datasets/F64M.yaml --bucket $SYNTH_BUCKET --instance $SRC_CONTROL
python3 study.py datasets plans/envelope.yaml --phase P3                # refs a phase needs
python3 synth.py create F1K "F1K+fanout=4" --bucket $SYNTH_BUCKET --instance $SRC_CONTROL
python3 synth.py create datasets/F1G.yaml --target posix --instance <src DTN> --root /data

# 2. Plan (offline): schedule, measurement count, hours, TB moved, dollars
python3 study.py plan plans/envelope.yaml [--phase P1] [--verbose]

# 3. Run, one phase at a time; rerunning resumes
python3 study.py run plans/envelope.yaml --phase P1

# 4. P1b: fetch access logs (delivery takes hours), then join them to points
python3 s3_access_logs.py fetch --log-bucket $S3_LOG_BUCKET --log-prefix $S3_LOG_PREFIX \
    --start 2026-11-20T10:00 --end 2026-11-20T18:00 --out ../results/<campaign>/s3logs
python3 s3_access_logs.py join ../results/<campaign>/points.jsonl ../results/<campaign>/s3logs

# 5. Analyze: tables, fits, figures, attribution template
python3 analyze.py ../results/<campaign>
```

Both tools take dataset refs as well as paths: `F64M` is `datasets/F64M.yaml`,
and `F1K+fanout=4` or `F1K+naming=sequential+fanout=1` override top-level spec
fields (the name gains the overrides, so the prefix says what it is).

## Datasets

A spec in `datasets/*.yaml` sets:

- **size:** fixed (`size: 64MiB`), `lognormal` (median, sigma, min, max),
  `bimodal` (modes with size, weight and sigma), or `histogram` (replayed bins
  from `synth.py histogram s3://bucket/prefix/`);
- **count** and/or **total_bytes** (a cap);
- **layout:** `fanout` (top-level prefixes, 1–256), `depth` (1–6) and
  `subfanout`;
- **naming:** `hashed` (spread across partitions), `sequential` (adjacent
  keys, the hot-prefix case) or `dated` (year/month/day/hour);
- **target:** s3 or posix;
- **content:** `seed_max`, `part_size` and `quantize_bins`.

Keys and sizes are deterministic; the spec hash names the prefix
(`synth/<NAME>-<hash8>/`). Objects up to `seed_max` (1 GiB) are CopyObject
copies of a seed of the same size. Larger objects are multipart
UploadPartCopy from one `part_size` (512 MiB) seed, so their content repeats
every 512 MiB, which nothing in the path compresses. Distributions are
quantized to 64 geometric sizes, so they need at most 64 seeds. The manifest
(`<prefix>.manifest.json.gz`, all keys and sizes, plus the spec and its hash)
sits beside the dataset, never inside it. The MIX spec stops at its 1 TiB cap
after about 4,900 objects, because 20,000 draws from that lognormal would be
about 4.3 TiB.

## Measurement

Each point is a fresh task. In order, a point:

1. pins the instance type and the DTN count on both sides, and waits for the
   DNS gate: endpoint DNS equals exactly the serving DTNs' addresses;
2. sets `network_use` on both endpoints (custom c/p, or a preset);
3. applies netem on the sending side, toward the peer DTNs only;
4. starts `dtn_sampler.sh` on every DTN and control node, enables the GridFTP
   transfer log (`/etc/gridftp.d/zz-bench-transfer-log`) and records the log
   offsets;
5. submits k tasks through `bench_transfer.py`;
6. polls every 15 s until it finds steady state, then cancels;
7. collects the runner logs, samples, GridFTP log slices and `ss -tin`
   snapshots into `raw/<rid>/`, and deletes the transferred data.

**Steady state** (`steady.detect_steady`) uses the sum of the sending DTNs'
NIC tx on a common 1 s grid. The level is stable at time τ when the OLS slope
of the samples in (τ−10 s, τ] is not significantly different from 0 (p >
0.05). Steady state starts once that has held for 10 s, with the rolling mean
above `min_level_gbps` and at least half the running maximum, so the listing
phase and dips between files don't count. The 45 s after that are the
measurement window. It reports mean, sd, CV, a 95% CI with a lag-1
autocorrelation correction, and the window's own trend p-value. P3 uses
`mode: fixed`, which measures from 60 s to 300 s because small-file runs have
no bandwidth plateau.

**Design.**

- **Randomization:** order is random within a phase. The seed is
  `<plan seed>:<phase>:<round>`, and is recorded with the order index.
  `block_by` randomizes within blocks of a costly factor such as instance
  type.
- **Repeats:** headline points get n = 3. After refinement, the points
  around each knee get n = 3, and a seeded random 20% of the rest get a
  second run.
- **Refinement:** the gain between neighbouring points is scaled to its
  share of the enclosing coarse step. Where it falls below 10%, the run
  bisects both sides of the knee until it lies within one `step`.

## Records: `results/<campaign>/points.jsonl`

One JSON object per measurement (schema `envelope-point/1`). Later lines win
for the same `rid`, so a re-run of a failed point replaces it.

| Field | Meaning |
|---|---|
| `uid`, `rid`, `rep` | Point id (hash of phase + factors), run id `<uid>-r<rep>`, repetition |
| `phase`, `kind`, `factors`, `curves`, `headline` | What was measured; `factors` is fully normalized (`n_src`, `n_dst`, `instance_type_src`, ...) |
| `round`, `order_index`, `order_seed`, `blocked_by` | Randomization record |
| `status` | `ok` (complete window), `no_steady` (data but no complete window), `failed` (see `error`) |
| `stop_reason` | `steady_window`, `fixed_window`, `tasks_finished`, `timeout`, `runner_error` |
| `metrics` | `steady_gbps`, `steady_ci95`, `steady_cv`, `window_gbps`, `recv_rx_gbps` (cross-check), `wall_gbps`, `files_per_s`, `gridftp_window_gbps` |
| `steady` | Detector output: onset, window, stats (n, mean, sd, cv, ci95, n_eff, rho1, trend_p), parameters |
| `series` | The 1 s aggregate send rate (Gbps) from `t0`, for re-analysis |
| `tasks` | Per task: id, status, bytes, files, faults, wall, negotiated settings from `STARTED` events, event counts |
| `negotiated` | All tasks' negotiated `{concurrency, parallelism, ...}` |
| `network_use` | Each endpoint's network-use fields as `endpoint show` reported them |
| `nodes` | Per node over the window: rx/tx Gbps, CPU %, max core %, max GridFTP process CPU %, retransmits/s, min available memory, GridFTP processes |
| `gridftp` | Per direction: files, per-file rate median/p10/p90, streams, files in flight, window Gbps |
| `ss` | Per DTN: data connections, RTT min/median/max, congestion control counts |
| `netem`, `topology`, `instance_info`, `route`, `s3_prefixes`, `dataset*`, `split` | Conditions, paths and prefixes (used to join access logs) |
| `t_start`, `t_submit`, `t_end`, `harness_commit` | Timing (epoch s) and harness version |
| `iperf3`, `s3probe` | P0 results for those kinds |

Raw artifacts per run are in `results/<campaign>/raw/<rid>/`: runner JSONL,
`<instance>.nic` and `.proc` samples, GridFTP log slices, `ss` snapshots, and
after `s3_access_logs.py join`, `s3_profile.json` and `s3_objects.jsonl`.

## Tests

```bash
python3 -m pytest test/benchmark/harness/tests          # or, without pytest:
python3 -m unittest discover -s test/benchmark/harness/tests
```

The tests are offline. They cover dataset planning, the YAML subset parser
(checked against PyYAML when it is installed), steady-state detection on
synthetic series, both log parsers, plan expansion and adaptive refinement,
a full point against a fake backend, and the model fits.

## Not verifiable offline

These need checking on the first live points. Each is a single command or a
look at the first record.

- **GridFTP transfer records.** Check that GCS 5.4 honours `log_transfer` from
  a non-`globus*` file in `/etc/gridftp.d/`. Check the record fields (`NBYTES`,
  `STREAMS`, `TYPE`) for S3-connector transfers, and that the restart doesn't
  cut sessions with the #141 drop-in.
- **Negotiated settings.** Check the format of `STARTED` event `details`, and
  that `task_event_list(limit, offset)` works in globus-sdk 4.10. The parser
  accepts JSON or `key=value`.
- **`endpoint show -F json`.** Check its network-use field names, and whether
  `gcs_manager_url` gives the endpoint FQDN (otherwise set
  `*_ENDPOINT_FQDN`).
- **The instance-type override.** Check that `modify-instance-attribute`
  across c8in sizes works on stopped slots, and that the slot controller
  revives them unchanged.
- **The CRT probe.** Check `recv_filepath=/dev/null` and streamed PUT bodies
  in awscrt on Rocky 9, and pip access to PyPI from the DTN.
- **iperf3.** Check that Rocky's package installs, and that ports 50990–51000
  are open in the security group. They are inside the GridFTP range, so run
  P0 with no transfers active.
- **Access logs.** Check `object_size` for `REST.PUT.PART`, which should be
  the part size.
