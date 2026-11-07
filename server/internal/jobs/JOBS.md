# JOBS.md — scheduler de cron jobs (`internal/jobs`)

Documento de referencia del paquete `server/internal/jobs`: el port en Go de
la pila de cron jobs de upstream (`packages/cron-jobs`,
`packages/cron-jobs-core`, `packages/cron-job-status`, arrancados por
`apps/tasks`). Complementa `server/ARCHITECTURE.md`,
`server/internal/db/SCHEMA.md` y `server/internal/bus/bus.go`. Todas las
afirmaciones citan fuente upstream; las desviaciones están marcadas.
Fecha: 2026-11-06.

Fuentes citadas (todas en este repo):

- **[core]** `packages/cron-jobs-core/src/creator.ts` (createCronJob /
  createCallback), **[group]** `packages/cron-jobs-core/src/group.ts`
  (createJobGroupCreator: initializeAsync, startAllAsync, runManuallyAsync,
  stop), **[expr]** `packages/cron-jobs-core/src/expressions.ts` (constantes
  de cron), **[valid]** `packages/cron-jobs-core/src/validation.ts`.
- **[jobs]** `packages/cron-jobs/src/index.ts` (grupo con los 3 jobs),
  **[manager]** `packages/cron-jobs/src/job-manager.ts` (JobManager),
  **[ping]** `packages/cron-jobs/src/jobs/ping.ts`,
  **[analytics]** `packages/cron-jobs/src/jobs/analytics.ts`,
  **[icons]** `packages/cron-jobs/src/jobs/icons-updater.ts`.
- **[status]** `packages/cron-job-status/src/index.ts` (TaskStatus) +
  `packages/cron-job-status/src/publisher.ts` (publisher).
- **[router]** `packages/api/src/router/cron-jobs.ts` (procedimientos tRPC).
- **[tasks]** `apps/tasks/src/main.ts` + `src/on-start/index.ts`.
- **[schema]** `packages/db/schema/sqlite.ts:709-713` + migración
  `packages/db/migrations/sqlite/0033_add_cron_job_configuration.sql`.
- **[redis]** `packages/redis/src/index.ts:31-34` (canales ping),
  `packages/redis/src/lib/channel.ts` (createSubPubChannel con last-data).
- **[settings]** `packages/db/queries/server-setting.ts` +
  `packages/server-settings/src/index.ts:146` (default forceDisableStatus).
- **[ws-doc]** `server/docs/FRONTEND_EXPORT_SPIKE.md` §3.2 (protocolo WS).

## 1. Registro (API del paquete)

```go
sched := jobs.New(database, bus, jobs.Options{Logger: ...})
sched.Register(jobs.PingJob(database, bus))          // + Register(defs...) en M4
err := sched.Start(ctx)                              // initializeAsync + startAllAsync
sched.Trigger(ctx, "ping")                           // cronJobs.triggerJob
sched.StartJob(ctx, "ping") / sched.StopJob("ping")  // cronJobs.startJob / stopJob
sched.Enable(ctx, "ping") / sched.Disable(ctx, "ping")
sched.UpdateInterval(ctx, "ping", "*/5 * * * *")     // cronJobs.updateJobInterval
list, _ := sched.List(ctx)                           // cronJobs.getJobs
ts, ok := sched.LastStatus("ping")                   // último TaskStatus (in-process)
sched.Stop()
```

`Definition` replica `createCronJob(name, cronExpression, options).withCallback`
([core]): `Name`, `Schedule` (cron 5 campos, segundos opcional como 6º — el
parser de robfig se construye con `SecondOptional`), `Run`,
`RunOnStart`, `PreventManualExecution`, `PreventCustomInterval`, `Hidden` y
`ExpectedDuration` (warn de log al superarlo; default upstream 2500 ms,
[core]). `Register` entra en pánico ante nombre duplicado, schedule que no
parsea, `Run` nil o nombre con puntos/espacios (el nombre es segmento del
topic del bus); upstream garantiza lo primero en compilación (los jobs son
claves de un objeto literal en [jobs]) y lo segundo con el type-guard
`ValidateCron` ([valid]).

`Status` (`List`) es la forma exacta de `JobManager.getAllAsync` ([manager]):
`{name, cron, preventManualExecution, preventCustomInterval, isEnabled}`,
filtrando los `hidden` y usando el override de la BD si existe (salvo
`preventCustomInterval`).

## 2. Registro de jobs upstream (pinned)

| Job | Schedule ([expr]) | Opciones | Estado del port |
|---|---|---|---|
| `ping` | `EVERY_MINUTE` = `* * * * *` | `beforeStart: resetPreviousUrlsAsync` ([ping]:18) | ✅ portado (`jobs.PingJob`) |
| `analytics` | `EVERY_WEEK` = `0 0 * * 1` | `runOnStart, preventManualExecution, preventCustomInterval, hidden` ([analytics]:8-13) | ⏸ M4 (necesita `@homarr/analytics`) |
| `iconsUpdater` | `EVERY_WEEK` = `0 0 * * 1` | `runOnStart, expectedMaximumDurationInMillis: 10000` ([icons]:14-18) | ⏸ M4 (necesita `@homarr/icons`) |

Constantes de schedule portadas 1:1 en `definitions.go` (las 8 de [expr]).
El nombre `ping` es el PK de las filas `cron_job_configuration` que escribe
la página manage/tasks upstream, por eso NO se renombra a "ping-integrations".

Arranque upstream ([tasks] main.ts): `onStartAsync()` (limpia sesiones de
proveedores de auth inactivos — dominio de `internal/auth`, fuera del port de
jobs) → `jobGroup.initializeAsync()` → `jobGroup.startAllAsync()`.
`initializeAsync` ([group]) lee TODAS las filas `cron_job_configuration` y
crea la tarea paused de cada job salvo que `isEnabled === false`;
`startAllAsync` reanuda las tareas creadas y ejecuta `onStartAsync` de cada
job (beforeStart + callback si `runOnStart`). El Scheduler Go refleja esto en
`Start(ctx)`: lee la tabla, no agenda los deshabilitados, agenda el resto con
su override de cron y ejecuta los `RunOnStart` de forma síncrona.

## 3. Estado persistido: `cron_job_configuration`

La tabla tiene EXACTAMENTE tres columnas ([schema], migración 0033 — NO hay
`lastRunAt`/`lastStatus`):

```sql
CREATE TABLE `cron_job_configuration` (
  `name` text PRIMARY KEY NOT NULL,
  `cron_expression` text NOT NULL,
  `is_enabled` integer DEFAULT true NOT NULL
);
```

La semántica de escritura replica `JobManager.updateConfigurationAsync`
([manager]): si la fila existe, UPDATE solo de los campos proporcionados (el
PK `name` nunca se reescribe); si no existe, INSERT con los defaults del job
(schedule de la definición, `is_enabled = true` — o el override explícito,
así `disableJob` sobre un job sin fila inserta `{name, cron default, 0}`).
`disableAsync`/`enableAsync` escriben `is_enabled`; `updateIntervalAsync`
escribe `cron_expression` y rechaza el override con
`Custom cron expressions are not allowed for job <name>` si
`preventCustomInterval`.

**El estado de ejecución (última corrida / resultado) NO vive en la BD**:
upstream lo publica por Redis en el canal `cron-job-status:<name>` y guarda
la última copia en la clave `pubSub:last:cron-job-status:<name>`
([status], [redis] createSubPubChannel persist). El port lo replica con el
mismo JSON por el bus (§4) y conserva la última copia en memoria
(`Scheduler.LastStatus`), que sobrevive a Stop/Start pero no a un reinicio
del proceso — igual que upstream pierde el last-data si se reinicia Redis.
Si M4 quiere lastRun durable, sería una decisión de esquema nueva (ver §7).

## 4. Eventos: TaskStatus por el bus

`TaskStatus` ([status] TaskStatus, portado en `status.go`):

```jsonc
{
  "name": "ping",
  "status": "running",                    // "running" | "idle"
  "lastExecutionTimestamp": "2026-11-06T12:34:56.789Z",  // ISO, precisión ms (Date.toISOString)
  "lastExecutionStatus": null             // "success" | "error" | null (antes de la 1ª corrida)
}
```

Ciclo del publisher ([status] publisher.ts, portado en `Scheduler.execute`):

1. `beforeCallback`: publica `{status: "running", lastExecutionTimestamp:
   AHORA, lastExecutionStatus: el de la corrida anterior ?? null}`.
2. `onCallbackSuccess`: `{status: "idle", lastExecutionStatus: "success",
   lastExecutionTimestamp: el del PASO 1}` (mantiene el timestamp de inicio).
3. `onCallbackError`: ídem con `"error"`; el callback nunca tumba al
   scheduler ([core] catchingCallbackAsync).

**Mapeo de topics (contrato con `internal/ws` y el tRPC cronJobs):**

| Canal Redis upstream | Topic bus Go | Payload |
|---|---|---|
| `pubSub:cron-job-status:<name>` | `events.cronjob.<name>` | `TaskStatus` JSON (§4) |
| `pubSub:ping` ([redis]:31-33) | `events.ping.<appId>` (ver INTEGRATIONS.md §3) | `{url, statusCode, durationMs}` / `{url, error}` |
| `list:ping-url` ([redis]:34) | sin topic — el job Go lee los targets de la tabla `app` (§6) | — |

Consumidor upstream del status: la suscripción tRPC
`cronJobs.subscribeToStatusUpdates` ([router], sobre el WS de
apps/websocket) se suscribe a los canales `cron-job-status:*` de TODOS los
jobs y emite el `TaskStatus` tal cual. En el port, `internal/ws` cubre ese
procedimiento suscribiéndose a `events.cronjob.*` y reenviando el payload
sin cambios.

## 5. Trigger manual

`cronJobs.triggerJob` ([router], permiso `admin`) → `JobManager.triggerAsync`
→ `runManuallyAsync` ([group]): lanza `The job "<name>" can not be executed
manually.` si `preventManualExecution` y si no ejecuta
`tasks.get(name)?.trigger()` (no-op silencioso si la tarea no existe).
`Trigger(ctx, name)` del port valida el nombre (`Job <name> not found`,
mensaje de [manager]) y el flag, y ejecuta la corrida de forma SÍNCRONA en la
goroutine llamadora.

## 6. El job `ping`

Port de [ping] `pingJob = createCronJob("ping", EVERY_MINUTE, ...)`:

1. **Guard** ([ping]:22-26): lee `serverSetting("board")` y salta la corrida
   si `forceDisableStatus` (default `false`, [settings]). El port lee la fila
   superjson directamente (`{"json":{"forceDisableStatus":...}}`), misma
   consulta que `getServerSettingByKeyAsync`.
2. **Targets**: upstream itera la lista Redis `list:ping-url`
   (`pingUrlChannel.getAllAsync`, [ping]:28) **que en el upstream actual nada
   rellena** (grep en packages/ + apps/: `pingUrlChannel` solo se usa en
   ping.ts para clear/getAll) — el job upstream efectivamente no pinge nada
   hoy. El port usa la fuente canónica del resto del sistema: los targets son
   **apps** (`widget.app.ping` resuelve `app.pingUrl ?? app.href`,
   packages/api/src/router/widgets/app.ts:30; el onboarding crea apps con
   `pingUrl`, onboard-router.ts). Query del port:
   `SELECT id, COALESCE(NULLIF(ping_url,''), href) FROM "app"`, filtrando a
   esquemas http(s) en Go (`pingUrl` está validado a http(s) upstream,
   packages/validation/src/app.ts:22-29; los `href` con esquemas custom tipo
   `vscode://` no se intentan — upstream los intentaría y fallaría, el port
   evita el ruido de un evento de error por bookmark cada minuto).
3. **Dedupe** ([ping]:31 `[...new Set(urls)]`): cada URL única se comprueba
   UNA vez por corrida; el resultado se publica para cada app que la comparte
   (upstream publicaba por URL sin id; el port publica por app para que el
   topic lleve el `appId`).
4. **Ejecución** (ver INTEGRATIONS.md §3 para la semántica HTTP exacta) con
   paralelismo acotado (`PingOptions.Parallelism`, default 10) y publicación
   por el bus `events.ping.<appId>` ([ping]:42 publica en `pingChannel`
   ambos casos, éxito y fallo).

## 7. Decisiones y desviaciones documentadas

1. **robfig/cron/v3 en lugar de croner**: parser `SecondOptional` para
   aceptar los 5 y 6 campos de [expr]; timezone local (default de croner con
   `timezone` indefinido en [core]).
2. **Trigger síncrono** (§5): upstream dispara y olvida (`void job
   .executeAsync()` dentro del callback de croner); el port espera la
   corrida — da a la mutación tRPC semántica natural y tests deterministas.
3. **Corridas solapadas permitidas**: igual que upstream (croner sin
   `protect`); cada corrida va en su goroutine (robfig `startJob`).
4. **Schedule inválido en la BD** (fila editada a mano): upstream lanzaría
   dentro de `initializeAsync` (croner lanza con patrón inválido); el port
   loguea el error y omite ese job (no tumba el arranque). `UpdateInterval`
   SÍ valida antes de persistir.
5. **`StartJob` no consulta `is_enabled`**: espejo de upstream, donde el gate
   solo aplica al arranque masivo ([group] initializeAsync);
   `startAsync` reanuda sin mirar la config.
6. **`UpdateInterval` sobre un job no agendado** solo persiste la expresión;
   el job la toma en su próximo Start/Enable (upstream reemplazaba la tarea
   por una `Cron(..., {paused: !isEnabled})` nueva — el efecto neto para un
   job parado es el mismo).
7. **`beforeStart` del job ping sin port**: solo limpiaba la lista Redis
   `ping-url`, que el port no mantiene (lee la BD en cada corrida).
8. **Última corrida en memoria** (§3): misma semántica que el last-data de
   Redis upstream; durabilidad cross-proceso fuera de alcance (igual que
   upstream). Preguntas abiertas en §9.
9. **`ensureRunContext`**: las corridas agendadas heredan el ctx del primer
   `Start`/`StartJob`; `Stop` las cancela (equivalente del SIGTERM de
   upstream, apps/websocket hace broadcast de reconnect al recibirlo).

## 8. Cobertura de tests (`go test ./server/internal/jobs/...`)

| Test | Cubre |
|---|---|
| `TestSchedulerRunsJobOnCronSchedule` | cron de 1 s (`* * * * * *`), disparos repetidos y silencio tras Stop |
| `TestSchedulerTriggerRunsImmediately` | Trigger síncrono + TaskStatus idle/success en LastStatus |
| `TestSchedulerTriggerGuards` | `Job <name> not found` y el mensaje exacto de preventManualExecution |
| `TestSchedulerPersistsConfigurationAndResumes` | fila con columnas upstream exactas, UPDATE/INSERT del JobManager, restart: disabled no corre, override de cron se respeta tras Enable |
| `TestSchedulerUpdateIntervalRejected` | preventCustomInterval (mensaje upstream) y expresión inválida |
| `TestSchedulerPublishesTaskStatusEvents` | running(null)→idle(success) y running(success)→idle(error), timestamp de inicio conservado, JSON con las 4 claves |
| `TestSchedulerStatusTopicWildcard` | topic `events.cronjob.<name>` |
| `TestSchedulerLastStatusSurvivesRestart` | last-data sobrevive Stop/Start en el mismo proceso |
| `TestSchedulerRunOnStart` | corrida síncrona de arranque |
| `TestSchedulerListFiltersHidden` | hidden filtrado + forma de getJobs |
| `TestSchedulerConcurrentStartStop` | Start/Stop/StartJob/StopJob/Trigger/List concurrentes |
| `TestSchedulerRegisterValidation` | pánicos del registro (duplicado, cron inválido, Run nil, nombre con punto/espacio) |
| `TestPingJobEndToEnd` | seed de apps → Trigger → `events.ping.<appId>` con payload exacto (éxito y fallo), fallback href, esquemas excluidos, dedupe de URL (1 request, 2 eventos), TaskStatus en `events.cronjob.ping` |
| `TestPingJobForceDisableStatus` | guard de server settings |
| `TestPingJobParallelismBounded` | paralelismo acotado |
| `TestPingJobScheduleAndName` | nombre `ping`, schedule `* * * * *` |

Nota: `-race` requiere CGO y no hay compilador C en este entorno; el test de
concurrencia corre sin race detector localmente (los locks están auditados y
el closure de schedule captura valores, no campos mutables).

## 9. Wiring pendiente para M4 (para el integrador)

1. `cmd/homarr-go`: crear `bus.New()`, `jobs.New(database, bus, ...)`,
   `Register(jobs.PingJob(...))`, `Start(ctx)` tras la migración y `Stop()` en
   el shutdown (el ctx de signals.NotifyContext es el que heredan las
   corridas).
2. Router tRPC `cronJobs` (M4): 7 procedimientos de [router] sobre los
   métodos del Scheduler (§1); `subscribeToStatusUpdates` vía `internal/ws`
   suscrito a `events.cronjob.*`; el enum de nombres de job debe salir de
   `List`/registro (upstream usa `jobGroup.getKeys()`).
3. Jobs `analytics` e `iconsUpdater`: shell + callback cuando existan
   `@homarr/analytics` y `@homarr/icons` en Go; sus opciones upstream están
   en §2.
4. Durabilidad opcional del lastRun (§3): si se decide, valorar una tabla
   nueva Go-only (los snapshots drizzle de upstream NO la conocen — requiere
   documentarlo en SCHEMA.md) o reusar `serverSetting`.
5. Escalado a los demás jobs de dominio (docker stats, certificates,
   notifications si upstream los añade): solo `Definition` + `Register`.
