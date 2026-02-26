.PHONY: build synth deploy-control deploy-dtn deploy-all destroy test test-integration benchmark ami status clean

# === Build ===

build: build-lambdas build-ctl

build-lambdas:
	@echo "Building Lambda functions..."
	GOOS=linux GOARCH=arm64 go build -o lambda/lifecycle/bootstrap ./lambda/lifecycle/
	GOOS=linux GOARCH=arm64 go build -o lambda/scaleout/bootstrap ./lambda/scaleout/
	GOOS=linux GOARCH=arm64 go build -o lambda/scalein/bootstrap ./lambda/scalein/
	GOOS=linux GOARCH=arm64 go build -o lambda/cache_eviction/bootstrap ./lambda/cache_eviction/
	GOOS=linux GOARCH=arm64 go build -o lambda/demand_monitor/bootstrap ./lambda/demand_monitor/
	GOOS=linux GOARCH=arm64 go build -o lambda/version_check/bootstrap ./lambda/version_check/
	GOOS=linux GOARCH=arm64 go build -o lambda/waker/bootstrap ./lambda/waker/
	GOOS=linux GOARCH=arm64 go build -o lambda/idle_shutdown/bootstrap ./lambda/idle_shutdown/

build-ctl:
	@echo "Building CLI tool..."
	go build -o bin/elastic-gcs-ctl ./cmd/elastic-gcs-ctl/

# === CDK ===

synth:
	cdk synth

deploy-control:
	@echo "Deploying control node stack..."
	cdk deploy ElasticGCS-Control-$(REGION) --require-approval broadening

deploy-dtn:
	@echo "Deploying DTN pool stack..."
	cdk deploy ElasticGCS-DTNPool-$(REGION) --require-approval broadening

deploy-all:
	@echo "Deploying all stacks for region $(REGION)..."
	cdk deploy ElasticGCS-Control-$(REGION) ElasticGCS-DTNPool-$(REGION) --require-approval broadening

destroy:
	@echo "WARNING: Destroying all stacks for region $(REGION)..."
	cdk destroy ElasticGCS-DTNPool-$(REGION) ElasticGCS-Control-$(REGION)

# === Testing ===

test:
	go test ./... -v

test-integration:
	go test ./test/integration/ -v -tags=integration -timeout 30m

benchmark:
	@echo "Running scale-out benchmark in $(REGION)..."
	./bin/elastic-gcs-ctl benchmark --region $(REGION) --tier $(TIER)

# === AMI ===

ami:
	@echo "Building DTN AMI..."
	./scripts/build_ami.sh $(REGION)

# === Utility ===

status:
	./bin/elastic-gcs-ctl status --region $(REGION)

clean:
	rm -rf cdk.out/
	rm -f lambda/*/bootstrap
	rm -f bin/*

# === Default region ===
REGION ?= us-east-1
TIER ?= warm
