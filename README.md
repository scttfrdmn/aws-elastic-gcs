# Elastic Globus Connect Server on AWS

Auto-scaling Globus Connect Server infrastructure that scales from near-zero cost to high-throughput multi-node data transfer, with a primary use case of fronting the AWS Open Data Registry.

## What This Does

- Deploys GCS v5 endpoints backed by S3 (including public Open Data buckets)
- Maintains a tiny control node (~$12/month) when idle
- Scales out additional Data Transfer Nodes in 30-60 seconds when transfers are initiated
- Contracts back automatically when transfers complete
- Supports multi-region deployment to colocate with Open Data

## Quick Start

```bash
# Prerequisites: Go 1.21+, AWS CDK v2, AWS credentials configured, Globus subscription

# Clone and initialize
git clone <this-repo>
cd elastic-gcs
go mod tidy

# Configure (edit these files)
cp config/regions.example.yaml config/regions.yaml
cp config/open_data_buckets.example.yaml config/open_data_buckets.yaml

# Set required secrets
aws secretsmanager create-secret --name elastic-gcs/us-east-1/globus-client-id --secret-string "<your-client-id>"
aws secretsmanager create-secret --name elastic-gcs/us-east-1/globus-client-secret --secret-string "<your-secret>"

# Deploy Phase 1 (single node)
cdk deploy ElasticGCS-Control-us-east-1

# After control node is running, SSH in and create node identities for DTN pool
# Then deploy the DTN pool
cdk deploy ElasticGCS-DTNPool-us-east-1
```

## Architecture

See [CLAUDE.md](CLAUDE.md) for full architecture documentation, GCS CLI reference, and implementation phases.

## Related Projects

- [globus-go-sdk](https://github.com/scttfrdmn/globus-go-sdk) — Go SDK for Globus services
- [aws-globus-connector](https://github.com/scttfrdmn/aws-globus-connector) — High-performance S3 connector for GCS
- [globus-gcs-s3-deployment](https://github.com/scttfrdmn/globus-gcs-s3-deployment) — Shell-based GCS S3 deployment

## License

Apache-2.0
