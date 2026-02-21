# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

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

[Unreleased]: https://github.com/scttfrdmn/elastic-gcs/compare/v0.2.0...HEAD
[0.2.0]: https://github.com/scttfrdmn/elastic-gcs/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/scttfrdmn/elastic-gcs/releases/tag/v0.1.0
