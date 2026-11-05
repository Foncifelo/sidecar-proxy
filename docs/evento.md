# Lo que sale de casa del cliente: el evento

Una fila por petición. Solo metadatos. La definición ejecutable es `internal/events/event.go`; `event_test.go` falla si alguien añade un campo con nombre de contenido. `SchemaVersion` actual: 1.

Resumen por grupo:

| Grupo | Campos | Notas |
| --- | --- | --- |
| Identidad | `id`, `ts`, `customer_id`, `api_key_id`, `sidecar_version`, `tags` | `id` es el `X-Sidecar-Request-Id`; `tags` solo claves de la allowlist (`agent`, `env`, `team`), sin PII |
| Petición | `endpoint_kind`, `stream`, `raw_model`, `model_id`, `requirements` | `raw_model` es el string que envió el agente; `requirements` es una lista corta (`tools`, `json_schema`, …), no el contenido |
| Decisión | `mode`, `endpoint_used`, `provider_served`, `endpoint_best`, `decision_reason`, `policy_version`, `attempts[]`, `stream_state` | `attempts` incluye los intentos cancelados con su coste estimado |
| Uso | `tokens_in`, `tokens_out`, `tokens_reasoning`, `tokens_cached_read`, `tokens_cached_write`, `usage_source`, `finish_reason`, `error_type`, `ttft_ms`, `total_ms`, `tps`, `status` | `usage_source = none` cuando el proveedor no devolvió `usage` (`estimated` reservado, aún no se emite) |
| Dinero | `cost_actual`, `cost_best`, `price_id_actual`, `price_id_best`, `billable` | Microdólares enteros; `billable` solo en `route` |
| Reintentos | `retry_of_request_id`, `client_retry_count` | Detectados en el sidecar; no existe hash del cuerpo |

## Campo por campo

Mismo orden que `Event` en `internal/events/event.go`. "Omitible" significa `omitempty`: el campo no aparece en el JSON cuando está vacío o a cero. "Hoy" describe lo que rellena el proxy en modo `shadow` (`internal/proxy`, `TestEventShadow` y `TestStream` en `internal/proxy/proxy_test.go`); `passthrough` no emite evento. "Con política" significa que el sidecar tiene una política activa descargada de `GET /v1/policy` y la usa para decidir en `shadow` (`TestShadowPolicy*` en `internal/proxy/decide_test.go`); `TestEndToEndShadowPolicy` (`test/e2e/policy_test.go`, `make e2e`) lo comprueba de extremo a extremo con un plano de control falso.

| Campo JSON | Tipo Go | Omitible | Significado | Hoy |
| --- | --- | --- | --- | --- |
| `id` | `string` | no | ULID; el mismo valor que `X-Sidecar-Request-Id` | Sí |
| `ts` | `time.Time` (RFC 3339) | no | Instante fijado una vez por el sidecar; el servidor rechaza > ahora + 5 min | Sí |
| `customer_id` | `string` | sí | Lo rellena el plano de control a partir de la llave de API | No: el sidecar nunca lo envía; lo fija el plano de control al recibir el lote |
| `api_key_id` | `string` | sí | Llave de API con la que llegó el lote | No: el sidecar nunca lo envía; lo fija el plano de control al recibir el lote |
| `sidecar_version` | `string` | no | Versión del binario | Sí |
| `tags` | `map[string]string` | sí | Solo claves de la allowlist, sin PII | Pendiente (sin tarea) |
| `endpoint_kind` | `string`: `chat` \| `responses` \| `embeddings` \| `passthrough` | no | Superficie de la API usada | Sí |
| `stream` | `bool` | no | El agente pidió `stream: true` | Sí |
| `raw_model` | `string` | no | Exactamente el `model` que envió el agente | Sí |
| `model_id` | `string` | sí | Modelo canónico resuelto | Con política: el modelo cuyo `aliases[upstream.provider]` contiene `raw_model`; vacío sin política o con `unknown_model` |
| `requirements` | `[]string` | sí | `tools`, `json_schema`, `vision`, `logprobs`, `n>1`, `ctx>Nk` | Parcial: `tools`, `json_schema`, `logprobs`, `n>1`; `vision` y `ctx>Nk` pendientes (sin tarea) |
| `mode` | `string`: `shadow` \| `route` \| `passthrough` | no | Modo del sidecar al atender la petición | Sí |
| `endpoint_used` | `string` | sí | Endpoint que sirvió la respuesta | Sí: con política, el `endpoint_id` de `current` para (`upstream.provider`, `raw_model`); si no, `upstream` |
| `provider_served` | `string` | sí | Proveedor que el agregador dice que la sirvió | Sí (`upstream.provider`) |
| `endpoint_best` | `string` | sí | Endpoint más barato que cumplía | Con política: el ganador de `decision.Decide`, o `endpoint_used` si el actual es el más barato o ningún candidato pasa; `upstream` con `current_price_error`; vacío sin política o con `unknown_model` |
| `decision_reason` | `string` | sí | Razón corta; la misma que `X-Sidecar-Decision` | Sí: `no_policy` sin política; con política, `unknown_model`, `cheaper_endpoint`, `current_is_cheapest`, `no_candidate_passes` o `current_price_error` |
| `policy_version` | `string` | sí | Versión de la política aplicada | Con política: su `version` (también con `unknown_model`); vacío sin política |
| `attempts` | `[]Attempt` | no | Todas las llamadas al proveedor, ver abajo | Sí (un intento) |
| `stream_state` | `string`: `complete` \| `truncated` \| `aborted` | sí | Cómo terminó un stream; vacío si no hubo stream | Sí |
| `tokens_in` | `int64` | no | Tokens de entrada **no cacheados** (entrada total − `tokens_cached_read`) | Sí |
| `tokens_out` | `int64` | no | Tokens de salida **sin razonamiento** (salida total − `tokens_reasoning`) | Sí |
| `tokens_reasoning` | `int64` | no | Tokens de salida de razonamiento | Sí |
| `tokens_cached_read` | `int64` | no | Tokens de entrada servidos desde la caché del proveedor | Sí |
| `tokens_cached_write` | `int64` | no | Tokens escritos en caché; el formato OpenAI no lo reporta, queda en 0 | Sí (0) |
| `usage_source` | `string`: `provider` \| `estimated` \| `none` | no | De dónde salen los tokens | Sí (`provider` o `none`; `estimated` pendiente, sin tarea) |
| `finish_reason` | `string` | sí | `finish_reason` del proveedor | Sí |
| `error_type` | `string` | sí | Tipo de error, p. ej. `upstream_stream_error` | Sí |
| `ttft_ms` | `int64` | no | Milisegundos hasta el primer byte | Sí |
| `total_ms` | `int64` | no | Milisegundos totales | Sí |
| `tps` | `float64` | no | Tokens por segundo de generación (no es dinero) | Sí |
| `status` | `int` | no | Código HTTP devuelto al agente | Sí |
| `cost_actual` | `int64` (microdólares) | no | Suma del coste de todos los intentos | Con política y precio de `current`: ese precio sobre el uso real (un intento); si no, 0 |
| `cost_best` | `int64` (microdólares) | no | Coste en el mejor endpoint (`docs/metodologia-ahorro.md`) | Sí, con `savings.Simulated`: con `cheaper_endpoint`, el precio del ganador sobre el uso real; en el resto, igual a `cost_actual` |
| `price_id_actual` | `string` | sí | `price_id` con el que se calculó `cost_actual` | Con política y precio de `current`: el `price_id` de `current`; si no, vacío |
| `price_id_best` | `string` | sí | `price_id` con el que se calculó `cost_best` | Igual que `cost_best`: el del ganador con `cheaper_endpoint`; si no, el de `price_id_actual` |
| `billable` | `bool` | no | `true` solo en `route` | Sí (`false`) |
| `retry_of_request_id` | `string` | sí | Petición de la que esta es reintento | Pendiente (sin tarea) |
| `client_retry_count` | `int` | sí | Reintentos del cliente detectados | Pendiente (sin tarea) |

Los cinco campos `tokens_*` son disjuntos: cada token cae en uno solo, y así se cobra una sola vez (fórmula de `docs/metodologia-ahorro.md`). La normalización está en `pkg/openai/usage.go` y la prueban `TestFromChat`, `TestFromResponses` y `TestFromEmbeddings`.

`Attempt` (cada elemento de `attempts`):

| Campo JSON | Tipo Go | Omitible | Significado |
| --- | --- | --- | --- |
| `endpoint_id` | `string` | no | Endpoint llamado |
| `provider` | `string` | no | Proveedor llamado |
| `channel` | `string`: `direct` \| `openrouter` | no | Canal |
| `status` | `int` | no | Código HTTP del proveedor; 0 si no hubo respuesta |
| `ttft_ms` | `int64` | no | Milisegundos hasta el primer byte de este intento |
| `total_ms` | `int64` | no | Milisegundos totales de este intento |
| `tokens_out_est` | `int64` | no | Tokens de salida estimados del intento |
| `cost_est` | `int64` (microdólares) | no | Coste estimado del intento, aunque se cancelara |
| `abort_reason` | `string`: `first_byte_timeout` \| `ttft_over_p95` \| `http_error` \| `rate_limited` | sí | Por qué se abandonó el intento |

`TestEventCarriesNoContent` y `TestMoneyIsInteger` (`internal/events/event_test.go`) comprueban por reflexión que no hay campos de contenido y que el dinero es entero.

## Lo que no está y nunca estará

`prompt`, `messages`, `completion`, `content`, `input`, `output`, cabeceras, cuerpos, hashes del cuerpo, direcciones IP del agente, identificadores de usuarios finales.

## Envelope de ingesta

```json
{
  "schema_version": 1,
  "sidecar_version": "0.1.0",
  "batch_id": "01J…",
  "items": [
    { "type": "request", "event": { "…": "…" } },
    { "type": "probe", "probe": { "endpoint_id": "…", "ttft_ms": 210, "ok": true } },
    { "type": "heartbeat", "heartbeat": { "mode": "shadow", "policy_version": "…", "queue_depth": 0, "dropped_events": 0, "inflight": 3, "uptime_s": 86400, "policy_age_s": 42 } }
  ]
}
```

Tipos implementados en `internal/ingest/envelope.go`: `request` y `heartbeat` (`TestSendEnvelopeAndAuth` en `internal/ingest/client_test.go`). El tipo `probe`: pendiente (sin tarea). El sidecar envía la cola local con este envelope cuando hay `control_plane.url` (`TestEventPipelineDelivers` en `cmd/sidecar/events_test.go`).

Heartbeat: el sidecar lo envía al arrancar y cada 60 s cuando hay `control_plane.url`. `mode` es el modo del YAML, `policy_version` la `version` de la política activa (vacío sin política) y `policy_age_s`, campo **opcional** nuevo (regla 11), los segundos desde la última respuesta válida (`200` o `304`) del control para esa política (`docs/politica.md`). Sin política, `policy_age_s` se omite. No cambia `schema_version` del envelope: es un campo opcional que un control que no lo conoce ignora. `inflight` sigue en `0` hasta que el proxy exponga el contador. Tests: `TestHeartbeatPolicyAge` (`internal/ingest/heartbeat_test.go`) y `TestHeartbeatStateFromPolicy` (`cmd/sidecar/policy_test.go`).

Recepción en el plano de control (`POST /v1/ingest`): autenticación con `Authorization: Bearer sk_live_…`/`sk_test_…` (401 si la llave no existe, está revocada o caducada); envelope de hasta 1 MiB (413); 400 si el JSON o el envelope no son válidos; 426 si `schema_version` es mayor que el actual. `customer_id` y `api_key_id` salen de la llave, nunca del cuerpo. Un `ts` más de 5 min por delante de la recepción se sustituye por la hora de recepción y se marca `ts_clamped` en `error_type`. Responde 202 con `accepted_n`, que cuenta los `request` y `heartbeat` aceptados; los `probe` aún no se guardan. Reenviar el mismo `batch_id` devuelve 202 con el mismo `accepted_n` sin duplicar eventos.

Compatibilidad: el plano de control acepta `schema_version` N, N-1 y N-2. Los campos nuevos son opcionales; los existentes no cambian de significado.
