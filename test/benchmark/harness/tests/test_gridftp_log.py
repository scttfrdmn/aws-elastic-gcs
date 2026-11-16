import calendar
import unittest

import _util  # noqa: F401
import gridftp_log as g

CLASSIC = ("DATE=20261116193121.219285 HOST=dtn-a PROG=globus-gridftp-server NL.EVNT=FTP_INFO "
           "START=20261116193120.219285 USER=gcs FILE=/bench/F64M/00/a.bin BUFFER=0 BLOCK=262144 "
           "NBYTES=67108864 VOLUME=/ STREAMS=4 STRIPES=1 DEST=[203.0.113.5] TYPE=RETR CODE=226")


def line(start, end, nbytes, typ="RETR", f="/x/a.bin", host="dtn-a", streams=4, code=226):
    return (f"DATE={end} HOST={host} PROG=globus-gridftp-server NL.EVNT=FTP_INFO START={start} USER=gcs "
            f"FILE={f} BUFFER=0 BLOCK=262144 NBYTES={nbytes} VOLUME=/ STREAMS={streams} STRIPES=1 "
            f"DEST=[198.51.100.7] TYPE={typ} CODE={code}")


class TestParse(unittest.TestCase):
    def test_classic(self):
        r = g.parse_line(CLASSIC)
        self.assertEqual(r["nbytes"], 64 * 2 ** 20)
        self.assertAlmostEqual(r["dur_s"], 1.0, places=6)
        self.assertEqual(r["direction"], "send")
        self.assertEqual(r["streams"], 4)
        self.assertEqual(r["dest"], ["203.0.113.5"])
        self.assertAlmostEqual(r["rate_bps"] / 1e9, 0.536870912, places=6)
        self.assertAlmostEqual(r["start"], calendar.timegm((2026, 11, 16, 19, 31, 20, 0, 0, 0)) + 0.219285, places=5)

    def test_prefixes_and_variants(self):
        syslog = "Nov 16 19:31:21 dtn-a globus-gridftp-server[1234]: Transfer stats: " + CLASSIC
        pidfmt = "[1234] Mon Nov 16 19:31:21 2026 :: Transfer stats: " + CLASSIC
        quoted = CLASSIC.replace("FILE=/bench/F64M/00/a.bin", 'FILE="/bench/with space/a.bin"') + " TASKID=abc"
        spaced = CLASSIC.replace("FILE=/bench/F64M/00/a.bin", "FILE=/bench/with space/a.bin")
        for ln in (syslog, pidfmt):
            self.assertEqual(g.parse_line(ln)["nbytes"], 64 * 2 ** 20)
        q = g.parse_line(quoted)
        self.assertEqual(q["file"], "/bench/with space/a.bin")
        self.assertEqual(q["taskid"], "abc")
        self.assertEqual(g.parse_line(spaced)["file"], "/bench/with space/a.bin")

    def test_store_and_bad_lines(self):
        recs, skipped = g.parse([
            CLASSIC.replace("TYPE=RETR", "TYPE=STOR"),
            "random noise",
            "DATE=garbage START=20261116193120 NBYTES=1",
            "DATE=20261116193121.0 HOST=x NBYTES=10",        # no START
            CLASSIC,
            CLASSIC,                                          # duplicate
        ])
        self.assertEqual(len(recs), 2)
        self.assertEqual(skipped, 2)
        self.assertEqual(recs[0]["direction"], "recv")


class TestSummary(unittest.TestCase):
    def test_window(self):
        lines = [line("20261116100000.0", "20261116100010.0", 125_000_000),      # 0-10 s, 0.1 Gbps
                 line("20261116100005.0", "20261116100015.0", 125_000_000),      # 5-15 s
                 line("20261116100020.0", "20261116100030.0", 1_250_000_000),    # outside window
                 line("20261116100000.0", "20261116100010.0", 125_000_000, typ="STOR", host="dtn-b"),
                 line("20261116100000.0", "20261116100010.0", 5, code=451)]      # failed: ignored
        recs, _ = g.parse(lines)
        t0 = g.parse_time("20261116100000.0")
        s = g.summarize(recs, t0, t0 + 15)
        self.assertEqual(s["send"]["files"], 2)
        self.assertAlmostEqual(s["send"]["rate_gbps_median"], 0.1, places=4)
        self.assertAlmostEqual(s["send"]["inflight_mean"], 20 / 15, places=2)
        self.assertAlmostEqual(s["send"]["window_gbps"], 8 * 250_000_000 / 15 / 1e9, places=3)
        self.assertEqual(s["recv"]["files"], 1)
        self.assertEqual(s["send"]["streams"], [4])
        ps = g.per_size(recs)
        self.assertEqual(ps[125_000_000]["n"], 2)

    def test_remote_helpers(self):
        c = g.enable_command()
        self.assertIn(g.CONF_PATH, c)
        self.assertIn("log_transfer /var/log/gridftp-transfer.log", c)
        self.assertIn("systemctl restart globus-gridftp-server", c)
        self.assertFalse(g.CONF_PATH.split("/")[-1].startswith("globus"))   # GCS overwrites globus* files
        offs = g.parse_offsets("/var/log/gridftp-transfer.log 1234\n/var/log/gridftp.log 0\njunk\n")
        self.assertEqual(offs, {"/var/log/gridftp-transfer.log": 1234, "/var/log/gridftp.log": 0})


if __name__ == "__main__":
    unittest.main()
