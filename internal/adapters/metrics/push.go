package metrics

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/v2/pkg/gincommon"
)

// pushTimeout bounds one Pushgateway call at the end of a CronJob run.
const pushTimeout = 10 * time.Second

// textFormat is the Prometheus text exposition the Pushgateway accepts.
const textFormat = "text/plain; version=0.0.4; charset=utf-8"

// PushRun pushes a CronJob run's Billing metrics from gincommon's registry
// to the Pushgateway (§13.2) under grouping key job=<cronjob name>. Only
// commercial_billing_* families are pushed (not the runtime or pool
// collectors), and of those carrying a job_name label only this CronJob's
// series. Empty url → nothing pushed.
//
// The exposition is rendered by gincommon.MetricsHandler, the same handler
// the server's /metrics serves, so the push names no Prometheus type
// (BIL-OPS-10); it is a plain HTTP PUT of that text.
//
// PUT replaces the whole group: use it for a run that succeeded (its
// commercial_billing_cron_job_last_success_timestamp_seconds is set). A
// failed run uses PushFailedRun.
func PushRun(pushURL, cronjob string) error { return push(pushURL, cronjob, http.MethodPut) }

// PushFailedRun pushes a failed run with POST: the Pushgateway replaces only
// the families in the body and keeps the group's others, so the previous
// run's last-success timestamp survives and BillingCronJobNotSucceeding
// measures the time since the last success.
func PushFailedRun(pushURL, cronjob string) error { return push(pushURL, cronjob, http.MethodPost) }

func push(pushURL, cronjob, method string) error {
	if pushURL == "" {
		return nil
	}
	body, err := jobExposition(cronjob)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), pushTimeout)
	defer cancel()
	target := strings.TrimRight(pushURL, "/") + "/metrics/job/" + url.PathEscape(cronjob)
	req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("pushgateway request: %w", err)
	}
	req.Header.Set("Content-Type", textFormat)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("pushgateway: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("pushgateway: %s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	return nil
}

// jobExposition renders gincommon's registry in the text format and keeps
// this CronJob's commercial_billing_* series.
func jobExposition(cronjob string) ([]byte, error) {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "/metrics", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", textFormat)
	rec := &bufferWriter{header: http.Header{}, code: http.StatusOK}
	gincommon.MetricsHandler().ServeHTTP(rec, req)
	if rec.code != http.StatusOK {
		return nil, fmt.Errorf("render metrics: status %d", rec.code)
	}
	return filterExposition(rec.body.Bytes(), cronjob), nil
}

// bufferWriter captures the handler's response in memory.
type bufferWriter struct {
	header http.Header
	body   bytes.Buffer
	code   int
}

func (w *bufferWriter) Header() http.Header         { return w.header }
func (w *bufferWriter) Write(b []byte) (int, error) { return w.body.Write(b) }
func (w *bufferWriter) WriteHeader(code int)        { w.code = code }

// filterExposition keeps the families named commercial_billing_* and, in
// them, drops samples labelled job_name="<another job>". A family left
// without samples is dropped with its HELP and TYPE lines.
func filterExposition(text []byte, cronjob string) []byte {
	var out, family bytes.Buffer
	samples := 0
	flush := func() {
		if samples > 0 {
			out.Write(family.Bytes())
		}
		family.Reset()
		samples = 0
	}
	current := ""
	sc := bufio.NewScanner(bytes.NewReader(text))
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		name := ""
		if rest, ok := strings.CutPrefix(line, "# "); ok {
			// "# HELP <name> …" / "# TYPE <name> …"
			if f := strings.Fields(rest); len(f) >= 2 {
				name = f[1]
			}
		} else {
			name = sampleFamily(line)
		}
		if name != current {
			flush()
			current = name
		}
		if !strings.HasPrefix(name, "commercial_billing_") {
			continue
		}
		if !strings.HasPrefix(line, "#") {
			if v, ok := labelValue(line, "job_name"); ok && v != cronjob {
				continue
			}
			samples++
		}
		family.WriteString(line)
		family.WriteByte('\n')
	}
	flush()
	return out.Bytes()
}

// sampleFamily returns the family a sample line belongs to: its metric name
// without the histogram/summary suffixes.
func sampleFamily(line string) string {
	end := strings.IndexAny(line, "{ ")
	if end < 0 {
		return line
	}
	name := line[:end]
	for _, suffix := range []string{"_bucket", "_sum", "_count"} {
		if base, ok := strings.CutSuffix(name, suffix); ok {
			return base
		}
	}
	return name
}

// labelValue returns the value of label in a sample line's label set.
func labelValue(line, label string) (string, bool) {
	i := strings.Index(line, label+`="`)
	if i < 0 {
		return "", false
	}
	rest := line[i+len(label)+2:]
	j := strings.IndexByte(rest, '"')
	if j < 0 {
		return "", false
	}
	return rest[:j], true
}
