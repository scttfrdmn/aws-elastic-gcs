# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added
- **Slot-controller and cost settings** in `regions.yaml` (#126).
  - New settings: `pool`, `cost_profile` (`balanced` | `minimal`), `warm_slots`,
    `cold_ttl`, `drain_timeout`, `scale_in_idle`, `unhealthy_after`,
    `max_restarts`, `instance_types` (a fallback list), `control_node`
    (`always` | `on_demand` | `none`) and `dashboard`.
  - All are resolved and validated in `config.Resolve` and documented in
    `docs/configuration.md`.
  - Profile defaults never exceed `max_dtns`, and explicit settings beat the
    profile.
- **`pool: slots` stack** (#128, #129). With `pool: slots` the DTN stack
  builds no ASG, warm pool or lifecycle hooks. Instead it creates:
  - a runtime role with no secret access, and a setup role that can read the
    deployment key and `node-slot-*`, each with its own instance profile;
  - a launch template whose UserData installs a node boot/shutdown unit;
  - the durable controller Lambda, with tag-scoped permissions for launch,
    start/stop/terminate, Elastic IPs, `PassRole`, the profile swap,
    setup-session revocation, SSM and its parameters;
  - EventBridge triggers: a 1-minute schedule and changes to `desired-dtns`.

  `demand_monitor` writes `desired-dtns`: it only rises while transfers are
  active, and drops to 0 after `scale_in_idle`. A synth test checks the shape,
  including that only the setup role can read secrets. `pool: asg` is unchanged.
- **Slot controller runtime** (#128, part 1).
  - `internal/slotctl` observes slots: instances and Elastic IPs, health from SSM
    ping plus endpoint DNS, and GridFTP data connections. Controller state is
    kept in tags only the controller can write.
  - It executes the reconciler's actions: start, drain (local `node disable`),
    stop, restart, rebuild, evict (releasing the Elastic IP), revive (launch,
    attach the slot's Elastic IP, `identity.Setup`, falling back through
    `instance_types` on capacity errors), and making sure the control node is
    serving.
  - Only one controller runs at a time, enforced by an SSM lock.
  - `lambda/slot_controller` is a Lambda durable function (AWS Durable Execution
    SDK for Go v1.0.0). Each round observes, plans and executes, with a durable
    wait in between. An action error is reported and re-planned rather than
    failing the run. It is tested with `durabletest`.
- `internal/identity`: one-time identity delivery for slots (#127, part of #42).
  - The instance profile is swapped to a setup role and `node setup` runs over
    SSM; then the profile is swapped back and every setup-role session issued
    earlier is revoked.
  - The swap back and the revocation always run, even when setup fails.
  - Verified live: credentials copied during setup were refused about 5 s after
    the revocation.
- `internal/slots`: the slot controller's pure decision core (#125, design in
  #65).
  - `Plan(observed, desired, cfg)` returns start, revive, drain, stop, restart,
    rebuild and evict actions over HOT, WARM and COLD slots.
  - Rules: warm slots start first and in parallel; cold slots revive one at a
    time; scale-in goes one slot at a time with the fewest data connections
    first; the last DTN never goes unless the control node is serving; unhealthy
    slots are restarted, then rebuilt; warm slots beyond `warm_slots` are evicted
    after `cold_ttl`.
  - Table-tested.

### Changed
- The CLI publishes a Homebrew **cask** (`homebrew_casks`, `Casks/`, macOS and
  Linux). goreleaser deprecated binary-installing formulas (`brews`) in v2.10 and
  v2.16. The tap's short-lived formula migrates to the cask via
  `tap_migrations.json`. On macOS the cask clears the quarantine flag
  (`install_steps`). Homebrew 7 needs `brew trust scttfrdmn/tap`. A missing tap
  or bucket token now fails the release instead of silently skipping. Re-running
  the release workflow for an existing tag uses the default branch's
  `.goreleaser.yaml`.

- **The control node leaves the data path while DTNs serve (#146).** Globus
  balances sessions across nodes without regard to capacity, so the t3a.large
  control node took half of each task's sessions next to a c8in DTN.
  - The slot controller disables it locally once a DTN has been healthy for
    `control_demote_after` (new setting, default `2m`; `off` disables).
  - It re-enables it as soon as no DTN is healthy, and before the last DTN
    drains.
  - In-flight sessions on the control node finish (#141).
  - A control node stopped on purpose stays stopped.

- **DTN network tuning (#145).** The DTN AMI ships ESnet fasterdata host
  tuning (`scripts/configure_network.sh`): BBR with fq pacing, TCP buffers
  autotuning up to 32 MB, and MTU probing. `build_ami.sh` refuses to publish an
  AMI without it. The control node stays on kernel defaults.
  - Measured A/B: neutral on clean paths (1.05 vs 1.01 Gbps).
  - At +100 ms and 0.1% loss: 0.97 Gbps with BBR against 0.16 Gbps with
    `cubic` (`docs/performance/methodology.md`).

### Fixed
- **A transient first-boot failure no longer leaves a slot broken (#146).** On a
  new deployment's first revive, IAM hadn't propagated the new runtime role
  yet. The UserData download of the node unit's script got a 403, and the slot
  worked once (node setup enables the node) but never came back after a stop.
  - The download now retries.
  - Revive waits for cloud-init and requires the node unit to be installed;
    otherwise it terminates the instance and revives a fresh one.
  - The restart → rebuild path also healed it live.
- **Slot controller, from the first live run on dst (#132):**
  - EventBridge targets a `live` alias. Durable functions reject unqualified
    invokes, and CDK grants no invoke permission for `$LATEST`.
  - Slots launch in the deployment's public subnets, with the named
    `dtn-runtime` profile that the controller swaps back to after setup.
  - A revive whose controller run died is rebuilt after `unhealthy_after`.
    Before, it counted as serving forever and blocked every other cold revive.
    A slot that never finishes starting is restarted.
  - Slot health also requires `gcs_manager` and `globus-gridftp-server` to be
    active. A slot's Elastic IP can stay in endpoint DNS after its instance
    dies.
- **Node joins and leaves no longer reset in-flight transfers (#141).** GCS
  restarts GridFTP on every node at each node join or leave, and on `node
  disable`. Its unit's default `KillMode=control-group` killed every session.
  - `scripts/configure_gridftp.sh` installs a `KillMode=process` drop-in, so
    only the listener restarts and sessions finish in their own processes.
  - Drains are now graceful: sessions finish on a disabled node while DNS
    sends new work elsewhere.
  - It ships in the DTN AMI (`build_ami.sh`, which now refuses to publish an
    AMI without it) and in control-node init. `control_node_power.sh install`
    applies it to existing control nodes.
  - Measured live: 128 of 128 sessions survived a restart, a disable and an
    enable with no task faults.
- `waker` waited for `globus-connect-server-manager`; on GCS 5.4 the unit is
  `gcs_manager`, so the wait always timed out.

### Removed
- **The Auto Scaling group pool (#133).** The slot controller is the only DTN
  pool; src and dst were migrated in #132.
  - **Stacks:** the ASG, warm pool and its custom resources, lifecycle hooks,
    the scale-out/scale-in alarms and SNS topics, and the dashboard. The DTN
    stack builds the slot controller only.
  - **Lambdas and packages:** `scaleout`, `scalein`, `lifecycle` and
    `cache_eviction`; `internal/warmpool`, `internal/slotlock` and
    `internal/eviction`. demand_monitor loses its ASG branch and always writes
    `desired-dtns`.
  - **Scripts:** `dtn_boot.sh`, `dtn_shutdown.sh` and
    `spot_interrupt_monitor.sh`. Slots run `control_node_boot.sh` as their boot
    unit.
  - **Config:** `warm_pool_size`, `use_spot`, `spot_max_price`,
    `idle_shutdown_minutes` and `min_uptime_minutes` are rejected with a pointer
    to their replacement. `pool` is optional and accepts only `slots`. The
    `elastic-gcs:warmPoolSize` and `elastic-gcs:cooldownMinutes` context keys
    are gone. The control node pre-creates `max_dtns` node identities (it was
    `max_dtns + warm_pool_size`).
  - **CLI:** `refresh` (ASG instance refresh) is gone; slot rebuild and evict
    come with #131. `status` shows slots and desired-dtns, `scale` sets
    desired-dtns (and refreshes `last-active` so demand holds it), and
    `deprovision` terminates slot instances and releases their Elastic IPs
    before deleting the stacks.
  - **Tests:** the ASG integration tests (autoscale, scale-out latency, node
    lifecycle) are replaced by `TestSlotScaleOutIn`. The benchmark timing tests
    and `harness/scale.sh`, `netem.sh` and `netsnap.sh` now drive slots.

  Not deployed by the slots stack yet: `version_check` and its alarm, the
  operator alert topic and the dashboard (#144); `idle_shutdown` and `waker`
  (#130). Spot isn't supported on slots yet.

  **Upgrade:** delete `warm_pool_size` (and `pool: asg`, if present) from
  `config/regions.yaml`. Deploying the DTN stacks changes only demand_monitor's
  environment and IAM. Deploying a control stack applies its new UserData
  (`TOTAL_SLOTS = max_dtns`), which stops and starts the control node; existing
  endpoints keep their identities.

## [0.13.0] - 2026-11-15

Validated live on the us-west-2 `src` and `dst` deployments. Upgrade notes:
- **Refresh the warm pool after deploying** so DTNs get the `elastic-gcs:role=dtn`
  tag; role-scoped Lambdas require it.
- **Rebuild the DTN AMI** with `make ami`, to pick up verified installs and the
  TLS policy.
- **Apply network use** to existing endpoints with
  `aws-elastic-gcs-ctl endpoint network-use --apply`.
- **Retained log group:** if a DTN stack deploy fails and rolls back, it leaves
  `/elastic-gcs/<deployment>/ssm-commands` behind, and the next deploy fails until
  that (empty) log group is deleted.

### Added
- **Endpoint network use from config** (#56). `regions.yaml` `network_use`
  sets preferred and max concurrency and parallelism.
  - When it's unset, the values are derived from `max_dtns`: 32/64 concurrency
    per DTN, capped at 128, with parallelism 8/16. This is the setup measured in
    #58: ~9.2 Gbps per c8in.2xlarge versus ~0.35 Gbps at Globus's default.
  - The control node's first setup applies it.
    `aws-elastic-gcs-ctl endpoint network-use [--apply]` shows it and sets it on
    existing endpoints.
- **Packaged operator CLI.** Each `v*` tag publishes `aws-elastic-gcs-ctl`
  through goreleaser (`.goreleaser.yaml`, `.github/workflows/release.yml`):
  archives for Linux, macOS and Windows (amd64 and arm64), `.deb` and `.rpm`
  packages, a Homebrew cask in `scttfrdmn/homebrew-tap` and a Scoop manifest in
  `scttfrdmn/scoop-bucket`. Release notes come from this file. `aws-elastic-gcs-ctl
  version` reports the tag; it used to print a hard-coded `v0.6.0`.

### Changed
- **globus-go-sdk v4** (#68). The project moves to `v4.10.0-3` and v3 is
  dropped.
  - `demand_monitor`, the CLI and the integration tests use the SDK's
    auto-refreshing `ClientCredentialsAuthorizer`.
  - This replaces `demand_monitor`'s hand-rolled token cache and the CLI's
    hand-rolled OAuth request (part of #55).
  - `internal/demand.Lister` accepts an authorizer.
- CDK writes to `.cdk.out/` instead of `cdk.out/`. Go ignores dot-directories,
  so `go list ./...` and `go mod tidy` no longer pick up stale Lambda copies
  from synth output.
- **`stacks/dtn_pool.go` split by concern** (#53): IAM, UserData, ASG and warm
  pool, Lambdas, EventBridge rules, alarms and dashboard each have a file.
  One `newGoLambda` helper replaces eight copies of the Lambda definition,
  `egcsMetric` replaces seven of the metric, and a test keeps DTN UserData
  under 16 KB. The synthesized templates are unchanged. Lambdas still use the
  deprecated `LogRetention`: an explicit log group would collide with the
  `/aws/lambda/...` groups deployed stacks already have.
- **Pure decision logic with unit tests** (#52): `internal/scaling`
  (`DesiredDTNs`, `ScaleUp`, `AnyPositive`), `internal/idle` (`ShouldStop`),
  `internal/eviction` (`OrphanNodes`, `Stale`) and `internal/demand` (paginated
  task listing, tested against an `httptest` server).
- **One SSM command runner** (#51). `internal/ssmexec` replaces seven
  hand-rolled SendCommand/poll loops in the Lambdas, the CLI and the test
  helpers.
  - It honours the context deadline. The command's execution timeout follows the
    wait time.
  - A `Failed` command is now a result, not an error, and each caller decides
    what that means. Before, the copies disagreed: some treated `Failed` as
    success.
  - `internal/gcscli` holds the shared credential preamble, node-list parsing and
    UUID-validated node deletion. Node lists now read every result page, not just
    the first.
  - The waker waits for the GCS Manager with one on-node loop instead of an SSM
    round trip every 5 s.
- **One config resolver** (#49). `config.Resolve` turns a `regions.yaml` entry
  plus CDK context into every derived value: warm pool, max DTNs, total slots,
  TTL, throughput threshold, idle settings, instance types and GCS version. Both
  stacks take only that result, so they can't disagree on slot counts.
  - It validates input: exact GCS version, a real CIDR (and refuses
    `0.0.0.0/0`), non-negative sizes and a numeric Spot price.
  - Integer CDK context values are parsed with errors reported. They used to go
    through `fmt.Sscanf` with errors ignored.
  - With Spot, the warm pool counts as 0 slots.
  - Templates for the existing deployments are unchanged.
- **Shared names** (#50).
  - Tag keys, lifecycle hook names and the metric namespace come from
    `internal/names`. `dtn_boot.sh` receives them through the systemd unit
    environment, with matching defaults.
  - Lambdas read the region only from `AWS_REGION`. The stack-set `REGION`
    variable is gone, and version_check used to read it.
  - Slot locks are released through `internal/slotlock`, which deletes a lock only
    when the instance still owns it. cache_eviction used to delete it
    unconditionally.
  - The CLI's `logs` and status lists include the waker, idle-shutdown and
    throughput-alarm entries.
- **CLI split into one file per command** (#55). `cmd/aws-elastic-gcs-ctl/main.go`
  keeps the root command, flags and shared helpers; each command lives in its
  own file. Commands, flags and output are unchanged.

### Fixed
- `build_ami.sh` no longer publishes an AMI when the TLS policy step fails or
  times out (#54). It also removes the stale, unused
  `scripts/systemd/elastic-gcs-dtn.service` (the stack generates the unit).
- **CLI nil-dereference panics** (#55). `status` no longer panics when
  `DescribeInstances` (warm-pool tags) or `DescribeAlarms` fails, or when EC2
  omits an instance's `State` (also in `deprovision`). `getSSMParam` checks for
  a missing `Parameter`, and the alarm label no longer indexes past the end of
  an unexpected name.
- **Cache eviction actually evicts** (#33).
  - It used to terminate stale warm instances, and the ASG replaced them
    immediately: no saving, just cold-boot churn.
  - Now, when nothing is desired and every warm instance has been stopped longer
    than the TTL, it deletes the warm pool. It still releases slot locks and
    deletes the nodes.
  - The next scale-out (scale-out Lambda or `demand_monitor`) restores the warm
    pool with `internal/warmpool`. That scale-out is a cold launch.
  - A partially stale pool is left alone.
  - Re-importing a deleted node identity was tested live and works.
- **Scaling follows demand** (#26).
  - Scale-in goes to 0 in one step once there have been no active transfers for
    15 minutes. Stepping down by one per alarm left the pool stuck, because the
    alarm fires only on entering ALARM.
  - `demand_monitor` now also scales up every minute, to
    `ceil(throughput / per-DTN threshold)` capped at `max_dtns`. It only ever
    raises the pool. The throughput alarm used to add at most one DTN per
    OK→ALARM transition.
- **`demand_monitor` reads every task** (#28).
  - It pages through `endpoint_manager/task_list` (`limit=1000` with `last_key`)
    instead of counting only the first page.
  - The HTTP client has a 20 s timeout.
  - The endpoint ID, credentials and token are cached across warm invocations.
- **No more wake-ups on suspended tasks** (#29). `INACTIVE` tasks are suspended
  (for example by expired credentials), and one stuck task could cycle capacity
  every 15–20 minutes. A task against an endpoint with no running node stays
  `ACTIVE`, which already scales out DTNs; they serve without the control node.
  `QueuedTransfers` now counts suspended tasks, for observability only.
- **The orphan-node sweep could delete the control node's registration.** While
  the control node is out of the data path, its node is inactive and has no DTN
  instance behind it. The sweep now protects the control node's IP.
- The sweep lists the ASG's instances by tag, with pagination. It used to make
  an account-wide `DescribeAutoScalingInstances` call without pagination.
- Scale-in, idle shutdown and the CLI read every CloudWatch datapoint, not just
  `Datapoints[0]` (CloudWatch doesn't order them).

### Security
- **DTNs can no longer tamper with scaling or other DTNs** (#43).
  - DTNs publish metrics only to a namespace of their own, `ElasticGCS/DTN`
    (boot latency), so they can't forge the demand metrics that drive scaling.
  - `ec2:CreateTags` stays limited to this ASG's instances and the slot and
    node-ID tag keys. IAM rejects `${ec2:SourceInstanceARN}` as a resource, so a
    DTN can't be limited to tagging only itself.
  - Slot and setup locks can be created but not overwritten. DTNs can delete the
    setup lock only; slot locks are released by the Lambdas, which check the
    owner.
- **Lambdas act only on the instance role they expect** (#44).
  - DTNs are tagged `elastic-gcs:role=dtn`, and the control node is
    `elastic-gcs:role=control`.
  - Lambdas that target the control node (waker, idle shutdown, scale-in, cache
    eviction's GCS CLI) require `role=control`. Those that target DTNs (lifecycle,
    eviction terminate) require `role=dtn`. A tampered `control-node-id`
    parameter can't redirect them to another instance.
  - The warm-pool and Spot custom resources are scoped to the deployment's ASG.
  - **Upgrade:** refresh the warm pool after deploying, so existing DTNs get the
    role tag.
- **Verified installs** (#46). The control node and the AMI builder install
  their software through `scripts/lib/verified_install.sh`, which fails closed.
  - The SSM agent comes from the regional bucket, and it and the AWS CLI v2 are
    checked against detached signatures with pinned keys (`scripts/keys/`, each
    fingerprint-checked).
  - EPEL comes from the distribution's own signed `epel-release`.
  - The Globus repo is defined by this project, with the pinned Globus key and
    `gpgcheck`/`repo_gpgcheck` on. It used to come from the unverified
    `globus-repo` RPM's repo file.
  - The `|| true` fallbacks are gone, and the unused Ubuntu path in the control
    node setup is removed.
- **TLS policy enforces the approved list** (#47). The RHEL 9 crypto-policy
  module also removes static RSA key exchange (no forward secrecy) and
  HMAC-SHA1, which DEFAULT still allowed for TLS 1.2 CBC suites. The control
  node now runs the same `configure_tls.sh` as the DTN AMI. That script merges
  GCS's `gsi.conf` instead of overwriting it.
  - **Limitation, measured live:** port 443 is served by the GCS Manager
    VirtualHost, whose explicit `SSLCipherSuite` overrides the system policy.
    CHACHA20 and DHE-GCM are still offered there; static RSA and SHA1 CBC are
    refused. Everything offered is forward-secret AEAD. See #47.
- **Hardening** (#48).
  - SSM Run Command output from the Lambdas goes to a 90-day log group
    (`/elastic-gcs/<deployment>/ssm-commands`), so commands run on instances are
    auditable. Lambda logs are kept for 90 days, up from 7.
  - The DTN launch template sets the IMDS hop limit to 1.
  - The waker no longer returns raw error text to Function URL callers.
  - The CLI's Globus HTTP calls have a 30 s timeout.
  - Benchmark and integration tests read Globus credentials on the node instead
    of putting them in the SSM command text, which SSM keeps in its history.
  - The README stores the client secret from stdin.
  - `go.mod` pins `toolchain go1.27.4` (GO-2026-6859), and `.gitignore` covers
    slot identity files.
- **No instance can read every SSM parameter** (#45).
  `AmazonSSMManagedInstanceCore` is replaced by the same permissions minus
  `ssm:GetParameter(s)` on `*`. The control node, DTNs and AMI builder read only
  their deployment's parameters. So do the Lambdas: their `ssm:GetParameter` is
  scoped to the deployment's parameter prefix too.
- **The AMI builder has its own role and security group** (#45). `build_ami.sh`
  launches the builder with `elastic-gcs-ami-builder-<region>-<deployment>`: an
  SSM-agent-only role with no secret or parameter access, and an egress-only
  security group. It used to borrow the control node's profile and the endpoint's
  internet-facing group.

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

[Unreleased]: https://github.com/scttfrdmn/aws-elastic-gcs/compare/v0.13.0...HEAD
[0.13.0]: https://github.com/scttfrdmn/aws-elastic-gcs/compare/v0.12.0...v0.13.0
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
