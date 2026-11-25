import json
import random
import tempfile
import unittest
from pathlib import Path

import _util  # noqa: F401
import analyze as a

MiB = 2 ** 20


class TestFits(unittest.TestCase):
    def test_rs(self):
        t0, b = 0.4, 0.9
        sizes = [MiB, 16 * MiB, 64 * MiB, 256 * MiB, 1024 * MiB, 4096 * MiB]
        rates = [(8 * s / 1e9) / (t0 + 8 * s / 1e9 / b) for s in sizes]
        f = a.fit_rs(sizes, rates)
        self.assertAlmostEqual(f["t0_s"], t0, places=6)
        self.assertAlmostEqual(f["b_gbps"], b, places=6)
        self.assertAlmostEqual(a.predict_rs(f, 64 * MiB), rates[2], places=6)
        v = a.validate("rs", sizes, rates)
        self.assertLess(v["loo"]["mape_pct"], 1e-6)

    def test_min(self):
        cs = [1, 2, 4, 6, 8, 12, 16, 24, 32, 48, 64]
        ys = [min(0.3 * c, 9.2) for c in cs]
        f = a.fit_min(cs, ys)
        self.assertAlmostEqual(f["r_gbps"], 0.3, places=6)
        self.assertAlmostEqual(f["cap_gbps"], 9.2, places=6)
        self.assertAlmostEqual(f["c_star"], 9.2 / 0.3, places=4)
        rng = random.Random(1)
        noisy = [y * (1 + rng.gauss(0, 0.03)) for y in ys]
        g = a.fit_min(cs, noisy)
        self.assertAlmostEqual(g["cap_gbps"], 9.2, delta=0.4)
        lin = a.fit_min(cs, [0.3 * c for c in cs])
        self.assertIsNone(lin["cap_gbps"])

    def test_usl(self):
        lam, sigma, kappa = 9.0, 0.05, 0.002
        ns = [1, 2, 3, 4, 5, 6, 8, 12, 16]
        ys = [lam * n / (1 + sigma * (n - 1) + kappa * n * (n - 1)) for n in ns]
        f = a.fit_usl(ns, ys)
        self.assertAlmostEqual(f["lambda"], lam, places=6)
        self.assertAlmostEqual(f["sigma"], sigma, places=6)
        self.assertAlmostEqual(f["kappa"], kappa, places=6)
        self.assertAlmostEqual(f["n_peak"], ((1 - sigma) / kappa) ** 0.5, places=6)
        v = a.validate("usl", ns, ys)
        self.assertLess(v["loo"]["mape_pct"], 1e-6)
        self.assertIn("holdout", v)
        self.assertEqual(a.validate("usl", ns[:3], ys[:3])["note"], "too few points to validate")


def make_records(tmp):
    """Synthetic points.jsonl: a concurrency curve with repetitions, P1 per-file rates."""
    rng = random.Random(2)
    recs = []
    for c in [1, 2, 4, 8, 16, 32, 64, 128]:
        for rep in range(3 if c in (32, 64) else 1):
            g = min(0.3 * c, 9.2) * (1 + rng.gauss(0, 0.02))
            recs.append({"rid": f"c{c}-r{rep}", "uid": f"c{c}", "phase": "P2", "status": "ok", "round": "coarse",
                         "factors": {"c": c, "n_src": 1, "dataset": "F1G", "k": 1}, "curves": ["P2:c:0:aaaaaa"],
                         "metrics": {"steady_gbps": g, "steady_ci95": [g - 0.1, g + 0.1]}, "sender": "src",
                         "instance_info": {"c8in.2xlarge": {"baseline_gbps": [12.5]}},
                         "nodes": [{"side": "src", "role": "dtn", "type": "c8in.2xlarge", "tx_gbps": g,
                                    "cpu_pct": 40.0, "gridftp_proc_cpu_max_pct": 20.0}],
                         "negotiated": [{"concurrency": min(c, 128), "parallelism": 4}],
                         "gridftp": {"send": {"inflight_mean": min(c, 30), "rate_gbps_median": 0.3}}, "tasks": []})
    for size, rate in [("F1M", 0.05), ("F64M", 0.3), ("F1G", 0.8)]:
        recs.append({"rid": f"p1-{size}", "uid": f"p1-{size}", "phase": "P1", "status": "ok", "round": "coarse",
                     "factors": {"c": 1, "dataset": size, "route": "s3_to_s3"}, "curves": [],
                     "metrics": {"steady_gbps": rate}, "gridftp": {"send": {"rate_gbps_median": rate}}})
    recs.append({"rid": "bad-r0", "uid": "bad", "phase": "P2", "status": "failed", "metrics": {}, "factors": {}})
    p = Path(tmp) / "points.jsonl"
    p.write_text("".join(json.dumps(r) + "\n" for r in recs))
    return recs


class TestPipeline(unittest.TestCase):
    def test_aggregate_and_outputs(self):
        with tempfile.TemporaryDirectory() as d:
            recs = make_records(d)
            rows = a.aggregate(recs)
            self.assertEqual(rows["c32"]["n"], 3)
            self.assertEqual(rows["c32"]["ci_kind"], "reps")
            self.assertEqual(rows["c1"]["ci_kind"], "within")
            self.assertNotIn("bad", rows)
            curves = a.curves_from(rows)
            c = curves["P2:c:0:aaaaaa"]
            self.assertEqual([x for x, _ in c["points"]], [1, 2, 4, 8, 16, 32, 64, 128])
            self.assertEqual(c["fixed"], {"dataset": "F1G", "k": 1, "n_src": 1})
            att = a.attribution(curves, recs, "steady_gbps")
            self.assertEqual(len(att), 1)
            self.assertEqual(att[0]["knee_x"], 32)
            # NIC 9.2/12.5 = 74%, CPU 40%, no 503s, c < 128: nothing saturated, so the rule says L4
            self.assertEqual(att[0]["suggested_layer"], "L4")
            self.assertAlmostEqual(att[0]["evidence"]["nic_util_max"], 0.74, delta=0.03)
            fr = a.fits(curves, recs, 151)
            self.assertAlmostEqual(fr["curves"]["P2:c:0:aaaaaa"]["fit"]["cap_gbps"], 9.2, delta=0.3)
            self.assertIn("s3_to_s3", fr["rs"])
            self.assertEqual(a.main([d, "--no-figures"]), 0)
            out = Path(d) / "analysis"
            for f in ("tables.md", "points_agg.csv", "fits.json", "attribution.md"):
                self.assertTrue((out / f).exists(), f)
            self.assertIn("| 32 | 3 |", (out / "tables.md").read_text())
            try:
                import matplotlib  # noqa: F401
            except ImportError:
                return
            self.assertEqual(a.main([d]), 0)
            self.assertTrue((out / "fig-P2-c-0.png").exists())
            self.assertTrue((out / "fig-P2-c-0.csv").exists())
            self.assertTrue((out / "fig-P1-rs.pdf").exists())

    def test_suggest_layer(self):
        self.assertEqual(a.suggest_layer({"nic_util_max": 0.9}, {}), "L1-nic")
        self.assertEqual(a.suggest_layer({"nic_util_max": 0.3, "cpu_pct_max": 95}, {}), "L1-cpu")
        self.assertEqual(a.suggest_layer({"gridftp_proc_cpu_max_pct": 99}, {}), "L3")
        self.assertEqual(a.suggest_layer({"s3_503": 4}, {}), "L5")
        self.assertEqual(a.suggest_layer({}, {"c": 128}), "L2")
        self.assertEqual(a.suggest_layer({}, {"c": 16}), "L4")


if __name__ == "__main__":
    unittest.main()
