// Package ingest is the sidecar's client for POST /v1/ingest on the control plane.
//
// Client.Send is an events.Sink: the events.Batcher calls it from its own
// goroutine, so the request path only ever touches events.Queue.Append and never
// waits on the network. Send returns nil (and the batch is acknowledged) only on
// 202 with a coherent accepted_n. It retries with exponential backoff and full
// jitter, honours Retry-After on 429, splits the batch in two on 413 and stops
// sending for good on 426. RunHeartbeat sends a heartbeat item every minute.
//
// Envelopes only carry events.Event values (metadata, CONTRIBUTING.md rule 1). The API
// key is a logging.Secret and is revealed only when the Authorization header is set.
package ingest
