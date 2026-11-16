"""Shared helpers for the envelope-study harness (#151).

- bench.env loading (same file the shell tools source)
- byte-size parsing and formatting
- a YAML-subset parser, so dataset specs and run plans need no PyYAML
- canonical hashing of specs and points
- SSM command execution with chunked file transfer (the ssm_run.sh and
  pull_runs.sh patterns, in Python)
- command builders for the GCS CLI and the transfer runner that fetch Globus
  secrets from Secrets Manager *on the instance*. Secret values never pass
  through this process: commands carry `$(aws secretsmanager ...)`
  substitutions that the instance's shell expands.

boto3 is imported lazily, so everything except live AWS calls works offline.
"""
import base64
import gzip
import hashlib
import json
import os
import re
import shlex
import subprocess
import time
from pathlib import Path

HARNESS = Path(__file__).resolve().parent
REPO = HARNESS.parents[2]
RESULTS = REPO / "test" / "benchmark" / "results"
BENCH_DIR = "/opt/elastic-gcs/bench"          # on the src control node (runner, venv, runs/)
REMOTE_TMP = "/var/tmp/bench"                 # on DTNs (samplers, log slices)

# ---------------------------------------------------------------- bench.env


def load_env(path=None):
    """Parse bench.env (KEY=VALUE lines, '#' comments). Process env overrides nothing."""
    path = Path(path or os.environ.get("BENCH_ENV") or HARNESS / "bench.env")
    env = {}
    for line in path.read_text().splitlines():
        line = line.strip()
        if not line or line.startswith("#") or "=" not in line:
            continue
        k, v = line.split("=", 1)
        v = re.split(r"\s+#", v, maxsplit=1)[0].strip().strip('"').strip("'")
        env[k.strip()] = v
    return env


# ---------------------------------------------------------------- sizes

_UNITS = {"": 1, "b": 1,
          "k": 1000, "kb": 1000, "m": 1000 ** 2, "mb": 1000 ** 2, "g": 1000 ** 3, "gb": 1000 ** 3,
          "t": 1000 ** 4, "tb": 1000 ** 4,
          "kib": 1024, "mib": 1024 ** 2, "gib": 1024 ** 3, "tib": 1024 ** 4}


def parse_size(v):
    """'64MiB' -> 67108864. Ints pass through. SI (KB) and IEC (KiB) units."""
    if isinstance(v, bool):
        raise ValueError(f"bad size {v!r}")
    if isinstance(v, (int, float)):
        return int(v)
    m = re.fullmatch(r"\s*([0-9]*\.?[0-9]+)\s*([A-Za-z]*)\s*", str(v))
    if not m or m.group(2).lower() not in _UNITS:
        raise ValueError(f"bad size {v!r}")
    return int(float(m.group(1)) * _UNITS[m.group(2).lower()])


def fmt_bytes(n):
    n = float(n)
    for u in ("B", "KiB", "MiB", "GiB", "TiB", "PiB"):
        if abs(n) < 1024 or u == "PiB":
            return f"{n:.0f} {u}" if u == "B" else f"{n:.2f} {u}"
        n /= 1024


def size_label(n):
    """Compact exact label for key names: 1048576 -> '1MiB', 1500 -> '1500B'."""
    for u, f in (("TiB", 1024 ** 4), ("GiB", 1024 ** 3), ("MiB", 1024 ** 2), ("KiB", 1024)):
        if n >= f and n % f == 0:
            return f"{n // f}{u}"
    return f"{n}B"


# ---------------------------------------------------------------- hashing


def canonical(obj):
    return json.dumps(obj, sort_keys=True, separators=(",", ":"), default=str)


def chash(obj, n=12):
    return hashlib.sha256(canonical(obj).encode()).hexdigest()[:n]


# ---------------------------------------------------------------- YAML subset
#
# Supported: block mappings and sequences (indentation), "- key: v" items,
# flow sequences/mappings ([..], {..}), quoted and plain scalars, ints,
# floats, true/false, null/~, and '#' comments. Not supported: anchors, tags,
# multi-line scalars, multi-document streams. That is all the specs and plans use.


class YAMLError(ValueError):
    pass


def _strip_comment(s):
    out, q = [], None
    for i, ch in enumerate(s):
        if q:
            if ch == q:
                q = None
        elif ch in "'\"":
            q = ch
        elif ch == "#" and (i == 0 or s[i - 1] in " \t"):
            break
        out.append(ch)
    return "".join(out).rstrip()


def _split_key(s):
    """Split 'key: value' at the first top-level ': ' (or a trailing ':'). None if not a mapping line."""
    depth, q = 0, None
    for i, ch in enumerate(s):
        if q:
            if ch == q:
                q = None
            continue
        if ch in "'\"":
            q = ch
        elif ch in "[{":
            depth += 1
        elif ch in "]}":
            depth -= 1
        elif ch == ":" and depth == 0 and (i + 1 == len(s) or s[i + 1] in " \t"):
            key = s[:i].strip()
            if key.startswith(("'", '"')):
                key = key[1:-1]
            return key, s[i + 1:].strip()
    return None


def _scalar(s):
    s = s.strip()
    if s == "":
        return None
    if s[0] in "[{":
        v, i = _flow(s, 0)
        if s[i:].strip():
            raise YAMLError(f"trailing text after flow value: {s!r}")
        return v
    if s[0] in "'\"":
        if len(s) < 2 or s[-1] != s[0]:
            raise YAMLError(f"unterminated string: {s!r}")
        body = s[1:-1]
        return body.replace("''", "'") if s[0] == "'" else bytes(body, "utf-8").decode("unicode_escape")
    low = s.lower()
    if low in ("true", "yes", "on"):
        return True
    if low in ("false", "no", "off"):
        return False
    if low in ("null", "~"):
        return None
    if re.fullmatch(r"[-+]?\d+", s):
        return int(s)
    if re.fullmatch(r"[-+]?(\d+\.\d*|\.\d+|\d+)([eE][-+]?\d+)?", s):
        return float(s)
    return s


def _flow(s, i):
    """Parse a flow collection starting at s[i]; returns (value, next index)."""
    def skip(j):
        while j < len(s) and s[j] in " \t":
            j += 1
        return j

    def atom(j, stops):
        j = skip(j)
        if j < len(s) and s[j] in "[{":
            return _flow(s, j)
        if j < len(s) and s[j] in "'\"":
            q = s[j]
            k = s.index(q, j + 1)
            return _scalar(s[j:k + 1]), k + 1
        k = j
        while k < len(s) and s[k] not in stops:
            k += 1
        return _scalar(s[j:k]), k

    if s[i] == "[":
        out, j = [], skip(i + 1)
        if j < len(s) and s[j] == "]":
            return out, j + 1
        while True:
            v, j = atom(j, ",]")
            out.append(v)
            j = skip(j)
            if j >= len(s):
                raise YAMLError(f"unterminated flow sequence: {s!r}")
            if s[j] == "]":
                return out, j + 1
            j += 1
    out, j = {}, skip(i + 1)
    if j < len(s) and s[j] == "}":
        return out, j + 1
    while True:
        k, j = atom(j, ":")
        j = skip(j)
        if j >= len(s) or s[j] != ":":
            raise YAMLError(f"expected ':' in flow mapping: {s!r}")
        v, j = atom(j + 1, ",}")
        out[str(k)] = v
        j = skip(j)
        if j >= len(s):
            raise YAMLError(f"unterminated flow mapping: {s!r}")
        if s[j] == "}":
            return out, j + 1
        j += 1


def load_yaml(text):
    lines = []
    for raw in text.splitlines():
        if raw.strip() in ("---", "..."):
            continue
        if "\t" in raw[: len(raw) - len(raw.lstrip())]:
            raise YAMLError("tabs are not allowed in indentation")
        s = _strip_comment(raw)
        if s.strip():
            lines.append([len(s) - len(s.lstrip(" ")), s.strip()])

    def block(i, indent):
        if lines[i][1].startswith("- ") or lines[i][1] == "-":
            return seq(i, indent)
        return mapping(i, indent)

    def child(i, parent_indent, seq_ok_same_indent):
        """Value for a key/item whose inline value was empty."""
        if i < len(lines) and (lines[i][0] > parent_indent or
                               (seq_ok_same_indent and lines[i][0] == parent_indent and
                                (lines[i][1].startswith("- ") or lines[i][1] == "-"))):
            return block(i, lines[i][0])
        return None, i

    def seq(i, indent):
        out = []
        while i < len(lines) and lines[i][0] == indent and (lines[i][1].startswith("- ") or lines[i][1] == "-"):
            rest = lines[i][1][1:].strip()
            if not rest:
                v, i = child(i + 1, indent, False)
            elif _split_key(rest) and not rest.startswith(("[", "{", "'", '"')):
                lines[i] = [indent + 2, rest]          # "- k: v" opens a mapping at indent+2
                v, i = mapping(i, indent + 2)
            else:
                v, i = _scalar(rest), i + 1
            out.append(v)
        return out, i

    def mapping(i, indent):
        out = {}
        while i < len(lines) and lines[i][0] == indent:
            kv = _split_key(lines[i][1])
            if kv is None:
                raise YAMLError(f"expected 'key: value', got {lines[i][1]!r}")
            k, v = kv
            if v == "":
                val, i = child(i + 1, indent, True)
            else:
                val, i = _scalar(v), i + 1
            out[k] = val
        if i < len(lines) and lines[i][0] > indent:
            raise YAMLError(f"bad indentation at {lines[i][1]!r}")
        return out, i

    if not lines:
        return None
    v, i = block(0, lines[0][0])
    if i != len(lines):
        raise YAMLError(f"unexpected content at {lines[i][1]!r}")
    return v


def load_yaml_file(path):
    return load_yaml(Path(path).read_text())


# ---------------------------------------------------------------- AWS (lazy)


def boto3_client(service, region):
    import boto3  # noqa: PLC0415 - only live paths need boto3
    from botocore.config import Config
    return boto3.client(service, region_name=region,
                        config=Config(retries={"max_attempts": 10, "mode": "adaptive"}, max_pool_connections=128))


def git_commit():
    try:
        return subprocess.run(["git", "-C", str(REPO), "rev-parse", "HEAD"], capture_output=True,
                              text=True, check=True).stdout.strip()
    except (OSError, subprocess.CalledProcessError):
        return None


class SSM:
    """Run shell commands as root over SSM (AWS-RunShellScript), like ssm_run.sh.

    Output is capped by SSM at 24,000 characters per stream, so large files
    come back gzip+base64 in 20,000-character chunks (as pull_runs.sh does).
    """

    CHUNK = 20000

    def __init__(self, region, client=None, poll_s=2.0):
        self.region = region
        self._c = client
        self.poll_s = poll_s

    @property
    def c(self):
        if self._c is None:
            self._c = boto3_client("ssm", self.region)
        return self._c

    def send(self, instance_ids, command, timeout=300):
        r = self.c.send_command(InstanceIds=list(instance_ids), DocumentName="AWS-RunShellScript",
                                Parameters={"commands": [command], "executionTimeout": [str(timeout)]})
        return r["Command"]["CommandId"]

    def wait(self, command_id, instance_id, max_s=None):
        deadline = time.time() + (max_s or 3600)
        while True:
            time.sleep(self.poll_s)
            try:
                r = self.c.get_command_invocation(CommandId=command_id, InstanceId=instance_id)
            except self.c.exceptions.InvocationDoesNotExist:
                continue
            if r["Status"] in ("Success", "Failed", "TimedOut", "Cancelled") or time.time() > deadline:
                return r["Status"], r.get("StandardOutputContent", ""), r.get("StandardErrorContent", "")

    def run(self, instance_id, command, timeout=300, check=False):
        st, out, err = self.wait(self.send([instance_id], command, timeout), instance_id, timeout + 60)
        if check and st != "Success":
            raise RuntimeError(f"SSM {instance_id}: {st}: {err.strip()[-400:]}")
        return st, out, err

    def run_many(self, instance_ids, command, timeout=300):
        ids = list(instance_ids)
        if not ids:
            return {}
        cid = self.send(ids, command, timeout)
        return {i: self.wait(cid, i, timeout + 60) for i in ids}

    def fetch_file(self, instance_id, path, offset=0):
        """Bytes of a remote file from byte `offset` (gzip+base64, chunked). b'' if missing."""
        tmp = f"/tmp/fetch-{abs(hash((instance_id, path, offset, time.time()))) % 10 ** 9}"
        p = shlex.quote(path)
        st, out, _ = self.run(instance_id, f"if [ -e {p} ]; then tail -c +{offset + 1} {p} | gzip -c | base64 -w0 > {tmp}; "
                                           f"wc -c < {tmp}; else echo -1; fi")
        total = int((out.strip().splitlines() or ["-1"])[-1])
        if total < 0:
            return b""
        buf, off = [], 0
        while off < total:
            _, out, _ = self.run(instance_id, f"tail -c +{off + 1} {tmp} | head -c {self.CHUNK}")
            buf.append(out.strip())
            off += self.CHUNK
        self.run(instance_id, f"rm -f {tmp}")
        return gzip.decompress(base64.b64decode("".join(buf)))

    def put_file(self, instance_id, path, data, mode="0644"):
        """Write bytes to a remote path (gzip+base64 chunks appended, then decoded)."""
        b64 = base64.b64encode(gzip.compress(data)).decode()
        p, tmp = shlex.quote(path), shlex.quote(path + ".b64")
        self.run(instance_id, f"mkdir -p $(dirname {p}); : > {tmp}", check=True)
        for i in range(0, len(b64), self.CHUNK):
            self.run(instance_id, f"printf %s {b64[i:i + self.CHUNK]} >> {tmp}", check=True)
        self.run(instance_id, f"base64 -d {tmp} | gunzip -c > {p} && chmod {mode} {p} && rm -f {tmp}", check=True)


# ---------------------------------------------------------------- command builders
#
# These mirror gcs.sh and xfer_bg.sh. The `$(...)` substitutions are expanded by
# bash on the instance; this process never sees a secret.


def _secret(region, dep, name):
    return (f"$(aws secretsmanager get-secret-value --secret-id elastic-gcs/{region}/{dep}/{name} "
            f"--query SecretString --output text --region {region})")


def gcs_command(region, dep, cli):
    """Shell command that runs `cli` (a globus-connect-server command line) on dep's control node."""
    p = f"elastic-gcs/{region}/{dep}"
    return (f"export GCS_CLI_CLIENT_ID={_secret(region, dep, 'globus-client-id')}; "
            f"export GCS_CLI_CLIENT_SECRET={_secret(region, dep, 'globus-client-secret')}; "
            f"export GCS_CLI_ENDPOINT_ID=$(aws ssm get-parameter --name /{p}/endpoint-id "
            f"--query Parameter.Value --output text --region {region}); {cli}")


def network_use_cli(concurrency=None, parallelism=None, preset=None):
    """`endpoint update` for a preset (normal/minimal/aggressive) or custom c/p."""
    if preset and preset != "custom":
        return f"globus-connect-server endpoint update --network-use {shlex.quote(preset)}"
    c, p = int(concurrency), int(parallelism)
    return ("globus-connect-server endpoint update --network-use custom "
            f"--preferred-concurrency {c} --max-concurrency {c} --preferred-parallelism {p} --max-parallelism {p}")


def runner_command(region, run_id, label, src_col, dst_col, items, env=None):
    """Start bench_transfer.py detached on the src control node (xfer_bg.sh, plus extra env)."""
    env = dict(env or {})
    env.setdefault("CANCEL_FILE", f"{BENCH_DIR}/runs/{run_id}.cancel")
    envs = " ".join(f"{k}={shlex.quote(str(v))}" for k, v in sorted(env.items()))
    args = " ".join(shlex.quote(a) for a in [src_col, dst_col, label, *items])
    log = f"{BENCH_DIR}/runs/{shlex.quote(run_id)}.jsonl"
    return (f"mkdir -p {BENCH_DIR}/runs; "
            f"export GLOBUS_CLIENT_ID={_secret(region, 'src', 'globus-client-id')}; "
            f"export GLOBUS_CLIENT_SECRET={_secret(region, 'src', 'globus-client-secret')}; "
            f"{envs} nohup {BENCH_DIR}/venv/bin/python {BENCH_DIR}/bench_transfer.py {args} > {log} 2>&1 & "
            f"sleep 8; head -1 {log}")


SECRET_PATTERN = re.compile(r"(GLOBUS_CLIENT_SECRET|GCS_CLI_CLIENT_SECRET)=(?!\$\(aws secretsmanager)")


def assert_no_inline_secret(cmd):
    """Guard: a client secret may only appear as a Secrets Manager substitution."""
    if SECRET_PATTERN.search(cmd):
        raise ValueError("command would carry an inline Globus secret")
    return cmd
