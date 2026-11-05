# 0011 — `base_url` en `http` solo para loopback y solo en `dev` o `staging`

- Estado: aceptado
- Fecha: 2026-10-29
- Reemplaza / reemplazado por: — (complementa 0008; no lo reemplaza)

## Contexto

ADR 0008 fija que `providers.<id>.base_url` es solo `https`. La demo necesita enseñar `route` con fallback entre varios proveedores sin llaves reales. El generador de demo levanta proveedores falsos en la propia máquina, en `http://127.0.0.1:<puerto>`. Con la regla de 0008, el sidecar los rechaza al arrancar. La única forma de mostrar `route` hoy es contra OpenRouter real, lo que exige llave y presupuesto.

Servir los falsos con TLS obligaría a añadir al sidecar configuración de CA de confianza. Eso agranda la superficie de configuración de producción solo para una demo.

## Decisión

El sidecar acepta `base_url` con esquema `http` solo si se cumplen las dos condiciones:

1. El host es exactamente `127.0.0.1`, `::1` o `localhost`. No vale ningún otro nombre ni IP, y no se resuelve DNS para decidirlo.
2. La variable de entorno `SIDECAR_ENV` vale exactamente `dev` o `staging`. Si está vacía, falta o vale otra cosa, la regla de 0008 aplica sin cambios: solo `https`.

Cuando se usa la excepción, el sidecar lo dice una vez en el log de arranque, con el id del proveedor y sin la URL completa.

## Alternativas consideradas

- **TLS con CA propia en la demo:** añade `ca_file` a la configuración de producción solo para la demo; descartada.
- **Permitir `http` a cualquier host fuera de producción:** abre la puerta a enviar llaves en claro por la red; descartada.
- **Usar los falsos solo como `upstream`, que ya admite `http`:** no permite fallback entre varios candidatos, que es lo que se quiere enseñar.

## Consecuencias

- Lo bueno: la demo de `route` con fallback funciona sin llaves ni gasto.
- Lo malo: hay un caso más en la validación de configuración. Un `SIDECAR_ENV=dev` olvidado en una máquina real permitiría `http` hacia loopback. El riesgo está acotado porque el tráfico nunca sale de la máquina.
- Queda prohibido aceptar `http` hacia cualquier host que no sea loopback, en cualquier entorno, y deducir el entorno de otra cosa que no sea `SIDECAR_ENV`.
- La regla 8 no cambia: la política sigue sin poder añadir URLs; la excepción solo afecta a URLs escritas en el YAML del cliente.

## Cómo se verifica

Test de tabla en `internal/config` que cubre:

- `http://127.0.0.1`, `http://[::1]` y `http://localhost`, aceptados con `SIDECAR_ENV` igual a `dev` y a `staging`;
- los mismos rechazados con `SIDECAR_ENV` vacío, igual a `production` o igual a `DEV`;
- `http://10.0.0.1`, `http://example.com` y `http://localhost.example.com` rechazados en todos los entornos;
- `https` sin cambios.

Más un test de que el log de arranque menciona la excepción sin la URL.
