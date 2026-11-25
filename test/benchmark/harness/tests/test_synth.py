import math
import unittest

import _util  # noqa: F401
import hlib
import synth

GiB, MiB, KiB = 2 ** 30, 2 ** 20, 2 ** 10

# The #151 section 5 table: name -> (objects, bytes)
TABLE = {"F1K": (100000, 100000 * KiB), "F1M": (16384, 16 * GiB), "F16M": (4096, 64 * GiB),
         "F64M": (4096, 256 * GiB), "F256M": (1024, 256 * GiB), "F1G": (512, 512 * GiB),
         "F4G": (64, 256 * GiB), "F16G": (16, 256 * GiB), "HOT": (4096, 256 * GiB), "DATED": (16384, 16 * GiB)}


def spec(**kw):
    base = {"name": "T", "size": "1MiB", "count": 100}
    base.update(kw)
    return synth.normalize(base)


class TestCoreDatasets(unittest.TestCase):
    def test_table(self):
        for name, (n, b) in TABLE.items():
            p = synth.build_plan(synth.load_spec(hlib.HARNESS / "datasets" / f"{name}.yaml"))
            self.assertEqual(len(p["ops"]), n, name)
            self.assertEqual(sum(o["size"] for o in p["ops"]), b, name)
            self.assertEqual(len({o["key"] for o in p["ops"]}), n, f"{name}: keys unique")

    def test_mix_capped(self):
        p = synth.build_plan(synth.load_spec(hlib.HARNESS / "datasets" / "MIX.yaml"))
        total = sum(o["size"] for o in p["ops"])
        self.assertLessEqual(total, 2 ** 40)
        self.assertGreater(total, 0.9 * 2 ** 40)
        self.assertLessEqual(len({o["size"] for o in p["ops"]}), 64)
        self.assertLessEqual(max(o["size"] for o in p["ops"]), 16 * GiB)

    def test_hot_is_one_prefix(self):
        p = synth.build_plan(synth.load_spec(hlib.HARNESS / "datasets" / "HOT.yaml"))
        rel = [o["key"][len(p["prefix"]) + 1:] for o in p["ops"]]
        self.assertEqual({r.split("/")[0] for r in rel}, {"d0000"})
        self.assertEqual(rel, sorted(rel))           # lexicographically adjacent

    def test_dated_layout(self):
        p = synth.build_plan(synth.load_spec(hlib.HARNESS / "datasets" / "DATED.yaml"))
        rel = p["ops"][0]["key"][len(p["prefix"]) + 1:]
        self.assertTrue(rel.startswith("2024/01/01/00/obs-20240101T00-00000"), rel)
        dirs = {o["key"].rsplit("/", 1)[0] for o in p["ops"]}
        self.assertEqual(len(dirs), 16384 // 64)

    def test_large_objects_multipart(self):
        p = synth.build_plan(synth.load_spec(hlib.HARNESS / "datasets" / "F16G.yaml"))
        self.assertTrue(all(o["op"] == "mpu" and o["parts"] == 32 for o in p["ops"]))
        self.assertEqual(list(p["seeds"]), [str(512 * MiB)])


class TestPlanning(unittest.TestCase):
    def test_deterministic_and_hashed(self):
        a, b = synth.build_plan(spec()), synth.build_plan(spec())
        self.assertEqual(a["ops"], b["ops"])
        c = synth.build_plan(spec(fanout=4))
        self.assertNotEqual(a["prefix"], c["prefix"])
        self.assertNotEqual(a["spec_hash"], c["spec_hash"])

    def test_fanout_and_depth(self):
        p = synth.build_plan(spec(count=5000, fanout=256, depth=3, subfanout=4))
        rel = [o["key"][len(p["prefix"]) + 1:].split("/") for o in p["ops"]]
        self.assertTrue(all(len(r) == 4 for r in rel))
        self.assertEqual(len({r[0] for r in rel}), 256)
        self.assertLessEqual(len({r[1] for r in rel}), 4)

    def test_sequential_depth(self):
        p = synth.build_plan(spec(count=64, naming="sequential", fanout=2, depth=2, subfanout=4))
        rel = [o["key"][len(p["prefix"]) + 1:] for o in p["ops"]]
        self.assertEqual(rel, sorted(rel))
        self.assertEqual(len({r.rsplit("/", 1)[0] for r in rel}), 8)

    def test_total_bytes(self):
        p = synth.build_plan(spec(count=None, total_bytes="10MiB"))
        self.assertEqual(len(p["ops"]), 10)

    def test_lognormal_median(self):
        s = spec(size={"dist": "lognormal", "median": "32MiB", "sigma": 1.0, "min": "1KiB", "max": "16GiB"},
                 count=4000, content={"quantize_bins": 0})
        sizes = sorted(synth.sample_sizes(s))
        med = sizes[len(sizes) // 2]
        self.assertLess(abs(math.log(med / (32 * MiB))), 0.1)

    def test_bimodal(self):
        s = spec(size={"dist": "bimodal", "modes": [{"size": "1MiB", "weight": 3}, {"size": "1GiB", "weight": 1}]},
                 count=4000)
        sizes = synth.sample_sizes(s)
        small = sum(1 for x in sizes if x < 10 * MiB)
        self.assertAlmostEqual(small / len(sizes), 0.75, delta=0.03)
        self.assertLessEqual(len(set(sizes)), 2)

    def test_histogram_replay(self):
        bins = [[MiB, 2 * MiB - 1, 30], [64 * MiB, 128 * MiB - 1, 10]]
        s = spec(size={"dist": "histogram", "bins": bins}, count=None, content={"quantize_bins": 0})
        sizes = synth.sample_sizes(s)
        self.assertEqual(len(sizes), 40)
        self.assertTrue(all(MiB <= x <= 128 * MiB for x in sizes))

    def test_quantize(self):
        s = spec(size={"dist": "lognormal", "median": "4MiB", "sigma": 2, "min": "1KiB", "max": "1GiB"}, count=3000,
                 content={"quantize_bins": 16})
        self.assertLessEqual(len(set(synth.sample_sizes(s))), 16)

    def test_validation(self):
        for bad in ({"fanout": 0}, {"depth": 7}, {"naming": "random"}, {"size": {"dist": "zipf"}},
                    {"content": {"part_size": "1MiB"}}, {"count": None}):
            raw = {"name": "T", "size": "1MiB", "count": 10, **bad}
            if raw.get("count") is None:
                raw.pop("count")
            with self.assertRaises(ValueError, msg=bad):
                synth.normalize(raw)

    def test_summary_and_manifest(self):
        p = synth.build_plan(synth.load_spec(hlib.HARNESS / "datasets" / "F1K.yaml"))
        s = synth.plan_summary(p)
        self.assertEqual(s["copy_object"], 100000)
        self.assertGreater(s["est_request_usd"], 0.49)
        self.assertLess(s["est_request_usd"], 0.52)
        self.assertEqual(s["prefixes_top"], 64)
        mk = synth.manifest_key(p["spec"])
        self.assertFalse(mk.startswith(p["prefix"] + "/"), "manifest must sit beside the dataset, not in it")
        m = synth.manifest(p, "b")
        self.assertEqual(len(m["objects"]), 100000)
        self.assertEqual(m["spec_hash"], p["spec_hash"])

    def test_posix_list(self):
        p = synth.build_plan(spec(count=3))
        lines = synth.posix_list(p).splitlines()
        self.assertEqual(len(lines), 3)
        size, rel = lines[0].split(" ")
        self.assertEqual(int(size), MiB)
        self.assertFalse(rel.startswith("/"))

    def test_seed_commands(self):
        p = synth.build_plan(spec())
        cmds = synth.seed_commands(p, "bkt", "us-west-2", {})
        self.assertEqual(len(cmds), 1)
        self.assertIn(f"head -c {MiB} /dev/urandom | aws s3 cp - s3://bkt/synth/_seeds/{MiB}", cmds[0])
        self.assertEqual(synth.seed_commands(p, "bkt", "us-west-2", {f"synth/_seeds/{MiB}": MiB}), [])


class FakeS3:
    def __init__(self):
        self.calls = []
        self.objects = {}

    def copy_object(self, **kw):
        self.calls.append(("copy", kw))
        self.objects[kw["Key"]] = 1

    def create_multipart_upload(self, **kw):
        return {"UploadId": "u1"}

    def upload_part_copy(self, **kw):
        self.calls.append(("part", kw))
        return {"CopyPartResult": {"ETag": f"e{kw['PartNumber']}"}}

    def complete_multipart_upload(self, **kw):
        self.calls.append(("complete", kw))

    def abort_multipart_upload(self, **kw):
        self.calls.append(("abort", kw))


class TestCopy(unittest.TestCase):
    def test_copy_object(self):
        s3 = FakeS3()
        op = {"key": "k", "size": 5, "op": "copy", "seed": "seeds/5"}
        self.assertEqual(synth.copy_one(s3, "b", op), 5)
        self.assertEqual(s3.calls[0][1]["CopySource"], {"Bucket": "b", "Key": "seeds/5"})

    def test_multipart_ranges(self):
        s3 = FakeS3()
        op = {"key": "k", "size": 2 * 512 * MiB + 100, "op": "mpu", "seed": "s", "parts": 3, "part_size": 512 * MiB}
        synth.copy_one(s3, "b", op)
        parts = [c[1] for c in s3.calls if c[0] == "part"]
        self.assertEqual([p["CopySourceRange"] for p in parts],
                         [f"bytes=0-{512 * MiB - 1}"] * 2 + ["bytes=0-99"])
        done = [c[1] for c in s3.calls if c[0] == "complete"][0]
        self.assertEqual([p["PartNumber"] for p in done["MultipartUpload"]["Parts"]], [1, 2, 3])

    def test_retry_then_abort(self):
        class Flaky(FakeS3):
            def upload_part_copy(self, **kw):
                raise RuntimeError("boom")
        s3 = Flaky()
        orig = synth.time.sleep
        synth.time.sleep = lambda s: None
        try:
            with self.assertRaises(RuntimeError):
                synth.copy_one(s3, "b", {"key": "k", "size": 10 * MiB, "op": "mpu", "seed": "s", "parts": 2,
                                         "part_size": 5 * MiB})
        finally:
            synth.time.sleep = orig
        self.assertTrue(any(c[0] == "abort" for c in s3.calls))


class TestCLI(unittest.TestCase):
    def test_plan_dry_run(self):
        import contextlib
        import io
        buf = io.StringIO()
        with contextlib.redirect_stdout(buf):
            self.assertEqual(synth.main(["plan", str(hlib.HARNESS / "datasets" / "F64M.yaml")]), 0)
        self.assertIn("F64M", buf.getvalue())
        self.assertIn("256.00 GiB", buf.getvalue())


if __name__ == "__main__":
    unittest.main()
