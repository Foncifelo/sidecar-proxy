package ingest

import (
	"context"
	"time"
)

// RunHeartbeat sends a heartbeat at start and then every HeartbeatEvery until
// ctx is done. Each heartbeat is its own envelope with a fresh batch_id and a
// single item. Failures are logged; the next heartbeat replaces a lost one.
// Run it in its own goroutine.
func (c *Client) RunHeartbeat(ctx context.Context) {
	t := time.NewTicker(c.cfg.HeartbeatEvery)
	defer t.Stop()
	for {
		c.sendHeartbeat(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (c *Client) sendHeartbeat(ctx context.Context) {
	if c.stopped.Load() || ctx.Err() != nil {
		return
	}
	hb := c.heartbeat()
	id, err := c.ids.New(c.now())
	if err != nil {
		c.log.WarnContext(ctx, "ingest: heartbeat id", "err", err)
		return
	}
	if err := c.deliver(ctx, id, []Item{{Type: ItemHeartbeat, Heartbeat: &hb}}); err != nil && ctx.Err() == nil {
		c.log.WarnContext(ctx, "ingest: heartbeat not delivered", "err", err)
	}
}

// heartbeat snapshots the sidecar state.
func (c *Client) heartbeat() Heartbeat {
	hb := Heartbeat{UptimeS: int64(c.now().Sub(c.started) / time.Second)}
	if c.cfg.Queue != nil {
		hb.QueueDepth = c.cfg.Queue.Depth()
		hb.DroppedEvents = c.cfg.Queue.DroppedEvents()
	}
	if c.cfg.State != nil {
		s := c.cfg.State()
		hb.Mode, hb.PolicyVersion, hb.Inflight = s.Mode, s.PolicyVersion, s.Inflight
		if s.PolicyVersion != "" {
			age := int64(max(s.PolicyAge, 0) / time.Second)
			hb.PolicyAgeS = &age
		}
	}
	return hb
}
