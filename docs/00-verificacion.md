# 00 — Verificación del contexto

Contraste de los hechos de `context/02` y de las afirmaciones de `context/00`
contra la fuente, en agosto de 2026. Es la base de evidencia del README: las
cotas y la comparación con las alternativas se citan desde aquí.

Entorno de medida: `go1.27.0 darwin/arm64`, Apple M3.

---

## Confirmado

| Hecho | Fuente | Nota |
|---|---|---|
| Draft IETF v11, 23-may-2026, campos `RateLimit` y `RateLimit-Policy` | datatracker | Sigue Internet-Draft, expira nov-2026. No es RFC. |
| `x/time/rate` sobre-permite bajo contención | golang/go #71360, #45245, #65508 | Hasta **5000×**. Causa: `time.Now()` se lee *antes* de tomar el lock, así que `advance` recibe un instante pasado y retrocede `lim.last`. Abiertos. |
| `testing/synctest` estable | Go 1.25 release notes | Experimental en 1.24 tras `GOEXPERIMENT`. |
| Ventana deslizante: error 0,003% | Cloudflare, 400M peticiones / 270k orígenes | Desviación media 6%; errores todos falsos negativos, ≤15% sobre el umbral. |
| GCRA: 64 bits, CAS, sin goteo | `governor` (Rust) en `AtomicU64`; `redis_rate` en Lua | |
| El reloj del almacén como fuente de verdad | `redis_rate` usa `redis.call("TIME")` | |
| Las alternativas filtran el interno del algoritmo | `sethvargo/go-limiter` | `Take(ctx, key) (tokens, remaining, reset uint64, ok bool, err error)` |

## Corregido

### `t` del draft-11 no es un instante de reset

Es la **ventana efectiva en delta-segundos**, deliberadamente relativa para no
depender de relojes sincronizados. El draft dice explícitamente que el cliente
**no debe asumir** que su cuota se restaura por completo al expirar.

### El draft recomienda jitter sobre la ventana efectiva

§8.5 propone jitter en `t` para mitigar la tormenta sincronizada — lo contrario
que `context/04` D-10. Pero el draft también exige:

- `Retry-After` **MUST take precedence** sobre `RateLimit`.
- `Retry-After` **SHOULD NOT** apuntar antes del final de la ventana efectiva.

Por tanto el jitter sobre `Retry-After` **tiene que ser de un solo lado, solo
hacia arriba**. Un jitter simétrico viola el draft la mitad de las veces y le
dice al cliente que vuelva antes de tener cuota.

### No hay "una" familia legacy: hay dos, incompatibles

| Familia | Estado | `Reset` significa |
|---|---|---|
| `RateLimit-Limit/Remaining/Reset` | drafts ≤07, casi sin consumidores | delta-segundos |
| `X-RateLimit-*` | de facto real (GitHub, Twitter, Stripe) | **epoch en GitHub, delta en Twitter** |

No existe una emisión correcta de `X-RateLimit-Reset`. Emitir un número cuyo
significado depende de quién lo lea es el fallo silencioso que el proyecto
prohíbe.

### El draft ya tiene unidades para concurrencia y bytes

`qu` admite `requests` (default), `content-bytes` y **`concurrent-requests`**.
El límite de concurrencia y el coste por bytes tienen forma **estándar** de
anunciarse. Ninguna librería del ecosistema lo hace.

`pk` (partition key) es un Byte Sequence: la huella hash encaja como `pk`
opaco, pero es un identificador estable por cliente — rastreador. Opt-in.

### La acusación contra las alternativas era la equivocada

`context/00` dice "fail-open silencioso". Falso:

- `throttled` falla **cerrado**: su `DefaultError` devuelve **500** ante error
  del backend.
- `redis_rate` devuelve `(nil, err)` y delega.

La acusación cierta y más dura: **ninguna tiene política de degradación**. Te
entregan un `error`. El default es un 500 o lo que escribas tú. Una caída de
Redis se convierte en una caída de tu API.

Además `throttled` **sí** distingue `ResetAfter` de `RetryAfter` en su
`RateLimitResult`. Lo que ninguna hace es emitir los campos del draft-11.

Munición mejor y verificable en `sethvargo/go-limiter`:

- Documenta `X-RateLimit-*` como *"the recommended return header values from
  IETF"*. No lo son, nunca lo fueron.
- `IPKeyFunc(headers ...string)` confía a ciegas en la cabecera que le nombres.
  Es el bypass de RF-G1 ofrecido como API pública.

## Estado del ecosistema (GitHub API, agosto 2026)

| Repo | ★ | Último push | Última release |
|---|---|---|---|
| `didip/tollbooth` | 2855 | 2025-01 | sin releases |
| `ulule/limiter` | 2342 | 2024-12 | v3.11.2 (2023-05) |
| `throttled/throttled` | 1595 | 2026-04 | v2.15.0 (2025-08) |
| `go-redis/redis_rate` | 1039 | 2026-08 | sin releases, 40 issues |
| `sethvargo/go-limiter` | 722 | 2026-07 | v1.2.0 (2026-07) |
| `uber-go/ratelimit` | 4711 | 2024-05 | sin releases |

---

## Mediciones propias

Reproducibles; se convierten en tests y benchmarks del repositorio.

### La gramática de `ServeMux` no sirve como matcher (funda D-A1)

```
single mux devuelve solo el patrón más específico: "GET /api/v1/search"
   -> "/api/v1/" nunca aparece. D-05 necesita TODAS las coincidencias.

registrar patrones que se solapan hace panic:
   /{y}/b and /a/{x} both match some paths, like "/a/b".
   But neither is more specific than the other.
   -> un límite global sobre /{tenant}/b más uno específico sobre /a/{x}
      es un caso legítimo de nivel 3 y ServeMux lo rechaza al arrancar.

BenchmarkServeMuxHandler   254.8 ns/op   51 B/op
   -> 13x el coste de componer la clave entera. Por regla.

pattern "BAD METHOD /a" -> ACEPTADO sin queja
   -> ServeMux no valida el método. Un "GTE /api/" sería un límite
      que nunca aplica, en silencio. Justo lo que RF-B9 existe para impedir.
```

La precedencia de `ServeMux` es **sustitutiva**; la de D-05 es **aditiva**.
Resuelven preguntas contrarias.

### Cota de ráfaga de un solo nodo (funda D-A2)

GCRA, martilleo desde estado frío, paso de 1 ms:

```
100/min  τ=W    (ráfaga = cuota entera)   1ª ventana = 199  (1.99x)   estable = 100 (1.00x)
100/min  τ=W/10                           1ª ventana = 109  (1.09x)   estable = 100 (1.00x)
100/min  τ=0                              1ª ventana =   0  (0.00x)   estable =   0 (0.00x)
 10/s    τ=W                              1ª ventana =  19  (1.90x)   estable =  10 (1.00x)
```

Tres consecuencias:

1. **Ráfaga = cuota entera admite ~2× en la primera ventana.** Es inherente al
   token bucket. Es la *misma* cota 2× por la que `context/04` excluye la
   ventana fija. La diferencia defendible es que en ventana fija se repite en
   cada borde y aquí ocurre una vez desde estado recuperado, y es suave.
   Omitirla no es defendible. **Se publica.**
2. **`τ = 0` no es pacing estricto: deniega todo.** La primera petición ya
   llega "demasiado pronto". `throttled` lo esquiva porque su `MaxBurst=0` da
   `τ = un intervalo de emisión`, no cero. **Validación: `τ ≥ emisión`.**
3. **"N por ventana W" con GCRA ≠ "nunca más de N en cualquier ventana móvil
   W".** Solo el contador deslizante entrega la segunda. El nombre del
   constructor no puede prometer la que no da.

### La deriva entera no muerde (cierra A-01)

```
N=3       W=1s     emisión=333.333333ms  deriva/ventana=1ns       +0.0000%
N=7       W=1s     emisión=142.857142ms  deriva/ventana=6ns       +0.0000%
N=999983  W=1h     emisión=3.600061ms    deriva/ventana=201.037µs +0.0000%
```

`int64` de nanosegundos basta. No hace falta punto fijo racional (la
preocupación de Bucket4j no aplica a esta resolución). Nota heredada: los
backends con scripting Lua **sí** tienen el problema — `redis_rate` necesita un
offset de época (Jan-2017) porque Lua usa `double` y se le acaba la precisión
en 2048. Pertenece al contrato de la costura.

### `hash/maphash` cierra A-02

```
BenchmarkMaphashComposite   19.38 ns/op   0 B/op   0 allocs/op   (2 dimensiones)
BenchmarkMaphashString      10.62 ns/op   0 B/op   0 allocs/op
```

Stdlib, semilla por proceso, AES en amd64/arm64, cero asignaciones. Un atacante
no puede precomputar colisiones contra una víctima porque no conoce la semilla.

---

## Prior art relevante para el lazo remoto

| Sistema | Contrato | Lección |
|---|---|---|
| Cloudflare | contadores locales aproximados + sync periódico | el patrón por defecto a escala |
| Doorman (YouTube/Google) | capacidad repartida como **leases** con plazo | publicar demanda, recibir asignación |
| DRL / *flow proportional share* (SIGCOMM'07) | peso gossipeado, reparto max-min | el sobrepaso deja de ser proporcional a N |
| Envoy | filtro **local** + filtro **global** separados, `failure_mode_deny` | la degradación como knob declarado |
| LINE | reparto **estático** `total / nº_instancias` | su defecto: los nodos ociosos desperdician su parte |
| Figma | contadores de ventana deslizante en hashes de Redis | eligieron redondear **hacia lo estricto**, no hacia lo laxo |
| .NET `System.Threading.RateLimiting` | `RateLimitLease` con metadatos | el lease como primitiva unificadora |
