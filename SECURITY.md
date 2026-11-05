# Seguridad

## Reportar una vulnerabilidad

Usa el reporte privado de GitHub: pestaña **Security** → **Report a vulnerability** en `https://github.com/Foncifelo/sidecar-proxy`. Incluye una descripción, pasos para reproducir y la versión afectada (`sidecar --version`). Respuesta en 72 horas; corrección prioritaria para cualquier cosa que exponga llaves de proveedor o contenido de clientes. No abras un issue público.

## Modelo de amenazas (resumen)

Tres atacantes guían los controles: un proceso o pod vecino que use el sidecar para consumir inferencia con las llaves del cliente; un plano de control comprometido que intente desviar tráfico; un operador o un log que filtre llaves o prompts. Los controles están en `docs/adr/0003` y `0006` y en las reglas 1–4 y 8 de `CONTRIBUTING.md`:

- Escucha en loopback; fuera de loopback exige `inbound_token`.
- La política del plano de control solo elige entre lo que permite el YAML local: nunca añade URLs, proveedores ni llaves.
- El `Authorization` del agente no se reenvía; las llaves de proveedor no salen de la máquina.
- Logs con redacción (`internal/logging`, `logging.Secret`); el evento no tiene campos de contenido.

## Garantías

- Los binarios de cada release se publican con `checksums.txt` firmado (cosign keyless, identidad del workflow de release de este repositorio), un SBOM por archivo y una atestación de provenance de GitHub; `docs/quickstart.md` explica cómo verificarlos.
- Las llaves de proveedor nunca salen de la infraestructura del cliente.
- Ningún prompt, respuesta, cabecera o cuerpo sale del sidecar; `docs/evento.md` lista exactamente qué sale.
- Sin auto-actualización.

## Dependencias

Dos dependencias directas (`gopkg.in/yaml.v3`, `github.com/oklog/ulid/v2`); `internal/proxy` solo usa la librería estándar. Dependabot semanal; `govulncheck` y `gitleaks` en cada PR.
