package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/Foncifelo/sidecar-proxy/internal/config"
	"github.com/Foncifelo/sidecar-proxy/internal/events"
	"github.com/Foncifelo/sidecar-proxy/internal/ingest"
	"github.com/Foncifelo/sidecar-proxy/internal/policy"
)

// policyRunner owns the background policy refresh.
type policyRunner struct {
	r      *policy.Refresher
	cancel context.CancelFunc
	done   chan struct{}
}

// startPolicy loads the cached policy and starts refreshing it from
// control_plane.url in its own goroutine, off the request path (ADR 0001).
// Without control_plane.url it returns nil: there is no policy.
func startPolicy(cfg config.Config, log *slog.Logger) (*policyRunner, error) {
	cp := cfg.ControlPlane
	if cp.URL == "" {
		return nil, nil
	}
	if cp.APIKey == "" {
		return nil, errors.New("control_plane.api_key: required when control_plane.url is set")
	}
	plog := log.With("component", "policy")
	r, err := policy.NewRefresher(policy.RefresherConfig{
		BaseURL:   cp.URL,
		APIKey:    cp.APIKey,
		Config:    cfg,
		CachePath: policy.CachePath(queueDir(cp)),
		Interval:  cp.PolicyRefresh.Std(),
		Logger:    plog,
	})
	if err != nil {
		return nil, fmt.Errorf("policy client: %w", err)
	}
	if err := r.LoadCached(); err != nil {
		plog.Warn("cached policy not loaded; starting without a policy", "err", err)
	} else if a := r.Current(); a != nil {
		plog.Info("cached policy loaded", "policy_version", a.Version(), "age", a.Age(time.Now()).Round(time.Second).String())
	}
	ctx, cancel := context.WithCancel(context.Background())
	pr := &policyRunner{r: r, cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(pr.done)
		r.Run(ctx)
	}()
	return pr, nil
}

// Stop ends the refresh loop and waits for it. Safe on a nil runner.
func (pr *policyRunner) Stop() {
	if pr == nil {
		return
	}
	pr.cancel()
	<-pr.done
}

// heartbeatState reports the real mode, policy version and policy age for
// ingest heartbeats. A nil runner means no policy.
func heartbeatState(mode config.Mode, pr *policyRunner, now func() time.Time) func() ingest.State {
	return func() ingest.State {
		s := ingest.State{Mode: events.Mode(mode)}
		if pr == nil {
			return s
		}
		if a := pr.r.Current(); a != nil {
			s.PolicyVersion, s.PolicyAge = a.Version(), a.Age(now())
		}
		return s
	}
}
