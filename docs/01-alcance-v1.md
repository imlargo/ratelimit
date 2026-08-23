# 01 — Alcance de la v1

Objetivo declarado por el autor: **una librería que sirva para el 99% de los
casos de rate limiting en APIs backend en Go.** Este documento es el corte.

El criterio del corte no es "qué es difícil" sino **qué usa el 99%**. Todo lo
recortado sigue siendo posible después porque la espina de la arquitectura
(§02) no cambia: una sola representación de estado, `int64`. Lo aplazado se
añade encima, nunca reescribiendo.

---

## Dentro de la v1

| Capacidad | Por qué está en el 99% |
|---|---|
| GCRA, un `int64` por clave, CAS, sin mutex por clave | es el motor; no hay versión más pequeña |
| Almacén de capacidad fija, expulsión segura O(1), **cero goroutines de fondo** | el fallo de producción más frecuente del ecosistema |
| Clave por hash, sin cadena intermedia | cero asignaciones por decisión |
| Selectores con sintaxis de `ServeMux`, matcher propio | reglas distintas por endpoint es el caso 2 más común |
| Varias reglas a la vez, gana la más restrictiva, **con devolución** | la devolución cuesta una resta; no ahorrarla sería gratuito y erróneo |
| `Decision` tipada + cabeceras draft-11 + `Retry-After` con jitter de un lado | es el producto |
| **Modo sombra** por regla | nadie activa un limitador sobre tráfico real a ciegas. Se usa una vez, es imprescindible esa vez, y cuesta un motivo |
| Extracción de IP segura, proxies de confianza obligatorios | sin esto el limitador se elude con una cabecera |
| Cuota resuelta desde la identidad (planes/tiers) | cualquier API con más de un tipo de cliente |
| Exenciones declaradas | health checks; limitarlos es cómo un servicio se cae solo |
| Middleware `net/http` | el 90% de los casos |
| `slog` + struct de métricas nil-safe | operar y depurar |
| `ratelimittest` | si probar la librería es difícil, nadie la prueba |
| **Redis: local decide + sync periódico de deltas, cota publicada** | casi toda API en Go corre más de una réplica |
| **Degradación permisiva y declarada** | sale gratis si el local siempre decide |

### Por qué el modo distribuido no se puede recortar del todo

Un limitador por proceso con 4 réplicas y "100/min" configurado permite 400/min.
Es un 4× **silencioso**, exactamente el pecado que la librería dice combatir.

La v1 lo resuelve del modo más simple que sigue siendo honesto: el local decide
ya, publica su delta en segundo plano y recibe el total global. Sobrepaso
acotado por `N × tasa × T` y publicado. Y si no configuras backend, la librería
**te dice** que solo puede enforcear por proceso, en vez de dejar que lo
descubras.

---

## Aplazado a v1.x

No es "descartado": es "no lo pide el 99% y cada uno de estos multiplica la
superficie conceptual".

| Aplazado | A quién le hace falta | Coste de aplazarlo |
|---|---|---|
| **Leases**: concurrencia y coste reconciliado (`context/03` E1–E4) | APIs que cobran por tokens de un modelo o por bytes | ninguno: se añade como un tipo de regla más sobre el mismo `int64` |
| **Reparto proporcional a la demanda** (Doorman/FPS) | quien tenga una clave caliente martilleada desde muchos nodos | la cota `N × tasa × T` es peor, pero está publicada |
| **Modo exacto por regla** (`context/03` D8–D9) | quien no tolere sobrepaso en una regla concreta | pone un round-trip en la ruta de decisión; nadie del 99% lo quiere |
| **Recarga de reglas en caliente** (RF-B11) | configuración dinámica | ya era `[DES]` |
| **Adaptadores Gin/Fiber/Echo** | quien use esos frameworks | todos aceptan `net/http`; el adaptador es azúcar |
| **Contador de ventana deslizante** como algoritmo alternativo | quien necesite "nunca más de N en ventana móvil" | el conjunto es cerrado y ampliable sin romper nada |
| **Predicados por cabecera/tenant** (A-06) | selección que no es ruta ni método | se expresa como dimensión de clave |

---

## Fuera, definitivamente

Los no-objetivos de `context/01` se mantienen enteros y van al README en voz
alta: rate limiting saliente, encolado de peticiones entrantes, cuota exacta
transaccional, load shedding y limitación adaptativa, detección de abuso,
DDoS volumétrico, ser un servicio autónomo.

---

## Fases resultantes

Cinco, no ocho. La puerta de calidad de `context/05` aplica a todas sin cambios
(detección de fugas de goroutines, `synctest` para todo lo temporal, cero
`time.Sleep`, `-race`, presupuestos de asignación que fallan la build).

| # | Fase | Hito |
|---|---|---|
| 1 | Motor: GCRA, almacén, clave | tabla de traducción config→comportamiento; test de contención; 0 allocs |
| 2 | Reglas, selectores, composición con devolución, exenciones, cuota por identidad | devolución verificada contra el contador de la regla general |
| 3 | `Decision`, cabeceras, middleware, sombra | **`v0.1.0` — usable en producción en un nodo** |
| 4 | Seguridad de la identidad (IP y proxies) | vectores de suplantación. **No se publica IP sin esta fase** |
| 5 | Redis: sync de deltas y degradación declarada | con el backend caído, el camino de código es el del nodo único |

Documentación y ejemplos no son una fase: se escriben con cada una. El README
con garantías, cotas y no-objetivos existe **desde el primer commit**, porque
es la propuesta de valor y no un anexo.
