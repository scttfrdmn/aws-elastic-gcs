import unittest

import _util  # noqa: F401
import s3_access_logs as s

OWNER = "79a59df900b949e55d96a1e698fbacedfd6e09d98eacf8f8d5218e7cd47ef2be"
# Verbatim from the AWS "Amazon S3 server access log format" page.
AWS_EXAMPLES = [
    f'{OWNER} amzn-s3-demo-bucket1 [06/Feb/2019:00:00:38 +0000] 192.0.2.3 {OWNER} 3E57427F3EXAMPLE REST.GET.VERSIONING - "GET /amzn-s3-demo-bucket1?versioning HTTP/1.1" 200 - 113 - 7 - "-" "S3Console/0.4" - s9lzHYrFp76ZVxRcpX9+5cjAnEH2ROuNkd2BHfIa6UkFVdtjf5mKR3/eTPFvsiP/XV/VLi31234= SigV4 ECDHE-RSA-AES128-GCM-SHA256 AuthHeader amzn-s3-demo-bucket1.s3.us-west-1.amazonaws.com TLSV1.2 arn:aws:s3:us-west-1:123456789012:accesspoint/example-AP Yes us-east-1',
    f'{OWNER} amzn-s3-demo-bucket1 [06/Feb/2019:00:00:38 +0000] 192.0.2.3 {OWNER} A1206F460EXAMPLE REST.GET.BUCKETPOLICY - "GET /amzn-s3-demo-bucket1?policy HTTP/1.1" 404 NoSuchBucketPolicy 297 - 38 - "-" "S3Console/0.4" - BNaBsXZQQDbssi6xMBdBU2sLt+Yf5kZDmeBUP35sFoKa3sLLeMC78iwEIWxs99CRUrbS4n11234= SigV4 ECDHE-RSA-AES128-GCM-SHA256 AuthHeader amzn-s3-demo-bucket1.s3.us-west-1.amazonaws.com TLSV1.2 - Yes us-east-1',
    f'{OWNER} amzn-s3-demo-bucket1 [06/Feb/2019:00:01:57 +0000] 192.0.2.3 {OWNER} DD6CC733AEXAMPLE REST.PUT.OBJECT s3-dg.pdf "PUT /amzn-s3-demo-bucket1/s3-dg.pdf HTTP/1.1" 200 - - 4406583 41754 28 "-" "S3Console/0.4" - 10S62Zv81kBW7BB6SX4XJ48o6kpcl6LPwEoizZQQxJd5qDSCTLX0TgS37kYUBKQW3+bPdrg1234= SigV4 ECDHE-RSA-AES128-SHA AuthHeader amzn-s3-demo-bucket1.s3.us-west-1.amazonaws.com TLSV1.2 - Yes us-east-1',
]
T0 = "16/Nov/2026:10:00:"


def rec(sec, op, key, uri, status=200, err="-", sent="-", size="-", total=100, ta=10, ua="aws-sdk-cpp/1.11",
        bucket="bench", extra=""):
    return (f'{OWNER} {bucket} [{T0}{sec:02d} +0000] 198.51.100.7 arn:aws:sts::1:assumed-role/r/x REQ{sec}{op[-6:]} '
            f'{op} {key} "{uri}" {status} {err} {sent} {size} {total} {ta} "-" "{ua}" - hostid SigV4 '
            f'TLS_AES_128_GCM_SHA256 AuthHeader bench.s3.us-west-2.amazonaws.com TLSv1.3 - - us-west-2{extra}')


class TestParse(unittest.TestCase):
    def test_aws_examples(self):
        recs, bad = s.parse(AWS_EXAMPLES)
        self.assertEqual(bad, 0)
        self.assertEqual(len(recs), 3)
        put = recs[2]
        self.assertEqual(put["operation"], "REST.PUT.OBJECT")
        self.assertEqual(put["key"], "s3-dg.pdf")
        self.assertEqual(put["object_size"], 4406583)
        self.assertEqual(put["bytes_sent"], 0)
        self.assertEqual(put["total_ms"], 41754)
        self.assertEqual(put["turnaround_ms"], 28)
        self.assertEqual(put["source_region"], "us-east-1")
        self.assertEqual(recs[1]["error_code"], "NoSuchBucketPolicy")
        self.assertEqual(recs[0]["access_point_arn"], "arn:aws:s3:us-west-1:123456789012:accesspoint/example-AP")
        self.assertEqual(recs[0]["time"], 1549411238)

    def test_quotes_extras_encoding(self):
        ln = rec(1, "REST.GET.OBJECT", "dir/with%20space.bin", "GET /bench/dir/with%20space.bin HTTP/1.1",
                 sent=100, size=100, ua='agent "quoted" x', extra=" newfield1 newfield2")
        r = s.parse_line(ln)
        self.assertEqual(r["key"], "dir/with space.bin")
        self.assertEqual(r["user_agent"], 'agent "quoted" x')
        self.assertEqual(r["extra"], ["newfield1", "newfield2"])
        self.assertIsNone(s.parse_line("not a log line"))


class TestProfile(unittest.TestCase):
    def setUp(self):
        k = "synth/F64M-x/00/a.bin"
        size = 64 * 2 ** 20
        part = 8 * 2 ** 20
        lines = [rec(0, "REST.HEAD.OBJECT", k, f"HEAD /bench/{k} HTTP/1.1", size=size)]
        # 8 ranged GETs, four in flight at a time (two waves of ~2 s each)
        for i in range(8):
            lines.append(rec(1 + 2 * (i // 4), "REST.GET.OBJECT", k, f"GET /bench/{k} HTTP/1.1", status=206,
                             sent=part, size=size, total=1900, ta=15))
        up = "envelope/c/p1/00/a.bin"
        lines.append(rec(10, "REST.POST.UPLOADS", up, f"POST /bench/{up}?uploads HTTP/1.1"))
        for n in range(1, 5):
            lines.append(rec(11, "REST.PUT.PART", up, f"PUT /bench/{up}?partNumber={n}&uploadId=UP1 HTTP/1.1",
                             size=16 * 2 ** 20, total=800))
        lines.append(rec(11, "REST.PUT.PART", up, f"PUT /bench/{up}?partNumber=5&uploadId=UP1 HTTP/1.1",
                         status=503, err="SlowDown", size=16 * 2 ** 20, total=50))
        lines.append(rec(13, "REST.PUT.PART", up, f"PUT /bench/{up}?partNumber=5&uploadId=UP1 HTTP/1.1",
                         size=16 * 2 ** 20, total=800))
        lines.append(rec(15, "REST.POST.UPLOAD", up, f"POST /bench/{up}?uploadId=UP1 HTTP/1.1"))
        lines.append(rec(16, "REST.GET.OBJECT", up, f"GET /bench/{up} HTTP/1.1", sent=80 * 2 ** 20,
                         size=80 * 2 ** 20))                                   # read-back
        lines.append(rec(9, "REST.PUT.OBJECT", "envelope/c/p1/00/", "PUT /bench/envelope/c/p1/00/ HTTP/1.1",
                         size=0))                                              # directory marker
        lines.append(rec(9, "REST.GET.BUCKET", "-", "GET /bench?list-type=2&prefix=envelope/c/p1/ HTTP/1.1"))
        self.recs, bad = s.parse(lines)
        self.assertEqual(bad, 0)
        self.k, self.up = k, up

    def test_profile(self):
        summary, rows = s.profile(self.recs)
        by = {r["key"]: r for r in rows}
        g = by[self.k]["get"]
        self.assertEqual(g["n"], 8)
        self.assertEqual(g["ranged"], 8)
        self.assertEqual(g["range_bytes"]["median"], 8 * 2 ** 20)
        self.assertEqual(g["inflight_peak"], 4)
        mp = by[self.up]["multipart"][0]
        self.assertEqual(mp["parts"], 5)
        self.assertEqual(mp["part_requests"], 6)
        self.assertEqual(mp["part_bytes"]["median"], 16 * 2 ** 20)
        self.assertTrue(mp["completed"])
        self.assertGreaterEqual(mp["inflight_peak"], 4)
        self.assertEqual(by[self.up]["read_back"], 1)
        self.assertEqual(summary["directory_markers"], 1)
        self.assertEqual(summary["lists"], 1)
        self.assertEqual(summary["heads"], 1)
        self.assertEqual(summary["throttled_503"], 1)
        self.assertEqual(summary["retries"], 1)
        self.assertAlmostEqual(summary["backoff_s"]["median"], 13 - 11.05, places=3)
        self.assertEqual(summary["objects"], 2)

    def test_join(self):
        t = s.parse_time(f"{T0}00 +0000")
        pts = [{"uid": "p1", "t_submit": t, "t_end": t + 20,
                "s3_prefixes": [{"role": "read", "bucket": "bench", "prefix": "synth/F64M-x/"},
                                {"role": "write", "bucket": "bench", "prefix": "envelope/c/p1/"}]},
               {"uid": "p2", "t_submit": t + 3600, "t_end": t + 3700,
                "s3_prefixes": [{"role": "read", "bucket": "bench", "prefix": "synth/F64M-x/"}]}]
        res = s.join(pts, self.recs, slack=5)
        self.assertEqual(res["p1"]["read"][0]["records"], 9)
        self.assertEqual(res["p1"]["write"][0]["objects"], 1)
        self.assertEqual(res["p2"]["read"][0]["records"], 0)

    def test_overlap(self):
        self.assertEqual(s.max_overlap([(0, 2), (1, 3), (2.5, 4)])[0], 2)
        self.assertEqual(s.max_overlap([(0, 1), (1, 2)])[0], 1)


if __name__ == "__main__":
    unittest.main()
