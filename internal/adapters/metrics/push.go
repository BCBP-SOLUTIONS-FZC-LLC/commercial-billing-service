package metrics

import (
	"slices"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/push"
	dto "github.com/prometheus/client_model/go"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/v2/pkg/gincommon"
)

// PushRun pushes a CronJob run's Billing metrics from gincommon's registry
// to the Pushgateway (§13.2, rev 0.37–0.38). Only commercial_* families are
// pushed (not the runtime or pool collectors), and of those carrying a job
// label only this CronJob's series, under grouping key job=<cronjob name>;
// the series' own job label (the same value) is dropped because the
// Pushgateway reserves it. Empty url → nothing pushed.
func PushRun(url, cronjob string) error {
	if url == "" {
		return nil
	}
	return push.New(url, cronjob).Gatherer(jobFamilies{next: gincommon.MetricsGatherer(), job: cronjob}).Push()
}

type jobFamilies struct {
	next prometheus.Gatherer
	job  string
}

func (j jobFamilies) Gather() ([]*dto.MetricFamily, error) {
	fams, err := j.next.Gather()
	out := fams[:0]
	for _, f := range fams {
		if !strings.HasPrefix(f.GetName(), "commercial_") {
			continue
		}
		// A series labelled with another job is not this run's.
		f.Metric = slices.DeleteFunc(f.Metric, func(m *dto.Metric) bool {
			for _, l := range m.Label {
				if l.GetName() == "job" && l.GetValue() != j.job {
					return true
				}
			}
			return false
		})
		for _, m := range f.Metric {
			m.Label = slices.DeleteFunc(m.Label, func(l *dto.LabelPair) bool { return l.GetName() == "job" })
		}
		if len(f.Metric) > 0 {
			out = append(out, f)
		}
	}
	return out, err
}
