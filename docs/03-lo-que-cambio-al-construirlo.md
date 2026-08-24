# 03 — Lo que cambió al construirlo

`02-arquitectura.md` es la arquitectura propuesta. Este documento registra las
decisiones que **la medición cambió** durante la implementación, con el número
que forzó cada cambio.

Está aquí y no reescrito encima de `02` a propósito: en un proyecto cuya
propuesta de valor es no mentir sobre lo que hace, el registro de lo que se
creyó y resultó falso vale más que un documento que finge haber acertado a la
primera.

Entorno de medida: `go1.27.0 darwin/arm64`, Apple M3.

---

## C-01 · El contrato remoto de deltas no acota nada

**Lo propuesto (D-A7).** Cada nodo publica su consumo, recibe el total global y
lo resta. Cota de sobrepaso `N × tasa × T`.

**Lo medido.** Implementado así, ocho nodos sostenían **8,46 ×** la cuota
configurada. Cuatro nodos, 4,19 ×. La corrección no servía para nada.

**Por qué.** El nivel local de cada nodo se recupera a velocidad de reloj de
pared. Sumar niveles acota el nivel *instantáneo* del sistema, pero cada uno de
los N niveles decae a 1 ns/ns, así que el caudal sostenido del sistema es
`N × tasa` **por muchas veces que se sincronice**. Acortar el intervalo no lo
arregla porque el problema no es la latencia de la corrección, es la estructura.

**Lo que se construyó.** Cada nodo publica **demanda** —lo que le están
pidiendo, admitido o no— y recibe lo que piden los demás. Enforza localmente su
**parte** de la cuota, escalando su intervalo de emisión por el recíproco de su
cuota asignada. Escalar la emisión escala tasa y ráfaga por el mismo factor,
porque la ráfaga es la tolerancia dividida por la emisión y la tolerancia no se
toca.

Es la forma a la que llegaron Doorman y la literatura de rate limiting
distribuido, y no un refinamiento opcional: es la única que permite publicar una
cota.

**Cota resultante, medida en seis configuraciones al 61–100 % de la fórmula:**

```
nodos × ráfaga  +  cuota  +  nodos × tasa × intervalo
```

| nodos | ráfaga | intervalo | admitido 1ª ventana | × cuota |
|---|---|---|---|---|
| 1 | 600 | 200 ms | 1199 | 2,00 × |
| 4 | 600 | 200 ms | 2407 | 4,01 × |
| 4 | 60 | 200 ms | 666 | **1,11 ×** |
| 8 | 60 | 200 ms | 672 | **1,12 ×** |
| 2 | 600 | 500 ms | 6801 | 1,13 × |

**El hallazgo operativo que sale de la tabla:** el término dominante es
`nodos × ráfaga`, no el intervalo. Ocho nodos con ráfaga de un décimo de la cuota
son más precisos que cuatro con ráfaga completa. Quien quiera precisión en
distribuido baja la ráfaga; acortar el intervalo de sincronización casi no
compra nada.

**Dos correcciones más que salieron de la misma medición.**

- **La demanda decae a la mitad por ronda, no se resetea.** Con reset, un
  intervalo en silencio liberaba la asignación y el nodo volvía a la cuota
  global completa — un cliente conseguía el límite entero solo con pausar.
- **Al cambiar la asignación se reescala el nivel consumido.** Sin eso, apretar
  la parte de un nodo revalorizaba a la baja la cuota que ya había gastado, y le
  regalaba una ráfaga nueva cada vez que su asignación se movía.

---

## C-02 · `hash/maphash` no sirve, y la razón es el modo distribuido

**Lo propuesto (D-A9).** `hash/maphash`: stdlib, semilla por proceso, AES,
19 ns, cero asignaciones. Cerraba A-02.

**Lo medido.** Los tests de tres nodos no correlacionaban nada. La semilla de
`maphash` es **por proceso y no se puede fijar**, así que dos nodos calculan
huellas distintas para la misma clave y el backend no puede emparejar nada.

Es una tensión que `02` no vio: la aleatorización por proceso era la respuesta
de seguridad a A-02, y es incompatible con la coordinación.

**Lo que se construyó.** **SipHash-1-3** propio, 90 líneas, con clave de 128
bits:

- Sin backend, la clave es aleatoria por proceso — más fuerte que antes y sin
  configuración.
- Con backend, se deriva de `Config.ClusterKey`, un secreto compartido. **Sin él
  el limitador no arranca**, con un error que explica por qué: un default
  derivable (el nombre de la regla, el hostname, una constante) le regalaría al
  atacante la capacidad de computar offline una colisión con la clave de una
  víctima y compartir su contador.

Coste: 13,6 ns por dimensión frente a 10,6 de `maphash`. La composición completa
de una clave de dos dimensiones quedó en **15,7 ns, 0 asignaciones**. Es más
rápido que la versión con `maphash` que se midió primero (27 ns), porque una
llamada con clave por dimensión y mezcla sale más barata que montar y drenar un
hasher en streaming para una o dos dimensiones.

---

## C-03 · El sondeo lineal se agrupa muy por debajo de la cota publicada

**Lo propuesto (A-02).** Tabla de capacidad fija, direccionamiento abierto,
sondeo lineal acotado a 16 ranuras. Probabilidad de saturación
`(activas/capacidad)^16`.

**Lo medido.** Con 100 claves activas en 128 celdas —78 % de carga— saturaba en
la clave 86. La fórmula predecía 1,9 %. El error es de libro: el sondeo lineal
**se agrupa**, y la aproximación de ranuras independientes no vale.

**Lo que se construyó.** Tabla **asociativa por conjuntos de dos vías**: una
clave vive en uno de dos buckets de 16 celdas, elegidos por dos hashes
independientes (el segundo mezclado con SplitMix64). Dos opciones por clave en
vez de una es lo que mantiene la tabla usable casi llena.

**Curva medida** (16384 celdas, todas las claves con cuota consumida, nada
reciclable):

| carga | claves activas | rechazadas | tasa |
|---|---|---|---|
| 25 % | 4096 | 0 | 0 |
| 50 % | 8192 | 0 | 0 |
| 60 % | 9830 | 1 | 0,010 % |
| 70 % | 11468 | 15 | 0,131 % |
| 75 % | 12288 | 34 | 0,277 % |

**Regla de dimensionado publicada:** capacidad ≥ 2 × claves simultáneamente
activas. Es una medición, no una fórmula deducida.

---

## C-04 · El array del ledger en pila era el coste dominante de la decisión

**Lo medido.** `evaluate` costaba 134 ns con solo 60 ns atribuibles a hash,
reloj, selector y almacén. Los 74 ns restantes eran **poner a cero 2 KB de pila**
por petición: el ledger de devolución era `[64]refundEntry` con un `string`
dentro, 32 bytes por entrada.

**Lo que se construyó.**

- `refundEntry` pasó a `{slot uint32, rule uint32, dec int64}` = 16 bytes. El
  índice de celda viene del `outcome`, así que devolver es un acceso directo sin
  segunda búsqueda — y es seguro porque una celda de la que se acaba de consumir
  no puede reciclarse: solo se recicla una recuperada del todo.
- `maxRules` bajó de 64 a **16**. Una tabla de más de 16 reglas no es el 99 %:
  un límite global, uno por llamante, un puñado de endpoints apretados y un par
  de exenciones son menos de diez.

Resultado: 134 → 96 ns en `evaluate`, y **118 ns** en `Check` completo.

---

## C-05 · Cosas que resultaron estar mal y no eran de arquitectura

Cada una habría sido un fallo silencioso en producción. Todas tienen test.

**`WithBurst(0)` se aceptaba.** El campo era `burst int64` con 0 = «sin fijar»,
así que una ráfaga de cero era indistinguible del default. Una ráfaga de cero no
es *pacing* estricto: rechaza la primera petición contra un contador vacío y
todas las siguientes, para siempre. Se guarda como `burst+1` y se valida.

**La regla en sombra se elegía como regla vinculante de las cabeceras.** Con la
regla candidata más apretada que las vivas, `RateLimit` anunciaba al cliente un
límite que no deniega. Las reglas en sombra quedan excluidas de la elección.

**`clientIP` asignaba 2 veces por petición.** Un closure sobre cuatro variables
las mandaba al heap. Reescrito como struct con métodos.

**`netip.ParseAddrPort` asigna al construir el error.** Y una cabecera de
reenvío es dato controlado por el cliente: entradas malformadas
deliberadamente no pueden costar más que válidas. Parseo IPv4 propio y
prefiltro por clase de caracteres; cero asignaciones en éxito y en fallo, y de
acuerdo con `netip` en los 17 vectores del test.

**Una entrada basura en la posición más a la derecha del `X-Forwarded-For`.**
Devolvía la dirección válida que había a su izquierda. Pero un proxy de
confianza siempre añade una dirección real, así que basura en esa posición
significa que la cabecera no la escribió nuestro proxy y no se puede confiar en
nada. Ahora invalida lo encontrado a su izquierda.

**`Peek` con coste 0 decía «permitido» con el contador exhausto.** La prueba de
admisión con incremento cero pasa siempre en el límite exacto. `Peek` evalúa un
evento de coste unidad y no escribe.

**`stripPort("[::1]")` devolvía `":"`.** Buscaba el último `:` antes de tratar
los corchetes.

**El aviso de cadena de proxies era ruidoso.** Saltaba con la primera petición
sin cabecera de reenvío, que es normal (tráfico interno, una sonda contra la IP
del pod). Ahora exige 128 fallos y **ningún** éxito previo.

**En el backend de Redis, `Script.Run` dentro de un pipeline usa EVALSHA** y
falla con NOSCRIPT la primera vez, porque el error vuelve después de enviar el
lote entero. El script se carga antes del primer lote y cualquier NOSCRIPT
fuerza recargarlo — que es lo que pasa tras reiniciar Redis o un `SCRIPT FLUSH`.

Detalle que vale por sí solo: **el test end-to-end de dos nodos pasaba con este
bug**. Los nodos estaban degradados y cada uno enforzaba el límite completo
localmente, lo que caía dentro del umbral que el test comprobaba. Lo cazó el
test de contrato del backend, no el de integración. Un test que puede pasar por
el motivo equivocado no es un test.

---

## C-06 · Correcciones al posicionamiento del README

`context/00` afirmaba «el comportamiento habitual es fail-open silencioso».
Verificado contra el código: **falso**. `throttled` falla **cerrado** con un 500;
`redis_rate` devuelve el error y delega. La acusación cierta y más dura es que
**ninguna tiene política de degradación**: te entregan un `error` y el default es
un 500 o lo que escribas tú.

Y `throttled` **sí** distingue `ResetAfter` de `RetryAfter`, que es justo la
distinción de D-10. Lo que ninguna hace es emitir los campos del draft-11.

Munición mejor, verificable hoy en el código: `sethvargo/go-limiter` documenta
`X-RateLimit-*` como *"the recommended return header values from IETF"* —no lo
son— y su `IPKeyFunc(headers ...string)` confía a ciegas en la cabecera que le
nombres, que es el bypass de RF-G1 ofrecido como API pública.

---

## Números finales

`go test -run xxx -bench . -benchmem`, Apple M3. La máquina tenía ruido térmico
notable durante la sesión; son los mejores de varias tandas y hay que tomarlos
como orden de magnitud, no como cifras de referencia.

| Benchmark | ns/op | B/op | allocs/op |
|---|---|---|---|
| decisión permitida, 1 regla | 118 | 0 | **0** |
| decisión denegada | 128 | 0 | **0** |
| tres reglas compuestas | 336 | 0 | **0** |
| alta cardinalidad (65 k claves) | 162 | 0 | **0** |
| una clave caliente, en paralelo | 223 | 0 | **0** |
| muchas claves, en paralelo | 42 | 0 | **0** |
| middleware HTTP completo | 577 | 96 | 5 |
| composición de clave, 4 dimensiones | 65 | 0 | **0** |
| IP de cliente, cadena de 3 saltos | 272 | 0 | **0** |

Las 5 asignaciones del middleware son la emisión de cabeceras:
`http.Header.Set` inserta en un mapa y el valor de `RateLimit` hay que
formatearlo. `RateLimit-Policy` no cuesta nada porque no varía para una tabla de
reglas dada y se renderiza una vez al construir. Los dos presupuestos —0 en la
decisión, 5 en el middleware— son aserciones separadas que fallan la build.

---

## C-07 · Recorte de periferia, después de revisar el tamaño

Con el código escrito, el reparto real era **2.327 líneas de código y 1.331 de
documentación** (36 %). Comparado con el ecosistema, midiendo solo código:

| | código real |
|---|---|
| `redis_rate` | 255 |
| `sethvargo/go-limiter` | 316 |
| `throttled` | 1011 |
| `ulule/limiter` | 1116 |
| esto, antes del recorte | 2327 |

El núcleo de un nodo era ~1,9 × `throttled` para hacer cinco cosas que
`throttled` no hace (composición con devolución, sombra, cabeceras del draft,
extracción de IP no eludible, almacén acotado). Esa proporción es defendible.

Lo que no lo era: superficie pública que no pidió nadie. **Fuera:**

| Quitado | loc | Por qué |
|---|---|---|
| `ClientIPForwarded`, `IdentityOrIPForwarded` y el parseo de RFC 7239 | ~85 | Casi nada emite `Forwarded`, y ofrecer dos cabeceras es cómo se reabre el agujero de suplantación que ambas venían a cerrar |
| `LegacyDraft07` y `LegacyXPrefixEpoch` | ~20 | Quedaba un solo dialecto: `X-RateLimit-*` en segundos restantes, que es el que concuerda con la semántica del campo estándar |
| `TestingClock` y `Config.WithClock` de la API pública | 0 | Los envoltorios exportados se movieron a un archivo `_test.go`: la costura sigue existiendo sin ser API |
| `Literal()` | ~5 | No resolvía ningún caso real |

Resultado: **2.242 líneas de código**, cuatro conceptos públicos menos y una
cabecera menos que parsear. Sin cambios en el comportamiento del camino feliz.

Lo que **no** se recortó, y la razón: el modo distribuido son ~400 líneas (18 %
del código) y la mayor parte del peso conceptual, pero sin él N réplicas
multiplican el límite configurado por N **en silencio** — que es exactamente el
fallo contra el que existe esta librería. Y no cuesta nada a quien no lo active:
sin backend no hay goroutine, ni array `ext`, ni `ClusterKey`.
