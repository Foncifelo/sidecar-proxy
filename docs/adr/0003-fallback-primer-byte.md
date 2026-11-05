# 0003 — Proxy propio; fallback solo antes del primer byte; el agente recibe el `model` que envió

- Estado: aceptado
- Fecha: 2026-10-28

## Contexto

En modo `route` el sidecar reescribe el nombre del modelo hacia el proveedor y puede reintentar contra otro. Una vez que un byte de respuesta llegó al agente ya no es posible reintentar de forma transparente, y un proveedor cuya petición se cancela puede cobrarla igual. `httputil.ReverseProxy` no permite mirar el primer chunk ni reintentar con el cuerpo bufferizado.

## Decisión

`internal/proxy` es un handler propio sobre `net/http` con `http.Client` y un parser SSE propio. El cuerpo de la petición se bufferiza (límite configurable). El fallback se dispara solo antes del primer byte al agente: por error HTTP, por `429`, por primer token más tarde que el p95 del cliente + margen, o por 2 s sin cabeceras. Después del primer byte un fallo se transmite como `event: error` SSE y el evento queda `stream_state = truncated`. Todos los intentos se registran en `events.attempts` y suman en `cost_actual`. El agente recibe exactamente el `model` que envió; la verdad va en cabeceras `X-Sidecar-Request-Id`, `-Provider`, `-Endpoint`, `-Decision`, `-Fallback`. Lo no reconocido (`/v1/responses`, `/v1/embeddings`, `/v1/models`, rutas desconocidas) pasa intacto al `upstream` del YAML.

## Alternativas consideradas

- `httputil.ReverseProxy` con `ModifyResponse` — sin peek del primer chunk ni reintento con cuerpo.
- Reescribir `model` en cada chunk para que el agente "vea" el proveedor real — rompe la copia de bytes y el contrato con la observabilidad del cliente.
- Reintento después del primer byte reconstruyendo el stream — imposible sin duplicar contenido.

## Consecuencias

`internal/proxy` solo importa `net/http`. `Accept-Encoding: identity` hacia el proveedor. `stream_options.include_usage` se añade siempre y el chunk de `usage` se retira si el agente no lo pidió. La respuesta al agente es siempre `chunked`.

## Cómo se verifica

`CONTRIBUTING.md` reglas 5 y 6; `forbidigo` prohíbe `httputil.ReverseProxy`; `docs/contrato-proxy.md`; tests de caos en `internal/proxy` (proveedor que falla, que tarda, que devuelve 429, que corta a mitad de stream).
