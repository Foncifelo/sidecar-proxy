// Package sse parses and writes Server-Sent Events tolerantly: comment lines (keepalives), several data: lines per event, a trailing usage chunk that is dropped when the agent did not ask for it.
package sse
