# 0006 — Sin prompts, sin cuerpos, sin `request_hash`; redacción de logs y cadena de suministro firmada

- Estado: aceptado
- Fecha: 2026-10-28

## Contexto

El sidecar corre con las llaves de proveedor del cliente y ve sus prompts en memoria. Un SHA-256 del cuerpo sin clave es reversible por diccionario para prompts cortos o plantillas con pocas variables, y contradice un DPA que promete no guardar contenido.

## Decisión

El evento no tiene campos para prompts, respuestas, cabeceras ni cuerpos, y un test por reflexión lo impide. No existe `request_hash`: los reintentos se detectan en el sidecar con `x-stainless-retry-count` y una ventana local (`retry_of_request_id`). Todo log pasa por `internal/logging` (allowlist de cabeceras, tipo `Secret`, lint que prohíbe formatear `*http.Request`). El `Authorization` entrante nunca se reenvía. Los binarios e imágenes se firman con cosign keyless, llevan SBOM y atestación de provenance; las acciones de CI se pinnean por SHA antes de la primera release; no hay auto-update.

## Alternativas consideradas

- HMAC del cuerpo con secreto por cliente — sigue siendo un derivado del contenido que sale de casa del cliente; el valor que aporta (detectar reintentos) se obtiene localmente.
- Logs "con cuidado" sin handler de redacción — no es retrofiteable; un leak basta.

## Consecuencias

`tags` admite solo claves de una allowlist con valores acotados y el DPA prohíbe PII en ellos. `docs/evento.md` publica la estructura exacta de lo que sale de casa del cliente.

## Cómo se verifica

`CONTRIBUTING.md` reglas 1, 2 y 3; `internal/events/event_test.go`; `internal/logging/logging_test.go`; `gitleaks` en CI y en el hook de pre-commit; `release.yml`.
