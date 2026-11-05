# Quickstart (modos `shadow` y `route`)

Esta guía cubre `shadow` (observa), `passthrough` (túnel) y `route` (actúa). Los pasos 2–5 se han probado a mano en macOS con un proveedor de prueba ficticio en `127.0.0.1`; `route` lo comprueba `make e2e` con un OpenRouter y un plano de control ficticios.

## 1. Obtener el binario

### Binario de release

Cada release (`https://github.com/Foncifelo/sidecar-proxy/releases`) trae el binario `sidecar` para linux y darwin en amd64 y arm64, y para windows en amd64 y arm64:

| Fichero | Qué es |
|---|---|
| `sidecar_<versión>_<os>_<arch>.tar.gz` (`.zip` en windows) | binario, `LICENSE`, `README.md`, `sidecar.example.yaml` y esta guía |
| `checksums.txt` | SHA-256 de cada archivo |
| `checksums.txt.sigstore.json` | firma cosign keyless de `checksums.txt` (bundle de Sigstore: certificado, firma y prueba del log de transparencia) |
| `<archivo>.sbom.json` | SBOM (SPDX) de cada archivo |

Además, cada archivo tiene una atestación de provenance de GitHub (SLSA) que liga el binario con el workflow y el commit que lo construyeron. `<versión>` va sin la `v` (`0.1.0`).

```bash
VERSION=v0.1.0
ARCHIVE=sidecar_${VERSION#v}_linux_amd64.tar.gz      # o darwin_arm64, linux_arm64, darwin_amd64
BASE=https://github.com/Foncifelo/sidecar-proxy/releases/download/$VERSION
curl -fsSLO "$BASE/$ARCHIVE" -O "$BASE/checksums.txt" -O "$BASE/checksums.txt.sigstore.json"
```

Verifica primero la firma de `checksums.txt` y después el archivo contra él. Hace falta cosign v2.4 o posterior (v3 recomendado). La identidad es exacta: el workflow de release de este repositorio, ejecutado por ese tag, con el emisor OIDC de GitHub Actions. Cualquier otra identidad (otro repo, otra rama, otro workflow) debe fallar.

```bash
cosign verify-blob \
  --bundle checksums.txt.sigstore.json \
  --certificate-identity "https://github.com/Foncifelo/sidecar-proxy/.github/workflows/release.yml@refs/tags/${VERSION}" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  checksums.txt                                       # debe imprimir "Verified OK"

sha256sum --check --ignore-missing checksums.txt      # macOS: shasum -a 256 --check --ignore-missing checksums.txt
gh attestation verify --repo Foncifelo/sidecar-proxy "$ARCHIVE"   # opcional: provenance de GitHub
tar -xzf "$ARCHIVE" sidecar
./sidecar --version                                   # v0.1.0 (<commit>, <fecha del commit>)
```

Si alguno de los pasos falla, no ejecutes el binario. `sidecar --version` y `GET /version` muestran el tag y el commit con el que se construyó; el commit debe ser el del tag en este repositorio.

### Desde el código fuente

Requiere Go 1.25 o posterior:

```bash
git clone https://github.com/Foncifelo/sidecar-proxy.git
cd sidecar-proxy
make build                 # deja ./bin/sidecar
./bin/sidecar --version
```

### Contenedor

No se publica imagen. Para construir la tuya: `docker build -f deploy/Dockerfile.sidecar -t sidecar .` (distroless, usuario sin privilegios, cola en el volumen `/data/queue`). Monta tu `sidecar.yaml` como fichero de solo lectura y pásalo con `--config`.

## 2. Configurar

Crea `sidecar.yaml` (está en `.gitignore`; nunca lo subas). Para `shadow` basta con esto; pon en `upstream` el proveedor al que tu agente llama hoy:

```yaml
mode: shadow
listen: 127.0.0.1:8787
metrics_listen: 127.0.0.1:9797
upstream:
  provider: together
  base_url: https://api.together.xyz/v1
  api_key: ${TOGETHER_API_KEY}
control_plane:
  queue_dir: ./data/queue
```

`sidecar.example.yaml` documenta todas las opciones. Si partes de él (`cp sidecar.example.yaml sidecar.yaml`), cada `${VAR}` que contenga tiene que estar definida en el entorno (`TOGETHER_API_KEY`, `SIDECAR_API_KEY`, `OPENROUTER_API_KEY`, `DEEPINFRA_API_KEY`) o el arranque falla con `environment variable … is not set`; borra los bloques que no uses.

## 3. Arrancar el sidecar

```bash
export TOGETHER_API_KEY=…          # la llave del proveedor; nunca sale hacia el plano de control
./bin/sidecar --config sidecar.yaml
```

La primera línea del log es `"msg":"listening"` con `"mode":"shadow"`, `"api":"127.0.0.1:8787"` y `"metrics":"127.0.0.1:9797"`. `Ctrl-C` lo para drenando los streams abiertos.

`curl -s http://127.0.0.1:9797/metrics` devuelve las métricas en formato de texto Prometheus: `sidecar_overhead_seconds` (latencia que añade el sidecar), `sidecar_requests_total`, `sidecar_fallbacks_total`, `sidecar_attempts_total`, `sidecar_queue_depth`, `sidecar_dropped_events_total`, `sidecar_queue_disk_errors_total` (fallos de disco de la cola local; mientras el disco falle, el envío espera con backoff exponencial y jitter) y `sidecar_policy_age_seconds`. Ninguna etiqueta lleva el `model` de tu agente ni contenido de las peticiones.

## 4. Apuntar el agente al sidecar

En tu agente: `OPENAI_BASE_URL=http://127.0.0.1:8787/v1`. Nada más cambia: el sidecar no reenvía tu `Authorization` al proveedor, usa la llave del YAML.

Para comprobarlo con el agente de ejemplo del repositorio (solo `net/http`, una petición en streaming), en otra terminal:

```bash
go run ./examples/agent -url http://127.0.0.1:8787 -model <modelo-de-tu-proveedor> -prompt "hola"
```

Salida esperada: en stderr `status: 200` y las cabeceras `X-Sidecar-Decision: no_policy`, `X-Sidecar-Endpoint: upstream`, `X-Sidecar-Fallback: 0`, `X-Sidecar-Mode: shadow`, `X-Sidecar-Provider: together` y `X-Sidecar-Request-Id: <ULID>`; en stdout los chunks SSE del proveedor tal cual, terminando en `data: [DONE]`. El chunk de `usage` no aparece porque el agente no lo pidió (`docs/contrato-proxy.md`, garantía 6).

Cada petición deja un evento (solo metadatos, `docs/evento.md`) en `control_plane.queue_dir`, en ficheros `seg-*.jsonl`.

## 5. Conectar con tu plano de control

Opcional en `shadow`: sin `control_plane.url` el sidecar no envía nada y lo dice una vez en el log (`"msg":"no control plane configured: …"`). Con el plano de control alojado recibes una URL y una llave de ingesta (`sk_live_…`, o `sk_test_…` para pruebas). La llave solo viaja a esa URL, nunca a un proveedor:

```yaml
control_plane:
  url: https://<tu plano de control>
  api_key: ${SIDECAR_API_KEY}
  queue_dir: ./data/queue
```

Arranca el sidecar con `SIDECAR_API_KEY` exportada en esa terminal (paso 3) y lanza una petición (paso 4). El log del sidecar dice `"msg":"sending events to the control plane"`; los lotes salen cada pocos segundos. Si el plano de control no responde, los lotes se quedan en `control_plane.queue_dir` y se reintentan; `Ctrl-C` envía lo que pueda en unos segundos y el resto queda para el siguiente arranque. Qué sale exactamente: `docs/evento.md`.

**Política.** Con `control_plane.url`, el sidecar también descarga `GET /v1/policy` (`docs/politica.md`) al arrancar y cada `control_plane.policy_refresh` (60 s por defecto, con `If-None-Match`; un `304` solo renueva la política). Guarda la última válida junto a `queue_dir` y la sigue usando si el control cae. Solo puede elegir entre los canales y proveedores de `routing.allowed_channels` y `routing.allowed_providers` del YAML. Cuando la política trae el modelo y su precio, el evento lleva `model_id`, `endpoint_best`, `cost_actual`, `cost_best` y `policy_version` (`docs/evento.md`, `docs/metodologia-ahorro.md`); si no trae tu modelo, `X-Sidecar-Decision: unknown_model` y los costes quedan en 0.

## 6. Modo `route` (actúa)

`route` mueve cada `POST /v1/chat/completions` (con o sin stream) al endpoint más barato que cumple latencia y disponibilidad, y cae al siguiente si falla antes del primer byte. Necesita el paso 5: sin una política que traiga el modelo con `route_enabled`, todo va al `upstream` como en `shadow`. En tu `sidecar.yaml`:

- `mode: route`.
- `routing.allowed_channels` (`openrouter` y/o `direct`) y `routing.allowed_providers`: lo único entre lo que la política puede elegir.
- `providers.openrouter` con su llave (como en `sidecar.example.yaml`) y, opcionales, `http_referer` y `x_title`; para el canal `direct`, cada proveedor con su llave y su `base_url` https.
- `limits.first_byte_timeout` (2 s por defecto), `limits.nonstream_first_byte_timeout` (30 s, para peticiones sin stream en `route`, porque las cabeceras solo llegan con la respuesta entera) y `limits.ttft_p95_slack` (500 ms): los disparadores del fallback.

Qué pasa en cada petición (`docs/contrato-proxy.md`, garantías 1, 2 y 4):

- El plan de intentos son los candidatos de la política que el YAML permite y, siempre el último, tu `upstream`. La política nunca añade URLs, proveedores ni llaves.
- Por el canal `openrouter`, cada intento fija un solo proveedor, `"provider": {"order": ["<proveedor>"], "allow_fallbacks": false}`, con `model` reescrito al nombre del proveedor y la llave de `providers.openrouter`. El `Authorization` de tu agente no sale del sidecar; de sus cabeceras solo pasan `Accept`, `Content-Type` y `User-Agent`.
- Un `429`, un `5xx`, un error de red, `first_byte_timeout` sin cabeceras (`nonstream_first_byte_timeout` si la petición es sin stream) o, con stream, un primer token tardío pasan al siguiente intento sin haber escrito nada al agente. Tras el primer byte no hay reintento: un corte llega como `event: error` y el evento queda `stream_state = truncated`.
- Tu agente recibe el mismo `model` que envió, en el JSON y en cada chunk. Quién respondió de verdad va en `X-Sidecar-Provider`, `X-Sidecar-Endpoint` y `X-Sidecar-Fallback` (`1` si hubo fallback).
- El evento lleva un elemento en `attempts` por intento, con su `abort_reason`, y `cost_actual` es la suma de sus `cost_est`. Un intento abortado por primer token tardío o por `first_byte_timeout` (o `nonstream_first_byte_timeout`) con la petición ya enviada lleva coste estimado; los abortados por `rate_limited` o `http_error` suman 0.

Para volver atrás sin tocar el agente: `mode: shadow` o `--mode passthrough`.

## Probar sin proveedor real

`make e2e` compila el sidecar y el agente de ejemplo y los ejecuta contra un proveedor y un plano de control ficticios (`test/e2e`). En `route` usa además un OpenRouter falso por HTTPS y provoca `429`, `503`, timeouts, primer token tardío y un corte a mitad de stream. Para que el sidecar confíe en su certificado de prueba usa `SSL_CERT_FILE`; en macOS eso exige Go ≥ 1.27 (`GODEBUG=x509sslcertoverrideplatform=1`) y, con un Go anterior, el test de `route` se salta.

## Lo que todavía no hay

- Precios negociados: el sidecar solo aplica los precios de lista que trae la política.
- `tags` por petición (`X-Sidecar-Tags`): definido en el YAML, todavía no se envía en el evento.
- En `route`, coste estimado de los intentos abortados por `rate_limited` o `http_error` (hoy 0). `route` solo enruta `/v1/chat/completions`; `/v1/embeddings` y `/v1/responses` van al `upstream`.
- Homebrew e imagen de contenedor publicada.
