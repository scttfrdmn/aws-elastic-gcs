# Why Globus transfers to and from S3 are slow

A Globus transfer between an S3 collection and anything else passes through a
stack of independent limits. Each is documented somewhere (AWS networking, S3
performance guidance, Globus's scheduling model), but they are rarely put
together, and together they explain why "Globus to S3 is slow" even on fast
instances. Every number below is measured in this project unless it cites a
source; the study behind them is #151.

## The stack, from one request up to a whole task

### 1. One S3 request moves about 0.6 Gbps

A single S3 GET on one connection runs at **0.4–0.8 Gbps (50–100 MB/s)**, on
every instance size from c8in.2xlarge to c8in.96xlarge (P0, #151). This is S3's
per-request rate, not the instance: AWS's guidance is to "make one concurrent
request for each 85–90 MB/s of desired network throughput"
([AWS whitepaper, *Best Practices Design Patterns: Optimizing Amazon S3 Performance*](https://docs.aws.amazon.com/pdfs/whitepapers/latest/s3-optimizing-performance-best-practices/s3-optimizing-performance-best-practices.pdf)).
Clients that need more per object split it into parallel byte ranges: the AWS
CRT client moves **14–24 Gbps for one 4 GiB object** on the same instances,
25–40× one connection.

### 2. The stock GCS S3 connector moves each file on one connection

Globus hands each file to one GridFTP process, and the stock S3 connector reads
or writes it at about the single-request rate: **~0.6 Gbps for one 111 GB file**
(November campaign), the same as one S3 connection. More TCP streams per file
(GridFTP parallelism) don't help, because they all wait on that one S3 request
(4.80 vs 5.24 Gbps at parallelism 8 vs 1; methodology). Small and mid-size files
also pay a fixed per-file cost: 59 MB files ran at ~0.29 Gbps per process, and
1 MiB files at ~0.07 Gbps per file (P1 smoke test, ~116 ms per file).

### 3. Globus Transfer schedules whole files, at most 128 at a time per task

Transfer assigns whole files to processes ("concurrency"), capped at **128 per
task**. So one task moves at most about **128 × per-file rate**: roughly
**77 Gbps** with ideal large files on the stock connector, however large the
DTNs, and far less with small files. A typical remote endpoint on Globus's
default `normal` setting negotiates **4 × 8** and ran at **~1 Gbps** (#145).
Transfer also spreads sessions across endpoint nodes round robin, without
regard to capacity ([Globus FAQ](https://docs.globus.org/faq/transfer-sharing/)):
a small control node gets the same share as a large DTN (#146 takes it out of
the data path).

### 4. AWS caps every single TCP flow at 5 Gbps

"When instances are not in the same cluster placement group, bandwidth for
single-flow traffic is limited to 5 Gbps"
([EC2 instance network bandwidth](https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/ec2-instance-network-bandwidth.html)).
A flow is one 5-tuple. Measured: **4.4–4.8 Gbps per flow at every size**, while
the same instances carry many flows far faster (11.9 Gbps on c8in.2xlarge,
96 Gbps on c8in.16xlarge). This is per flow, not per instance, and not a kernel
or buffer limit (1 ms RTT needs only ~0.6 MB of window). It doesn't bind the
stock connector (0.6 Gbps per file), but it will bind any faster one: a file at
15–20 Gbps needs at least 4–5 GridFTP streams. Placement groups (10 Gbps per
flow) and ENA Express (25 Gbps, same AZ) don't apply to remote endpoints.

### 5. Most instances get only 5 Gbps *in total* to the internet

Traffic through an internet gateway, which is how a remote Globus peer is
reached, has a **per-instance** limit separate from the per-flow one: instance
types "with less than 32 vCPUs" are "limited to 5 Gbps"; larger ones to "50% of
the available bandwidth"; and only the types AWS lists by name, **"C8in, C8ine,
M8in, M8ine, M8idn, R8in, R8idn"**, are "limited to the baseline bandwidth for
the instance type" (same AWS page). It is the 8th-generation network-optimized
types specifically, not the `n` suffix: earlier network-optimized families
(c5n, m5n, c6in, m6in, r6in) follow the 5 Gbps / 50% rule like any other type.
So a c7i.2xlarge or a c6in.2xlarge DTN tops out at 5 Gbps across all its
transfers, whatever its "up to" rating, while a c8in.2xlarge measured
**11.9 Gbps** and a c8in.16xlarge **96 Gbps**. See
[instance sizing](../instance-sizing.md).

### 6. "Up to" ratings are burst, and big instances span network cards

Instances of 4xlarge and below are rated "up to" a bandwidth they can burst to
for "typically from 5 to 60 minutes", then fall to their baseline (same AWS
page). And a network interface sits on one network card: c8in.96xlarge's
600 Gbps is two 300 Gbps cards, so a single-interface DTN can use at most 300.
(Our public-IP measurements plateau at 171–193 Gbps on 32xlarge and up; whether
that is AWS or our test tool is open, #151.)

### 7. Small files hit S3 request rates and per-file overhead

Each file costs several S3 requests on the stock connector (lookup, upload
start/part/complete, directory markers, and a checksum read-back when
`verify_checksum` is on). Writing 123,676 × 35 KB files into one fresh prefix
drew S3 `503 SlowDown` at ~50 files/s with concurrency 128, far below S3's
3,500 writes/s per partition, and stalled the task (`ENDPOINT_TOO_BUSY`).

## What each layer means in practice

| Limit | Who it binds | Lever |
|---|---|---|
| ~0.6 Gbps per S3 request | every file | an S3 connector that splits files into parallel ranges/parts (AWS-optimized connector, in design) |
| one connection per file (stock connector) | large files most | same; until then, raise concurrency |
| 128 files in flight per task | single big tasks | more concurrent tasks; more, larger files per task |
| 5 Gbps per TCP flow (AWS) | fast per-file paths | GridFTP parallelism ≥ 4–5 once files move faster than 5 Gbps |
| 5 Gbps per instance to the internet (most types < 32 vCPU) | DTN choice | use the listed 8th-gen network-optimized types (c8in, m8in, r8in, …); not c6in/m6in |
| "up to" burst ratings | sustained transfers | size on baseline, not "up to" |
| peer settings (`normal` = 4 × 8) | most real transfers | ask peers to raise `network_use`; the slower endpoint sets the pace |
| small files and request rate | many-file datasets | bundle files; spread destinations across prefixes |
| capacity-blind balancing | mixed node sizes | keep the control node out of the data path (#146); same-class instance types |

## Pending results

Numbers from P0 (instance and S3 reference) and the November campaign are
final. P1 (per-file rate vs object size through Globus, reads and writes
separately) and P1b (how the stock connector uses S3, from server access logs)
are running and will replace the per-file figures in sections 2 and 7.
