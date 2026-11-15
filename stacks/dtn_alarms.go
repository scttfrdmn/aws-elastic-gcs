package stacks

import (
	"fmt"

	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/aws-cdk-go/awscdk/v2/awscloudwatch"
	"github.com/aws/aws-cdk-go/awscdk/v2/awscloudwatchactions"
	"github.com/aws/aws-cdk-go/awscdk/v2/awssns"
	"github.com/aws/aws-cdk-go/awscdk/v2/awssnssubscriptions"
	"github.com/aws/jsii-runtime-go"

	"github.com/scttfrdmn/aws-elastic-gcs/internal/names"
)

// dtnAlarms are the alarms of one DTN pool.
type dtnAlarms struct {
	scaleOut, scaleIn, throughput, versionDrift awscloudwatch.Alarm
	scaleOutName                                string
}

// egcsMetric is a deployment's custom metric (namespace names.MetricNamespace,
// dimensions Region and Deployment), Maximum over period. label may be empty.
func (p *dtnPool) egcsMetric(name string, period awscdk.Duration, label string) awscloudwatch.Metric {
	props := &awscloudwatch.MetricProps{
		Namespace:  jsii.String(names.MetricNamespace),
		MetricName: jsii.String(name),
		DimensionsMap: &map[string]*string{
			"Region":     jsii.String(p.dep.Region),
			"Deployment": jsii.String(p.dep.Name),
		},
		Period:    period,
		Statistic: jsii.String("Maximum"),
	}
	if label != "" {
		props.Label = jsii.String(label)
	}
	return awscloudwatch.NewMetric(props)
}

// alarms creates the scaling and operator SNS topics and the CloudWatch alarms.
//
// Scale-out uses two alarms in sequence:
//  1. ActiveTransfers alarm  — fires when ANY transfer is active (0→1 DTN)
//  2. TransferThroughput alarm — fires when endpoint is saturating (1→2 DTN, etc.)
//
// Both route to the same scaleout Lambda via the same SNS topic; the Lambda
// increments DesiredCapacity by 1 per invocation and caps at MAX_DTNS.
//
// Scale-in uses ActiveTransfers == 0 for 15 minutes → scalein Lambda.
//
// TreatMissingData=NOT_BREACHING on all alarms: if demand_monitor is
// unavailable (e.g. during a deploy), don't trigger false scale events.
func (p *dtnPool) alarms(fns dtnLambdas) dtnAlarms {
	stack, dep := p.stack, p.dep
	throughputThreshold := p.cfg.ThroughputThresholdBps

	scaleOutTopic := awssns.NewTopic(stack, jsii.String("ScaleOutTopic"), &awssns.TopicProps{
		TopicName:   jsii.String(dep.Resource("scaleout")),
		DisplayName: jsii.String("Elastic GCS Scale-Out Trigger"),
	})
	scaleOutTopic.AddSubscription(awssnssubscriptions.NewLambdaSubscription(fns.scaleOut, nil))

	scaleInTopic := awssns.NewTopic(stack, jsii.String("ScaleInTopic"), &awssns.TopicProps{
		TopicName:   jsii.String(dep.Resource("scalein")),
		DisplayName: jsii.String("Elastic GCS Scale-In Trigger"),
	})
	scaleInTopic.AddSubscription(awssnssubscriptions.NewLambdaSubscription(fns.scaleIn, nil))

	var a dtnAlarms

	// Scale-out alarm: fires when ElasticGCS/ActiveTransfers > 0 for 1 minute.
	a.scaleOutName = dep.Resource("scaleout")
	a.scaleOut = awscloudwatch.NewAlarm(stack, jsii.String("ScaleOutAlarm"), &awscloudwatch.AlarmProps{
		AlarmName:          jsii.String(a.scaleOutName),
		AlarmDescription:   jsii.String("Trigger DTN scale-out when active Globus transfers detected"),
		Metric:             p.egcsMetric("ActiveTransfers", minutes(1), ""),
		Threshold:          jsii.Number(0),
		ComparisonOperator: awscloudwatch.ComparisonOperator_GREATER_THAN_THRESHOLD,
		EvaluationPeriods:  jsii.Number(1),
		TreatMissingData:   awscloudwatch.TreatMissingData_NOT_BREACHING,
	})
	a.scaleOut.AddAlarmAction(awscloudwatchactions.NewSnsAction(scaleOutTopic))

	// Scale-in alarm: fires when ElasticGCS/ActiveTransfers == 0 for 15 min.
	// Three consecutive 5-minute periods with Maximum <= 0 means no active
	// transfers for at least 15 minutes.
	a.scaleIn = awscloudwatch.NewAlarm(stack, jsii.String("ScaleInAlarm"), &awscloudwatch.AlarmProps{
		AlarmName: jsii.String(dep.Resource("scalein")),
		AlarmDescription: jsii.String(
			"Trigger DTN scale-in when no active Globus transfers for 15 minutes"),
		Metric:             p.egcsMetric("ActiveTransfers", minutes(5), ""),
		Threshold:          jsii.Number(0),
		ComparisonOperator: awscloudwatch.ComparisonOperator_LESS_THAN_OR_EQUAL_TO_THRESHOLD,
		EvaluationPeriods:  jsii.Number(3),                               // 3 × 5min = 15 min with no active transfers
		TreatMissingData:   awscloudwatch.TreatMissingData_NOT_BREACHING, // demand monitor down = don't scale in
	})
	a.scaleIn.AddAlarmAction(awscloudwatchactions.NewSnsAction(scaleInTopic))

	// Throughput scale-out alarm: fires when TransferThroughput (sum of
	// effective_bytes_per_second across all ACTIVE tasks) exceeds the threshold
	// for 2 consecutive minutes. This drives 1→2 DTN and beyond. The 2-period
	// requirement (vs 1 for the first alarm) avoids spinning up a second DTN for
	// brief throughput spikes during task start/stop transitions.
	a.throughput = awscloudwatch.NewAlarm(stack, jsii.String("ThroughputScaleOutAlarm"), &awscloudwatch.AlarmProps{
		AlarmName: jsii.String(dep.Resource("scaleout-throughput")),
		AlarmDescription: jsii.String(fmt.Sprintf(
			"Trigger additional DTN when transfer throughput exceeds %d MB/s for 2 minutes",
			throughputThreshold/1_000_000)),
		Metric:             p.egcsMetric("TransferThroughput", minutes(1), ""),
		Threshold:          jsii.Number(float64(throughputThreshold)),
		ComparisonOperator: awscloudwatch.ComparisonOperator_GREATER_THAN_THRESHOLD,
		EvaluationPeriods:  jsii.Number(2),
		TreatMissingData:   awscloudwatch.TreatMissingData_NOT_BREACHING,
	})
	a.throughput.AddAlarmAction(awscloudwatchactions.NewSnsAction(scaleOutTopic))

	// Operator notifications (#27): health alarms go here, never to the
	// scale-out topic. Subscribe an email with -c elastic-gcs:operatorEmail=...
	operatorTopic := awssns.NewTopic(stack, jsii.String("OperatorTopic"), &awssns.TopicProps{
		TopicName:   jsii.String(dep.Resource("operator")),
		DisplayName: jsii.String("Elastic GCS Operator Alerts"),
	})
	if p.props.OperatorEmail != "" {
		operatorTopic.AddSubscription(awssnssubscriptions.NewEmailSubscription(jsii.String(p.props.OperatorEmail), nil))
	}
	awscdk.NewCfnOutput(stack, jsii.String("OperatorTopicArn"), &awscdk.CfnOutputProps{
		Value:       operatorTopic.TopicArn(),
		Description: jsii.String("SNS topic for operator alerts (version drift)"),
	})

	// GCS Version Drift alarm: fires when GCSVersionDrift > 0 (any drift detected).
	a.versionDrift = awscloudwatch.NewAlarm(stack, jsii.String("VersionDriftAlarm"), &awscloudwatch.AlarmProps{
		AlarmName:          jsii.String(dep.Resource("version-drift")),
		AlarmDescription:   jsii.String("GCS version drift detected — nodes running different GCS versions"),
		Metric:             p.egcsMetric("GCSVersionDrift", awscdk.Duration_Hours(jsii.Number(6)), ""),
		Threshold:          jsii.Number(0),
		ComparisonOperator: awscloudwatch.ComparisonOperator_GREATER_THAN_THRESHOLD,
		EvaluationPeriods:  jsii.Number(1),
		TreatMissingData:   awscloudwatch.TreatMissingData_NOT_BREACHING,
	})
	a.versionDrift.AddAlarmAction(awscloudwatchactions.NewSnsAction(operatorTopic))

	return a
}
