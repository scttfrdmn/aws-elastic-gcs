# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.12.0] - 2026-11-15

Validated live on the us-west-2 `src` and `dst` deployments.

### Fixed
- `config/open_data_buckets.example.yaml`: `sentinel-cogs` is public, not
  requester-pays. The retired `landsat-pds` is replaced by the requester-pays
  `usgs-landsat`.
- **Scale-in never leaves a running endpoint with zero active nodes** (#71).
  Before removing the last DTN, the scale-in Lambda re-enables a running control
  node and waits for it to be in endpoint DNS. If that fails, it keeps the DTN. A
  stopped control node means the endpoint is meant to be offline, and scale-in
  goes ahead.
- **DTN boot fails fast and stays out of DNS when it should** (#34, #69).
  - Any boot failure completes the launch hook with `ABANDON` right away. The ASG
    then replaces the instance in seconds instead of after the 15-minute hook
    timeout.
  - First boots headed for the warm pool (`Warmed:Pending:Wait`) disable their
    node and wait for it to leave endpoint DNS before signalling. Before, these
    nodes were active, and could take transfers, until the instance stopped.
  - The hook is signalled before self-diagnostics and metrics. Fixed sleeps are
    replaced by polling, and the lifecycle state is read once.
  - A failed Globus credential fetch on resume now stops the boot instead of being
    masked.
  - Warm-pool first boots publish their boot-time metric before signalling,
    because the ASG stops the instance right after the signal (#84).
- **Control node lifecycle** (#30, #31).
  - The resume block in `control_node_init.sh` never ran, because cloud-init runs
    UserData once per instance. It is replaced by `elastic-gcs-control.service`,
    which enables the GCS node on every boot and disables it on shutdown. Both
    steps wait for endpoint DNS.
  - Install the unit on existing control nodes with
    `scripts/control_node_power.sh <region> <deployment> install`.
  - `idle_shutdown` now disables the node via SSM before stopping it, and doesn't
    stop when the disable fails or when `ActiveTransfers` has no data.
- **Deployment key handling** (#32).
  - The key is stored in Secrets Manager right after `endpoint setup`, not at the
    end of setup, so a failed setup can't lead a replacement control node to
    create a second endpoint.
  - The key and slot identities live only in a 0700 temporary directory that is
    removed on exit. Previously they stayed in `/deployment-key.json` and `/tmp`.
  - When setup creates a new endpoint, existing slot secrets are replaced instead
    of being reused for the wrong endpoint.
- **Service-account role** (#66). Setup no longer reports "admin role ensured"
  when role creation fails. It prints the owner's `set-subscription-id` and
  `role create` commands, which are also documented in the operator guide.
- **`cdk deploy` no longer resets the DTN pool** (#71). The ASG template no
  longer sets `DesiredCapacity`, which CloudFormation re-applied on every deploy:
  live DTNs were stopped mid-transfer.
- **The version-drift alarm no longer launches a DTN** (#27). It notified the
  scale-out topic, whose Lambda treated it as demand. It now goes to a new operator
  topic (`elastic-gcs:operatorEmail` subscribes an address). The scale-out Lambda
  also ignores every alarm except the deployment's two scale-out alarms, and any
  non-`ALARM` state.

### Changed
- `CLAUDE.md` matches the code (#36): t3a.large control node, slot locks instead
  of DTN EIPs, the real project layout, current milestones and the site.
- Upgrade note: install the new control-node unit on existing control nodes with
  `scripts/control_node_power.sh <region> <deployment> install`. Deploying the
  control stack updates its UserData in place, which stops and starts the node.

### Security
- **No SSH ingress by default** (#35). An empty `adminCIDR` used to open port 22 to
  `0.0.0.0/0` on the control node and every DTN. SSH is now opened only to an
  explicitly configured CIDR.
- **Lambda permissions are confined to their own deployment** (#35):
  - `ssm:SendCommand` is allowed only with `AWS-RunShellScript`, and only on
    instances tagged with the deployment;
  - `ec2:Start/Stop/TerminateInstances` and `ec2:CreateTags` require that tag;
  - `autoscaling:SetDesiredCapacity` and `CompleteLifecycleAction` are limited to
    the deployment's ASG, for the Lambdas and the DTN role alike;
  - `demand_monitor` reads only the deployment's secrets (part of #44);
  - the lifecycle Lambda deletes only the deployment's SSM parameters.

## [0.11.0] - 2026-11-14

### Added
- Apache-2.0 `LICENSE`. The repository is public, and the project site at
  https://scttfrdmn.github.io/aws-elastic-gcs/ publishes on every push to `main`.
- **Project site, blog post and talk outline** (#70). `mkdocs.yml` builds
  `docs/` into a site (Material for MkDocs), with a hook that turns links to files
  outside `docs/` into repository links. `.github/workflows/docs.yml` builds it with
  `--strict` on every docs PR and publishes to GitHub Pages on every push to `main`. Also adds `docs/blog/2026-11-its-not-aws.md`,
  `docs/talks/its-not-aws-outline.md`, and two more charts: per-DTN throughput by
  network-use setting, and warm-pool scale-out timing.
- `scripts/control_node_power.sh`: stop and start a control node between sessions,
  removing it from endpoint DNS first (~$6/month idle instead of ~$55).
- **Performance documentation** (#70) from the #58 benchmark campaign:
  `docs/performance/` (overview, user/admin guide, operator tuning guide,
  methodology and results), SVG charts generated from
  `test/benchmark/results/2026-11-us-west-2/summary.json`, and
  `scripts/throughput_model.py`, a calculator calibrated on the measured runs.
- Benchmark harness (`test/benchmark/harness/`) and raw results for the
  `2026-11-us-west-2` campaign.
- **Multiple deployments per region** (#57). Each `regions.yaml` entry has a
  `deployment` name (default `default`), and `internal/names` derives every
  stack, resource, secret, SSM path and alarm name from `(region, deployment)`.
  Custom metrics carry a `Deployment` dimension, IAM is scoped to the
  deployment's own secrets and parameters, and everything is tagged
  `elastic-gcs:deployment`. The CLI takes `--deployment`, the Makefile
  `DEPLOYMENT=`, and the tests `ELASTIC_GCS_DEPLOYMENT`. Buckets can be limited
  to specific deployments with `deployments:`.
- `docs/instance-sizing.md` and `scripts/instance_sizing.py`: DTN and control node
  sizing from live EC2 specs and prices, ranked by sustained internet-gateway
  throughput per dollar. Compares Intel, AMD and Graviton, and prices horizontal
  N-node fleets (`--fleet-gbps`) as well as single nodes.
- **`TransferThroughput` metric and a multi-DTN scale-out alarm.** `demand_monitor`
  publishes the sum of `effective_bytes_per_second` across active tasks next to
  `ActiveTransfers` and `QueuedTransfers`. A second alarm scales out once the
  running DTNs approach saturation, not only when the first transfer arrives.
- Integration tests `TestAutoScaleOut` and `TestAutoScaleIn` exercise automatic
  scaling end to end.

### Changed
- README: warm scale-out is about two minutes until the node is in endpoint DNS
  (measured), not 30–60 s.
- **Boot scripts are delivered as S3 assets** (#61). UserData downloads them
  from the CDK asset bucket instead of inlining them, which takes DTN UserData from
  14.7 KB to 2.5 KB and control node UserData from ~17 KB (over the 16 KB EC2
  limit) to 1.9 KB. The DTN UserData no longer installs GCS, the AWS CLI or the
  SSM agent; the mandatory DTN AMI provides them. The control node installs AWS
  CLI v2 before downloading its init script.
- **Breaking (naming):** names now include the deployment, e.g.
  `elastic-gcs/<region>/<deployment>/globus-client-id` and
  `ElasticGCS-Control-<region>-<deployment>`. Existing deployments need fresh
  secrets/parameters under the new paths.
- Enable `@aws-cdk/aws-ec2:uniqueImdsv2TemplateName`. Without it, every control
  stack creates a launch template named `ControlNodeLaunchTemplate`, which
  collides with a second deployment in the region.
- The Globus endpoint display name includes the deployment ID.
- **Default DTN type is now `c8in.2xlarge`** (12.5 Gbps sustained to the internet,
  $0.544/hr in us-east-1). `m6i.xlarge` is previous-generation and sustains
  1.56 Gbps; its internet traffic is capped at 5 Gbps even while bursting.
- The throughput scale-out threshold now defaults to ~70% of what one DTN of the
  configured type sustains (1,093 MB/s for `c8in.2xlarge`) instead of a fixed
  500 MB/s. Unknown types keep 500 MB/s with a synth warning.
- **GCS pinned to 5.4.101** via a single `globus:gcsVersion` CDK context key. The
  control node, DTN UserData fallback, and `scripts/build_ami.sh` all install that
  exact version. GCS 5.4.98 and lower lose support on April 22, 2027; 5.4.101
  (2026-11-04) includes the 5.4.99 S3 end-of-transfer fixes and fixes S3 listing
  timestamps.
- DTN AMIs are now named `elastic-gcs-dtn-gcs<version>-<timestamp>`, and the DTN
  pool looks up the AMI for the pinned version only. Run `make ami` after bumping
  the version. The old Rocky Linux fallback lookup could never run and is removed.
- Dependencies: aws-cdk-go v2.177 → v2.276, jsii-runtime-go v1.142, aws-sdk-go-v2
  v1.47, aws-lambda-go v1.55, Go 1.26. CDK CLI ≥ 2.1145 is now required.
- `config/regions.example.yaml` now uses `t3a.large` for the control node.
  `t4g.medium` is ARM (incompatible with the x86-64 Rocky AMI) and has only 4 GB RAM.
- **DTN AMI is resolved at deploy time** (#63) from the version-keyed SSM
  parameter `/elastic-gcs/<region>/dtn-ami/gcs-<version>`, which `build_ami.sh`
  publishes. Synth no longer fails before an AMI exists, so a fresh deployment can
  bootstrap. Warm pool sizing was also corrected.
- Project renamed to `aws-elastic-gcs`. The Go module is
  `github.com/scttfrdmn/aws-elastic-gcs`, and the CLI is `aws-elastic-gcs-ctl`.
  AWS resource names were not changed by the rename.

### Fixed
- **Scale-out never fired for researchers' transfers.** `demand_monitor` called
  `task_list`, which only returns the caller's own tasks. It now uses the
  `endpoint_manager` task list, which covers every user's tasks on the endpoint.
- **Concurrent DTN first boots** (#64, #67). Simultaneous
  `node setup --import-node` calls raced on the endpoint keychain, and 4 of 8
  instances failed on a fresh pool. First boots now take turns via an SSM setup
  lock (~7 s each), retry with jittered backoff, and run `node cleanup` only when
  setup left partial state.
- **DTN resume and stop with endpoint DNS** (#72). Resume runs `node enable`
  locally and updates the IP through the node's own manager, so a DTN can rejoin
  when no other node is active. The launch hook completes only once the node's IP
  is in endpoint DNS, and shutdown waits for it to leave, so stopped instances no
  longer leave dead IPs behind.
- The scale-in Lambda retries `ScalingActivityInProgress` internally instead of
  failing while a scale-out lifecycle hook completes.
- The demand monitor runs every minute: EventBridge `rate()` rules can't run
  more often, so the 30-second schedule was invalid.
- Integration tests retry transfer submission for up to 3 minutes after
  scale-out, to cover DNS propagation.
- The cost-model test's CloudWatch namespace is now `ElasticGCS`.

### Security
- **GCS IDs are validated before being put into root SSM commands.**
  `cache_eviction` built `globus-connect-server node delete <id>` from an EC2 tag
  that any DTN could write. A compromised DTN could therefore run commands as
  root on the control node. Node and endpoint IDs are now checked as UUIDs
  (`internal/gcsid`) in `cache_eviction` and in the CLI's `nodes delete` and
  `deprovision` paths. The bucket JSON in `collections setup` is base64-encoded.

## [0.10.0] - 2026-02-27

### Added
- **GlobusWorld demo runbook** (#23): `docs/demo-runbook.md` with 7-step live demo
  script (≤8 min), timing summary table, contingency paths for common failure modes
  (cold pool, expired auth, DTN activation failure, CloudWatch gaps), post-demo
  cleanup, and benchmark numbers placeholder for slide deck.

### Changed
- **README overhaul** (#23):
  - Added three-tier cold/warm/hot ASCII architecture diagram.
  - Expanded cost model from a 4-row comparison table to a full 4-scenario idle-cost
    breakdown referencing `docs/ephemeral-control-node.md`.
  - Updated CLI reference table with `wake`, `collections list/setup`, and `deprovision`.
  - Added `docs/ephemeral-control-node.md` and `docs/demo-runbook.md` to the docs index.

## [0.9.0] - 2026-02-27

### Added
- **Benchmarks** (#22): new `test/benchmark/` package (`//go:build benchmark`) with
  per-phase timing for warm, hot, and cold scale-out paths and a cost model validator:
  - `TestBenchmarkWarmPath` — N iterations (default 5) of warm-path: T0 (SetDesiredCapacity) →
    T1 (EC2 running) → T2 (SSM online) → T3 (GCS node active) → T4 (transfer SUCCEEDED).
    Records per-phase timing; asserts P95 SLOs (EC2 resume < 90s, node enable < 60s).
  - `TestBenchmarkHotPath` — N iterations (default 3) of transfer with DTN already
    InService; measures submit→first-bytes and first-bytes→SUCCEEDED phases.
  - `TestBenchmarkColdPath` — N iterations (default 3) of cold launch from AMI; opt-in
    via `ELASTIC_GCS_TEST_COLD_PATH=true` (terminates warm pool, destructive).
  - `TestCostModel` — samples 7 days of CloudWatch `ActiveTransfers` and `QueuedTransfers`
    metrics, extrapolates monthly DTN cost, and compares elastic vs always-on static cost.
  - JSON results written to `test/benchmark/results/YYYY-MM-DD-HHmm-REGION-TIER.json`
    for slide deck use.
- Updated `make benchmark` target to invoke `go test ./test/benchmark/ -tags=benchmark`
  with `REGION`, `TIER` (default: warm), and `ITERATIONS` variables forwarded.

## [0.8.0] - 2026-02-26

### Added
- **Integration tests** (#21): replaced stub `lifecycle_test.go` with four real test files
  under `test/integration/` tagged `//go:build integration`:
  - `helpers_test.go` — shared `testEnv`, AWS/Globus client init, SSM command runner,
    ASG polling helpers (`waitForASGInService`, `waitForWarmPool`, `inServiceIDs`),
    GCS node list via SSM RunCommand + JSON parse, transfer submit/wait helpers.
  - `node_lifecycle_test.go` (`TestNodeLifecycle`) — full warm-pool cycle: assert
    Warmed:Stopped → scale-out → InService → GCS node active → transfer SUCCEEDED →
    scale-in → Warmed:Stopped → GCS node inactive. Uses `t.Cleanup` to restore ASG
    desired=0 on failure.
  - `transfer_test.go` (`TestTransferEndToEnd`) — self-contained transfer test;
    scales out a DTN if none are InService, submits 15KB file from 1000 Genomes
    collection to Globus Tutorial Endpoint 2, asserts SUCCEEDED within 5 min.
  - `scaling_test.go` (`TestScaleOutLatency`) — warm-path scale-out timing with
    per-phase breakdown: desired=1→InService, InService→GCS active, GCS ready→submit,
    submit→first bytes, first bytes→SUCCEEDED. Supports `ELASTIC_GCS_ITERATIONS`
    for multi-run mean/max stats. Asserts InService < 120s, GCS active < 90s.
- `make test-integration REGION=<r>` now forwards `REGION` as `ELASTIC_GCS_REGION`
  and accepts `ITERATIONS=<n>` and `TEST_RUN=-run <TestName>` variables.

## [0.7.0] - 2026-02-26

### Added
- **Auto-wake on queued transfers**: `demand_monitor` Lambda now detects INACTIVE Globus
  Transfer tasks (tasks queued while the endpoint is offline) and automatically invokes
  the Waker Lambda with `scale_out=true`. This closes the auto-wake gap: the system now
  wakes itself when a user submits a transfer against a stopped endpoint, without any
  operator intervention.
- **`QueuedTransfers` CloudWatch metric**: `demand_monitor` now publishes both
  `ActiveTransfers` and `QueuedTransfers` metrics to `ElasticGCS/{Region}`.
- **Waker ScaleOut fix (already-running path)**: When the control node is already running,
  the Waker Lambda now checks ASG `DesiredCapacity` and triggers scale-out if
  `scale_out=true` is requested and no DTNs are active. Previously this path returned
  early without checking whether DTNs needed to be started.
- `WAKER_FUNCTION_NAME` environment variable injected into `demand_monitor` Lambda.
- `lambda:InvokeFunction` IAM permission added to `demand_monitor` Lambda role.
- Waker Lambda timeout increased from 5 to 6 minutes (accounts for 3 min SSM agent
  wait + 2 min GCS Manager wait + headroom).

## [0.6.0] - 2026-02-26

### Added
- **`aws-elastic-gcs-ctl deprovision [--confirm]`** (#20): complete ordered teardown of all
  Elastic GCS resources in a region. Without `--confirm` performs a dry run: inventories
  every resource and prints what would be deleted, then exits cleanly. With `--confirm`
  executes a 10-step teardown in order:
  1. Scale ASG to zero, force-delete warm pool, drain all instances (10 min timeout)
  2. Delete GCS nodes via GCS Manager API (falls back to SSM RunCommand if API unavailable)
  3. Delete mapped collections via GCS Manager API (with SSM fallback)
  4. Delete storage gateways via GCS Manager API (with SSM fallback)
  5. Delete CloudFormation stacks (DTNPool first, then Control); poll to completion (15 min each)
  6. Delete Secrets Manager secrets with `ForceDeleteWithoutRecovery`
  7. Delete SSM parameters (batch, 10 at a time)
  8. Delete CloudWatch log groups
  9. Deregister DTN AMI and delete its EBS snapshots
  10. Delete the Globus endpoint registration via GCS Manager API
  Steps 2–4 and 10 use the GCS Manager API directly (via `github.com/scttfrdmn/globus-gcs-go`)
  and do not require the control node to be running. SSM RunCommand is used as a fallback for
  steps 2–4 if the GCS API client cannot be initialised (e.g. missing client credentials).
- Added `github.com/scttfrdmn/globus-gcs-go v0.1.0` dependency for direct GCS Manager API access.
- Added `github.com/aws/aws-sdk-go-v2/service/cloudformation` dependency.
- `secretsmanager` SDK promoted to direct import in the CLI.

### Changed
- `toolVersion` bumped to `v0.6.0`.

## [0.5.1] - 2026-02-26

### Security
- **Waker Lambda: replace bearer token auth with AWS_IAM** — the Lambda Function
  URL now uses `AuthType=AWS_IAM`. The Lambda service enforces SigV4 authentication
  before code runs; unauthenticated callers receive HTTP 403 from the service layer,
  never reaching application code.
- Removed `WakerTokenSecret` Secrets Manager secret and `WakerTokenSecretArn` stack
  output (no longer needed).
- Removed `checkBearerToken` application-level auth logic from `lambda/waker/main.go`.

### Changed
- `docs/ephemeral-control-node.md`: updated Function URL section to describe AWS_IAM
  auth and SigV4 signing (`awscurl`). Replaced Globus Flows bearer token example with
  a proxy-based pattern (required because Globus Flows HTTP actions cannot sign AWS
  SigV4 requests).

## [0.5.0] - 2026-02-26

### Added
- **#17 Ephemeral control node** (`lambda/waker`, `lambda/idle_shutdown`):
  automatically stop the control node when idle, reducing cost from ~$58/month
  to ~$4.50/month at rest.
  - `elastic-gcs-waker` Lambda: starts the control node on demand, polls until
    GCS Manager is healthy (~40s), optionally triggers DTN scale-out in parallel.
    Exposed via a Lambda Function URL (HTTPS) for Globus Flows HTTP actions and
    direct Lambda invocation (IAM-signed) for the CLI.
  - `elastic-gcs-idle-shutdown` Lambda: EventBridge every 15 min; stops the
    control node when desired=0, all warm-pool instances are `Warmed:Stopped`,
    and `ActiveTransfers` has been 0 for the configured idle window.
  - Warm-pool DTNs remain stopped (not terminated) when the control node stops,
    preserving the fast (~60s) wake path. Cache eviction still terminates
    instances after 72h (two-stage cost model: ~$4.50/month → ~$2.50/month).
- `aws-elastic-gcs-ctl wake [--scale-out]`: new CLI command invokes the waker Lambda
  directly and reports warm-pool vs cold-boot path.
- `config/regions.yaml`: new per-region fields `idle_shutdown_minutes` (0 =
  disabled, default) and `min_uptime_minutes` (default 30).
- `docs/ephemeral-control-node.md`: full documentation covering availability
  implications, configuration, wake mechanisms (CLI, Function URL, cron schedule),
  Globus Flow sample definition, security notes, cost model, and a detailed
  section on what Globus could do to make ephemeral endpoints fully transparent
  to end users.
- Stack output: `WakerFunctionUrl`.

### Fixed
- `aws-elastic-gcs-ctl status`: gracefully handles a stopped control node — shows
  `○ stopped` with a wake hint and skips SSM-dependent queries (GCS node list)
  rather than timing out.

## [0.4.2] - 2026-02-26

### Added
- `scripts/configure_tls.sh`: standalone script to apply organization-approved TLS cipher
  policy to any GCS node (control or DTN). Enforces cipher restrictions via two layers:
  (1) RHEL 9 system crypto-policy module `ELASTIC-GCS.pmod` — constrains OpenSSL at the
  OS level so approved ciphers are enforced even when GCS regenerates its Apache VirtualHost
  `SSLCipherSuite` settings; (2) `/etc/grid-security/gsi.conf` for GridFTP data channels
  (ports 50000–51000). Reloads httpd and GridFTP if running.
- `scripts/build_ami.sh`: runs `configure_tls.sh` via SSM after GCS install so the policy
  is baked into every DTN AMI (`ami-0890f89156e2fd66f`, `elastic-gcs-dtn-20260226-124654`).
- `scripts/userdata/control_node_init.sh`: section 1a applies the same crypto-policy and
  `gsi.conf` settings immediately after GCS installation, before endpoint setup.

### Security
- Removed from DEFAULT TLS policy: `CHACHA20-POLY1305`, `DHE-RSA-*` (all FFDHE groups).
- Approved cipher set (IANA names):
  `TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256`, `TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384`,
  `TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA256`, `TLS_ECDHE_ECDSA_WITH_AES_256_CBC_SHA384`,
  `TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256`, `TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384`,
  `TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA256`, `TLS_ECDHE_RSA_WITH_AES_256_CBC_SHA384`.
- All nodes updated: control node live via SSM, warm pool DTNs via AMI rebuild + refresh.
- DTN AMI rebuilt (`ami-0890f89156e2fd66f`); warm pool refresh completed successfully.

### Fixed
- Initial `ELASTIC-GCS.pmod` included `key_exchange = ECDHE ECDHE-PSK` (from RHEL's
  `ECDHE-ONLY.pmod`), which causes `auth.globus.org` to close the TLS handshake with
  `close_notify`, breaking `globus-connect-server node setup`. Removed the directive;
  `group = -FFDHE-*` alone is sufficient to eliminate DHE without affecting Globus
  connectivity. Bisected via per-directive policy isolation testing on a live instance.

## [0.4.1] - 2026-02-23

### Added
- `docs/architecture.md`: three-tier caching model, component overview, scaling trigger
  flow, design decisions, observed timings, cost model (#18)
- `docs/operator-guide.md`: full deploy-through-destroy lifecycle runbook including
  first deploy, day-1 validation, day-2 operations (scaling, logging, GCS version
  upgrade, warm pool eviction, stale node cleanup), teardown procedure, and
  troubleshooting (#18, #19)
- `docs/configuration.md`: CDK context parameters, Secrets Manager keys, SSM parameters,
  Lambda environment variables, S3 bucket configuration reference (#18)
- `README.md`: rewritten for any Globus subscriber (not Open Data-specific); adds cost
  comparison table, architecture summary, `aws-elastic-gcs-ctl` command reference, project
  structure, documentation links (#18)

### Fixed
- `aws-elastic-gcs-ctl nodes delete`: resolve 8-char node ID prefix to full UUID before
  issuing `globus-connect-server node delete` (GCS CLI requires full UUID)
- `aws-elastic-gcs-ctl transfer test`: replaced 170-line OAuth/PKCE/SDK flow with
  `exec.Command("globus", "transfer", ...)` — delegates to the operator's installed
  and authenticated `globus` CLI

## [0.4.0] - 2026-02-23

### Added
- **#13 Structured logging** (`internal/logger`): all Lambda functions now emit JSON log
  lines via `log/slog`. Fields `component`, `region`, and event-specific key-value pairs
  are attached to every line. CloudWatch Logs Insights can filter and aggregate by field.
- **#14 GCS version drift detection** (`lambda/version_check`): new Lambda runs every 6
  hours via EventBridge. Runs `globus-connect-server --version` on the control node and
  all InService DTNs via SSM RunCommand, then publishes `ElasticGCS/GCSVersionDrift`
  (0 = all nodes agree, >0 = drift). CloudWatch alarm fires when drift > 0; Makefile
  `build-lambdas` target updated.
- **#12 CloudWatch dashboard** (`ElasticGCS-{region}`): three-row, 3-column layout:
  Row 1: Active Transfers line, DTN scale (InService + warm pool), alarm status widget.
  Row 2: Scale events bar chart, lifecycle P99 latency, all-Lambda error counts.
  Row 3: GCS Version Drift single-value, demand monitor invocations/errors, cache eviction duration.
- **`aws-elastic-gcs-ctl` CLI** (full implementation, previously stubs):
  `status`, `scale up/down [--to N] [--watch]`, `nodes list/delete/version-check`,
  `logs <lambda> [--since] [--tail] [--json]`, `metrics [--since]`, `refresh [--force]`,
  `transfer test`, `build-ami`, `version`. Uses Cobra + fatih/color.

### Dependencies
- Added: `github.com/spf13/cobra`, `github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs`,
  `github.com/aws/aws-sdk-go-v2/service/lambda`,
  `github.com/aws/aws-sdk-go-v2/service/autoscaling/types` (indirect, already present)
- `github.com/fatih/color` promoted from indirect to direct dependency

## [0.3.1] - 2026-02-23

### Fixed
- **Spot↔warm pool mode switching** (`stacks/dtn_pool.go`): Switching `use_spot: true`
  previously required manually deleting the warm pool before `cdk deploy` due to a CFN
  race between the ASG update (MixedInstancesPolicy) and the WarmPoolCR deletion.
  Fixed by adding `SpotCleanupCR` when `use_spot=true`: it calls `deleteWarmPool` with
  `IgnoreErrorCodesMatching=".*"` (no-op if no warm pool exists), and
  `asg.Node().AddDependency(spotCleanupCR)` ensures CFN deletes the warm pool before
  updating the ASG. `WarmPoolCR.OnDelete` also gains `IgnoreErrorCodesMatching=".*"` to
  make stack deletion and mode switching idempotent.

### Validated
- **Spot interruption test via AWS FIS** (`aws:ec2:send-spot-instance-interruptions`):
  FIS experiment `EXPxWeT5b6mgJLscfU` sent 2-minute interruption notice to Spot DTN
  `i-03353d4160e0859df`. Monitor detected within 5s, called `node disable`.
  Instance terminated at t+135s; GCS node removed from active rotation. ASG launched
  replacement Spot DTN `i-0dc33ee6445c4d1d5` (same slot/node identity), came InService,
  endpoint remained healthy throughout. Spot interruption handler confirmed working.

## [0.3.0] - 2026-02-23

### Added
- **Spot instances for DTNs** (#7): DTNs can now launch as EC2 Spot instances.
  Set `use_spot: true` in `config/regions.yaml` to enable.
  Uses ASG `MixedInstancesPolicy` with `OnDemandPercentageAboveBaseCapacity=0`
  and `SpotAllocationStrategy=PRICE_CAPACITY_OPTIMIZED`.
  Optional `spot_max_price` (string, e.g. `"0.10"`) caps exposure; empty = up to on-demand.
  Expected savings: 60–80% on active DTN compute cost vs on-demand m6i.xlarge.
- **Spot interruption monitor** (`scripts/userdata/spot_interrupt_monitor.sh`):
  Systemd service (`elastic-gcs-spot-monitor.service`) polls IMDS every 5 seconds for
  a spot interruption notice (`/spot/instance-action`). On detection, calls
  `globus-connect-server node disable` within the 2-minute warning window before
  EC2 stops the instance. Harmless on on-demand instances (IMDS returns 404).
- `config/config.go`: `UseSpot bool` and `SpotMaxPrice string` added to `RegionConfig`.
- New DTN AMI `ami-072a6d44588f30309` (elastic-gcs-dtn-20260222, GCS 5.4.92) with
  updated `dtn_shutdown.sh` (no slot lock release) and `spot_interrupt_monitor.sh`.

### Changed
- **Spot and warm pool are mutually exclusive** (AWS API constraint): when
  `use_spot: true`, the warm pool is skipped — DTNs cold-start (2–3 min) but run
  at ~70% lower cost. When `use_spot: false` (default), warm pool provides 30–60 s
  scale-out at on-demand pricing. Choose based on workload: interactive = warm pool,
  batch/cost-sensitive = Spot.

## [0.2.3] - 2026-02-23

### Added
- **Stale GCS node cleanup** (#9): `lambda/cache_eviction/main.go` now sweeps orphaned GCS
  nodes — nodes registered on the endpoint that have no corresponding live EC2 instance —
  by running `globus-connect-server node delete NODE_ID` on the control node via SSM RunCommand.
  Orphan sweep only proceeds when ALL warm pool + InService instances carry the
  `elastic-gcs:gcs-node-id` tag, preventing false-positive deletes during the transition
  period.
- `dtn_boot.sh` now tags each instance with `elastic-gcs:gcs-node-id=NODE_ID` after the GCS
  node is configured (first boot or resume), enabling the orphan sweep to correlate EC2
  instances with their GCS nodes.
- **Slot lock lifecycle fix**: SSM slot locks are now held for the lifetime of the EC2
  instance, not just the running period. Locks are released only on permanent termination
  (lifecycle Lambda `Destination=="EC2"`), preventing two warm pool instances from sharing
  the same slot/GCS node identity when slots were prematurely released on stop.
- `lambda/lifecycle/main.go`: `handleScaleIn` releases the slot lock on termination via
  `releaseSlotLock`, with an ownership check to prevent releasing a lock held by a different
  instance. Adds `Origin`/`Destination` fields to `LifecycleEvent`.
- `stacks/dtn_pool.go`: Lifecycle Lambda IAM gains `ssm:GetParameter` + `ssm:DeleteParameter`
  for slot lock release; cache eviction Lambda gains `CONTROL_NODE_ID_PARAM` env var and
  `ssm:GetParameter` for control node SSM lookup.

### Fixed
- `dtn_shutdown.sh`: Removed slot lock release — locks are now managed exclusively by the
  lifecycle Lambda, which has the context to distinguish warm-pool stop from termination.

## [0.2.2] - 2026-02-22

### Fixed
- `control_node_init.sh`: IMDS token TTL increased from 60 s to 3600 s — the 60 s TTL
  expired during the multi-minute setup sequence, leaving `INSTANCE_ID` empty and causing
  `aws ssm put-parameter` to fail with `ValidationException` (#18)
- `control_node_init.sh`: Storage gateway/collection idempotency — skip creating a storage
  gateway if one with the same display name already exists, preventing duplicate collections
  on every control node replacement
- `control_node_init.sh`: DTN node identity idempotency — skip `node create` for slots
  where a valid `node_id` already exists in Secrets Manager, preventing orphaned nodes on
  control node replacement
- `stacks/control_node.go`: Comment clarifies GCS 5.4 is x86-64 only; t4g (Graviton) is
  not usable until Globus publishes aarch64 EL9 packages

### Validated
- End-to-end transfer SUCCEEDED after control node replacement:
  `1000genomes/release/20130502/20140625_related_individuals.txt`
  (1,168 bytes, task `d0875190-102b-11f1-811a-025da3873719`)

## [0.2.1] - 2026-02-22

### Added
- EIP on control node for stable public IP across stop/start and CDK deploys
- Idempotency guard in `control_node_init.sh`: if GCS already configured, skip
  full setup and only update IP via `node update` (prevents duplicate
  endpoint/gateway/node creation on cloud-init re-runs)

### Fixed
- DTN boot script resume path: `node update` requires `GCS_CLI_CLIENT_ID`,
  `GCS_CLI_CLIENT_SECRET`, and `GCS_CLI_ENDPOINT_ID` env vars; resume path now
  fetches these from Secrets Manager before calling `node update` (#16)
- Slot-assignment race on warm pool resume: node_id now persisted to
  `/var/lib/elastic-gcs/node-id` at first boot; resume path reads from file
  instead of deriving from claimed slot number, ensuring correct GCS node is
  updated even when slot claim differs between boots (#16)
- Removed DTN Elastic IPs — replaced with SSM Parameter Store atomic slot
  locking (`put-parameter` without `--overwrite` = atomic claim); saves ~$21/mo
  per region when DTNs are idle (#15)

### Changed
- `maxDTNs` reduced from 5 to 4 to stay within EIP quota during transition
- DTN AMI updated to `ami-05c0cd8644ab1b149` (`elastic-gcs-dtn-20260221-212845`)
  with all boot script fixes baked in

### Validated
- Scale-out (warm resume): **~64 seconds** desired=1 → InService
- Resume latency (`node update --ip-address --enable`): **~7 seconds**
- SSM slot locking: atomic claim verified across concurrent boot attempts
- End-to-end transfer SUCCEEDED: `1000genomes/release/20130502/20140625_related_individuals.txt`
  (1,168 bytes, task `c7c3d7e6-0fb5-11f1-ac79-025da3873719`)
- Note: S3 connector path format with `s3_allow_multi_keys: true` uses first component as
  bucket name — correct path is `/1000genomes/<key>`, not `/<key>`

## [0.2.0] - 2026-02-21

### Added
- Demand monitor Lambda polls Globus Transfer API every 1 minute and publishes
  `ElasticGCS/ActiveTransfers` CloudWatch metric (#3)
- `MaxDTNs` config field controls maximum active DTN count independently of warm
  pool size; `elastic-gcs:maxDTNs` CDK context key (default: 5) (#2)
- Warm pool CDK custom resource via `AwsCustomResource` — eliminates the manual
  `aws autoscaling put-warm-pool` CLI step after every `cdk deploy` (#6)
- Cache eviction TTL now uses `elastic-gcs:warm-pool-entry-time` EC2 tag (set by
  lifecycle Lambda on scale-in) rather than instance launch time (#5)

### Fixed
- SNS envelope unwrapping in scale-out and scale-in Lambdas — CloudWatch alarms
  routed through SNS now correctly parse and trigger scaling actions (#1)
- Scale-in Lambda checks `ElasticGCS/ActiveTransfers` metric before decrementing
  desired capacity; skips scale-in when transfers are active (#4)
- Scale-in alarm changed from `NetworkIn < 5 MB` (always fires when idle) to
  `ActiveTransfers == 0 for 15 min`; `TreatMissingData=NOT_BREACHING` prevents
  false scale-in when demand monitor is unavailable (#3)

## [0.1.0] - 2026-02-20

### Added
- Control node stack: VPC, security group (TCP 443 + 50000-51000), EC2
  (t3a.large), GCS endpoint setup, Secrets Manager for deployment key and node
  identities
- DTN pool stack: ASG (min=0, max=3), warm pool (2 stopped instances), lifecycle
  hooks, Elastic IPs (one per ASG slot, tagged for runtime discovery)
- IAM roles: control node (GCS + Secrets Manager + SSM) and DTN (S3 read + EIP
  association + lifecycle signaling)
- Boot script (`dtn_boot.sh`): first-boot detection, EIP claim, `node setup
  --import-node` (cold path) and `node enable` (warm path)
- Shutdown script (`dtn_shutdown.sh`): `node disable` with lock file permission
  fix (`chown gcsweb:gcsweb subscription_pause`)
- Lifecycle Lambda: SSM Run Command → `node disable` before instance stops;
  scale-in latency ~30 seconds
- Scale-out Lambda: increments ASG desired capacity, capped at MAX_DTNS
- Scale-in Lambda: decrements ASG desired capacity when idle
- Cache eviction Lambda: terminates warm pool instances stopped > 72 hours
- DTN AMI `elastic-gcs-dtn-20260218-175354`: GCS 5.4.92 pre-installed on Rocky
  Linux 9 with AWS CLI v2, jq, unzip

### Validated
- Globus endpoint `c6b7629b-da91-479a-bd30-c60383ba05c5` with 1000 Genomes Open
  Data collection `c8274220-8a6d-42da-873d-79d5501a5a03`
- End-to-end transfer (warm path): warm pool resume → `node enable` → transfer
  SUCCEEDED (15,977 bytes, 3 seconds, task `867b66d1-0e99-11f1-a9bc-0affe9df1385`)
- Scale-out latency (warm): ~60 s DTN InService; scale-in: ~30 s (SSM path)

[Unreleased]: https://github.com/scttfrdmn/aws-elastic-gcs/compare/v0.12.0...HEAD
[0.12.0]: https://github.com/scttfrdmn/aws-elastic-gcs/compare/v0.11.0...v0.12.0
[0.11.0]: https://github.com/scttfrdmn/aws-elastic-gcs/compare/v0.10.0...v0.11.0
[0.10.0]: https://github.com/scttfrdmn/aws-elastic-gcs/compare/v0.9.0...v0.10.0
[0.9.0]: https://github.com/scttfrdmn/aws-elastic-gcs/compare/v0.8.0...v0.9.0
[0.8.0]: https://github.com/scttfrdmn/aws-elastic-gcs/compare/v0.7.0...v0.8.0
[0.7.0]: https://github.com/scttfrdmn/aws-elastic-gcs/compare/v0.6.0...v0.7.0
[0.6.0]: https://github.com/scttfrdmn/aws-elastic-gcs/compare/v0.5.1...v0.6.0
[0.5.1]: https://github.com/scttfrdmn/aws-elastic-gcs/compare/v0.5.0...v0.5.1
[0.5.0]: https://github.com/scttfrdmn/aws-elastic-gcs/compare/v0.4.2...v0.5.0
[0.4.2]: https://github.com/scttfrdmn/aws-elastic-gcs/compare/v0.4.1...v0.4.2
[0.4.1]: https://github.com/scttfrdmn/aws-elastic-gcs/compare/v0.4.0...v0.4.1
[0.4.0]: https://github.com/scttfrdmn/aws-elastic-gcs/compare/v0.3.1...v0.4.0
[0.3.1]: https://github.com/scttfrdmn/aws-elastic-gcs/compare/v0.3.0...v0.3.1
[0.3.0]: https://github.com/scttfrdmn/aws-elastic-gcs/compare/v0.2.3...v0.3.0
[0.2.3]: https://github.com/scttfrdmn/aws-elastic-gcs/compare/v0.2.2...v0.2.3
[0.2.2]: https://github.com/scttfrdmn/aws-elastic-gcs/compare/v0.2.1...v0.2.2
[0.2.1]: https://github.com/scttfrdmn/aws-elastic-gcs/compare/v0.2.0...v0.2.1
[0.2.0]: https://github.com/scttfrdmn/aws-elastic-gcs/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/scttfrdmn/aws-elastic-gcs/releases/tag/v0.1.0
