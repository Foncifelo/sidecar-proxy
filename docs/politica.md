# Contrato de la política: `GET /v1/policy`

Lo que el plano de control envía al sidecar para decidir. Es parte del protocolo sidecar ↔ control (regla 11): cambiarlo exige subir `schema_version` y mantener la compatibilidad N-2. Las decisiones de fondo están en ADR 0001 (la política se conserva si el control cae) y ADR 0008 (las URLs salen solo del YAML).

Implementado. Los tipos, el filtro y el plan de intentos están en `internal/policy`; el sidecar descarga la política, la guarda en disco y la aplica (`internal/policy`, `cmd/sidecar`).

## Lo que la política nunca lleva

URLs, `base_url`, cabeceras, nombres de cabecera de autenticación, llaves ni secretos de ningún tipo. Tampoco prompts, cuerpos ni nada derivado de ellos.

La política **solo elige** entre los endpoints y canales que el YAML del cliente permite (regla 8). El sidecar descarta todo candidato que no pase el filtro de abajo, y la URL y la llave de cada intento se leen siempre del YAML (ADR 0008). Los tipos Go del documento no tienen campos para URLs ni llaves. Si un documento trae campos extra como `url`, `base_url` o `api_key`, se ignoran y no cambian el resultado (`TestDecodeIgnoresSecretLikeFields` en `internal/policy/decode_test.go`).

## Petición

```http
GET /v1/policy HTTP/1.1
Authorization: Bearer sk_live_…
X-Sidecar-Schema-Version: 1
If-None-Match: "<schema_version>:<version>"
```

- **`Authorization`:** la llave de API del cliente (`control_plane.api_key`, `sk_live_`/`sk_test_`). Se resuelve igual que en la ingesta. Nunca es una llave de proveedor.
- **`X-Sidecar-Schema-Version`:** la `schema_version` de política más alta que entiende el sidecar. Sin la cabecera se asume `1`.
- **`If-None-Match`:** opcional. Lleva el `ETag` de la última política válida que el sidecar tiene guardada.

El sidecar pide la política cada `control_plane.policy_refresh`, con jitter y backoff. La petición nunca va en el camino de una petición del agente (ADR 0001).

## Respuestas

| Código | Cuándo | Cuerpo |
| --- | --- | --- |
| `200` | Hay un documento distinto del de `If-None-Match`, o no se envió `If-None-Match` | El documento (abajo) |
| `304` | `If-None-Match` coincide con el `ETag` vigente para la `schema_version` servida | Vacío |
| `401` | Llave ausente, desconocida, revocada o caducada | Error JSON corto, sin detalles de la llave |
| `426` | `X-Sidecar-Schema-Version` más vieja que N-2 respecto a la versión actual del control | Error JSON corto |

Cabeceras de `200` y `304`:

- `ETag: "<schema_version>:<version>"`: la `schema_version` servida y el campo `version`, entre comillas. Así, un mismo contenido servido en dos versiones de formato tiene dos `ETag` distintos.
- `Cache-Control: private, no-cache`: se puede guardar, pero hay que revalidarla con `If-None-Match` antes de darla por actual.
- `Vary: Authorization, X-Sidecar-Schema-Version`.

Un cliente sin política calculada recibe `200` con un documento vacío (`models: {}`) y una `version` estable. El sidecar lo trata como "sin política": las decisiones quedan `no_policy`.

Ante `426`, el sidecar deja de pedir la política hasta reiniciarse, como hace la ingesta, y sigue con la que tiene guardada.

## Compatibilidad (regla 11)

- **Versión propia:** `schema_version` del documento es un entero propio de la política, independiente del `schema_version` del envelope de ingesta. Empieza en `1`.
- **Negociación:** el control sirve `min(X-Sidecar-Schema-Version, N)`, siendo N su versión actual.
  - Un sidecar más nuevo que el control recibe la versión N del control, que entiende por la regla de compatibilidad. No recibe `426`.
  - Un sidecar que pide N-1 o N-2 recibe un documento de esa versión, sin los campos que esa versión no conoce.
  - Solo un sidecar más viejo que N-2 recibe `426`.
- **Campos nuevos:** son siempre opcionales. Un campo existente nunca cambia de significado.
- **Lectura en el sidecar:**
  - El sidecar rechaza un documento con `schema_version` mayor que la que soporta, y conserva la política anterior.
  - Ignora los campos que no conoce.
- **Sentido de los campos nuevos:** un campo nuevo solo puede reducir el enrutado. Un sidecar viejo que lo ignora nunca enruta de más por no entenderlo.
- **Valores nuevos de un enum:** el sidecar valida los enums al decodificar, así que un valor nuevo sube `schema_version` y el control no lo sirve a las versiones anteriores.
- **Versiones:**
  - `1`: la inicial.
  - `2`: añade `fp4` a `quantization`. Al servir la versión 1, el control quita los candidatos `fp4` y presenta un `current` `fp4` como `int4`, el valor con el que empata. Como `current` es solo referencia, un sidecar de la versión 1 ve elegibles los candidatos `int4` o mejores, igual que la versión 2 sin los `fp4`. No se presenta como `unknown`, porque eso haría elegibles a los candidatos `unknown`.

## Documento

Ejemplo **ficticio** (modelo, endpoint, importes y latencias inventados para ilustrar el formato):

```json
{
  "schema_version": 2,
  "version": "p_3f9a1c…",
  "computed_at": "2026-10-29T14:00:00Z",
  "flags": {},
  "models": {
    "deepseek-v4.1-flash": {
      "aliases": {
        "together": ["deepseek-ai/DeepSeek-V4.1-Flash"],
        "openrouter": ["deepseek/deepseek-v4.1-flash"]
      },
      "client_ttft_p95_ms": 900,
      "client_tps_p50": 60.5,
      "ref_usage": {
        "tokens_in": 1800,
        "tokens_out": 350,
        "tokens_reasoning": 0,
        "tokens_cached_read": 600,
        "tokens_cached_write": 0
      },
      "route_enabled": true,
      "current": [
        {
          "provider": "together",
          "raw_model": "deepseek-ai/DeepSeek-V4.1-Flash",
          "endpoint_id": "together/deepseek-ai/DeepSeek-V4.1-Flash@fp8/standard/global/direct",
          "quantization": "fp8",
          "price": {
            "price_id": "4790",
            "input_per_m": 270000,
            "output_per_m": 1100000,
            "cached_in_per_m": 0,
            "cache_write_per_m": 0,
            "reasoning_out_per_m": 0,
            "per_request": 0,
            "channel_fee_bps": 0
          }
        }
      ],
      "candidates": [
        {
          "endpoint_id": "deepinfra/deepseek/deepseek-v4.1-flash@fp8/standard/global/openrouter",
          "provider": "deepinfra",
          "channel": "openrouter",
          "provider_model_name": "deepseek/deepseek-v4.1-flash",
          "quantization": "fp8",
          "caps": { "tools": true, "json_schema": true, "vision": false, "logprobs": false, "n": 1 },
          "context_length": 131072,
          "price": {
            "price_id": "4812",
            "input_per_m": 100000,
            "output_per_m": 400000,
            "cached_in_per_m": 25000,
            "cache_write_per_m": 0,
            "reasoning_out_per_m": 0,
            "per_request": 0,
            "channel_fee_bps": 550
          },
          "stats": { "uptime_1d": 0.998, "p90_ttft_ms": 640, "tps_p50": 72.0, "source": "openrouter" }
        }
      ]
    }
  }
}
```

### Nivel superior

| Campo | Tipo | Significado |
| --- | --- | --- |
| `schema_version` | entero | Versión del formato de este documento |
| `version` | string | Hash estable del contenido, sin `computed_at`: los mismos datos dan la misma `version`. Es también el `ETag` y el `policy_version` del evento (`docs/evento.md`) |
| `computed_at` | RFC 3339 UTC | Cuándo se calculó. Informativo: no entra en `version` |
| `flags` | objeto | Interruptores globales. En las `schema_version` 1 y 2 no hay ninguno definido y llega `{}`. Un flag futuro solo puede reducir el enrutado, y el sidecar ignora los que no conoce |
| `models` | objeto `{model_id: Modelo}` | Una entrada por modelo canónico (`models.id`). Un modelo ausente equivale a `unknown_model` |

### Modelo

| Campo | Tipo | Significado |
| --- | --- | --- |
| `aliases` | objeto `{provider_id: [string]}` | Strings de `model` con los que cada proveedor o canal conoce este modelo (`endpoints.provider_model_name`). El sidecar resuelve `raw_model` → `model_id` buscando el `raw_model` del agente en `aliases[upstream.provider]` |
| `client_ttft_p95_ms` | entero | p95 del primer token del cliente para este modelo en 7 días (`events_hourly`). `0` = sin datos y el filtro no se aplica (`decision.Thresholds.ClientTTFTP95`) |
| `client_tps_p50` | número | p50 de tokens por segundo del cliente en 7 días. `0` = sin datos (`decision.Thresholds.ClientTPSP50`). No es dinero |
| `ref_usage` | objeto | Uso de referencia con el que se ordenan los candidatos. Usa los mismos cinco contadores disjuntos del evento (`tokens_in`, `tokens_out`, `tokens_reasoning`, `tokens_cached_read`, `tokens_cached_write`) y se corresponde con `decision.Requirements.RefUsage` |
| `route_enabled` | bool | Interruptor por modelo. `false` hace que este modelo no se enrute: en `route` va al `upstream` del YAML con su motivo. En `shadow` sigue sirviendo para calcular `cost_best`. Si falta, el sidecar lo trata como `false` |
| `current` | lista de Actual, opcional | Solo referencia: los endpoints a los que resuelve cada alias directo, con su precio. Ver "Endpoint actual" abajo. **Nunca es enrutable** |
| `candidates` | lista de Candidato, **ordenada** | Endpoints del modelo, del más barato al más caro según `pricing.Cost(price, ref_usage)` (incluye `channel_fee_bps`). En empate, se ordenan por `endpoint_id`. El sidecar respeta este orden tras filtrar |

### Candidato

| Campo | Tipo | Significado |
| --- | --- | --- |
| `endpoint_id` | string | `endpoints.id` |
| `provider` | string | Proveedor que sirve la llamada (`endpoints.provider_id`). En canal `openrouter` es el proveedor que se fija con pinning |
| `channel` | `direct` \| `openrouter` | Canal de ejecución |
| `provider_model_name` | string | Lo que va en `model` hacia ese canal |
| `quantization` | `bf16` \| `fp16` \| `fp8` \| `int8` \| `int4` \| `fp4` \| `unknown` | Igual que `decision.Quantization` y el CHECK de `endpoints.quantization`. Orden del filtro "igual o mejor": `bf16` = `fp16` > `fp8` > `int8` > `int4` = `fp4`; `unknown` solo iguala a `unknown`. `fp4` agrupa FP4, MXFP4 y NVFP4. `fp4` existe desde la `schema_version` 2; ver "Compatibilidad" para lo que recibe un sidecar de la versión 1 |
| `caps` | objeto | `{tools, json_schema, vision, logprobs, n}`, igual que `endpoints.caps`, salvo un caso: `tools` llega a `false` si el endpoint no pasa el criterio de calidad de tool calls (ver "Calidad de tool calls") |
| `context_length` | entero | Ventana de contexto en tokens. `0` o ausente = desconocida |
| `price` | Precio | El precio de lista vigente en `computed_at`  |
| `stats` | Stats | Salud y latencia observadas |

### Precio

Importes en **microdólares `int64`** por millón de tokens, salvo `per_request` (por petición). Nunca `float` (regla 9). Los nombres son los de las columnas de `prices` y los campos de `pricing.Price`.

| Campo | Tipo | Significado |
| --- | --- | --- |
| `price_id` | string | `prices.id` en decimal: el precio con el que se calcula el coste. Viaja tal cual a `price_id_actual` o `price_id_best` del evento y no se recalcula |
| `input_per_m`, `output_per_m`, `cached_in_per_m`, `cache_write_per_m`, `reasoning_out_per_m` | entero | Tarifas. Un `0` en `cached_in_per_m`, `cache_write_per_m` o `reasoning_out_per_m` se cobra con la tarifa por defecto que fija `docs/metodologia-ahorro.md` |
| `per_request` | entero | Importe fijo por petición |
| `channel_fee_bps` | entero | Comisión del canal en puntos básicos, aplicada encima |

`image_per_k` no viaja: `pricing.Cost` no lo usa.

### Endpoint actual

`current` resuelve el problema de `cost_actual` en `shadow`: el endpoint al que resuelve el `upstream` puede no pasar los filtros de salud y no estar en `candidates`, pero su precio hace falta igual.

| Campo | Tipo | Significado |
| --- | --- | --- |
| `provider` | string | Proveedor directo (`endpoints.provider_id`, canal `direct`) |
| `raw_model` | string | Uno de los `aliases[provider]` del modelo |
| `endpoint_id` | string | El endpoint al que resuelve ese par |
| `quantization` | igual que en Candidato | La que el sidecar usa como referencia para el filtro "cuantización igual o mejor" |
| `price` | Precio | Precio de lista vigente de ese endpoint |

- **Cobertura:** el control incluye una entrada por par `(provider, raw_model)` de canal `direct` que resuelve a un único endpoint `enabled`, pase o no los filtros de salud. Si un par resuelve a varios endpoints (cuantización, tier o región distintos), no se publica y el sidecar se queda sin precio actual (`current_price_error`).
- **Uso en el sidecar:** busca la entrada con `provider = upstream.provider` y `raw_model` igual al del agente. La usa solo como endpoint actual (`endpoint_used`, `cost_actual`, `price_id_actual` y la referencia de cuantización).
- **Nunca enrutable:** `current` no se mezcla con `candidates`, no pasa por el plan de intentos de `route` y ningún código de decisión la lee como candidato. Por eso no se marca con un flag dentro de `candidates`: un sidecar N-2 que no conociera ese flag podría enrutar a un endpoint que no pasa los filtros.
- **Compatibilidad:** es un campo opcional. Un sidecar que no lo conoce lo ignora y se queda en `current_price_error`, que es lo seguro.

### Stats

| Campo | Tipo | Significado |
| --- | --- | --- |
| `uptime_1d` | número 0..1 | Fracción de éxito en el último día (`endpoint_stats.uptime_1d`, `decision.Stats.Uptime1d`) |
| `p90_ttft_ms` | entero | p90 del primer token. `0` = sin datos |
| `tps_p50` | número | Tokens por segundo, mediana. `0` = sin datos |
| `source` | `probe` \| `openrouter` | De dónde salen las cifras: sondas propias si hay muestras suficientes, si no la API de OpenRouter |

## Filtro en el sidecar (regla 8)

Antes de publicar una política, el sidecar elimina candidatos en dos pasos.

**En todos los modos (regla 8).** Elimina todo candidato que cumpla una de estas condiciones:

- su `channel` no está en `routing.allowed_channels`;
- su `provider` no está en `routing.allowed_providers`.

**Solo en `route`.** Elimina además todo candidato cuyo canal no tenga credenciales en el YAML:

- en `direct`, que `providers.<provider>` no tenga `api_key` o no tenga `base_url` (ADR 0008);
- en `openrouter`, que `providers.openrouter` no tenga `api_key`.

En `shadow` no se exigen credenciales: el sidecar no llama a esos endpoints, solo calcula `cost_best`. Así `cost_best` compara con los endpoints que el cliente permite aunque aún no tenga llave para ellos, como en el paso 3 de `docs/metodologia-ahorro.md`. La restricción a `allowed_providers` y `allowed_channels` sí se aplica también en `shadow`: el ahorro simulado solo compara con lo que el cliente permitiría enrutar.

El filtro conserva el orden y cuenta los descartes por motivo. Los umbrales del YAML (`routing.min_uptime_1d`, `routing.max_ttft_p95_ms`) se aplican encima y también solo pueden quitar candidatos. Nada de la política puede añadir un endpoint, un proveedor, un canal, una URL o una llave que el YAML no tenga.

El reparto de filtros es este:

- **Plano de control.** Aplica los filtros que no dependen de la petición, con las reglas de `internal/decision`: disponibilidad, p90 frente a `client_ttft_p95_ms` y `tps_p50` frente a `client_tps_p50`. Además decide el criterio de calidad de tool calls y lo expresa en `caps.tools` (ver "Calidad de tool calls").
- **Sidecar.** Aplica los que sí dependen de la petición, con `decision`: `caps`, contexto y cuantización igual o mejor que la del endpoint actual.

Lo que la petición exige se lee de su cuerpo sin guardar ni registrar contenido (regla 1):

- **Visión:** se exige si algún mensaje tiene una parte de contenido de tipo `image_url`. Solo se lee el `type` de cada parte.
- **Contexto:** es una estimación conservadora, `bytes del cuerpo / 4 + max_completion_tokens` (o `max_tokens` si no viene el primero). Solo se usa la longitud del cuerpo. El marco JSON suma bytes, así que la estimación suele quedar por encima de los tokens reales. Un candidato con `context_length` desconocido (`0`) no pasa este filtro.

Si ningún candidato pasa, la petición sigue en el upstream del YAML, como con cualquier otro filtro.

### Calidad de tool calls

El plano de control mide, con sondas sintéticas propias, si cada endpoint responde bien a las llamadas a herramientas. Las sondas usan un conjunto fijo y versionado de 3 herramientas con esquema JSON y 5 prompts inventados. Nunca usan prompts ni respuestas de clientes. Una respuesta es válida si cumple todo esto:

- trae al menos una tool call;
- cada llamada nombra una herramienta del conjunto;
- sus `arguments` son un string JSON con un objeto que valida contra el esquema de esa herramienta;
- `finish_reason` es `tool_calls`.

El agregado es `toolcall_ok_rate_7d`: las respuestas válidas entre las evaluadas en los últimos 7 días. Un error de transporte o un estado distinto de `200` no cuenta como muestra.

- **Criterio:** `toolcall_ok_rate_7d ≥ umbral`. El umbral es configuración del plano de control y por defecto vale 0,95. La comparación se hace con enteros (`ok × 10.000 < umbral_bps × muestras`), sin `float`.
- **Muestras mínimas:** 10 respuestas evaluadas en 7 días (dos pasadas completas del conjunto). Con menos, o sin datos recientes, el endpoint queda **sin datos de calidad** y el criterio **no lo excluye**, igual que pasa con las sondas de latencia.
- **Solo para peticiones con `tools`:** un candidato por debajo del umbral se publica con `caps.tools = false`. El filtro de `caps` del sidecar lo quita entonces de las peticiones que declaran `tools` y lo deja para las demás: el filtro lo descarta por `caps_tools`.
- **Sin campos nuevos:** el documento no cambia ni sube `schema_version`, así que todas las versiones servidas y los sidecars anteriores aplican el criterio. `caps.tools` mantiene su sentido de "este candidato puede atender peticiones con `tools`", y el criterio solo puede pasarlo de `true` a `false`, nunca al revés (regla de "Sentido de los campos nuevos").
- **Oscilación:** un endpoint cerca del umbral puede entrar y salir del criterio de una hora a otra, y cada cambio de `caps.tools` cambia `version` y el `ETag`, así que el sidecar descarga la política otra vez. Es el mismo efecto que tienen ya los filtros de salud. No hay histéresis por ahora; si la rotación molesta, se añadirá en el cálculo, sin cambiar el documento.
- **Dónde se ve:** el panel y el comparador público muestran la tasa por endpoint, o "—" cuando el endpoint está sin datos de calidad.

## Política vieja

- **Conservación:** el sidecar guarda en disco la última política válida y la conserva **indefinidamente**. No caduca por edad (ADR 0001). Si el control cae o responde con errores, sigue decidiendo con ella.
- **Validez:** una respuesta inválida nunca reemplaza a una política válida.
- **Edad:** son los segundos desde la última respuesta válida del control (`200` o `304`) para la política activa. Un `304` la renueva aunque `computed_at` sea antiguo, porque `version` solo cambia cuando cambia el contenido. La edad se publica en dos sitios:
  - En el heartbeat de la ingesta, como campo opcional nuevo `policy_age_s`, junto a `policy_version`. Lo envía `internal/ingest` y está en el envelope de `docs/evento.md`.
  - En `/metrics`, como `sidecar_policy_age_seconds`.
- **Sin política:** sin ninguna política, `policy_version` va vacío, `policy_age_s` se omite y la métrica no se expone.

## Cómo se verifica

- **Tipos y filtro** (`internal/policy`):
  - `Decode` rechaza una `schema_version` no soportada, importes no enteros y `fp4` en un documento que declara la versión 1.
  - `Filter` descarta por canal y proveedor en todos los modos, y por credenciales solo en `route`. Conserva el orden.
  - Ningún candidato sale de `current`: un test comprueba que el plan de `route` no contiene nunca un endpoint que solo esté en `current`.
  - Un test por reflexión comprueba que ningún tipo tiene campos de URL ni de llave.

- **Endpoint** (plano de control): tests de handler para `200`, `304`, `401`, `426` (sidecar más viejo que N-2), sidecar más nuevo (recibe N), `ETag` con `schema_version`, documento vacío y N-2. Un documento con `fp4` servido en la versión 1 pasa el decodificador anterior sin `fp4`, y en la 2 lo conserva, con un `ETag` distinto por versión.
- **Descarga y caché en el sidecar** (`TestSaveLoadCache`, `TestRefreshStatuses` en `internal/policy`): caché en disco, control caído, `304` que renueva la edad y política inválida que no reemplaza a la buena.
