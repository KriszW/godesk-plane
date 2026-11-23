package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestTelemetryLoggingCorrelationAndPrometheus(t *testing.T) {
	var logBuf bytes.Buffer
	ctx := context.Background()

	p, err := Init(ctx, Options{
		ServiceName:    "godesk-test",
		LogLevel:       "debug",
		LogFormat:      "json",
		LogWriter:      &logBuf,
		MetricsEnabled: true,
	})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer p.Shutdown(ctx)

	// Start a span and emit a structured log with that context -> should contain trace_id & span_id
	spanCtx, span := p.Tracer.Start(ctx, "test.operation")
	p.Logger.InfoContext(spanCtx, "hello telemetry", "peer_id", "123456789")
	p.Metrics.RendezvousUDPPackets.Add(spanCtx, 3)
	p.Metrics.RelayBytesTransferred.Add(spanCtx, 4096)
	span.End()

	if err := p.Metrics.RegisterPeerGauges(func() int { return 2 }, func() int { return 5 }); err != nil {
		t.Fatalf("RegisterPeerGauges: %v", err)
	}

	// Verify JSON log contains trace_id, span_id, peer_id
	var logLine map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(logBuf.Bytes()), &logLine); err != nil {
		t.Fatalf("parse json log (%q): %v", logBuf.String(), err)
	}
	if logLine["trace_id"] == "" || logLine["span_id"] == "" || logLine["peer_id"] != "123456789" {
		t.Fatalf("expected trace correlation in slog JSON, got %+v", logLine)
	}

	// Verify Prometheus /metrics exposition includes custom GoDesk metrics
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	p.MetricsHandler.ServeHTTP(rec, req)
	body := rec.Body.String()
	for _, want := range []string{
		"godesk_rendezvous_udp_packets_total",
		"godesk_relay_bytes_total",
		"godesk_peers_online{",
		"} 2\n",
		"godesk_peers_registered{",
		"} 5\n",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("Prometheus /metrics missing %q:\n%s", want, body)
		}
	}
}

func TestTelemetryOTLPHTTPExport(t *testing.T) {
	var traceHits, metricHits, logHits atomic.Int32
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/traces":
			traceHits.Add(1)
		case "/v1/metrics":
			metricHits.Add(1)
		case "/v1/logs":
			logHits.Add(1)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer collector.Close()

	var logBuf bytes.Buffer
	ctx := context.Background()
	p, err := Init(ctx, Options{
		ServiceName:    "godesk-otlp-test",
		LogLevel:       "info",
		LogFormat:      "json",
		LogWriter:      &logBuf,
		OTelEnabled:    true,
		OTelEndpoint:   collector.URL,
		OTelProtocol:   "http/protobuf",
		OTelInsecure:   true,
		MetricsEnabled: true,
	})
	if err != nil {
		t.Fatalf("Init OTLP: %v", err)
	}

	spanCtx, span := p.Tracer.Start(ctx, "otlp.test.span")
	p.Logger.InfoContext(spanCtx, "exporting log via otlp", "component", "test")
	p.Metrics.RelaySessionsTotal.Add(spanCtx, 1)
	span.End()

	if err := p.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	if traceHits.Load() < 1 || metricHits.Load() < 1 || logHits.Load() < 1 {
		t.Fatalf("expected OTLP HTTP exports for traces/metrics/logs, got traces=%d metrics=%d logs=%d",
			traceHits.Load(), metricHits.Load(), logHits.Load())
	}
}
