// Package tracing sets up OpenTelemetry tracing. Spans are exported over
// OTLP/HTTP when OTEL_EXPORTER_OTLP_TRACES_ENDPOINT or
// OTEL_EXPORTER_OTLP_ENDPOINT is set; otherwise the global no-op tracer
// is used and spans cost next to nothing.
package tracing

import (
	"context"
	"fmt"
	"strings"

	"github.com/boson-chat/files/config"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
)

// ScopeName is the instrumentation scope.
const ScopeName = "github.com/boson-chat/files"

// Tracer returns the service tracer (global provider).
func Tracer() trace.Tracer { return otel.Tracer(ScopeName) }

// Init installs an OTLP trace provider when configured. The returned
// shutdown flushes pending spans (no-op when tracing is off).
func Init(ctx context.Context, cfg config.Config) (func(context.Context) error, error) {
	t := cfg.TelemetryConfig
	if strings.EqualFold(strings.TrimSpace(t.TracesExporter), "none") || (t.OTLPEndpoint == "" && t.OTLPTracesEndpoint == "") {
		return func(context.Context) error { return nil }, nil
	}
	var opts []otlptracehttp.Option
	if t.OTLPTracesEndpoint != "" {
		opts = append(opts, otlptracehttp.WithEndpointURL(t.OTLPTracesEndpoint))
	} else {
		opts = append(opts, otlptracehttp.WithEndpointURL(strings.TrimRight(t.OTLPEndpoint, "/")+"/v1/traces"))
	}
	exp, err := otlptracehttp.New(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("tracing: otlp exporter: %w", err)
	}
	res, _ := resource.New(ctx, resource.WithAttributes(
		semconv.ServiceName(cfg.AppConfig.APPName), semconv.ServiceVersion(cfg.AppConfig.Version),
		semconv.DeploymentEnvironmentNameKey.String(cfg.AppConfig.Env),
	), resource.WithFromEnv())
	tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exp), sdktrace.WithResource(res))
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))
	return tp.Shutdown, nil
}
