import json
import random
import tempfile
import time
import unittest
from pathlib import Path

import _util  # noqa: F401
import bench_transfer
import hlib
import study


def tiny_plan(**phase):
    ph = {"name": "PX", "base": {"dataset": "F64M", "c": 4}, **phase}
    plan = {"campaign": "unit", "seed": 7, "phases": [ph],
            "routes": json.loads(json.dumps(study.load_plan(hlib.HARNESS / "plans" / "envelope.yaml")["routes"]))}
    return study.prepare_plan(plan)


class TestExpansion(unittest.TestCase):
    def test_envelope_plan(self):
        plan = study.load_plan(hlib.HARNESS / "plans" / "envelope.yaml")
        self.assertEqual([p["name"] for p in plan["phases"]], ["P0-net", "P0-s3", "P1", "P3", "P2"])
        pts, curves = study.expand_phase(plan, study.get_phase(plan, "P1"))
        self.assertEqual(len(pts), 4 * 7 * 3)                    # c x dataset x route
        self.assertEqual(len(curves), 7 * 3)
        heads = [p for p in pts.values() if p["headline"]]
        self.assertEqual(len(heads), 2 * 3)                       # (c=8|c=1, F1G) x 3 routes
        for p in pts.values():
            self.assertEqual(p["factors"]["n_src"], 1)
            self.assertEqual(p["factors"]["instance_type_dst"], "c8in.2xlarge")

    def test_shared_points_dedupe(self):
        plan = tiny_plan(sweeps=[{"factor": "c", "values": [1, 4, 8]}, {"factor": "p", "values": [1, 4, 8]}])
        pts, curves = study.expand_phase(plan, plan["phases"][0])
        self.assertEqual(len(pts), 5)                             # (c=4, p=4) is in both sweeps
        shared = [p for p in pts.values() if len(p["curves"]) == 2]
        self.assertEqual(len(shared), 1)
        self.assertEqual(shared[0]["factors"]["c"], 4)

    def test_composite_and_fixed(self):
        plan = tiny_plan(sweeps=[{"factor": "cfg", "values": [{"n": 8, "instance_type": "c8in.2xlarge"},
                                                              {"n": 2, "instance_type": "c8in.8xlarge"}],
                                  "fixed": {"k": [1, 4]}}])
        pts, curves = study.expand_phase(plan, plan["phases"][0])
        self.assertEqual(len(pts), 4)
        self.assertEqual(len(curves), 2)
        self.assertEqual({(p["factors"]["n_dst"], p["factors"]["k"]) for p in pts.values()},
                         {(8, 1), (8, 4), (2, 1), (2, 4)})

    def test_uid_stable(self):
        f = study.normalize_factors({"dataset": "F1G", "c": 8})
        self.assertEqual(study.point_uid("P", f), study.point_uid("P", dict(reversed(list(f.items())))))


class TestRandomization(unittest.TestCase):
    def test_deterministic(self):
        plan = tiny_plan()
        r1, s1 = study.round_rng(plan, "PX", "coarse")
        r2, _ = study.round_rng(plan, "PX", "coarse")
        items = list(range(30))
        self.assertEqual(study.blocked_shuffle(items, r1), study.blocked_shuffle(items, r2))
        self.assertEqual(s1, "7:PX:coarse")

    def test_blocked(self):
        items = [(i, i % 3) for i in range(30)]
        out = study.blocked_shuffle(items, random.Random(1), block_key=lambda it: it[1])
        keys = [k for _, k in out]
        changes = sum(1 for a, b in zip(keys, keys[1:]) if a != b)
        self.assertEqual(changes, 2)                              # each block contiguous
        self.assertNotEqual([i for i, _ in out if _ == 0], sorted(i for i, k in items if k == 0))


class TestRefinement(unittest.TestCase):
    coarse = [1, 2, 4, 6, 8, 12, 16, 24, 32, 48, 64, 96, 128]

    def model(self, c):
        return min(0.3 * c, 5.0)

    def test_converges_to_knee(self):
        xs = list(self.coarse)
        for _ in range(10):
            props = study.refine_proposals(xs, [self.model(x) for x in xs], self.coarse)
            if not props:
                break
            xs = sorted(xs + props)
        lo, k, hi = study.knee_bracket(xs, [self.model(x) for x in xs], self.coarse)
        self.assertLessEqual(k - lo, 1)
        self.assertLessEqual(hi - k, 1)
        self.assertTrue(lo <= 5 / 0.3 <= hi, (lo, k, hi))
        self.assertLess(len(xs), len(self.coarse) + 10)

    def test_linear_and_flat(self):
        self.assertEqual(study.refine_proposals(self.coarse, [0.3 * c for c in self.coarse], self.coarse), [])
        flat = study.knee_bracket(self.coarse, [5.0] * len(self.coarse), self.coarse)
        self.assertEqual(flat[1], 1)

    def test_float_step(self):
        xs = [0.0, 50.0, 100.0]
        props = study.refine_proposals(xs, [5, 5, 5], xs, step=10, integer=False)
        self.assertEqual(props, [])                               # flat from the start: nothing to refine
        props = study.refine_proposals(xs, [1, 5, 5], xs, step=10, integer=False)
        self.assertEqual(props, [30.0, 80.0])                     # midpoints 25, 75 snapped to the 10 grid


class TestPhaseLoop(unittest.TestCase):
    def test_loop_refines_repeats_and_resumes(self):
        plan = tiny_plan(sweeps=[{"factor": "c", "values": TestRefinement.coarse, "refine": True}],
                         headline=[{"c": 128}])
        calls = []

        def measure(point, rep, meta):
            calls.append((point["uid"], rep, meta["round"]))
            c = point["factors"]["c"]
            return {"uid": point["uid"], "rid": f"{point['uid']}-r{rep}", "rep": rep, "status": "ok",
                    "metrics": {"steady_gbps": min(0.3 * c, 5.0)}, **meta}

        points, curves, results = study.phase_loop(plan, plan["phases"][0], measure, log=lambda *_: None)
        rounds = {r for _, _, r in calls}
        self.assertTrue({"coarse", "refine1", "knee", "remeasure"} <= rounds)
        cs = sorted(points[u]["factors"]["c"] for u in curves[0]["points"].values())
        self.assertIn(16, cs)
        self.assertIn(17, cs)
        head = [r for u, r, _ in calls if points[u]["factors"]["c"] == 128]
        self.assertEqual(sorted(head), [0, 1, 2])
        knee_reps = [r for _, r, rnd in calls if rnd == "knee"]
        self.assertEqual(len(knee_reps), 3 * 2)
        n0 = len(calls)
        study.phase_loop(plan, plan["phases"][0], measure, done=results, log=lambda *_: None)
        self.assertEqual(len(calls), n0)                          # resumable: nothing re-run

    def test_max_points(self):
        plan = tiny_plan(sweeps=[{"factor": "c", "values": [1, 2, 4, 8]}])
        n = []
        study.phase_loop(plan, plan["phases"][0], lambda p, r, m: n.append(1) or
                         {"uid": p["uid"], "status": "ok", "metrics": {"steady_gbps": 1.0}}, max_points=2,
                         log=lambda *_: None)
        self.assertEqual(len(n), 2)


class TestRoutes(unittest.TestCase):
    env = {"SRC_SYNTH_COL": "SCOL", "DST_COL": "DCOL", "SYNTH_BUCKET": "synth-b", "DST_BENCH_BUCKET": "bench-b",
           "SRC_POSIX_COL": "SP", "DST_POSIX_COL": "DP", "SRC_POSIX_BASE": "/data", "DST_POSIX_BASE": "/sink",
           "DST_POSIX_FS": "/data", "SRC_COL": "RODA"}

    def test_s3_to_s3_split(self):
        plan = tiny_plan()
        ds = study.resolve_dataset(plan, "F64M")
        f = study.normalize_factors({"dataset": "F64M", "k": 4})
        rt = study.resolve_route(plan, f, ds, self.env, "uid1-r0")
        self.assertEqual(rt["src_col"], "SCOL")
        self.assertEqual(rt["src_path"], f"/synth-b/{ds['prefix']}/")
        self.assertEqual(rt["dst_path"], "/bench-b/envelope/unit/uid1-r0/")
        self.assertEqual(len(rt["tasks"]), 4)
        self.assertEqual(rt["tasks"][0]["split"], "prefix")
        self.assertEqual(sum(len(t["items"]) for t in rt["tasks"]), 16)
        self.assertEqual(rt["cleanup"], [{"kind": "s3", "bucket": "bench-b", "prefix": "envelope/unit/uid1-r0/"}])
        self.assertEqual({p["role"] for p in rt["s3_prefixes"]}, {"read", "write"})

    def test_duplicate_split_and_fanout(self):
        plan = tiny_plan()
        ds = study.resolve_dataset(plan, "HOT")
        rt = study.resolve_route(plan, study.normalize_factors({"dataset": "HOT", "k": 2}), ds, self.env, "u")
        self.assertEqual(rt["tasks"][1]["items"], [f"/synth-b/{ds['prefix']}/=>/bench-b/envelope/unit/u/t1/:r"])
        ds = study.resolve_dataset(plan, "F1K")
        rt = study.resolve_route(plan, study.normalize_factors({"dataset": "F1K", "dst_fanout": 4}), ds, self.env, "u")
        dsts = {it.split("=>")[1].split("/")[5] for it in rt["tasks"][0]["items"]}
        self.assertEqual(dsts, {"m000", "m001", "m002", "m003"})

    def test_posix_and_real_dataset(self):
        plan = study.load_plan(hlib.HARNESS / "plans" / "envelope.yaml")
        ds = study.resolve_dataset(plan, "F1M")
        rt = study.resolve_route(plan, study.normalize_factors({"dataset": "F1M", "route": "posix_to_s3"}), ds,
                                 self.env, "u")
        self.assertEqual(rt["src_path"], f"/data/{ds['dir']}/")
        self.assertEqual(rt["drop_caches"], "src")
        rt = study.resolve_route(plan, study.normalize_factors({"dataset": "F1M", "route": "s3_to_posix"}), ds,
                                 self.env, "u")
        self.assertEqual(rt["cleanup"], [{"kind": "posix", "side": "dst", "path": "/data/sink/u"}])
        nasa = study.resolve_dataset(plan, "nasa-power")
        rt = study.resolve_route(plan, study.normalize_factors({"dataset": "nasa-power"}), nasa, self.env, "u")
        self.assertEqual(rt["src_col"], "RODA")
        self.assertEqual([p["role"] for p in rt["s3_prefixes"]], ["write"])

    def test_dataset_overrides(self):
        a = study.resolve_dataset(tiny_plan(), "F1K+fanout=4")
        self.assertEqual(len(a["top_dirs"]), 4)
        self.assertIn("F1K-fanout4-", a["prefix"])


class TestDryRun(unittest.TestCase):
    def test_simulate(self):
        plan = study.load_plan(hlib.HARNESS / "plans" / "envelope.yaml")
        est, order, points, _ = study.simulate(plan, study.get_phase(plan, "P1"))
        s = est.summary()
        self.assertGreater(s["points"], len(points))              # headline repeats and re-measurement
        self.assertGreater(s["hours"], 1)
        self.assertGreater(s["total_usd"], 0)
        self.assertEqual(order[0][2]["round"], "coarse")


class TestBenchTransfer(unittest.TestCase):
    def test_parse_started(self):
        self.assertEqual(bench_transfer.parse_started('{"type": "GridFTP Transfer", "concurrency": 4, '
                                                      '"parallelism": 8, "protocol": "Globus GridFTP"}'),
                         {"type": "GridFTP Transfer", "concurrency": 4, "parallelism": 8,
                          "protocol": "Globus GridFTP"})
        self.assertEqual(bench_transfer.parse_started("concurrency=32 parallelism=4 pipelining=20"),
                         {"concurrency": 32, "parallelism": 4, "pipelining": 20})
        self.assertEqual(bench_transfer.parse_started({"transfer": {"concurrency": "16"}}), {"concurrency": 16})
        self.assertEqual(bench_transfer.parse_started(None), {})
        self.assertEqual(bench_transfer.parse_started("started"), {})

    def test_events_summary(self):
        ev = [{"code": "STARTED", "details": '{"concurrency": 4, "parallelism": 8}', "time": "t1"},
              {"code": "PROGRESS", "is_error": False}, {"code": "CONNECTION_RESET", "is_error": True}]
        s = bench_transfer.events_summary(ev)
        self.assertEqual(s["started"][0]["negotiated"], {"concurrency": 4, "parallelism": 8})
        self.assertEqual(s["counts"]["PROGRESS"], 1)
        self.assertEqual(len(s["errors"]), 1)


# ---------------------------------------------------------------- end to end with a fake backend


def gftime(t):
    return time.strftime("%Y%m%d%H%M%S", time.gmtime(t)) + f".{int((t % 1) * 1e6):06d}"


class FakeBackend:
    """Simulated AWS + Globus: a 20 s listing delay, a 30 s ramp, then `level` Gbps."""

    def __init__(self, level=9.2, fail_submit=False, finish_after=None):
        self.env = dict(TestRoutes.env, REGION="us-west-2")
        self.t = 1_800_000_000.0
        self.level, self.fail_submit, self.finish_after = level, fail_submit, finish_after
        self.calls, self.t_submit, self.cancelled_at = [], None, None
        self.rng = random.Random(3)
        self.noise = [self.rng.gauss(0, 0.02) for _ in range(5000)]

    def now(self):
        return self.t

    def sleep(self, s):
        self.t += s

    def ensure_instance_type(self, dep, t):
        self.calls.append(("type", dep, t))
        return {"changed": False}

    def ensure_dtns(self, dep, n):
        self.calls.append(("dtns", dep, n))
        return {"changed": True, "ready_s": 120.0}

    def ensure_network_use(self, dep, f):
        self.calls.append(("nu", dep, f["c"], f["p"]))
        return {"network_use": "custom", "max_concurrency": f["c"], "max_parallelism": f["p"]}

    def ensure_netem(self, rtt, loss, on):
        self.calls.append(("netem", rtt, loss, on))
        return []

    def nodes(self):
        return {"src": [{"id": "i-s1", "ip": "198.51.100.1", "type": "c8in.2xlarge", "role": "dtn"}],
                "dst": [{"id": "i-d1", "ip": "198.51.100.2", "type": "c8in.2xlarge", "role": "dtn"}]}

    def controls(self):
        return {"src": [{"id": "i-cs", "role": "control"}], "dst": [{"id": "i-cd", "role": "control"}]}

    def instance_info(self, types):
        return {"c8in.2xlarge": {"vcpu": 8, "baseline_gbps": [12.5]}}

    def prep(self, dtn_ids, ctl_ids, tag, duration, drop=()):
        self.calls.append(("prep", tuple(dtn_ids), tuple(ctl_ids)))
        return {i: {} for i in dtn_ids}

    def submit(self, run_id, label, src, dst, items, env):
        if self.fail_submit:
            raise RuntimeError("submit failed")
        self.calls.append(("submit", run_id, src, dst, tuple(items), tuple(sorted(env.items()))))
        self.t_submit = self.t
        return {"event": "submitted", "task_id": "task-1"}

    def rate(self, t):
        e = t - self.t_submit
        if self.cancelled_at and t > self.cancelled_at + 5:
            return 0.0
        if e < 20:
            return 0.001
        return self.level * min((e - 20) / 30, 1.0) * (1 + self.noise[int(e) % 5000])

    def nic_text(self, side, until):
        rows, acc, t = [], 0.0, self.t_submit - 5
        busy = total = 0
        while t <= until:
            acc += self.rate(t) * 1e9 / 8
            busy, total = busy + 400, total + 800
            tx = int(acc) if side == "send" else 1000
            rx = int(acc) if side == "recv" else 1000
            rows.append(f"{t:.3f} {rx} {tx} {busy} {total} 8000000 0 5")
            t += 1
        return "\n".join(rows)

    def poll_samples(self, ids, tag):
        return {i: study.parse_nic(self.nic_text("send", self.t)) for i in ids}

    def snapshot_ss(self, ids):
        return {i: "ESTAB 0 0 a:50001 b:50002 bbr rtt:0.42/0.1 cwnd:100" for i in ids}

    def runner_state(self, runs):
        if self.cancelled_at or (self.finish_after and self.t - self.t_submit > self.finish_after):
            return {r: {"event": "done", "status": "FAILED" if self.cancelled_at else "SUCCEEDED"} for r in runs}
        return {r: {"event": "progress"} for r in runs}

    def cancel(self, runs):
        self.calls.append(("cancel", tuple(runs)))
        self.cancelled_at = self.t

    def collect(self, pdir, runs, dtn_ids, ctl_ids, tag, offsets, ss):
        end = self.t
        lines = [{"event": "submitted", "task_id": "task-1", "t": self.t_submit},
                 {"event": "progress", "status": "ACTIVE", "bytes_transferred": 10 ** 11},
                 {"event": "done", "status": "FAILED", "bytes_transferred": 10 ** 11, "files_transferred": 1400,
                  "faults": 0, "wall_s": end - self.t_submit, "cancelled": True},
                 {"event": "events", "started": [{"details": '{"concurrency": 4, "parallelism": 4}',
                                                  "negotiated": {"concurrency": 4, "parallelism": 4}}],
                  "counts": {"STARTED": 1}}]
        recs, t = [], self.t_submit + 20
        while t + 2 < end:
            for j in range(4):
                recs.append(f"DATE={gftime(t + 2)} HOST=dtn-s PROG=globus-gridftp-server NL.EVNT=FTP_INFO "
                            f"START={gftime(t)} USER=gcs FILE=/x/{t:.0f}-{j} NBYTES=67108864 STREAMS=4 STRIPES=1 "
                            f"DEST=[198.51.100.2] TYPE=RETR CODE=226")
            t += 2
        return {"runner": {r: lines for r in runs},
                "nic": {"i-s1": self.nic_text("send", end), "i-d1": self.nic_text("recv", end),
                        "i-cs": self.nic_text("idle", end), "i-cd": self.nic_text("idle", end)},
                "proc": {"i-s1": "".join(f"P {self.t_submit + k} 11 {int(25 * k)} {int(12.5 * k)}\n"
                                         for k in range(0, int(end - self.t_submit), 5))},
                "gridftp": {"i-s1": {"/var/log/gridftp-transfer.log": "\n".join(recs)}}, "ss": ss}

    def cleanup(self, targets):
        self.calls.append(("cleanup", json.dumps(targets)))
        return [{"deleted": 1}]


class TestRunPoint(unittest.TestCase):
    def setUp(self):
        self.plan = study.load_plan(hlib.HARNESS / "plans" / "envelope.yaml")
        self.phase = study.get_phase(self.plan, "P1")
        pts, _ = study.expand_phase(self.plan, self.phase)
        self.point = next(p for p in pts.values() if p["factors"]["dataset"] == "F64M"
                          and p["factors"]["c"] == 4 and p["factors"]["route"] == "s3_to_s3")
        self.point["kind"] = "globus"
        self.tmp = tempfile.TemporaryDirectory()

    def tearDown(self):
        self.tmp.cleanup()

    def test_steady_point(self):
        be = FakeBackend()
        rec = study.run_globus_point(be, self.plan, self.point, 0, {"round": "coarse", "order_index": 0},
                                     Path(self.tmp.name), log=lambda *_: None)
        self.assertEqual(rec["status"], "ok", rec.get("error"))
        self.assertEqual(rec["stop_reason"], "steady_window")
        self.assertAlmostEqual(rec["metrics"]["steady_gbps"], 9.2, delta=0.3)
        ci = rec["metrics"]["steady_ci95"]
        self.assertLess(ci[0], rec["metrics"]["steady_gbps"])
        self.assertGreaterEqual(rec["steady"]["onset_t"] - rec["t_submit"], 50)     # after listing + ramp
        self.assertEqual(rec["negotiated"], [{"concurrency": 4, "parallelism": 4}])
        self.assertTrue(any(c[0] == "cancel" for c in be.calls))
        self.assertEqual({c[1] for c in be.calls if c[0] == "nu"}, {"src", "dst"})
        sub = next(c for c in be.calls if c[0] == "submit")
        self.assertEqual(sub[2:4], ("SCOL", "DCOL"))
        self.assertIn(("ENCRYPT_DATA", 0), sub[5])
        src = next(n for n in rec["nodes"] if n["id"] == "i-s1")
        self.assertAlmostEqual(src["tx_gbps"], 9.2, delta=0.3)
        self.assertAlmostEqual(src["cpu_pct"], 50.0, delta=1)
        self.assertAlmostEqual(src["gridftp_proc_cpu_max_pct"], 37.5, delta=0.5)   # 37.5 ticks/s at 100 Hz
        self.assertAlmostEqual(rec["metrics"]["recv_rx_gbps"], 9.2, delta=0.3)
        self.assertAlmostEqual(rec["gridftp"]["send"]["inflight_mean"], 4.0, delta=0.3)
        self.assertGreater(rec["metrics"]["files_per_s"], 1.5)
        self.assertEqual(rec["ss"]["i-s1"]["rtt_ms_median"], 0.42)
        self.assertTrue(any(c[0] == "cleanup" and rec["rid"] in c[1] for c in be.calls))
        self.assertEqual(rec["uid"], self.point["uid"])
        json.dumps(rec, default=str)

    def test_task_finishes_first(self):
        be = FakeBackend(finish_after=40)
        rec = study.run_globus_point(be, self.plan, self.point, 0, {}, Path(self.tmp.name), log=lambda *_: None)
        self.assertEqual(rec["stop_reason"], "tasks_finished")
        self.assertEqual(rec["status"], "no_steady")
        self.assertFalse(any(c[0] == "cancel" for c in be.calls))

    def test_failure_still_cleans_up(self):
        be = FakeBackend(fail_submit=True)
        rec = study.run_globus_point(be, self.plan, self.point, 0, {}, Path(self.tmp.name), log=lambda *_: None)
        self.assertEqual(rec["status"], "failed")
        self.assertIn("submit failed", rec["error"])
        self.assertTrue(any(c[0] == "cleanup" for c in be.calls))

    def test_parse_ss(self):
        s = study.parse_ss("a bbr rtt:100.1/0.2\nb bbr rtt:100.3/0.1\nc cubic rtt:99.9/0.5")
        self.assertEqual(s["connections"], 3)
        self.assertEqual(s["rtt_ms_median"], 100.1)
        self.assertEqual(s["cc"], {"bbr": 2, "cubic": 1})


if __name__ == "__main__":
    unittest.main()
