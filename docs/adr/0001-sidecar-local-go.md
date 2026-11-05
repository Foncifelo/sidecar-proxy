# 0001 — Un binario en Go como sidecar local; plano de control fuera del camino

- Estado: aceptado
- Fecha: 2026-10-28

## Contexto

El cliente corre agentes en Python o TypeScript contra proveedores OpenAI-compatibles. La integración debe costar un cambio de `base_url`; la empresa no posee ni reserva GPU.

## Decisión

Un binario único en Go (`cmd/sidecar`) hace de proxy local OpenAI-compatible en `127.0.0.1:8787`, registra metadatos, decide y reenvía. Un plano de control pequeño (`cmd/control`, `cmd/collector`, Postgres gestionado) recoge precios y latencias y sirve el panel, y nunca está en el camino de una petición. Un lenguaje para todo el servidor (panel con `templ` + HTMX). Postgres para todo al inicio. Shadow mode es el primer producto.

## Alternativas consideradas

- Rust para el proxy — el cuello de botella es datos y confianza, no rendimiento; duplica el tiempo.
- SDK por lenguaje — hay que mantener dos librerías; el proxy local cubre ambos sin código del cliente.
- Gateway de terceros como base (LiteLLM, Portkey) — el producto es el gateway; no se puede ser neutral encima de otro.
- Kafka/NATS, ClickHouse, gRPC, plugins, operador K8s — no cambian el núcleo si llegan después; consumen semanas que no hay.

## Consecuencias

Si el plano de control cae, el sidecar sigue con la última política guardada y acumula eventos en disco. El sidecar es código abierto (Apache 2.0) para que el cliente verifique que sus llaves no salen. Queda prohibido añadir infraestructura nueva sin un ADR.

## Cómo se verifica

`CONTRIBUTING.md` reglas 4 y 12; `forbidigo` en `.golangci.yml`.
