package telemetry

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/log/global"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
)

const InstrumentationName = "github.com/KriszW/godesk-plane"

// Options controls structured logging, OpenTelemetry OTLP exporters, and Prometheus metrics.
type Options struct {
	ServiceName    string
	ServiceVersion string
	LogLevel       string // debug, info, warn, error
	LogFormat      string // text, json
	LogWriter      io.Writer
	OTelEnabled    bool
	OTelEndpoint   string
	OTelProtocol   string // http/protobuf or grpc
	OTelInsecure   bool
	MetricsEnabled bool
}

// Provider manages OTel Trace, Metric, and Log providers, structured slog logger, and Prometheus handler.
type Provider struct {
	Logger         *slog.Logger
	Tracer         trace.Tracer
	Meter          metric.Meter
	Metrics        *Metrics
	MetricsHandler http.Handler

	tracerProvider *sdktrace.TracerProvider
	meterProvider  *sdkmetric.MeterProvider
	loggerProvider *sdklog.LoggerProvider
	shutdownFns    []func(context.Context) error
}

// Init initializes structured logging, OpenTelemetry (traces, metrics, logs), and Prometheus metrics.
func Init(ctx context.Context, opts Options) (*Provider, error) {
	if opts.ServiceName == "" {
		opts.ServiceName = "godesk-plane"
	}
	if opts.ServiceVersion == "" {
		opts.ServiceVersion = "1.0.0"
	}
	if opts.LogWriter == nil {
		opts.LogWriter = os.Stdout
	}

	level := parseLogLevel(opts.LogLevel)
	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceName(opts.ServiceName),
			semconv.ServiceVersion(opts.ServiceVersion),
		),
		resource.WithFromEnv(),
		resource.WithProcess(),
		resource.WithHost(),
	)
	if err != nil {
		res = resource.Default()
	}

	p := &Provider{}

	// 1. Propagators
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	// 2. Metrics: Prometheus reader + optional OTLP metric exporter
	var metricOpts []sdkmetric.Option
	metricOpts = append(metricOpts, sdkmetric.WithResource(res))

	if opts.MetricsEnabled {
		reg := prometheus.NewRegistry()
		promExp, err := otelprom.New(otelprom.WithRegisterer(reg))
		if err != nil {
			return nil, err
		}
		metricOpts = append(metricOpts, sdkmetric.WithReader(promExp))
		p.MetricsHandler = promhttp.HandlerFor(reg, promhttp.HandlerOpts{})
	}

	if opts.OTelEnabled && opts.OTelEndpoint != "" {
		// Traces exporter
		traceExp, err := buildTraceExporter(ctx, opts)
		if err != nil {
			return nil, err
		}
		tp := sdktrace.NewTracerProvider(
			sdktrace.WithResource(res),
			sdktrace.WithBatcher(traceExp),
		)
		otel.SetTracerProvider(tp)
		p.tracerProvider = tp
		p.shutdownFns = append(p.shutdownFns, tp.Shutdown)

		// Metrics exporter
		metricExp, err := buildMetricExporter(ctx, opts)
		if err != nil {
			return nil, err
		}
		metricOpts = append(metricOpts, sdkmetric.WithReader(
			sdkmetric.NewPeriodicReader(metricExp, sdkmetric.WithInterval(15*time.Second)),
		))

		// Logs exporter
		logExp, err := buildLogExporter(ctx, opts)
		if err != nil {
			return nil, err
		}
		lp := sdklog.NewLoggerProvider(
			sdklog.WithResource(res),
			sdklog.WithProcessor(sdklog.NewBatchProcessor(logExp)),
		)
		global.SetLoggerProvider(lp)
		p.loggerProvider = lp
		p.shutdownFns = append(p.shutdownFns, lp.Shutdown)
	} else {
		// Still install a local TracerProvider so spans carry valid trace_id / span_id in structured logs
		tp := sdktrace.NewTracerProvider(sdktrace.WithResource(res))
		otel.SetTracerProvider(tp)
		p.tracerProvider = tp
		p.shutdownFns = append(p.shutdownFns, tp.Shutdown)
	}

	mp := sdkmetric.NewMeterProvider(metricOpts...)
	otel.SetMeterProvider(mp)
	p.meterProvider = mp
	p.shutdownFns = append(p.shutdownFns, mp.Shutdown)

	// 3. Structured slog handler (stdout with trace_id/span_id injection + optional OTLP log bridge)
	hOpts := &slog.HandlerOptions{Level: level}
	var stdoutHandler slog.Handler
	if strings.EqualFold(opts.LogFormat, "json") {
		stdoutHandler = slog.NewJSONHandler(opts.LogWriter, hOpts)
	} else {
		stdoutHandler = slog.NewTextHandler(opts.LogWriter, hOpts)
	}
	handlers := []slog.Handler{&traceCorrelatingHandler{next: stdoutHandler}}
	if p.loggerProvider != nil {
		otelHandler := otelslog.NewHandler(opts.ServiceName, otelslog.WithLoggerProvider(p.loggerProvider))
		handlers = append(handlers, &levelFilteredHandler{level: level, next: otelHandler})
	}

	p.Logger = slog.New(&multiHandler{handlers: handlers})
	slog.SetDefault(p.Logger)

	p.Tracer = otel.Tracer(InstrumentationName)
	p.Meter = otel.Meter(InstrumentationName)

	metrics, err := newMetrics(p.Meter)
	if err != nil {
		return nil, err
	}
	p.Metrics = metrics

	return p, nil
}

// Shutdown flushes and closes all OpenTelemetry exporters cleanly.
func (p *Provider) Shutdown(ctx context.Context) error {
	var errs []error
	for _, fn := range p.shutdownFns {
		if err := fn(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func parseLogLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

func normalizeOTLPEndpoint(raw string) (hostPort string, isHTTPS bool) {
	trimmed := strings.TrimSpace(raw)
	if strings.HasPrefix(trimmed, "http://") || strings.HasPrefix(trimmed, "https://") {
		if u, err := url.Parse(trimmed); err == nil && u.Host != "" {
			return u.Host, u.Scheme == "https"
		}
	}
	return trimmed, false
}

func isGRPCProtocol(proto string) bool {
	return strings.EqualFold(strings.TrimSpace(proto), "grpc")
}

func buildTraceExporter(ctx context.Context, opts Options) (sdktrace.SpanExporter, error) {
	ep, isHTTPS := normalizeOTLPEndpoint(opts.OTelEndpoint)
	insecure := opts.OTelInsecure && !isHTTPS
	if isGRPCProtocol(opts.OTelProtocol) {
		gOpts := []otlptracegrpc.Option{otlptracegrpc.WithEndpoint(ep)}
		if insecure {
			gOpts = append(gOpts, otlptracegrpc.WithInsecure())
		}
		return otlptracegrpc.New(ctx, gOpts...)
	}
	hOpts := []otlptracehttp.Option{otlptracehttp.WithEndpoint(ep)}
	if insecure {
		hOpts = append(hOpts, otlptracehttp.WithInsecure())
	}
	return otlptracehttp.New(ctx, hOpts...)
}

func buildMetricExporter(ctx context.Context, opts Options) (sdkmetric.Exporter, error) {
	ep, isHTTPS := normalizeOTLPEndpoint(opts.OTelEndpoint)
	insecure := opts.OTelInsecure && !isHTTPS
	if isGRPCProtocol(opts.OTelProtocol) {
		gOpts := []otlpmetricgrpc.Option{otlpmetricgrpc.WithEndpoint(ep)}
		if insecure {
			gOpts = append(gOpts, otlpmetricgrpc.WithInsecure())
		}
		return otlpmetricgrpc.New(ctx, gOpts...)
	}
	hOpts := []otlpmetrichttp.Option{otlpmetrichttp.WithEndpoint(ep)}
	if insecure {
		hOpts = append(hOpts, otlpmetrichttp.WithInsecure())
	}
	return otlpmetrichttp.New(ctx, hOpts...)
}

func buildLogExporter(ctx context.Context, opts Options) (sdklog.Exporter, error) {
	ep, isHTTPS := normalizeOTLPEndpoint(opts.OTelEndpoint)
	insecure := opts.OTelInsecure && !isHTTPS
	if isGRPCProtocol(opts.OTelProtocol) {
		gOpts := []otlploggrpc.Option{otlploggrpc.WithEndpoint(ep)}
		if insecure {
			gOpts = append(gOpts, otlploggrpc.WithInsecure())
		}
		return otlploggrpc.New(ctx, gOpts...)
	}
	hOpts := []otlploghttp.Option{otlploghttp.WithEndpoint(ep)}
	if insecure {
		hOpts = append(hOpts, otlploghttp.WithInsecure())
	}
	return otlploghttp.New(ctx, hOpts...)
}

// traceCorrelatingHandler enriches stdout slog records with trace_id and span_id from ctx.
type traceCorrelatingHandler struct {
	next slog.Handler
}

func (h *traceCorrelatingHandler) Enabled(ctx context.Context, lvl slog.Level) bool {
	return h.next.Enabled(ctx, lvl)
}

func (h *traceCorrelatingHandler) Handle(ctx context.Context, r slog.Record) error {
	sc := trace.SpanContextFromContext(ctx)
	if sc.IsValid() {
		r = r.Clone()
		r.AddAttrs(
			slog.String("trace_id", sc.TraceID().String()),
			slog.String("span_id", sc.SpanID().String()),
		)
	}
	return h.next.Handle(ctx, r)
}

func (h *traceCorrelatingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &traceCorrelatingHandler{next: h.next.WithAttrs(attrs)}
}

func (h *traceCorrelatingHandler) WithGroup(name string) slog.Handler {
	return &traceCorrelatingHandler{next: h.next.WithGroup(name)}
}

type levelFilteredHandler struct {
	level slog.Level
	next  slog.Handler
}

func (h *levelFilteredHandler) Enabled(ctx context.Context, lvl slog.Level) bool {
	return lvl >= h.level && h.next.Enabled(ctx, lvl)
}

func (h *levelFilteredHandler) Handle(ctx context.Context, r slog.Record) error {
	if r.Level < h.level {
		return nil
	}
	return h.next.Handle(ctx, r)
}

func (h *levelFilteredHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &levelFilteredHandler{level: h.level, next: h.next.WithAttrs(attrs)}
}

func (h *levelFilteredHandler) WithGroup(name string) slog.Handler {
	return &levelFilteredHandler{level: h.level, next: h.next.WithGroup(name)}
}

type multiHandler struct {
	handlers []slog.Handler
}

func (m *multiHandler) Enabled(ctx context.Context, lvl slog.Level) bool {
	for _, h := range m.handlers {
		if h.Enabled(ctx, lvl) {
			return true
		}
	}
	return false
}

func (m *multiHandler) Handle(ctx context.Context, r slog.Record) error {
	var firstErr error
	for _, h := range m.handlers {
		if h.Enabled(ctx, r.Level) {
			if err := h.Handle(ctx, r.Clone()); err != nil && firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

func (m *multiHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	next := make([]slog.Handler, len(m.handlers))
	for i, h := range m.handlers {
		next[i] = h.WithAttrs(attrs)
	}
	return &multiHandler{handlers: next}
}

func (m *multiHandler) WithGroup(name string) slog.Handler {
	next := make([]slog.Handler, len(m.handlers))
	for i, h := range m.handlers {
		next[i] = h.WithGroup(name)
	}
	return &multiHandler{handlers: next}
}
