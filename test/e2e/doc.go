// Package e2e holds the end-to-end test of the sidecar binary. It only builds
// with the e2e tag (make e2e): it compiles cmd/sidecar and examples/agent,
// runs them against a fake upstream and a fake control plane (all data is
// fictitious) and checks headers, bytes and ingest.
package e2e
