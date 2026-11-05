package ingest

import "github.com/Foncifelo/sidecar-proxy/internal/events"

// ItemType tags an envelope item (docs/evento.md).
type ItemType string

// Item types sent by the sidecar.
const (
	ItemRequest   ItemType = "request"
	ItemHeartbeat ItemType = "heartbeat"
)

// Envelope is the body of POST /v1/ingest. SchemaVersion is events.SchemaVersion.
type Envelope struct {
	SchemaVersion  int    `json:"schema_version"`
	SidecarVersion string `json:"sidecar_version"`
	BatchID        string `json:"batch_id"`
	Items          []Item `json:"items"`
}

// Item is one entry of an envelope; exactly one payload field matches Type.
type Item struct {
	Type      ItemType      `json:"type"`
	Event     *events.Event `json:"event,omitempty"`
	Heartbeat *Heartbeat    `json:"heartbeat,omitempty"`
}

// Heartbeat reports the sidecar's health. It carries no request data.
type Heartbeat struct {
	Mode          events.Mode `json:"mode"`
	PolicyVersion string      `json:"policy_version"`
	QueueDepth    int         `json:"queue_depth"`
	DroppedEvents uint64      `json:"dropped_events"`
	Inflight      int64       `json:"inflight"`
	UptimeS       int64       `json:"uptime_s"`
	// PolicyAgeS is the seconds since the control plane last validated the
	// active policy (docs/politica.md). Optional (rule 11): omitted when the
	// sidecar has no policy.
	PolicyAgeS *int64 `json:"policy_age_s,omitempty"`
}

// Response is the body of a 202 from the control plane.
type Response struct {
	AcceptedN *int `json:"accepted_n"`
}
