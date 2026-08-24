# `rate-limiter` — Especificación de construcción

Paquete de especificación para construir una librería de rate limiting en Go,
completa y lista para producción. **La arquitectura está decidida.** Esto no es un
encargo de investigación: es un encargo de implementación.

Estos siete documentos son autónomos. Todo lo que necesitas está aquí.

## Orden de lectura

| Archivo | Contenido |
|---|---|
| `00-contexto.md` | Qué se construye, por qué, prioridades y principios. **Empezar aquí.** |
| `01-casos-de-uso.md` | La escalera de casos de uso que debe cubrir, y los no-objetivos. |
| `02-dominio-y-restricciones.md` | Hechos del dominio: algoritmos, semántica HTTP, cabeceras estándar, restricciones de seguridad y de runtime. |
| `03-requerimientos.md` | Requerimientos numerados con criterios de aceptación. |
| `04-arquitectura.md` | **La arquitectura decidida**, con el registro de por qué. |
| `05-plan-de-implementacion.md` | Fases, puertas de calidad y definición de terminado. |
| `06-decisiones-abiertas.md` | Lo que sí decides tú. |

## Cómo trabajar

1. **Lee los siete documentos antes de escribir una línea.**
2. **Verifica los hechos de `02` contra la fuente** antes de implementar sobre
   ellos. Algunos pueden haber cambiado.
3. **Construye por fases, en el orden de `05`.** Cada fase tiene una puerta. No
   avances con una puerta sin pasar.
4. **Los tests no son una fase final.** Se escriben con cada fase y son condición
   de avance.
5. **Lo decidido en `04` no se reabre.** Si crees que una decisión está mal,
   **para y dímelo con argumentos** antes de implementar la alternativa. No la
   implementes por tu cuenta.
6. **Lo abierto en `06` lo decides tú**, y documentas la decisión con su
   justificación en el registro de decisiones del repositorio.
7. **Agrupa las dudas.** Si te bloqueas, acumula y pregunta en tanda.

## Restricciones que no se negocian

- **Cero dependencias fuera de la stdlib en el módulo raíz**, verificado
  mecánicamente en integración continua, no prometido en el README.
- Nada de reflexión, asignaciones ni cadenas mágicas en la ruta de decisión.
- Nada de estado global, `init()` con efectos, ni configuración mutable tras
  arrancar.
- Todo lo que añada un `require` vive en un módulo satélite con su propio
  `go.mod`.
- **Un fallo declarado es aceptable; uno silencioso no.** Es el principio rector
  del proyecto y aquí es la propuesta de valor entera.
