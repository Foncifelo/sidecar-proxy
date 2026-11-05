# Cómo contribuir

Este repositorio publica el sidecar. Se desarrolla en un repositorio interno junto con el plano de control y se exporta aquí; los PR se revisan aquí y se aplican allí, así que el historial público es el de las exportaciones. Abre un issue antes de un cambio grande.

## Reglas no negociables

Romper una de estas no es un bug, es un cambio de arquitectura: requiere un ADR (`docs/adr/`) antes del código. Los ADR y los documentos de `docs/` se refieren a ellas por su número.

1. **Nunca** se guardan, envían ni registran prompts, respuestas, `messages`, cuerpos ni cabeceras. El evento (`internal/events`) solo lleva metadatos; `internal/events/event_test.go` lo verifica por reflexión.
2. **Nunca** se formatea ni se registra `*http.Request`, `http.Header` ni cuerpos. Todo log pasa por `internal/logging` (handler con redacción) y cualquier llave o token usa el tipo `logging.Secret`.
3. El `Authorization` que llega del agente **no se reenvía** a ningún proveedor. Las llaves de proveedores vienen solo del YAML, de variables de entorno o de ficheros de secretos, y nunca viajan al plano de control.
4. El sidecar escucha en loopback por defecto. Escuchar en `0.0.0.0` exige `inbound_token`.
5. **Fallback solo antes del primer byte** al agente. Después, el fallo se emite como `event: error` SSE y el evento queda `stream_state = truncated`. Todos los intentos se registran en `attempts` y suman en `cost_actual`.
6. El agente recibe el `model` que envió: en `route` solo se reescribe el campo `model` de nivel superior de la respuesta y de cada chunk (ADR 0008), nada más del cuerpo. La verdad va en cabeceras `X-Sidecar-*`.
7. La unidad de precio, latencia y ruta es el **endpoint** (proveedor × modelo × cuantización × tier × región × canal). Nada se indexa por `(provider, model)`.
8. La política descargada del plano de control **solo elige** entre endpoints y canales que el YAML del cliente permite. Nunca puede añadir URLs, proveedores ni llaves.
9. Dinero en **microdólares como `int64`**. Nunca `float` para importes. Cada coste guarda el `price_id` con el que se calculó y no se recalcula.
10. (Plano de control: aislamiento de clientes en la base de datos. No aplica a este repositorio.)
11. El protocolo sidecar ↔ control lleva `schema_version`. Los campos nuevos son opcionales; los viejos nunca cambian de significado; compatibilidad N-2.
12. Sin dependencias nuevas sin justificación en el PR. Primero la librería estándar. Ninguna dependencia en `internal/proxy` fuera de `net/http`.

## Convenciones

- Código, identificadores, comentarios y mensajes de commit en inglés; documentación y ADR en español.
- Commits: [Conventional Commits](https://www.conventionalcommits.org/) (`feat(proxy): …`, `fix(sse): …`). PR pequeños (< 400 líneas netas), una intención por PR.
- Go: `gofmt`, `go vet` y `golangci-lint` (`.golangci.yml`). Errores envueltos con `%w` y contexto; `context.Context` como primer argumento; sin `init()`; sin estado mutable a nivel de paquete; sin `panic` fuera de `main`.
- Timeouts explícitos en todo cliente y servidor HTTP. Nunca `http.DefaultClient` ni `http.DefaultTransport` en código de producción.
- Tests de tabla junto al código, siempre con `-race`. Los adaptadores de `internal/providers` se prueban contra grabaciones en `testdata/` (`make record PROVIDER=…`), nunca contra la API real. Ninguna llave real en código, tests, fixtures, logs ni issues; precios, latencias y proveedores inventados se marcan como ficticios.
- Un cambio en `internal/events` que añada campos sube `SchemaVersion` y actualiza `docs/evento.md` en el mismo PR. Un cambio de comportamiento del proxy actualiza `docs/contrato-proxy.md`.

## Antes de abrir un PR

```bash
make hooks          # una vez
make lint test e2e  # golangci-lint v2.15.0, la misma que CI
```

Seguridad: ver `SECURITY.md`. No abras issues públicos para vulnerabilidades.
