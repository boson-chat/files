package metrics

import (
	"context"
	"errors"
	"fmt"
	nethttp "net/http"
	"strings"

	"github.com/boson-chat/files/config"
	"github.com/boson-chat/files/internal/logger"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/contrib/instrumentation/runtime"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/exemplar"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
)

// Provider owns the OTel meter provider and its exporters.
type Provider struct {
	mp      *sdkmetric.MeterProvider
	handler nethttp.Handler
	otlp    string
}

// NewProvider builds the meter provider: a Prometheus reader served by
// Handler, an OTLP periodic reader when configured, and Go runtime metrics.
func NewProvider(ctx context.Context, cfg config.Config) (*Provider, error) {
	log := logger.FromCtx(ctx)
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		log.Warn().Err(err).Msg("otel")
	}))

	res, err := newResource(ctx, cfg.AppConfig)
	if err != nil {
		return nil, err
	}
	reg := prometheus.NewRegistry()
	promReader, err := otelprom.New(otelprom.WithRegisterer(reg), otelprom.WithoutScopeInfo())
	if err != nil {
		return nil, fmt.Errorf("metrics: prometheus exporter: %w", err)
	}
	opts := []sdkmetric.Option{
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(promReader),
		sdkmetric.WithExemplarFilter(exemplar.AlwaysOffFilter),
	}
	p := &Provider{handler: promhttp.HandlerFor(reg, promhttp.HandlerOpts{})}
	if proto, ok := otlpProtocol(cfg.TelemetryConfig); ok {
		exp, err := newOTLPExporter(ctx, cfg.TelemetryConfig, proto)
		if err != nil {
			return nil, err
		}
		opts = append(opts, sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exp)))
		p.otlp = proto
	}
	p.mp = sdkmetric.NewMeterProvider(opts...)
	if err := runtime.Start(runtime.WithMeterProvider(p.mp)); err != nil {
		_ = p.mp.Shutdown(ctx)
		return nil, fmt.Errorf("metrics: runtime: %w", err)
	}
	return p, nil
}

func (p *Provider) MeterProvider() metric.MeterProvider { return p.mp }

// Handler serves the Prometheus exposition for GET /metrics.
func (p *Provider) Handler() nethttp.Handler { return p.handler }

// OTLPProtocol is the OTLP protocol in use, or "" when push is off.
func (p *Provider) OTLPProtocol() string { return p.otlp }

// Shutdown flushes pending OTLP exports and stops all readers.
func (p *Provider) Shutdown(ctx context.Context) error { return p.mp.Shutdown(ctx) }

func newResource(ctx context.Context, app config.AppConfig) (*resource.Resource, error) {
	res, err := resource.New(ctx,
		resource.WithSchemaURL(semconv.SchemaURL),
		resource.WithAttributes(
			semconv.ServiceName(app.APPName),
			semconv.ServiceVersion(app.Version),
			semconv.DeploymentEnvironmentNameKey.String(app.Env),
			attribute.String("deployment.environment", app.Env),
		),
		resource.WithHost(),
		resource.WithTelemetrySDK(),
		resource.WithFromEnv(),
	)
	if err != nil && !errors.Is(err, resource.ErrPartialResource) {
		return nil, fmt.Errorf("metrics: resource: %w", err)
	}
	return res, nil
}

func otlpProtocol(c config.TelemetryConfig) (string, bool) {
	if c.OTLPEndpoint == "" && c.OTLPMetricsEndpoint == "" {
		return "", false
	}
	if strings.EqualFold(strings.TrimSpace(c.MetricsExporter), "none") {
		return "", false
	}
	proto := c.OTLPMetricsProtocol
	if proto == "" {
		proto = c.OTLPProtocol
	}
	if proto == "" {
		proto = "http/protobuf"
	}
	return proto, true
}

func newOTLPExporter(ctx context.Context, c config.TelemetryConfig, proto string) (sdkmetric.Exporter, error) {
	switch proto {
	case "http/protobuf":
		var opts []otlpmetrichttp.Option
		if c.OTLPMetricsEndpoint != "" {
			opts = append(opts, otlpmetrichttp.WithEndpointURL(c.OTLPMetricsEndpoint))
		} else {
			opts = append(opts, otlpmetrichttp.WithEndpointURL(strings.TrimRight(c.OTLPEndpoint, "/")+"/v1/metrics"))
		}
		exp, err := otlpmetrichttp.New(ctx, opts...)
		if err != nil {
			return nil, fmt.Errorf("metrics: otlp http exporter: %w", err)
		}
		return exp, nil
	case "grpc":
		ep := c.OTLPMetricsEndpoint
		if ep == "" {
			ep = c.OTLPEndpoint
		}
		exp, err := otlpmetricgrpc.New(ctx, otlpmetricgrpc.WithEndpointURL(ep))
		if err != nil {
			return nil, fmt.Errorf("metrics: otlp grpc exporter: %w", err)
		}
		return exp, nil
	default:
		return nil, fmt.Errorf("metrics: unsupported OTLP protocol %q (use http/protobuf or grpc)", proto)
	}
}
