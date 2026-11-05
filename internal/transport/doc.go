// Package transport builds one http.Transport per provider with explicit timeouts, a semaphore (bulkhead) and a circuit breaker per endpoint. Nothing in the repo uses http.DefaultTransport.
package transport
