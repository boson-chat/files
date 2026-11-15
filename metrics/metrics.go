// Package metrics defines the service's OpenTelemetry instruments and the
// meter provider that exports them (Prometheus on the internal listener's
// /metrics, optional OTLP push).
package metrics

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
)

// ScopeName is the instrumentation scope of every instrument.
const ScopeName = "github.com/boson-chat/files"

// Database results.
const (
	Success = "success"
	Error   = "error"
)

// Metrics holds the instruments. Use New or Noop.
type Metrics struct {
	uploads       metric.Int64Counter
	uploadBytes   metric.Int64Counter
	downloads     metric.Int64Counter
	expired       metric.Int64Counter
	guestTokens   metric.Int64Counter
	workerRuns    metric.Int64Counter
	workerSeconds metric.Float64Histogram
	dbSeconds     metric.Float64Histogram
}

// New creates the instruments on mp.
func New(mp metric.MeterProvider) (*Metrics, error) {
	m := mp.Meter(ScopeName)
	var err error
	c := func(name, desc, unit string) metric.Int64Counter {
		if err != nil {
			return nil
		}
		var ctr metric.Int64Counter
		ctr, err = m.Int64Counter(name, metric.WithDescription(desc), metric.WithUnit(unit))
		return ctr
	}
	h := func(name, desc string) metric.Float64Histogram {
		if err != nil {
			return nil
		}
		var hist metric.Float64Histogram
		hist, err = m.Float64Histogram(name, metric.WithDescription(desc), metric.WithUnit("s"))
		return hist
	}
	out := &Metrics{
		uploads:       c("files.uploads", "Uploads by principal kind and outcome (ok | quota_exceeded | file_too_large | storage_full | failed).", "{upload}"),
		uploadBytes:   c("files.upload.bytes", "Bytes stored by accepted uploads, by principal kind.", "By"),
		downloads:     c("files.downloads", "File responses by outcome (ok | gone | not_found).", "{request}"),
		expired:       c("files.expired", "Files removed from storage by the expiry worker.", "{file}"),
		guestTokens:   c("files.guest_tokens", "Guest tokens by outcome (issued | rate_limited | disabled).", "{token}"),
		workerRuns:    c("files.worker.runs", "Background worker runs by worker and outcome (ok | error | skipped).", "{run}"),
		workerSeconds: h("files.worker.duration", "Worker run time."),
		dbSeconds:     h("files.db.duration", "Repository query time by table, operation and result."),
	}
	if err != nil {
		return nil, fmt.Errorf("metrics: %w", err)
	}
	return out, nil
}

// Noop returns instruments that record nothing.
func Noop() *Metrics {
	m, _ := New(noop.NewMeterProvider())
	return m
}

var global = Noop()

// SetGlobal makes m the instruments GetAppMetrics returns (repositories).
func SetGlobal(m *Metrics) {
	if m != nil {
		global = m
	}
}

// GetAppMetrics returns the process-wide instruments.
func GetAppMetrics() *Metrics { return global }

func (m *Metrics) Upload(ctx context.Context, kind, outcome string, bytes int64) {
	m.uploads.Add(ctx, 1, metric.WithAttributes(attribute.String("kind", kind), attribute.String("outcome", outcome)))
	if outcome == "ok" {
		m.uploadBytes.Add(ctx, bytes, metric.WithAttributes(attribute.String("kind", kind)))
	}
}

func (m *Metrics) Download(ctx context.Context, outcome string) {
	m.downloads.Add(ctx, 1, metric.WithAttributes(attribute.String("outcome", outcome)))
}

func (m *Metrics) Expired(ctx context.Context, n int) {
	m.expired.Add(ctx, int64(n))
}

func (m *Metrics) GuestToken(ctx context.Context, outcome string) {
	m.guestTokens.Add(ctx, 1, metric.WithAttributes(attribute.String("outcome", outcome)))
}

func (m *Metrics) WorkerRun(ctx context.Context, worker, outcome string, seconds float64) {
	m.workerRuns.Add(ctx, 1, metric.WithAttributes(attribute.String("worker", worker), attribute.String("outcome", outcome)))
	if outcome != "skipped" {
		m.workerSeconds.Record(ctx, seconds, metric.WithAttributes(attribute.String("worker", worker)))
	}
}

// DatabaseMetric records one repository query.
func (m *Metrics) DatabaseMetric(ctx context.Context, seconds float64, table, op, result string) {
	m.dbSeconds.Record(ctx, seconds, metric.WithAttributes(
		attribute.String("table", table), attribute.String("op", op), attribute.String("result", result)))
}
