# Metodología del ahorro

Se publica tal cual al cliente. Si cambia, cambia la versión al final y se avisa.

## Ahorro simulado (modo `shadow`)

Para cada petición:

1. **Modelo canónico.** Se resuelve `raw_model` a `model_id` a través de `endpoints.provider_model_name`. Sin resolución, la petición no cuenta (se reporta aparte como "modelo desconocido").
2. **Requisitos.** De la petición se derivan `requirements` (herramientas, `response_format`, visión, contexto, `n`). Solo compiten endpoints cuyas `caps` los cubren. Si la petición declara herramientas, tampoco compite un endpoint cuya tasa de tool calls válidas en 7 días, medida con sondas sintéticas, esté por debajo del umbral (0,95 por defecto) con al menos 10 muestras. Un endpoint sin datos de calidad sí compite. La política lo expresa con `caps.tools = false` (`docs/politica.md`, "Calidad de tool calls").
3. **Candidatos.** Mismo `model_id`; cuantización igual o mejor que la del endpoint usado (`bf16` = `fp16` > `fp8` > `int8` > `int4` = `fp4`; `unknown` solo iguala a `unknown`); `uptime_1d` ≥ 99%; p90 de primer token ≤ p95 del cliente para ese modelo en los últimos 7 días; `tps` p50 ≥ el del cliente.
4. **Coste del mejor.** `cost_best = tokens_in × input + tokens_out × output + tokens_reasoning × reasoning_out + tokens_cached_read × cached_in + tokens_cached_write × cache_write + per_request`, multiplicado por `(1 + channel_fee_bps / 10.000)` si el endpoint se ejecuta vía OpenRouter, con el uso real de la petición, el precio vigente en el instante de la petición (`price_id`) y los precios negociados del cliente si existen. Los cinco contadores de tokens son disjuntos (`docs/evento.md`): `tokens_in` no incluye los `tokens_cached_read` y `tokens_out` no incluye los `tokens_reasoning`, así que cada token se cobra una sola vez. Una tarifa `reasoning_out` a 0 se cobra como `output`; `cached_in` y `cache_write` a 0 se cobran como `input`. Se redondea una sola vez, al microdólar, hacia arriba en la mitad.
5. **Ahorro.** `max(0, cost_actual − cost_best)`. Si el cliente ya está en el más barato, cero. Si no hay decisión posible (modelo desconocido, sin política o sin precio vigente para el mejor endpoint), `cost_best = cost_actual`, ahorro cero y el motivo queda explícito.

El informe muestra el porcentaje, el total en dólares, y la lista de candidatos descartados con el motivo.

### Motivos del ahorro simulado

Cada petición lleva exactamente uno de estos motivos (`savings.Simulated`). Solo `cheaper_endpoint` puede dar un ahorro distinto de cero; en todos los demás `cost_best = cost_actual` y el ahorro es cero. "Cuenta" indica si la petición entra en los totales.

| Motivo | Cuenta | Significado |
|---|---|---|
| `cheaper_endpoint` | sí | El mejor endpoint es otro distinto del usado y tiene precio vigente; `cost_best` se calcula con ese precio y el ahorro es `max(0, cost_actual − cost_best)`. |
| `current_is_cheapest` | sí | El mejor endpoint es el que ya se usó: el cliente ya está en el más barato. |
| `no_candidate_passes` | sí | Ningún candidato cumple los requisitos, la cuantización, la disponibilidad y la latencia del paso 3; se queda el endpoint usado. |
| `current_price_error` | sí | No hay precio vigente (o válido) para el endpoint usado, así que no se puede comparar con ningún candidato; se queda el endpoint usado. |
| `no_policy` | sí | No hay política ni decisión para la petición. |
| `no_price` | sí | El mejor endpoint no tiene precio vigente y válido en el instante de la petición. |
| `unknown_model` | no | `raw_model` no se resolvió a un `model_id` (paso 1); se reporta como "modelo desconocido". |
| `usage_source_none` | no | La petición no tiene uso medido (ver "Lo que no se cuenta"). |
| `passthrough` | no | La petición se hizo en modo `passthrough`. |
| `agent_error_4xx` | no | El agente recibió un `status` 4xx. |

El paquete declara además `no_previous_endpoint` (petición sin endpoint previo) para el ahorro real, pero ni `savings.Simulated` ni el cálculo del ahorro real lo devuelven hoy: el ahorro real cuenta esas peticiones como "sin precio" sin motivo por petición.

## Ahorro real (modo `route`)

`cost_actual` del mes (sumando todos los intentos, incluidos los cancelados) contra el coste que habrían tenido las mismas peticiones en el endpoint que el cliente usaba antes, con los precios vigentes de cada día (el precio en vigor a las 00:00 UTC del día de la petición, negociado si existe). Se agrega por cliente y modelo canónico; el mes y el día son UTC. Solo cuentan peticiones en modo `route`; las que no tienen endpoint previo o precio para ese día se cuentan aparte como "sin precio" y no entran en ninguno de los dos totales. El ahorro real es `coste_previo − cost_actual` y puede ser negativo: no se recorta a cero. Se compara con el simulado del mes anterior; una diferencia mayor de 5 puntos obliga a revisar las fuentes de latencia (sondas propias frente a las del agregador) y queda documentada aquí.

## Estado de la implementación

Las fórmulas de arriba están implementadas como funciones puras en `internal/pricing` (coste en microdólares), `internal/decision` (filtros y elección del paso 3) e `internal/savings` (ahorro simulado), con sus tests junto al código.

En el sidecar, modo `shadow` (`internal/proxy/decide.go`):

- Sin política activa, el evento lleva `cost_actual = cost_best = 0` y `decision_reason = no_policy`.
- Con política (descargada de `GET /v1/policy` y filtrada por el YAML del cliente), el paso 1 resuelve `raw_model` con los `aliases` del `upstream.provider` que trae la política. Si no se resuelve, el motivo es `unknown_model`. El endpoint usado es el `current` de la política para ese par. Si no tiene precio, el motivo es `current_price_error` y los costes quedan en 0.
- Los candidatos se filtran y ordenan con el `ref_usage` del modelo antes de reenviar la petición. El ganador va a `endpoint_best` y a `X-Sidecar-Decision`.
- Con el uso real ya medido, `cost_actual` se calcula con el precio del `current` y `cost_best` con `savings.Simulated`, cada uno con su `price_id`.
- Los precios son los de lista que trae la política: el sidecar todavía no aplica precios negociados.

Lo comprueban `TestShadowPolicyDecision` (`internal/proxy/decide_test.go`) y, de extremo a extremo, `TestEndToEndShadowPolicy` (`test/e2e/policy_test.go`). En este último, un plano de control falso sirve la política, el evento ingerido lleva `endpoint_best`, `cost_best > 0` y `policy_version`, y el segundo refresco recibe `304`.

## Lo que no se cuenta

Peticiones con `usage_source = none`; peticiones en `passthrough`; peticiones con error del agente (`status` 4xx devuelto al agente).

Versión de la metodología: 5 (2026-11-05).

Cambios de la 3 (2026-10-29) desde la 2: se enumeran los motivos del ahorro simulado; las fórmulas no cambian.

Cambios de la 2 (2026-10-28) desde la 1: el coste incluye `tokens_cached_write × cache_write` y las tarifas por defecto de `pricing`; se precisan los casos sin decisión, el precio de cada día del ahorro real y su signo.
