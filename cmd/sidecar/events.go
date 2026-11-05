package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"time"

	"github.com/Foncifelo/sidecar-proxy/internal/config"
	"github.com/Foncifelo/sidecar-proxy/internal/events"
	"github.com/Foncifelo/sidecar-proxy/internal/ingest"
)

// eventFlushTimeout bounds how long shutdown keeps sending queued events to
// the control plane after the HTTP servers stopped; whatever is left stays on
// disk and goes out after the next start (same event ids; the outstanding
// batch keeps its batch_id, so the control plane deduplicates it).
const eventFlushTimeout = 5 * time.Second

// eventPipeline owns the on-disk event queue (shadow and route) and, when a
// control plane is configured, the batcher that sends it to POST /v1/ingest
// and the ingest heartbeats.
type eventPipeline struct {
	q   *events.Queue   // nil in passthrough
	b   *events.Batcher // nil without a queue or a control plane
	log *slog.Logger

	stopHeartbeat func() // nil without heartbeats
}

// openEventPipeline opens the event queue in shadow and route and, with
// control_plane.url, the ingest client. Heartbeats built from a non-nil state
// run in every mode, so the control plane learns the real mode and policy
// even when no events are sent; in passthrough there is no queue and
// queue_depth and dropped_events stay 0. Without control_plane.url it only
// logs, once, that events are not sent. The control plane key only reaches
// ingest.Client, which sends it to control_plane.url alone (CONTRIBUTING.md rules
// 2 and 3).
func openEventPipeline(cp config.ControlPlane, mode config.Mode, log *slog.Logger, state func() ingest.State) (*eventPipeline, error) {
	if cp.URL != "" && cp.APIKey == "" {
		return nil, errors.New("control_plane.api_key: required when control_plane.url is set")
	}
	p := &eventPipeline{log: log}
	if mode == config.ModeShadow || mode == config.ModeRoute {
		q, err := openQueue(cp)
		if err != nil {
			return nil, err
		}
		p.q = q
	}
	if cp.URL == "" {
		if p.q != nil {
			log.Warn("no control plane configured: events stay in the local queue and are not sent", "queue_dir", queueDir(cp))
		}
		return p, nil
	}
	icfg := ingest.Config{BaseURL: cp.URL, APIKey: cp.APIKey, State: state, Logger: log.With("component", "ingest")}
	if p.q != nil {
		icfg.Queue = p.q // only when set: a typed nil would reach the heartbeat
	}
	client, err := ingest.New(icfg)
	if err != nil {
		p.closeQueue()
		return nil, fmt.Errorf("control plane client: %w", err)
	}
	if p.q != nil {
		p.b = events.StartBatcher(context.Background(), p.q, p.sink(client), events.BatcherConfig{Logger: log.With("component", "events")})
		log.Info("sending events to the control plane", "url", redactURL(cp.URL))
	}
	if state != nil {
		p.stopHeartbeat = startHeartbeat(client)
		log.Info("sending heartbeats to the control plane", "url", redactURL(cp.URL))
	}
	return p, nil
}

// sink wraps Client.Send and reports undelivered batches, which events.Sink
// leaves to its implementation. The batch stays in the queue.
func (p *eventPipeline) sink(c *ingest.Client) events.Sink {
	return func(ctx context.Context, b events.Batch) error {
		err := c.Send(ctx, b)
		if err == nil {
			return nil
		}
		if errors.Is(err, events.ErrRejected) {
			p.log.WarnContext(ctx, "event batch rejected by the control plane; set aside in the queue's rejected/ dir",
				"batch_id", b.ID, "events", len(b.Events), "err", err)
			return fmt.Errorf("send batch %s: %w", b.ID, err)
		}
		if ctx.Err() == nil {
			p.log.WarnContext(ctx, "event batch not delivered; it stays in the local queue",
				"batch_id", b.ID, "events", len(b.Events), "err", err)
		}
		return fmt.Errorf("send batch %s: %w", b.ID, err)
	}
}

// Close sends what it can until ctx expires, then closes the queue. Events
// not acknowledged by then stay on disk for the next start.
func (p *eventPipeline) Close(ctx context.Context) {
	if p.stopHeartbeat != nil {
		p.stopHeartbeat()
	}
	if p.b != nil {
		if err := p.b.Close(ctx); err != nil && p.q.Depth() > 0 {
			p.log.Warn("events left in the local queue for the next start", "depth", p.q.Depth(), "err", err)
		}
	}
	p.closeQueue()
}

func (p *eventPipeline) closeQueue() {
	if p.q == nil {
		return
	}
	if err := p.q.Close(); err != nil {
		p.log.Warn("close event queue", "err", err)
	}
}

// startHeartbeat runs c.RunHeartbeat in its own goroutine; the returned func
// stops it and waits.
func startHeartbeat(c *ingest.Client) func() {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.RunHeartbeat(ctx)
	}()
	return func() {
		cancel()
		<-done
	}
}

// redactURL keeps scheme, host and path only: user:pass@, the query (which may
// carry a token) and the fragment never reach the log.
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "<invalid url>"
	}
	return (&url.URL{Scheme: u.Scheme, Host: u.Host, Path: u.Path}).String()
}

func queueDir(cp config.ControlPlane) string {
	if cp.QueueDir == "" {
		return defaultQueueDir
	}
	return cp.QueueDir
}

func openQueue(cp config.ControlPlane) (*events.Queue, error) {
	dir := queueDir(cp)
	q, err := events.OpenQueue(events.QueueConfig{Dir: dir, MaxBytes: int64(cp.QueueMaxMB) * bytesPerMB})
	if err != nil {
		return nil, fmt.Errorf("open event queue %s: %w", dir, err)
	}
	return q, nil
}
