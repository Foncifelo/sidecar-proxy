// Package policy fetches GET /v1/policy with ETag, filters candidates against
// the local YAML (the YAML is the policy of record) and caches the last good
// policy on disk. Refresher runs the download off the request path and
// publishes an Active policy that requests read lock-free.
package policy
