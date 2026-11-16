.PHONY: build synth deploy-control deploy-dtn deploy-all destroy test test-integration benchmark ami status clean

# === Build ===

build: build-lambdas build-ctl

build-lambdas:
	@echo "Building Lambda functions..."
	GOOS=linux GOARCH=arm64 go build -o lambda/demand_monitor/bootstrap ./lambda/demand_monitor/
	GOOS=linux GOARCH=arm64 go build -o lambda/version_check/bootstrap ./lambda/version_check/
	GOOS=linux GOARCH=arm64 go build -o lambda/waker/bootstrap ./lambda/waker/
	GOOS=linux GOARCH=arm64 go build -o lambda/idle_shutdown/bootstrap ./lambda/idle_shutdown/
	GOOS=linux GOARCH=arm64 go build -o lambda/slot_controller/bootstrap ./lambda/slot_controller/

build-ctl:
	@echo "Building CLI tool..."
	go build -o bin/aws-elastic-gcs-ctl ./cmd/aws-elastic-gcs-ctl/

# === CDK ===

synth:
	cdk synth

deploy-control:
	@echo "Deploying control node stack..."
	cdk deploy ElasticGCS-Control-$(REGION)-$(DEPLOYMENT) --require-approval broadening

deploy-dtn:
	@echo "Deploying DTN pool stack..."
	cdk deploy ElasticGCS-DTNPool-$(REGION)-$(DEPLOYMENT) --require-approval broadening

deploy-all:
	@echo "Deploying all stacks for $(REGION)-$(DEPLOYMENT)..."
	cdk deploy ElasticGCS-Control-$(REGION)-$(DEPLOYMENT) ElasticGCS-DTNPool-$(REGION)-$(DEPLOYMENT) --require-approval broadening

destroy:
	@echo "WARNING: Destroying all stacks for $(REGION)-$(DEPLOYMENT)..."
	cdk destroy ElasticGCS-DTNPool-$(REGION)-$(DEPLOYMENT) ElasticGCS-Control-$(REGION)-$(DEPLOYMENT)

# === Testing ===

test:
	go test ./... -v

# TestSlotScaleOutIn drives the slot controller through one scale-out and scale-in (~20min).
test-integration:
	ELASTIC_GCS_REGION=$(REGION) ELASTIC_GCS_DEPLOYMENT=$(DEPLOYMENT) ELASTIC_GCS_ITERATIONS=$(ITERATIONS) \
	  go test ./test/integration/ -v -tags=integration -timeout 90m \
	  $(if $(TESTS),-run $(TESTS),) $(TEST_RUN)

benchmark:
	@echo "Running scale-out benchmark in $(REGION), tier=$(TIER)..."
	ELASTIC_GCS_REGION=$(REGION) ELASTIC_GCS_DEPLOYMENT=$(DEPLOYMENT) ELASTIC_GCS_TIER=$(TIER) ELASTIC_GCS_ITERATIONS=$(ITERATIONS) \
	  go test ./test/benchmark/ -v -tags=benchmark -timeout 45m

# === AMI ===

ami:
	@echo "Building DTN AMI..."
	./scripts/build_ami.sh $(REGION) $(DEPLOYMENT)

# === Utility ===

status:
	./bin/aws-elastic-gcs-ctl status --region $(REGION) --deployment $(DEPLOYMENT)

clean:
	rm -rf .cdk.out/ cdk.out/
	rm -f lambda/*/bootstrap
	rm -f bin/*

# === Defaults ===
REGION ?= us-east-1
DEPLOYMENT ?= default
TIER ?= warm
ITERATIONS ?=
