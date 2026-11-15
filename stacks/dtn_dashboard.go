package stacks

import (
	"github.com/aws/aws-cdk-go/awscdk/v2"
	"github.com/aws/aws-cdk-go/awscdk/v2/awscloudwatch"
	"github.com/aws/aws-cdk-go/awscdk/v2/interfaces/interfacesawscloudwatch"
	"github.com/aws/jsii-runtime-go"
)

// fnStat is the options for a Lambda metric: stat over period, labelled.
func fnStat(period awscdk.Duration, stat, label string) *awscloudwatch.MetricOptions {
	return &awscloudwatch.MetricOptions{
		Period:    period,
		Statistic: jsii.String(stat),
		Label:     jsii.String(label),
	}
}

// asgMetric is an AWS/AutoScaling metric of the DTN ASG, 1-minute Average.
func (p *dtnPool) asgMetric(name, label string) awscloudwatch.Metric {
	return awscloudwatch.NewMetric(&awscloudwatch.MetricProps{
		Namespace:  jsii.String("AWS/AutoScaling"),
		MetricName: jsii.String(name),
		DimensionsMap: &map[string]*string{
			"AutoScalingGroupName": jsii.String(p.asgName),
		},
		Period:    minutes(1),
		Statistic: jsii.String("Average"),
		Label:     jsii.String(label),
	})
}

// dashboard creates the CloudWatch dashboard ElasticGCS-{deployment}.
//
// Three rows × three 8-wide columns = full 24-column CloudWatch layout.
// Row 1: Transfer demand (active transfers, DTN scale, alarm status)
// Row 2: Scaling activity (scale events, lifecycle latency, Lambda errors)
// Row 3: Health (version drift, demand monitor, cache eviction)
func (p *dtnPool) dashboard(fns dtnLambdas, a dtnAlarms) {
	dashboard := awscloudwatch.NewDashboard(p.stack, jsii.String("Dashboard"), &awscloudwatch.DashboardProps{
		DashboardName: jsii.String(p.dep.Dashboard()),
	})
	w, h := jsii.Number(8), jsii.Number(6) // every widget: three 8-wide columns per row
	hour := awscdk.Duration_Hours(jsii.Number(1))
	hourlySum := func(label string) *awscloudwatch.MetricOptions { return fnStat(hour, "Sum", label) }

	// Row 1: Transfer demand
	dashboard.AddWidgets(
		awscloudwatch.NewGraphWidget(&awscloudwatch.GraphWidgetProps{
			Title:  jsii.String("Active Transfers & Throughput"),
			Width:  w,
			Height: h,
			Left:   &[]awscloudwatch.IMetric{p.egcsMetric("ActiveTransfers", minutes(1), "Active Transfers")},
			Right:  &[]awscloudwatch.IMetric{p.egcsMetric("TransferThroughput", minutes(1), "Throughput (bytes/s)")},
		}),
		awscloudwatch.NewGraphWidget(&awscloudwatch.GraphWidgetProps{
			Title:  jsii.String("DTN Scale"),
			Width:  w,
			Height: h,
			Left: &[]awscloudwatch.IMetric{
				p.asgMetric("GroupInServiceInstances", "In Service"),
				p.asgMetric("WarmPoolMinSize", "Warm Pool"),
			},
		}),
		awscloudwatch.NewAlarmStatusWidget(&awscloudwatch.AlarmStatusWidgetProps{
			Title:  jsii.String("Alarm Status"),
			Width:  w,
			Height: h,
			Alarms: &[]interfacesawscloudwatch.IAlarmRef{a.scaleOut, a.throughput, a.scaleIn, a.versionDrift},
		}),
	)

	// Row 2: Scaling activity
	dashboard.AddWidgets(
		awscloudwatch.NewGraphWidget(&awscloudwatch.GraphWidgetProps{
			Title:  jsii.String("Scale Events (24h)"),
			Width:  w,
			Height: h,
			View:   awscloudwatch.GraphWidgetView_BAR,
			Left: &[]awscloudwatch.IMetric{
				fns.scaleOut.MetricInvocations(hourlySum("Scale-Out")),
				fns.scaleIn.MetricInvocations(hourlySum("Scale-In")),
			},
		}),
		awscloudwatch.NewGraphWidget(&awscloudwatch.GraphWidgetProps{
			Title:  jsii.String("Lifecycle Latency P99 (24h)"),
			Width:  w,
			Height: h,
			Left: &[]awscloudwatch.IMetric{
				fns.lifecycle.MetricDuration(fnStat(hour, "p99", "lifecycle p99")),
			},
		}),
		awscloudwatch.NewGraphWidget(&awscloudwatch.GraphWidgetProps{
			Title:  jsii.String("Lambda Errors (24h)"),
			Width:  w,
			Height: h,
			View:   awscloudwatch.GraphWidgetView_BAR,
			Left: &[]awscloudwatch.IMetric{
				fns.lifecycle.MetricErrors(hourlySum("lifecycle")),
				fns.scaleOut.MetricErrors(hourlySum("scaleout")),
				fns.scaleIn.MetricErrors(hourlySum("scalein")),
				fns.cacheEviction.MetricErrors(hourlySum("cache-eviction")),
				fns.demandMonitor.MetricErrors(hourlySum("demand-monitor")),
				fns.versionCheck.MetricErrors(hourlySum("version-check")),
			},
		}),
	)

	// Row 3: Health
	fiveMin := minutes(5)
	dashboard.AddWidgets(
		awscloudwatch.NewSingleValueWidget(&awscloudwatch.SingleValueWidgetProps{
			Title:   jsii.String("GCS Version Drift"),
			Width:   w,
			Height:  h,
			Metrics: &[]awscloudwatch.IMetric{p.egcsMetric("GCSVersionDrift", awscdk.Duration_Hours(jsii.Number(6)), "Drift Count")},
		}),
		awscloudwatch.NewGraphWidget(&awscloudwatch.GraphWidgetProps{
			Title:  jsii.String("Demand Monitor (1h)"),
			Width:  w,
			Height: h,
			Left: &[]awscloudwatch.IMetric{
				fns.demandMonitor.MetricInvocations(fnStat(fiveMin, "Sum", "invocations")),
				fns.demandMonitor.MetricErrors(fnStat(fiveMin, "Sum", "errors")),
			},
		}),
		awscloudwatch.NewGraphWidget(&awscloudwatch.GraphWidgetProps{
			Title:  jsii.String("Cache Eviction Duration (24h)"),
			Width:  w,
			Height: h,
			Left: &[]awscloudwatch.IMetric{
				fns.cacheEviction.MetricDuration(fnStat(hour, "Maximum", "duration")),
			},
		}),
	)
}
