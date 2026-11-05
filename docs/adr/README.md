# Registros de decisiones de arquitectura (ADR)

Un ADR por decisión que cuesta revertir. Estados: `propuesto`, `aceptado`, `reemplazado por NNNN`, `rechazado`. Se numeran en orden; nunca se reescribe uno aceptado: se añade otro que lo reemplaza.

Aquí están los que afectan al sidecar. La numeración tiene huecos: otros ADR son internos y no se publican. Las "reglas" que citan son las de `CONTRIBUTING.md`.

| Nº | Título | Estado |
| --- | --- | --- |
| [0001](0001-sidecar-local-go.md) | Un binario en Go como sidecar local; plano de control fuera del camino | aceptado |
| [0003](0003-fallback-primer-byte.md) | Proxy propio; fallback solo antes del primer byte; el agente recibe el `model` que envió | aceptado |
| [0006](0006-sin-prompts-redaccion-firma.md) | Sin prompts, sin cuerpos, sin `request_hash`; redacción de logs y cadena de suministro firmada | aceptado |
| [0008](0008-route-modelo-cabeceras-base-url.md) | En `route` solo se reescribe el `model` de nivel superior; cabeceras por allowlist; `base_url` de cada proveedor sale del YAML | aceptado |
| [0011](0011-http-loopback-demo.md) | `base_url` en `http` solo para loopback y solo con `SIDECAR_ENV` igual a `dev` o `staging` | aceptado |
