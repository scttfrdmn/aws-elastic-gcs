package stacks

import (
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/aws/aws-cdk-go/awscdk/v2/awsec2"
	"github.com/aws/aws-cdk-go/awscdk/v2/awsiam"
	"github.com/aws/aws-cdk-go/awscdk/v2/awss3assets"
	"github.com/aws/jsii-runtime-go"

	"github.com/scttfrdmn/aws-elastic-gcs/internal/names"
)

// DTN UserData
// On first EC2 boot (when entering the warm pool), UserData:
//  1. Downloads dtn_boot.sh, dtn_shutdown.sh and spot_interrupt_monitor.sh
//     from the CDK asset bucket (S3 gateway endpoint; DTN role has read)
//  2. Installs the elastic-gcs-dtn and elastic-gcs-spot-monitor units
//  3. Starts elastic-gcs-dtn
//
// The scripts are S3 assets rather than inline, so UserData stays far below
// the 16 KB EC2 limit however large the scripts grow. Changing a script
// changes its asset key, and therefore the launch template.
//
// The DTN AMI (scripts/build_ami.sh) is mandatory and bakes in GCS, the AWS
// CLI and the SSM agent, so nothing is installed here.
//
// The systemd service runs dtn_boot.sh on every subsequent start (resume),
// which handles first-boot vs warm-resume detection automatically.

const dtnScriptDir = "/opt/elastic-gcs/scripts"

// dtnScriptFiles are the scripts/userdata files shipped to every DTN as S3 assets.
var dtnScriptFiles = []string{"dtn_boot.sh", "dtn_shutdown.sh", "spot_interrupt_monitor.sh"}

// dtnServiceUnit is the elastic-gcs-dtn systemd unit, with region, secrets and
// slot-count env vars baked in. DTN_SLOT_NUMBER is NOT included — the boot
// script claims a slot at runtime via SSM Parameter Store atomic locking
// (PutParameter without --overwrite).
func dtnServiceUnit(region, deployment, secretsPrefix, paramPrefix string, totalSlots int, asgName string) string {
	return fmt.Sprintf(`[Unit]
Description=Elastic GCS DTN Lifecycle Manager
After=network-online.target
Wants=network-online.target

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=%[1]s/dtn_boot.sh
ExecStop=%[1]s/dtn_shutdown.sh
TimeoutStartSec=600
TimeoutStopSec=300
Environment=AWS_REGION=%[2]s
Environment=DEPLOYMENT=%[3]s
Environment=SECRETS_PREFIX=%[4]s
Environment=PARAM_PREFIX=%[5]s
Environment=TOTAL_SLOTS=%[6]d
Environment=ASG_NAME=%[7]s
Environment=LIFECYCLE_HOOK=%[8]s
Environment=METRIC_NAMESPACE=%[9]s

[Install]
WantedBy=multi-user.target
`, dtnScriptDir, region, deployment, secretsPrefix, paramPrefix, totalSlots, asgName, names.HookReady, names.DTNMetricNamespace)
}

// spotMonitorUnit is the spot interruption monitor unit: it polls IMDS every
// 5 seconds for a spot termination/stop notice and disables the GCS node
// before shutdown. Harmless on on-demand instances (IMDS returns 404 for spot
// endpoint). Tied to elastic-gcs-dtn.service lifecycle via BindsTo.
func spotMonitorUnit() string {
	return fmt.Sprintf(`[Unit]
Description=Spot Instance Interruption Monitor for Elastic GCS DTN
After=elastic-gcs-dtn.service
BindsTo=elastic-gcs-dtn.service

[Service]
Type=simple
ExecStart=%s/spot_interrupt_monitor.sh
Restart=on-failure
RestartSec=5s

[Install]
WantedBy=elastic-gcs-dtn.service
`, dtnScriptDir)
}

// dtnUserData builds the DTN UserData and grants role read on the script assets.
func (p *dtnPool) dtnUserData(role awsiam.Role) awsec2.UserData {
	dep, region := p.dep, p.dep.Region
	svcB64 := base64.StdEncoding.EncodeToString([]byte(
		dtnServiceUnit(region, dep.Name, p.secretsPrefix, dep.ParamPrefix(), p.cfg.TotalSlots, p.asgName)))
	spotMonitorSvcB64 := base64.StdEncoding.EncodeToString([]byte(spotMonitorUnit()))

	ud := awsec2.UserData_ForLinux(nil)
	ud.AddCommands(
		jsii.String("set -euo pipefail"),
		jsii.String(`command -v aws >/dev/null || { echo "ERROR: AWS CLI missing from DTN AMI; rebuild it with scripts/build_ami.sh"; exit 1; }`),
		jsii.String("systemctl enable --now amazon-ssm-agent || true"),
	)
	for _, f := range dtnScriptFiles {
		asset := awss3assets.NewAsset(p.stack, jsii.String("DTNScript-"+strings.TrimSuffix(f, ".sh")),
			&awss3assets.AssetProps{Path: jsii.String("scripts/userdata/" + f)})
		asset.GrantRead(role)
		local := ud.AddS3DownloadCommand(&awsec2.S3DownloadOptions{
			Bucket:    asset.Bucket(),
			BucketKey: asset.S3ObjectKey(),
			LocalFile: jsii.String(dtnScriptDir + "/" + f),
			Region:    jsii.String(region),
		})
		ud.AddCommands(jsii.String("chmod 0755 " + *local))
	}
	ud.AddCommands(
		jsii.String(fmt.Sprintf("echo '%s' | base64 -d > /etc/systemd/system/elastic-gcs-dtn.service", svcB64)),
		jsii.String(fmt.Sprintf("echo '%s' | base64 -d > /etc/systemd/system/elastic-gcs-spot-monitor.service", spotMonitorSvcB64)),
		jsii.String("systemctl daemon-reload"),
		jsii.String("systemctl enable elastic-gcs-dtn elastic-gcs-spot-monitor"),
		jsii.String("systemctl start elastic-gcs-dtn"),
	)
	return ud
}
