import json
import tempfile
import unittest
from pathlib import Path

import _util  # noqa: F401
import hlib
import hstats


class TestYAML(unittest.TestCase):
    def test_subset(self):
        doc = hlib.load_yaml("""
# comment
name: F1K   # trailing comment
size: {dist: lognormal, median: 32MiB, sigma: 2.0}
list: [1, 2.5, "a b", [x, y]]
flag: true
none: null
quoted: "has # hash"
nested:
  a: 1
  b:
    - one
    - k: v
      k2: [1, 2]
    -
      deep: yes
seq_same_indent:
- 1
- 2
""")
        self.assertEqual(doc["name"], "F1K")
        self.assertEqual(doc["size"], {"dist": "lognormal", "median": "32MiB", "sigma": 2.0})
        self.assertEqual(doc["list"], [1, 2.5, "a b", ["x", "y"]])
        self.assertIs(doc["flag"], True)
        self.assertIsNone(doc["none"])
        self.assertEqual(doc["quoted"], "has # hash")
        self.assertEqual(doc["nested"]["b"], ["one", {"k": "v", "k2": [1, 2]}, {"deep": True}])
        self.assertEqual(doc["seq_same_indent"], [1, 2])

    def test_errors(self):
        with self.assertRaises(hlib.YAMLError):
            hlib.load_yaml("a: 1\n  b: 2\n")
        with self.assertRaises(hlib.YAMLError):
            hlib.load_yaml("a: [1, 2\n")

    def test_repo_files_match_pyyaml(self):
        files = list((hlib.HARNESS / "datasets").glob("*.yaml")) + list((hlib.HARNESS / "plans").glob("*.yaml"))
        self.assertGreaterEqual(len(files), 12)
        try:
            import yaml
        except ImportError:
            yaml = None
        for f in files:
            mine = hlib.load_yaml_file(f)
            self.assertIsInstance(mine, dict, f)
            if yaml:
                self.assertEqual(json.dumps(mine, sort_keys=True), json.dumps(yaml.safe_load(f.read_text()),
                                                                              sort_keys=True), f.name)


class TestUnits(unittest.TestCase):
    def test_parse_size(self):
        self.assertEqual(hlib.parse_size("1KiB"), 1024)
        self.assertEqual(hlib.parse_size("64 MiB"), 64 * 2 ** 20)
        self.assertEqual(hlib.parse_size("1.5GiB"), int(1.5 * 2 ** 30))
        self.assertEqual(hlib.parse_size("1TB"), 10 ** 12)
        self.assertEqual(hlib.parse_size(4096), 4096)
        for bad in ("12 parsecs", "", True):
            with self.assertRaises(ValueError):
                hlib.parse_size(bad)

    def test_labels(self):
        self.assertEqual(hlib.size_label(2 ** 20), "1MiB")
        self.assertEqual(hlib.size_label(1500), "1500B")
        self.assertEqual(hlib.chash({"a": 1, "b": 2}), hlib.chash({"b": 2, "a": 1}))

    def test_load_env(self):
        with tempfile.TemporaryDirectory() as d:
            p = Path(d) / "bench.env"
            p.write_text("# c\nREGION=us-west-2\nSRC_CONTROL=i-123   # comment\nQ=\"x y\"\n")
            self.assertEqual(hlib.load_env(p), {"REGION": "us-west-2", "SRC_CONTROL": "i-123", "Q": "x y"})


class TestSecrets(unittest.TestCase):
    def test_gcs_command_fetches_on_instance(self):
        c = hlib.gcs_command("us-west-2", "dst", hlib.network_use_cli(32, 4))
        self.assertIn("GCS_CLI_CLIENT_SECRET=$(aws secretsmanager get-secret-value --secret-id "
                      "elastic-gcs/us-west-2/dst/globus-client-secret", c)
        self.assertIn("--preferred-concurrency 32 --max-concurrency 32 --preferred-parallelism 4 "
                      "--max-parallelism 4", c)
        self.assertEqual(hlib.assert_no_inline_secret(c), c)

    def test_runner_command(self):
        c = hlib.runner_command("us-west-2", "abc-r0-t0", "label x", "S", "D", ["/a/=>/b/:r"],
                                {"VERIFY_CHECKSUM": 1})
        hlib.assert_no_inline_secret(c)
        self.assertIn("CANCEL_FILE=/opt/elastic-gcs/bench/runs/abc-r0-t0.cancel", c)
        self.assertIn("'label x'", c)
        self.assertIn("/a/=>/b/:r", c)

    def test_inline_secret_rejected(self):
        with self.assertRaises(ValueError):
            hlib.assert_no_inline_secret("export GLOBUS_CLIENT_SECRET=hunter2; python x")

    def test_presets(self):
        self.assertEqual(hlib.network_use_cli(preset="normal"),
                         "globus-connect-server endpoint update --network-use normal")


class TestStats(unittest.TestCase):
    def test_t(self):
        self.assertAlmostEqual(hstats.t_ppf(0.975, 10), 2.228139, places=4)
        self.assertAlmostEqual(hstats.t_ppf(0.975, 1e6), 1.959964, places=3)
        self.assertAlmostEqual(hstats.t_sf2(2.228139, 10), 0.05, places=4)
        self.assertAlmostEqual(hstats.t_ppf(0.025, 5), -hstats.t_ppf(0.975, 5), places=8)

    def test_ols(self):
        r = hstats.ols([0, 1, 2, 3, 4], [1, 3, 5, 7, 9.1])
        self.assertAlmostEqual(r["b"], 2.02, places=2)
        self.assertLess(r["p"], 1e-4)
        flat = hstats.ols([0, 1, 2, 3], [5, 5, 5, 5])
        self.assertEqual(flat["p"], 1.0)

    def test_mean_ci(self):
        s = hstats.mean_ci([10, 12, 11, 13, 9])
        self.assertAlmostEqual(s["mean"], 11)
        self.assertLess(s["ci"][0], 11)
        self.assertGreater(s["ci"][1], 11)
        # positive autocorrelation shrinks the effective sample size, widening the CI
        ar = [10 + (i // 5) * 0.5 for i in range(40)]
        a, b = hstats.mean_ci(ar), hstats.mean_ci(ar, autocorr=True)
        self.assertLess(b["n_eff"], a["n_eff"])
        self.assertGreater(b["ci"][1] - b["ci"][0], a["ci"][1] - a["ci"][0])

    def test_lstsq(self):
        beta = hstats.lstsq([[1, x] for x in range(5)], [3 + 2 * x for x in range(5)])
        self.assertAlmostEqual(beta[0], 3)
        self.assertAlmostEqual(beta[1], 2)


if __name__ == "__main__":
    unittest.main()
