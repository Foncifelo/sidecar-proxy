# sidecar

Proxy local, compatible con la API de OpenAI, que se pone entre tu agente y tus proveedores de modelos abiertos. Mide el coste real de cada petición y, cuando lo autorizas, mueve cada llamada al proveedor más barato que cumple tu latencia y disponibilidad, con fallback y sin cambiar de modelo.

*A local, OpenAI-compatible proxy that sits between your agent and your open-model providers. It measures the real cost of every request and, when you allow it, moves each call to the cheapest provider that meets your latency and availability, with fallback and without changing the model. Docs are in Spanish.*

Licencia Apache 2.0. Este repositorio contiene solo el sidecar; el plano de control (precios, latencias, política y panel) es un servicio alojado aparte. El sidecar funciona sin él en `shadow` y `passthrough`.

## Modos

| Modo | Qué hace |
| --- | --- |
| `shadow` | Reenvía cada petición al `upstream` de tu YAML sin tocarla, mide tokens, latencia y coste, y calcula cuánto habría costado en el endpoint más barato que cumple. Es el punto de partida. |
| `route` | Elige, entre los endpoints que tu YAML permite, el más barato que cumple; reescribe solo `model` hacia el proveedor y cae al siguiente si falla **antes del primer byte**. Tu agente recibe el mismo `model` que envió; la ruta real va en cabeceras `X-Sidecar-*`. |
| `passthrough` | Túnel de emergencia al `upstream`, sin medir ni decidir. |

Integrarlo es cambiar una URL: `OPENAI_BASE_URL=http://127.0.0.1:8787/v1`.

## Garantías de privacidad

- Nunca se guardan, envían ni registran prompts, respuestas, `messages`, cuerpos ni cabeceras. Lo único que sale es un evento de metadatos por petición, descrito campo a campo en [`docs/evento.md`](docs/evento.md); un test por reflexión impide añadirle campos de contenido.
- El `Authorization` que manda tu agente **no se reenvía** a ningún proveedor. Las llaves de proveedor salen solo de tu YAML, del entorno o de ficheros de secretos, y nunca viajan al plano de control.
- La política que descarga del plano de control **solo elige** entre los endpoints y canales que tu YAML permite: no puede añadir URLs, proveedores ni llaves.
- Escucha en `127.0.0.1` por defecto; escuchar en otra interfaz exige `inbound_token`.
- Sin auto-actualización. Binarios firmados (cosign keyless), con SBOM y atestación de provenance.

El contrato completo del proxy y qué test demuestra cada garantía: [`docs/contrato-proxy.md`](docs/contrato-proxy.md).

## Instalación

Descarga el archivo de tu plataforma desde [Releases](https://github.com/Foncifelo/sidecar-proxy/releases) (linux y darwin en amd64/arm64, windows en amd64/arm64) junto con `checksums.txt` y `checksums.txt.sigstore.json`, y verifica antes de ejecutar (cosign v2.4 o posterior):

```bash
VERSION=v0.1.0
ARCHIVE=sidecar_${VERSION#v}_linux_amd64.tar.gz
BASE=https://github.com/Foncifelo/sidecar-proxy/releases/download/$VERSION
curl -fsSLO "$BASE/$ARCHIVE" -O "$BASE/checksums.txt" -O "$BASE/checksums.txt.sigstore.json"

cosign verify-blob \
  --bundle checksums.txt.sigstore.json \
  --certificate-identity "https://github.com/Foncifelo/sidecar-proxy/.github/workflows/release.yml@refs/tags/${VERSION}" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  checksums.txt                                  # "Verified OK"
sha256sum --check --ignore-missing checksums.txt # macOS: shasum -a 256 --check --ignore-missing checksums.txt
gh attestation verify --repo Foncifelo/sidecar-proxy "$ARCHIVE"   # provenance de GitHub (opcional; requiere gh)
tar -xzf "$ARCHIVE" sidecar && ./sidecar --version
```

Si un paso falla, no ejecutes el binario. Desde el código fuente (Go 1.25 o posterior): `make build` deja `./bin/sidecar`. Imagen de contenedor propia: `deploy/Dockerfile.sidecar`.

## Configuración mínima

`sidecar.yaml` (nunca lo subas a un repositorio) para empezar en `shadow`:

```yaml
mode: shadow
listen: 127.0.0.1:8787
metrics_listen: 127.0.0.1:9797
upstream:                       # el proveedor al que tu agente llama hoy
  provider: together
  base_url: https://api.together.xyz/v1
  api_key: ${TOGETHER_API_KEY}
control_plane:
  queue_dir: ./data/queue       # añade url y api_key para enviar los eventos a tu plano de control
```

```bash
export TOGETHER_API_KEY=…
./sidecar --config sidecar.yaml
```

Todas las opciones están comentadas en [`sidecar.example.yaml`](sidecar.example.yaml). Guía paso a paso, `route` incluido: [`docs/quickstart.md`](docs/quickstart.md).

## Documentación

- [`docs/quickstart.md`](docs/quickstart.md) — instalar, configurar, `shadow`, plano de control, `route`.
- [`docs/contrato-proxy.md`](docs/contrato-proxy.md) — qué puede esperar tu agente del proxy.
- [`docs/evento.md`](docs/evento.md) — lo único que sale de tu máquina.
- [`docs/politica.md`](docs/politica.md) — el documento de `GET /v1/policy` y cómo lo filtra el sidecar.
- [`docs/metodologia-ahorro.md`](docs/metodologia-ahorro.md) — cómo se calcula el ahorro.
- [`docs/adr/`](docs/adr/README.md) — decisiones de diseño.
- [`CONTRIBUTING.md`](CONTRIBUTING.md), [`SECURITY.md`](SECURITY.md), [`CHANGELOG.md`](CHANGELOG.md).

## Desarrollo

```bash
make hooks   # una vez: hook de pre-commit (gofmt, go vet, gitleaks)
make build   # ./bin/sidecar
make test    # go test -race ./...
make lint    # gofmt, go vet, golangci-lint
make e2e     # sidecar + agente de ejemplo contra un proveedor y un plano de control ficticios
```
