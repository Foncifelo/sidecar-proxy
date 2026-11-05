# 0008 — En `route` solo se reescribe el `model` de nivel superior; cabeceras por allowlist; `base_url` de cada proveedor sale del YAML

- Estado: aceptado
- Fecha: 2026-10-29
- Reemplaza / reemplazado por: — (complementa 0003; no los reemplaza)

## Contexto

Antes de implementar `route` hay tres huecos entre el contrato, las reglas y el código.

1. **El `model` de la respuesta.** ADR 0003 decide que "el agente recibe exactamente el `model` que envió". En `route`, el sidecar reescribe `model` hacia el proveedor (`provider_model_name`), y el proveedor contesta con su propio nombre en el JSON y en cada chunk SSE. Si los bytes se copian tal cual, el agente ve un `model` que no envió. Pero ADR 0003 descarta "reescribir `model` en cada chunk" (para enseñar el proveedor real) porque rompe la copia de bytes. La regla 6 de `CONTRIBUTING.md` dice además "no reescribas chunks del cuerpo". Las dos cosas a la vez no se pueden cumplir.
2. **Las cabeceras del agente.** La garantía 1 de `docs/contrato-proxy.md` dice que "el cuerpo y las cabeceras del agente llegan al proveedor sin alterar" salvo excepciones. La implementación (`internal/providers/headers.go`) reenvía solo una allowlist: `Accept`, `Content-Type` y `User-Agent`. El estado de la garantía 1 lo marca como discrepancia abierta. OpenRouter acepta además `HTTP-Referer` y `X-Title` para atribuir el tráfico a una aplicación.
3. **El origen de `base_url`.** `providers.Endpoint` necesita `BaseURL` para cada intento, pero `ProviderConfig` en `internal/config` solo tiene `api_key`. El catálogo del plano de control tiene `providers.base_url` y `providers.auth_header`. La regla 8 prohíbe que la política añada URLs.

## Decisión

**1. `model` en la respuesta (`route`).** El sidecar reescribe solo el campo `model` de nivel superior del objeto JSON de la respuesta sin stream, y el de cada chunk SSE, al string exacto que envió el agente (`raw_model`). El resto de bytes no cambia: ni espacios, ni orden de claves, ni escapes, ni otros campos (tampoco un `model` anidado). Reglas de borde:

- Una respuesta o un chunk sin campo `model` de nivel superior pasa intacto. No se añade el campo.
- Un `data:` que no es un objeto JSON (`[DONE]`, comentarios, JSON inválido) pasa intacto.
- Las respuestas de error del proveedor siguen la misma regla.
- El chunk de `usage` que se conserva (garantía 6) también se reescribe.
- En `shadow` y `passthrough` no se reescribe nada: la respuesta llega byte a byte.
- `/v1/embeddings`, `/v1/responses` y el resto de rutas no se enrutan (contrato, superficie), así que tampoco se reescriben.

Como la respuesta al agente es siempre `chunked` (garantía 7), cambiar la longitud del valor no obliga a recalcular `Content-Length`. La verdad sobre quién sirvió sigue en `X-Sidecar-Provider` y `X-Sidecar-Endpoint`, y en `provider_served` del evento.

**2. Cabeceras del agente.** Se mantiene la allowlist: de las cabeceras del agente solo llegan al proveedor `Accept`, `Content-Type` y `User-Agent`. Nada más se reenvía, en ningún modo ni canal. El proveedor recibe además estas cabeceras, que pone el adaptador:

- La de autenticación, con la llave del YAML.
- `Accept-Encoding: identity`.
- Solo en el canal `openrouter`, `HTTP-Referer` y `X-Title`, si y solo si el YAML los define (`providers.openrouter.http_referer` y `providers.openrouter.x_title`). Nunca se toman del agente.

**3. `base_url` por proveedor.** Cada entrada de `providers` del YAML admite:

- `api_key`: lo que ya existe.
- `base_url`: base OpenAI-compatible, solo `https`. Es obligatoria para todo proveedor directo que `route` pueda usar.
- `auth_header`: opcional, nombre de la cabecera que lleva la llave. Si falta, se usa `Authorization: Bearer <llave>`. Si existe, la llave va sin prefijo en esa cabecera.

Hay un único valor por defecto incorporado en el binario: `base_url` de `openrouter` (`https://openrouter.ai/api/v1`). Ningún otro proveedor tiene `base_url` por defecto, y no se hereda de `upstream.base_url` aunque el proveedor coincida. En modo `route`, una entrada de `providers` distinta de `openrouter` que tenga llave y no tenga `base_url` es un error de configuración al arrancar. En `shadow` y `passthrough` no se exige.

La política descargada nunca lleva URLs, cabeceras ni llaves (regla 8, `docs/politica.md`). Las columnas `providers.base_url` y `providers.auth_header` del catálogo del plano de control sirven al collector (lectura de precios y sondas) y nunca viajan al sidecar.

## Alternativas consideradas

- **Copiar la respuesta byte a byte también en `route`.** El agente vería el nombre del proveedor (`meta-llama/...`, `accounts/fireworks/...`). Rompe la decisión central de ADR 0003 y la observabilidad del cliente, que agrupa por el `model` que envió. Se descarta.
- **Reescribir `model` con un parser JSON completo y volver a serializar.** Cambia espacios, orden y escapes en todos los chunks, y cuesta una asignación por chunk en el camino caliente. Se descarta: se reemplaza solo el valor del token, sin tocar el resto.
- **Reenviar todas las cabeceras del agente salvo una denylist (`Authorization`, `Cookie`, …).** Una denylist siempre llega tarde. Cualquier cabecera nueva del SDK del agente (con identificadores, trazas o tokens propios) saldría hacia un tercero. Contradice ADR 0006. Se descarta.
- **Aceptar `HTTP-Referer`/`X-Title` del agente.** El agente podría atribuir tráfico a otra aplicación con la llave del cliente. Además abre una excepción a la allowlist que habría que mantener. Se descarta: el YAML ya es la fuente de verdad del cliente.
- **Un mapa de `base_url` por defecto en el binario para los proveedores conocidos.** Una URL que el cliente no escribió acabaría recibiendo su llave. Además, cada cambio de URL de un proveedor obligaría a sacar una release. Solo OpenRouter tiene un valor por defecto, porque es el canal del que depende `route` y su URL es estable. Se descarta para el resto.
- **Que el plano de control envíe `base_url` en la política.** Viola la regla 8: un plano de control comprometido podría redirigir las llaves del cliente. Se descarta.

## Consecuencias

- **Lo bueno:**
  - El agente ve siempre su `model`, también en `route`.
  - El contrato describe lo que hace el código: la allowlist pasa de "discrepancia abierta" a garantía.
  - Todas las URLs y cabeceras de salida se leen en el YAML del cliente, que puede auditarlo.
- **Lo malo:**
  - En `route` la copia ya no es byte a byte. El cambio se limita al valor de un campo, pero hay que probarlo con fuzz.
  - El cliente tiene que escribir `base_url` de cada proveedor directo que quiera usar en `route`.
- **Queda prohibido:**
  - Reescribir cualquier otro campo del cuerpo de la respuesta.
  - Añadir un campo `model` donde no lo había.
  - Reescribir en `shadow` o `passthrough`.
  - Ampliar la allowlist de cabeceras sin un ADR nuevo.
  - Leer de la política o del catálogo remoto URLs, cabeceras o llaves.
- **ADR 0003.** Su alternativa descartada ("reescribir `model` en cada chunk para que el agente vea el proveedor real") sigue descartada: aquí se reescribe hacia el `model` del agente, no hacia el del proveedor. La consecuencia "rompe la copia de bytes" se acepta solo para el valor de ese campo en `route`. 0003 no se reescribe ni se reemplaza.
- **`CONTRIBUTING.md`, regla 6.** Queda así:
  > 6. El agente recibe el `model` que envió: en `route` solo se reescribe el campo `model` de nivel superior de la respuesta y de cada chunk (ADR 0008), nada más del cuerpo. La verdad va en cabeceras `X-Sidecar-*`.
- **`docs/contrato-proxy.md`.** Las garantías 1 y 2 se alinean con este ADR.

## Cómo se verifica

- **`internal/config`.** Tests de tabla:
  - En `route`, un proveedor directo con llave y sin `base_url` falla.
  - Un `base_url` `http://` falla.
  - `openrouter` sin `base_url` toma el valor por defecto.
  - Todos los errores se reportan a la vez.
  - Las llaves siguen enmascaradas en `%v`, `%#v`, JSON y YAML.
- **`internal/policy`.** Un test comprueba que ningún `providers.Endpoint` del plan tiene un host que no esté en el YAML.
- **`internal/proxy`.**
  - Respuesta sin stream en `route`: el `model` de nivel superior es el del agente y el resto de bytes es idéntico al del proveedor.
  - Hacia el proveedor solo salen de las cabeceras del agente las de la allowlist (`TestOnlyAllowlistedInboundHeadersReachProvider` se amplía a `route`).
  - `HTTP-Referer` y `X-Title` aparecen solo si el YAML los define, y nunca se copian del agente.
- **`internal/sse` y `pkg/openai`.**
  - Fuzz y tests de tabla: solo cambia el valor del `model` de nivel superior de cada chunk.
  - Los chunks sin `model`, el `model` anidado, `[DONE]` y el JSON inválido pasan intactos.
- **e2e de `route`.** La garantía 2 con su test, sin `Authorization` del agente en la petición saliente.
