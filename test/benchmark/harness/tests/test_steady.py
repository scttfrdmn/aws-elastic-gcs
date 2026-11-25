import unittest

import _util
from steady import SteadyTracker, aggregate_rates, detect_steady


class TestAggregate(unittest.TestCase):
    def test_two_nodes_misaligned(self):
        t0 = 1000.0
        a = [(t0 + i + 0.3, int(i * 1.25e9)) for i in range(60)]          # 10 Gbps
        b = [(t0 + i + 0.7, int(i * 0.625e9)) for i in range(60)]          # 5 Gbps
        ts, ys = aggregate_rates({"a": a, "b": b})
        self.assertGreater(len(ts), 50)
        for y in ys:
            self.assertAlmostEqual(y / 1e9, 15.0, places=3)
        self.assertEqual(ts[1] - ts[0], 1.0)

    def test_overlap_only(self):
        a = [(i, i * 100) for i in range(0, 50)]
        b = [(i, i * 100) for i in range(20, 70)]
        ts, _ = aggregate_rates({"a": a, "b": b})
        self.assertGreaterEqual(ts[0], 20)
        self.assertLessEqual(ts[-1], 49)

    def test_counter_reset(self):
        s = [(i, i * 1000) for i in range(10)] + [(10 + i, i * 1000) for i in range(10)]
        ts, ys = aggregate_rates({"a": s}, scale=1.0)
        self.assertTrue(all(y >= 0 for y in ys))
        self.assertAlmostEqual(sum(ys) / len(ys), 1000, delta=150)

    def test_empty(self):
        self.assertEqual(aggregate_rates({}), ([], []))
        self.assertEqual(aggregate_rates({"a": [(1, 1)]}), ([], []))


class TestDetect(unittest.TestCase):
    def test_ramp_then_plateau(self):
        for seed in range(10):
            ts, ys = _util.ramp_plateau(n=150, ramp_s=30, level=9.2, cv=0.03, seed=seed)
            r = detect_steady(ts, ys)
            self.assertTrue(r["found"], seed)
            onset = r["onset_t"] - ts[0]
            self.assertGreaterEqual(onset, 30, seed)    # never inside the ramp
            self.assertLessEqual(onset, 60, seed)
            self.assertTrue(r["complete"])
            st = r["stats"]
            self.assertAlmostEqual(st["mean"], 9.2, delta=0.25)
            self.assertLess(st["ci95"][0], st["mean"])
            self.assertGreater(st["ci95"][1], st["mean"])
            self.assertAlmostEqual(r["window"][1] - r["window"][0], 45)
            self.assertLess(st["cv"], 0.06)

    def test_ci_covers_truth(self):
        hits = 0
        for seed in range(40):
            ts, ys = _util.ramp_plateau(n=150, ramp_s=20, level=5.0, cv=0.05, seed=100 + seed)
            ci = detect_steady(ts, ys)["stats"]["ci95"]
            hits += ci[0] <= 5.0 <= ci[1]
        self.assertGreaterEqual(hits, 32)   # nominal 95%; allow sampling slack

    def test_never_steady(self):
        ts = [float(i) for i in range(120)]
        ys = [0.1 * i for i in range(120)]   # steady growth: slope always significant
        self.assertFalse(detect_steady(ts, ys)["found"])

    def test_idle_before_ramp(self):
        ts = [float(i) for i in range(160)]
        ys = [0.0] * 30 + [min((i - 30) / 20, 1.0) * 8 for i in range(30, 160)]
        r = detect_steady(ts, ys)
        self.assertTrue(r["found"])
        self.assertGreaterEqual(r["onset_t"], 50)

    def test_incomplete_window(self):
        ts, ys = _util.ramp_plateau(n=60, ramp_s=10, cv=0.01)
        r = detect_steady(ts, ys)
        self.assertTrue(r["found"])
        self.assertFalse(r["complete"])

    def test_too_short(self):
        self.assertFalse(detect_steady([0, 1, 2], [1, 1, 1])["found"])

    def test_constant(self):
        ts = [float(i) for i in range(100)]
        r = detect_steady(ts, [4.0] * 100)
        self.assertTrue(r["complete"])
        self.assertEqual(r["stats"]["mean"], 4.0)


class TestTracker(unittest.TestCase):
    def test_online_matches_offline(self):
        ts, ys = _util.ramp_plateau(n=120, ramp_s=25, seed=7)
        cum_a = _util.cumulative(ts, [y / 2 for y in ys])
        cum_b = _util.cumulative(ts, [y / 2 for y in ys])
        tr = SteadyTracker()
        for (t, a), (_, b) in zip(cum_a, cum_b):
            tr.add("a", t, a)
            tr.add("a", t, a)            # duplicates are harmless
            tr.add("b", t, b)
        r = tr.result()
        gts, gys = tr.series()
        off = detect_steady(gts, gys)
        self.assertEqual(r["onset_t"], off["onset_t"])
        self.assertAlmostEqual(r["stats"]["mean"] / 1e9, 9.2, delta=0.3)


if __name__ == "__main__":
    unittest.main()
