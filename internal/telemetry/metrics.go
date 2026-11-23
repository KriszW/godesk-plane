package telemetry

import (
	"context"

	"go.opentelemetry.io/otel/metric"
)

// Metrics holds all GoDesk Plane OpenTelemetry metric instruments.
type Metrics struct {
	// Rendezvous
	RendezvousUDPPackets    metric.Int64Counter
	RendezvousTCPMessages   metric.Int64Counter
	RendezvousRegistrations metric.Int64Counter
	RendezvousPunchHoles    metric.Int64Counter

	// Relay
	RelayActiveSessions   metric.Int64UpDownCounter
	RelayWaitingSessions  metric.Int64UpDownCounter
	RelaySessionsTotal    metric.Int64Counter
	RelayAuthFailures     metric.Int64Counter
	RelayWaiterTimeouts   metric.Int64Counter
	RelayBytesTransferred metric.Int64Counter
	RelaySessionDuration  metric.Float64Histogram

	// HTTP / Client & Admin API
	HTTPRequests     metric.Int64Counter
	HTTPDuration     metric.Float64Histogram
	ClientEvents     metric.Int64Counter
	AuthFailures     metric.Int64Counter
	AuditEventsTotal metric.Int64Counter

	meter metric.Meter
}

func newMetrics(m metric.Meter) (*Metrics, error) {
	var err error
	mt := &Metrics{meter: m}

	if mt.RendezvousUDPPackets, err = m.Int64Counter(
		"godesk.rendezvous.udp.packets",
		metric.WithDescription("Total RustDesk UDP rendezvous packets received"),
	); err != nil {
		return nil, err
	}
	if mt.RendezvousTCPMessages, err = m.Int64Counter(
		"godesk.rendezvous.tcp.messages",
		metric.WithDescription("Total RustDesk TCP/WS rendezvous messages processed"),
	); err != nil {
		return nil, err
	}
	if mt.RendezvousRegistrations, err = m.Int64Counter(
		"godesk.rendezvous.registrations",
		metric.WithDescription("Total RegisterPeer and RegisterPk operations by result"),
	); err != nil {
		return nil, err
	}
	if mt.RendezvousPunchHoles, err = m.Int64Counter(
		"godesk.rendezvous.punch_holes",
		metric.WithDescription("Total PunchHoleRequest operations by routing mode and result"),
	); err != nil {
		return nil, err
	}

	if mt.RelayActiveSessions, err = m.Int64UpDownCounter(
		"godesk.relay.sessions.active",
		metric.WithDescription("Currently active bridged relay sessions"),
	); err != nil {
		return nil, err
	}
	if mt.RelayWaitingSessions, err = m.Int64UpDownCounter(
		"godesk.relay.sessions.waiting",
		metric.WithDescription("Relay connections currently waiting for a peer leg"),
	); err != nil {
		return nil, err
	}
	if mt.RelaySessionsTotal, err = m.Int64Counter(
		"godesk.relay.sessions.total",
		metric.WithDescription("Total paired relay sessions started"),
	); err != nil {
		return nil, err
	}
	if mt.RelayAuthFailures, err = m.Int64Counter(
		"godesk.relay.auth_failures",
		metric.WithDescription("Total relay connections rejected due to invalid server key"),
	); err != nil {
		return nil, err
	}
	if mt.RelayWaiterTimeouts, err = m.Int64Counter(
		"godesk.relay.waiter_timeouts",
		metric.WithDescription("Total relay connections expired waiting for their pair"),
	); err != nil {
		return nil, err
	}
	if mt.RelayBytesTransferred, err = m.Int64Counter(
		"godesk.relay.bytes",
		metric.WithUnit("By"),
		metric.WithDescription("Total bytes forwarded through the relay bridge"),
	); err != nil {
		return nil, err
	}
	if mt.RelaySessionDuration, err = m.Float64Histogram(
		"godesk.relay.session.duration",
		metric.WithUnit("s"),
		metric.WithDescription("Duration of completed relay sessions in seconds"),
	); err != nil {
		return nil, err
	}

	if mt.HTTPRequests, err = m.Int64Counter(
		"godesk.http.requests",
		metric.WithDescription("Total HTTP API and console requests"),
	); err != nil {
		return nil, err
	}
	if mt.HTTPDuration, err = m.Float64Histogram(
		"godesk.http.duration",
		metric.WithUnit("s"),
		metric.WithDescription("HTTP request latency in seconds"),
	); err != nil {
		return nil, err
	}
	if mt.ClientEvents, err = m.Int64Counter(
		"godesk.client.events",
		metric.WithDescription("RustDesk client HTTP API events (sysinfo, heartbeat, audit, login)"),
	); err != nil {
		return nil, err
	}
	if mt.AuthFailures, err = m.Int64Counter(
		"godesk.auth.failures",
		metric.WithDescription("Failed authentication attempts across client and console APIs"),
	); err != nil {
		return nil, err
	}
	if mt.AuditEventsTotal, err = m.Int64Counter(
		"godesk.audit.events",
		metric.WithDescription("Total audit log events persisted"),
	); err != nil {
		return nil, err
	}

	return mt, nil
}

// RegisterPeerGauges registers live observable gauges for online and total known peers.
func (m *Metrics) RegisterPeerGauges(onlineFn, totalFn func() int) error {
	if m == nil || m.meter == nil {
		return nil
	}
	onlineGauge, err := m.meter.Int64ObservableGauge(
		"godesk.peers.online",
		metric.WithDescription("Number of currently online RustDesk peers"),
	)
	if err != nil {
		return err
	}
	totalGauge, err := m.meter.Int64ObservableGauge(
		"godesk.peers.registered",
		metric.WithDescription("Total registered RustDesk peers in memory"),
	)
	if err != nil {
		return err
	}
	_, err = m.meter.RegisterCallback(func(_ context.Context, obs metric.Observer) error {
		if onlineFn != nil {
			obs.ObserveInt64(onlineGauge, int64(onlineFn()))
		}
		if totalFn != nil {
			obs.ObserveInt64(totalGauge, int64(totalFn()))
		}
		return nil
	}, onlineGauge, totalGauge)
	return err
}
