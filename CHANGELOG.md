# Cambios

Formato basado en [Keep a Changelog](https://keepachangelog.com/es-ES/1.1.0/); versiones según [SemVer](https://semver.org/lang/es/). Antes de 1.0, una versión menor puede romper compatibilidad y lo dirá aquí.

## [0.1.0] — sin publicar

Primera release pública del sidecar. Fase 0: API y formato del evento todavía pueden cambiar entre versiones menores.

- Proxy local compatible con la API de OpenAI (`/v1/chat/completions`, `/v1/embeddings`, `/v1/responses` y el resto de `/v1/*` reenviado), en `127.0.0.1:8787` por defecto; escuchar fuera de loopback exige `inbound_token`.
- Tres modos: `shadow` (observa y mide sin cambiar el tráfico), `route` (elige el endpoint más barato que cumple latencia y disponibilidad entre los que permite el YAML del cliente) y `passthrough` (túnel de emergencia).
- Fallback solo antes del primer byte al agente; después, `event: error` SSE y evento `truncated`. Todos los intentos cuentan en el coste.
- En `route` el agente recibe el `model` que envió; la ruta real va en cabeceras `X-Sidecar-*`.
- Un evento de metadatos por petición (sin prompts, respuestas, cuerpos ni cabeceras; `docs/evento.md`), en cola local en disco y enviado al plano de control si está configurado.
- Descarga y caché de la política (`GET /v1/policy`), que solo elige entre lo que el YAML permite.
- `--version` y `GET /version` con tag y commit; `GET /healthz`; métricas Prometheus en `127.0.0.1:9797`.
- Binarios para linux y darwin (amd64, arm64) y windows (amd64, arm64), con `checksums.txt` firmado con cosign keyless, SBOM por archivo y provenance de GitHub (`docs/quickstart.md`, sección 1).

### Limitaciones conocidas

- `route` solo se ha probado contra proveedores ficticios (`make e2e`), no contra uno real.
- Sin Homebrew ni imagen de contenedor publicada (`deploy/Dockerfile.sidecar` para construir la tuya).
- Solo precios de lista; sin `tags` por petición.
