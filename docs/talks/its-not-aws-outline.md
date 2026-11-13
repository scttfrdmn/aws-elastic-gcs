# Talk outline: "It's not the cloud, it's the settings"

A 25-minute talk with Q&A, for Globus admins and research-computing staff.
Each slide gives a chart from [`docs/performance/charts/`](../performance/charts/)
or a table from the [methodology](../performance/methodology.md). The companion
write-up is the [blog post](../blog/2026-11-its-not-aws.md).

**One-line takeaway:** per-task Globus throughput is set by the slower endpoint's
settings and the shape of the data, not by AWS.

## 1. Setup (4 min)

1. **Title.** "It's not the cloud, it's the settings."
2. **The problem.** Public datasets sit in S3. Researchers want them through
   Globus. An always-on endpoint costs ~$400/month to mostly sit idle.
3. **Elastic GCS in one diagram.** The cold/warm/hot tiers from the README: a
   control node, plus DTNs stopped in an ASG warm pool that start on demand.
   - Pre-created node identities (`node create --export-node`) are what make the
     pool elastic.
4. **Readiness in about 2 minutes.** Chart: `scale-out.svg`.
   - Note that "running" is not "ready": wait for endpoint DNS (#72).

## 2. The experiment (3 min)

5. **Two endpoints, one region.** Topology diagram from the methodology. Public
   Registry buckets feed `src`; `src` sends to `dst`, which writes to a private
   bucket.
   - Traffic crosses public IPs, so the internet-gateway limits are real.
   - We control both ends, so we can play the remote peer.
6. **The data.** Sentinel-2 COGs (59 MB mean), one 111 GB HDF5 file, and a
   124k-object Zarr store (35 KB chunks).

## 3. The cloud side is fast (5 min)

7. **One DTN: 9.2 Gbps.** Chart: `network-use.svg`.
   - The same DTN gives 0.35 Gbps at `normal`, 1.1 at `aggressive` and 9.2 at
     concurrency 32.
8. **Linear to four nodes.** Chart: `dtn-scaling.svg`.
   - 9.2, then 18.1, then 37.2 Gbps.
   - Then the 128-process cap per task: past ~4 nodes, add tasks, not nodes.
9. **Picking the instance.** Chart: `instance-types.svg`.
   - The 5 Gbps internet-gateway cap applies below 32 vCPUs; `*8in` types
     escape it.
   - AMD is more CPU-efficient but capped. The network picks the instance.
   - Instance time is ~$0.13/TB; egress at $50–90/TB is the real cost.

## 4. The punchline: it's the peer (6 min)

10. **The money slide.** Chart: `peer-settings.svg`.
    - A `normal` peer gets 1.15 Gbps with our 1 DTN, and 1.13 Gbps with our 4.
    - A tuned peer gets 4.77 Gbps.
11. **Why.** Negotiated concurrency is the minimum of the two endpoints'
    maximums, and `normal` = 4.
    - Show the `endpoint update --network-use custom` command.
12. **Elastic still wins on aggregate.** 8 concurrent tasks against the default
    peer reached 5.0 Gbps, 4.4× one task.
    - This is what the pool is for: many users at once.
13. **Data shape.**
    - One file is one process: a big file moves at ~75 MB/s. Split it.
    - Tiny files move at ~175 files/s, and S3 `SlowDown` follows in a fresh
      prefix. Package them.
14. **Myths, busted.** Chart: `rtt.svg`.
    - 80 ms of added round-trip time didn't matter at concurrency 32.
    - Checksums cost 3.5%: keep them on.

## 5. What to do (4 min)

15. **Researchers:** many files per task; concurrent tasks for big datasets;
    package tiny files and split huge ones.
16. **Peer admins:** raise `network_use`, sized to what your own storage can
    sustain.
17. **Operators:**
    - Scale on the sum of concurrency across active tasks.
    - Keep the control node out of the data path, but never at zero active nodes.
    - Stop it between sessions.
18. **Predict it.** The model: `T = min(C·r, ours, peer, files·r)`, with
    `scripts/throughput_model.py` as a live example.
    - It matched most runs to within ~10%.

## 6. Close (3 min)

19. **Lessons from operating it.** These can become slides or backup material:
    - concurrent first-boot `node setup` races on the keychain (#67);
    - ghost DNS entries from nodes stopped before removal (#69/#72);
    - the zero-active-node deadlock;
    - `cdk deploy` resetting desired capacity (#71).
20. **Next steps.** A slot controller in place of the ASG (#65); Graviton once GCS
    ships arm64; a large-scale throughput demo.
21. **Links.** The repository, the performance guides and the raw data.

## Backup slides

- Throughput-model back-test table ([throughput model](../performance/throughput-model.md)).
- Checksum on/off runs, in both orders.
- Cost per TB: instance time vs egress, and the Global Data Egress Waiver.
- The per-run table (`test/benchmark/results/2026-11-us-west-2/runs.json`).
