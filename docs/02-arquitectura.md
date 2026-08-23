# 02 — Arquitectura

Módulo: `github.com/imlargo/ratelimit`, paquete `ratelimit`.
(Resuelve la contradicción entre `context/00` y D-11: sin guiones, y el último
elemento de la ruta coincide con el nombre del paquete.)

---

## La espina: un solo `int64`

Todo el estado de la librería es **un instante teórico de llegada por clave**,
8 bytes, actualizado con compare-and-swap.

```
admisión:   tat' = max(tat, ahora) + coste × emisión
            permite si  tat' − τ ≤ ahora
devolución: tat  = max(ahora, tat − coste × emisión)
recuperada: tat ≤ ahora        ⟹ indistinguible de una entrada nueva
```

Cuatro capacidades caen de esa única línea, y por eso es la espina y no un
detalle:

| Capacidad | Cómo sale de aquí |
|---|---|
| Límite de tasa | la admisión, sin más |
| Ráfaga | `τ`, literalmente |
| Devolución de cuota (RF-B6) | una resta con suelo en `ahora` |
| Expulsión segura (RF-F3) | `tat ≤ ahora` se lee del propio estado, O(1), sin recorrer nada |
| Expiración | implícita: una entrada recuperada ya es reutilizable |

**Consecuencia que vale la fase entera: no hay ninguna goroutine de fondo en
modo de un nodo.** No hay barrido de limpieza que gobernar, porque no hay nada
que limpiar. RF-F6 se cumple por construcción, no por un `Close` disciplinado.

Y cuando lleguen los leases (v1.x), un límite de concurrencia es el mismo
`int64`: `C` permisos por `plazoDeLease`, con devolución al liberar. Si nadie
libera, `tat` decae solo y los permisos vuelven al expirar el plazo — RF-E4
gratis, sin temporizadores.

### Reloj

- Origen capturado al construir; `ahora = int64(time.Since(origen))`.
- Usa la **lectura monotónica**, así que es inmune a saltos del reloj de pared.
- `time.Since` está virtualizado por `synctest`. La costura de reloj es un campo
  **no exportado** — nada público y mutable (RF-B10) — que `ratelimittest` fija.
- Horizonte: `int64` de nanosegundos ≈ **292 años** desde la construcción del
  limitador. Declarado en el README, irrelevante en la práctica.
- Deriva entera: medida y despreciable (ver `00-verificacion.md`).

### Cómo se evita el bug de `x/time/rate`

`x/time/rate` lee `time.Now()` **antes** de tomar el lock, así que bajo
contención `advance` recibe un instante pasado y hace **retroceder** el estado.
De ahí el 5000× de #71360.

Aquí el bucle CAS **relee el reloj en cada iteración** y solo hace CAS a un
valor **estrictamente mayor** en caso de admisión. `tat` es monotónicamente no
decreciente bajo admisión, por invariante. La corrección no es incidental: es
estructural, y el test de contención de la fase 1 la verifica.

---

## El almacén

Capacidad fija, particionado, direccionamiento abierto. **Sin mutex por clave y
sin lock global en la ruta de decisión** (RNF-2).

```
particiones = pot2(GOMAXPROCS × 4)
ranura      = { fp uint64 ; tat atomic.Int64 }        16 bytes exactos
memoria     = capacidad × 16 B + constante            exacta, no estimada
```

Ruta de decisión: `fp = maphash(dimensiones)` → partición por los bits altos →
sondeo lineal acotado a 8 ranuras (2 líneas de caché) → CAS sobre `tat`.

Inserción, en orden:

1. Ranura libre en la ventana de sondeo → se reclama con CAS sobre `fp`.
2. Ranura **recuperada** (`tat ≤ ahora`) en la ventana → se expulsa. Es segura:
   una entrada recuperada del todo es indistinguible de una nueva, así que
   expulsarla no pierde información (D-08).
3. Mano tipo CLOCK por partición, acotada a `K` pasos, buscando una recuperada.
4. Ninguna → **partición saturada**.

Coste de expulsión: O(1) amortizado, **independiente del tamaño del almacén**
(criterio de A-02).

### Saturación: refinamiento de RF-F4

`context/03` RF-F4 dice: almacén saturado ⇒ no se permite la petición. Literal,
eso significa devolver 429 a clientes legítimos cuando hay un pico de
cardinalidad — una caída autoinfligida, la misma forma de fallo que
`context/01` usa como ejemplo de lo que no hacer.

**Refinamiento: la saturación nunca deniega a una clave que ya tiene entrada.
Solo deniega claves nuevas.**

- El atacante que genera claves a voluntad es exactamente quien se lleva el 429.
- La víctima, y todo cliente ya presente, no se enteran: aciertan en su ranura.
- La propiedad anti-elusión de D-08 queda intacta: para expulsar el contador de
  la víctima sigue haciendo falta que la víctima esté recuperada del todo.
- Sigue declarado: motivo `StoreSaturated` + métrica dedicada.

Se elimina el modo de caída sin pagar nada.

### Clave: hash, nunca cadena (D-07)

`hash/maphash` — stdlib, semilla por proceso, AES en amd64/arm64.
**19,4 ns y 0 asignaciones** para dos dimensiones (medido).

- **Separación de dominio obligatoria:** cada dimensión escribe un byte de
  etiqueta antes de su valor. Sin eso, `Identity("a")+Path("b")` colisiona con
  `Identity("ab")` trivialmente.
- Cota de colisión (RF-F5): huella de 64 bits, `n` claves activas ⇒ `n²/2⁶⁵`.
  Con 1M de claves activas, **1,4 × 10⁻⁸**. Publicada.
- La semilla por proceso impide precomputar una colisión contra una víctima.
- Coste declarado de D-07: no se pueden enumerar claves para depurar. Se paga
  con un `KeyInspector` opt-in que sí materializa la cadena, fuera de la ruta
  caliente.

---

## Selectores: sintaxis de `ServeMux`, matcher propio

`context/04` D-03 pide usar la gramática **y las reglas de precedencia** de
`ServeMux`, "sin reimplementar un matcher". Medido, eso no es implementable
(evidencia en `00-verificacion.md`):

- Un `ServeMux` devuelve **solo la coincidencia más específica**; D-05 necesita
  todas. Su precedencia es *sustitutiva*, la de D-05 es *aditiva*: resuelven
  preguntas contrarias.
- Registrar patrones que se solapan **hace `panic`**, y se solapan justo los
  conjuntos de reglas que D-05 exige.
- `mux.Handler(req)` cuesta **255 ns y asigna**. Por regla.
- `ServeMux` **no valida el método**: `"BAD METHOD /a"` se acepta, y un `GTE`
  mal escrito sería un límite que nunca aplica, en silencio.

**Decisión: se conserva la sintaxis, se descarta el matcher.** La sintaxis era
todo el valor de D-03 — cero gramática nueva que aprender, prioridad #1 — y
sobrevive intacta.

- Matcher propio sobre un trie de segmentos; devuelve el **conjunto** de reglas
  coincidentes como un bitset en la pila, sin asignar.
- **Validación al construir delegada a la stdlib**: cada patrón se registra en
  un `ServeMux` desechable solo para validarlo. Sus mensajes de error son
  excelentes y salen gratis (RF-B9).
- **Encima, la validación de método que la stdlib no hace.**

### Orden de evaluación, sin coste por petición

D-05 quiere: lo local antes que lo remoto, y dentro de lo local lo que más
probablemente deniegue primero, para minimizar devoluciones.

Las reglas se ordenan **una vez al construir** por esa clave. En cada petición
se itera en ese orden global saltando las que no coinciden. Cero trabajo de
ordenación por petición.

Devolución: pila de `[8]consumido` para las reglas ya cobradas; al denegar se
recorre hacia atrás y se resta. Cero asignaciones para ≤8 reglas compuestas.

---

## La superficie pública (resuelve A-04)

Criterio de A-04: escribir los niveles de `context/01` como si la librería
existiera y elegir la forma que los deja más cortos. Gana el **literal de
struct con valores cero útiles**: subir de nivel **añade un campo o una regla,
nunca reescribe una línea**, y el editor enumera los campos sin abrir el README
(RNF-6).

```go
type Rule struct {
    Selector string                       // ""   → todas las peticiones
    Key      Key                          // cero → dirección del par TCP
    Quota    Quota                        // requerido
    QuotaFor func(*http.Request) Quota    // opcional: planes / tiers
    Cost     func(*http.Request) int64    // opcional: default 1
    Shadow   bool                         // opcional: evalúa, cuenta, no deniega
    Exempt   bool                         // opcional: coincide ⇒ permite y para
    Name     string                        // opcional: etiqueta de métrica
}

func New(rules ...Rule) (*Limiter, error)   // el 99%
func NewWith(Config) (*Limiter, error)      // capacidad, métricas, logger, backend
```

### Nivel 0 — toda la API, un límite

```go
lim, err := ratelimit.New(ratelimit.Rule{Quota: ratelimit.PerMinute(100)})
if err != nil { return err }

mux.Handle("/", lim.Limit(h))        // ← una línea de montaje (RF-B1)
```

### Nivel 1 — cambiar la clave

```go
lim, _ := ratelimit.New(ratelimit.Rule{
    Quota: ratelimit.PerMinute(100),
    Key:   ratelimit.ByIdentityOrIP(identify, proxies),   // RF-B3, una línea
})
```

### Nivel 2 — selectores por endpoint

```go
lim, _ := ratelimit.New(
    ratelimit.Rule{Selector: "POST /api/v1/search", Quota: ratelimit.PerMinute(10),   Key: k},
    ratelimit.Rule{Selector: "POST /api/v1/auth/",  Quota: ratelimit.PerMinute(5),    Key: ratelimit.ByIP(proxies)},
    ratelimit.Rule{Selector: "GET /healthz",        Exempt: true},
    ratelimit.Rule{                                 Quota: ratelimit.PerMinute(1000), Key: k},
)
```

### Nivel 3 — varias reglas a la vez, con devolución

Es el mismo código de arriba. **No hay nivel 3 en la API**: la composición es lo
que la tabla de reglas hace siempre. Gana la más restrictiva, y si una deniega
se devuelve lo que las anteriores cobraron. Eso es lo que ninguna alternativa
del ecosistema tiene, y aquí no cuesta un concepto extra.

### Desplegar una regla nueva sin denegar a nadie

```go
ratelimit.Rule{Selector: "POST /api/v1/export", Quota: ratelimit.PerHour(10), Shadow: true}
```

### Sin HTTP (RF-A5)

```go
d := lim.Check(ctx, ratelimit.Subject{Identity: userID, Path: "/jobs", Method: "POST"})
if !d.Allowed { return d.RetryAfter }
```

### Notas de diseño

- `Exempt` es una **regla declarada y visible** (RF-B8), nunca un `if` en el
  middleware.
- Los campos son exportados pero `New` **compila y copia**: mutar el struct
  después no tiene efecto (RF-B10).
- `Name` cierra A-07: **la regla es la etiqueta de métrica**, y el conjunto es
  acotado y estable porque es inmutable tras construir. La clave nunca es
  etiqueta (RNF-12).
- `Close()` existe siempre y es **no-op en un nodo**, porque no hay goroutines.
- `QuotaFor` y `Cost` son código de usuario en la ruta caliente. Documentado:
  no deben asignar ni entrar en pánico. Para selección por cabecera o tenant
  (A-06) **no se admite un predicado arbitrario**: se expresa como dimensión de
  clave, o la aplicación deja un valor tipado en el contexto y una dimensión lo
  lee. Código de usuario fuera de nuestras garantías.

---

## La decisión y las cabeceras

```go
type Decision struct {
    Allowed    bool
    Reason     Reason          // enum tipado, nunca cadenas (RF-A2)
    Rule       string          // qué regla decidió (RF-B5)
    Limit      int64
    Remaining  int64
    ResetAfter time.Duration   // el hecho
    RetryAfter time.Duration   // el consejo
    Degraded   bool
    Shadowed   bool
}
func (d Decision) WriteHeaders(h http.Header)   // RF-A3
```

Valor devuelto **por valor**, no puntero: no asigna.

`Reason`: `Allowed`, `AllowedShadow`, `AllowedDegraded`, `DeniedQuota`,
`Exempt`, `StoreSaturated`, **`CostExceedsBurst`**.

### El motivo que faltaba en `context/03`

Si `coste > τ / emisión`, la petición **no puede admitirse nunca**, ni con el
estado vacío. Tal como está `03`, eso es una denegación infinita sin
explicación: el fallo silencioso perfecto. Se cubre con validación al construir
cuando el coste es constante, y con el motivo `CostExceedsBurst` cuando se
deriva de la petición. RF-A2 no lo listaba.

### Cabeceras

Por defecto, **solo el par del draft-11** más `Retry-After`:

```
RateLimit-Policy: "search";q=10;w=60, "global";q=1000;w=60
RateLimit: "search";r=3;t=42
Retry-After: 44
```

- **`RateLimit-Policy` es estático para un conjunto de reglas dado**: se
  precomputa como cadena al construir y se escribe ya formada. Solo `RateLimit`
  varía por petición.
- `t` es la **ventana efectiva en delta-segundos**, no un instante de reset.
  Exacta, sin jitter.
- `Retry-After` lleva **jitter de un solo lado, solo hacia arriba**. El draft
  exige que tome precedencia y que **no** apunte antes del final de la ventana
  efectiva; un jitter simétrico lo violaría la mitad de las veces.
- **429, nunca 503.**
- La familia legacy es **opt-in con el dialecto nombrado**
  (`LegacyDraft07` / `LegacyXPrefixDelta` / `LegacyXPrefixEpoch`), porque
  `X-RateLimit-Reset` significa epoch en GitHub y delta en Twitter, y emitir un
  número cuyo significado depende del lector es el fallo silencioso que esta
  librería existe para no cometer.
- `pk` (huella de clave como partition key del draft) es opt-in: es un
  identificador estable por cliente, es decir un rastreador.

Cuando lleguen los leases, `qu="concurrent-requests"` y `qu="content-bytes"` del
draft dan forma **estándar** de anunciar concurrencia y coste por bytes. Nadie
lo hace.

---

## Nivel 0 y seguridad de la IP, sin compromiso

Hay una tensión real entre RF-B1 (una línea, clave por defecto sensata) y RF-G1
(dimensión de IP sin proxies declarados **falla al construir**).

Se resuelve sin ceder en ninguna:

- **La clave por defecto es la dirección del par TCP**, no una cabecera. No
  necesita configuración y **no es suplantable**.
- La IP derivada de cabeceras solo se obtiene con `ratelimit.ByIP(proxies)`,
  que **no se puede construir sin declarar los proxies**.
- Y el fallo silencioso restante se declara: si se usa la clave por defecto y
  `RemoteAddr` es una dirección privada o de loopback de forma sostenida,
  **aviso único por `slog`** — «pareces estar detrás de un proxy; la clave por
  defecto agrupa todo tu tráfico en un solo contador, declara tus proxies».

Extracción: **rightmost-non-trusted** sobre `X-Forwarded-For` o `Forwarded`
(RFC 7239), nunca leftmost, nunca varias cabeceras a la vez.

---

## Modo distribuido y degradación

El local **siempre** decide (D-01). El remoto corrige fuera de la ruta de
decisión.

```
Backend interface {
    Sync(ctx, []Demand) ([]View, error)      // lote, en segundo plano, deltas CON SIGNO
}
```

Un método. El adaptador de Redis es un script Lua.

- **Deltas con signo**, no `uint64`. RF-B6 obliga: si la regla A ya publicó
  consumo y la regla C deniega, la devolución tiene que poder viajar. Con un
  contrato sin signo, RF-B6 se rompe en distribuido y **no se nota en un nodo**.
- Se sincronizan solo las claves **activas** (`tat > ahora`) y por encima de un
  umbral configurable de consumo (p. ej. >25% de la cuota). Las claves lejos de
  su límite no necesitan corrección. El volumen crece con las claves activas,
  no con la cardinalidad total.
- El reloj del almacén compartido gobierna la **vista**, no la decisión local.
  La deriva entre nodos solo afecta al lazo de corrección, acotada por el
  intervalo de sincronización.

### Por qué la degradación no es una ruta de error (RF-D5)

La ruta de decisión lee una **vista atómica**. El lazo de sincronización la
actualiza. Si el backend cae, la vista simplemente **deja de actualizarse**: la
ruta de decisión es byte a byte la misma, y es la del modo de un nodo, el camino
más probado de la librería. **No hay rama de fallback que mantener ni que
probar**, y la puerta de la fase 5 («con el backend caído el camino ejercitado
es el del nodo único») se verifica por construcción y no por inspección.

Declarado: motivo `AllowedDegraded`, métrica dedicada, y **un** aviso por
`slog` — no uno por petición (RF-D4).

Recuperación: se publica la demanda actual y se recibe una vista nueva. **Nada
acumulado se empuja** (RF-D6), así que no hay pico de denegaciones justo
después del incidente.

---

## Cotas declaradas, que van al README

Esto es el producto. Se publica todo, incluido lo que incomoda.

| Cota | Valor |
|---|---|
| Un nodo, ráfaga = cuota entera | **≤ 1,99×** la cuota en la primera ventana desde estado recuperado; **1,00×** en régimen estable |
| Un nodo, ráfaga = `B` | ≤ `1 + B/N` veces la cuota en la primera ventana |
| Distribuido aproximado, `N` nodos, intervalo `T` | lo anterior **más** `N × tasa × T` |
| Colisión de clave, `n` claves activas | `n²/2⁶⁵` — 1,4 × 10⁻⁸ con 1M |
| Memoria del almacén | `capacidad × 16 B` **exactos** |
| Horizonte temporal | ~292 años desde la construcción |
| Asignaciones en `Check` | **0** |
| Asignaciones en el middleware | acotado y medido, **no** cero: `Header().Set` asigna |

### Sobre RNF-1

`context/03` RNF-1 dice «cero asignaciones por decisión permitida». Alcanzable
en `Check` y verificado. **No** alcanzable extremo a extremo: escribir cabeceras
inserta en un mapa y formatea enteros. La versión honesta son **dos
presupuestos, ambos medidos y ambos aserciones que fallan la build**. Prometer
cero en el middleware sería exactamente la clase de garantía incumplible que
esta librería dice no hacer.

---

## Registro de decisiones

| # | Decisión | Prioridad | Cubre | Cambia |
|---|---|---|---|---|
| D-A1 | Sintaxis de `ServeMux`, **matcher propio**; validación delegada a la stdlib + validación de método | #1, #2 | RF-B4, RF-B9 | **corrige D-03** |
| D-A2 | Ráfaga = cuota entera por defecto; **cota 1,99× publicada**; `τ ≥ emisión` validado | #1 | RF-D3, RNF-13 | **añade a D-02** |
| D-A3 | Draft-11 por defecto; legacy opt-in **con dialecto nombrado**; jitter de un solo lado | #5 | RF-A3, D-10 | **corrige D-10** |
| D-A4 | Saturación no deniega a claves existentes, solo a nuevas | #4 | RF-F4, RF-F2 | **refina D-08** |
| D-A5 | Motivo `CostExceedsBurst` + validación al construir | #1 | RF-A2, RF-A4 | **añade a RF-A2** |
| D-A6 | Dos presupuestos de asignación, decisión y middleware | #2 | RNF-1 | **corrige RNF-1** |
| D-A7 | Contrato de sync con **deltas con signo**; solo claves activas sobre umbral | #3 | RF-B6, RF-H5, A-03 | resuelve A-03 |
| D-A8 | `int64` de nanosegundos monotónicos desde origen; reloj no exportado | #2 | RNF-1, A-01 | resuelve A-01 |
| D-A9 | `hash/maphash` + separación de dominio por byte de etiqueta | #2, #4 | RF-F5, A-02 | resuelve A-02 |
| D-A10 | Literal de `Rule` con valores cero útiles; `New` / `NewWith` | #1 | RF-B1, RNF-5, RNF-6, A-04 | resuelve A-04 |
| D-A11 | Clave por defecto = par TCP; IP por cabecera imposible sin proxies; aviso único si hay proxy | #1, seguridad | RF-B1, RF-G1 | resuelve la tensión |
| D-A12 | Sin predicados arbitrarios: dimensión de clave o valor tipado en el contexto | #2 | A-06, RNF-8 | resuelve A-06 |
| D-A13 | La regla es la etiqueta de métrica; conjunto acotado por inmutabilidad | #5 | A-07, RNF-12 | resuelve A-07 |
| D-A14 | `github.com/imlargo/ratelimit`, paquete `ratelimit` | #1 | A-08 | resuelve la contradicción `00`/D-11 |

**Sin cambios:** D-01 (el local decide, el remoto corrige), D-02 (GCRA,
conjunto cerrado), D-04 (selector y clave separados), D-05 (tabla como unidad,
precedencia aditiva), D-06 (el lease unifica — aplazado a v1.x, no rediseñado),
D-07 (clave por hash), D-09 (la sombra es un motivo), D-11 (organización),
D-12 (métricas como struct de funciones). Las prioridades de `00`, los
no-objetivos de `01` y la puerta de calidad de `05`, intactas.
