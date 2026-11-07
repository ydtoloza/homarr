# INTEGRATIONS.md — framework de integraciones + ping (`internal/integrations`)

Documento de referencia del paquete `server/internal/integrations`: el port en
Go del framework de integraciones de upstream (`packages/integrations`) y de
la funcionalidad de ping (`packages/ping` + el cron job `ping` +
`widget.app.ping`). Complementa `server/ARCHITECTURE.md`,
`server/internal/jobs/JOBS.md` y `server/internal/bus/bus.go`. Todas las
afirmaciones citan fuente upstream; las desviaciones están marcadas.
Fecha: 2026-11-06.

Fuentes citadas (todas en este repo):

- **[factory]** `packages/integrations/src/base/creator.ts` (mapa
  `integrationCreators`, createIntegrationAsync), **[base]**
  `packages/integrations/src/base/integration.ts` (clase Integration +
  IntegrationInput), **[base-types]** `packages/integrations/src/base/types.ts`
  (IntegrationSecret), **[types]** `packages/integrations/src/types.ts`,
  **[index]** `packages/integrations/src/index.ts`.
- **[defs]** `packages/definitions/src/integration.ts` (integrationDefs,
  integrationKinds en :1078, integrationSecretKindObject, IntegrationKind).
- **[ping]** `packages/ping/src/index.ts` (sendPingRequestAsync),
  **[timeout]** `packages/core/src/infrastructure/http/timeout.ts`
  (withTimeoutAsync, default 10000),
  **[common-err]** `packages/common/src/error.ts` (extractErrorMessage).
- **[job-ping]** `packages/cron-jobs/src/jobs/ping.ts`,
  **[redis]** `packages/redis/src/index.ts:31-34` (pingChannel / pingUrlChannel),
  **[widget-ping]** `packages/api/src/router/widgets/app.ts` (widget.app.ping),
  **[ping-ind]** `packages/widgets/src/app/ping/ping-indicator.tsx`,
  **[val-app]** `packages/validation/src/app.ts:22-29` (pingUrl http(s)),
  **[onboard]** `packages/api/src/router/onboard/onboard-router.ts`
  (creación de apps con pingUrl).

## 1. Registro del framework

```go
integrations.Register(integrations.Definition{Kind: "plex", Name: "Plex"}, factory)
inst, err := integrations.New("plex", integrations.Input{ID: "...", URL: "..."})
res, err := inst.Check(ctx)                    // (integrations.CheckResult, error)
kinds := integrations.Kinds()
def, err := integrations.DefinitionOf("plex")
```

- `Definition{Kind, Name}` replica una entrada de `integrationDefs` ([defs]):
  hoy solo `kind`+`name`; el resto del metadata (`iconUrl`, `secretKinds`,
  `category`, `httpAuth`, `defaultPort`, `documentationSlug`) se añade al
  `Definition` cuando M4 porte los procedimientos que lo consumen (el
  router de integraciones y el modal de configuración del frontend).
- `Input` replica `IntegrationInput` ([base]): `ID`, `Kind`, `Name`, `URL`,
  `ExternalURL` (`external_url`, null upstream). Los secretos descifrados
  (`decryptedSecrets`, [base-types]) llegan en M4 junto a la capa de cifrado
  de `integrationSecret` (`"iv.datos"`, AES-GCM de @homarr/encryption).
- `Integration` es el núcleo mínimo útil de la clase base upstream:
  `Check(ctx) (CheckResult, error)`. Los interfaces de dominio de upstream
  (`IMediaOrganizerIntegration`, calendario, media-server, ...) se convierten
  en interfaces de capacidad Go opcionales sobre las mismas instancias
  (asserts `if m, ok := inst.(MediaOrganizer); ok`) cuando M4 los porte.
- `CheckResult` es la unión de resultado que el frontend ya consume para
  resultados de alcanzabilidad: éxito `{"statusCode":200,"durationMs":12.5}`,
  fallo `{"error":"..."}` (mismos miembros que el payload del canal Redis
  `ping`, [redis]; el frontend discrimina con `"error" in pingResult`,
  [ping-ind]:20). `omitempty` reproduce la unión TS.
- `Registry`: `NewRegistry()` para instancias aisladas (tests) + registro
  global (`Register`/`New`/`Kinds`/`DefinitionOf`). Pánicos ante kind
  duplicado, kind con `.`/espacios (separador de topics del bus), `Name`
  vacío o factory nil — el equivalente en runtime del error de compilación
  que daría una clave duplicada en el objeto literal `integrationCreators`
  ([factory]).

## 2. Patrón factory + subpaquetes versionados

Upstream: cada kind tiene una factory lazy en `integrationCreators`
([factory]) que importa el chunk de la implementación versionada (p. ej.
`../plex/plex-integration.ts`). El port mantiene la forma mecánica:

```
internal/integrations/<name>/        # Definition() + init() con Register + factory
internal/integrations/<name>/v1/     # implementación (package <name>v1)
```

Para añadir una integración en M4 (mecánico):

1. `internal/integrations/<name>/v1/<name>.go` — la implementación con su
   interfaz de dominio; construcción desde `integrations.Input`.
2. `internal/integrations/<name>/<name>.go` — `Definition()` (kind = el
   string EXACTO de la columna `integration`.`kind` upstream,
   [defs] `integrationKinds`), `init()` con
   `integrations.Register(Definition(), factory)`, y helpers de topic/payload
   si el kind publica eventos.
3. Un blank import en `internal/jobs/definitions.go` (o en el composition
   root) — el patrón driver de database/sql; upstream lo hace con el mapa
   explícito de creators.

El registro es thread-safe; las instancias deben serlo para `Check`
concurrente (el job ping corre checks en paralelo).

## 3. La integración `ping`

**Upstream NO modela el ping como integration kind**: no existe en
`integrationKinds` ([defs]:1078); es el paquete `packages/ping` consumido por
(a) el cron job `ping` ([job-ping]) y (b) el procedimiento tRPC
`widget.app.ping` ([widget-ping]). El port lo envuelve como el pseudo-kind
`ping` (`integrations.KindPing`) para que el framework tenga un primer
ciudadano y el job sea un loader/publisher fino; la implementación vive en
`ping/v1`.

**Qué identifica un target pingable en la BD**: las **apps**, no las
integraciones. `widget.app.ping` resuelve `app.pingUrl ?? app.href`
([widget-ping]:30) y devuelve `CONFLICT "No URL to ping configured for this
app"` si no hay ninguna; `pingUrl` se valida a http(s) con `""` ⇒ null
([val-app]); el onboarding crea apps con `pingUrl` para sus servicios
destacados ([onboard]:455-474). La tabla `integration` no participa en el
ping.

**Semántica HTTP exacta** (port de `sendPingRequestAsync`, [ping]):

- Método **GET** (default de fetch), redirects seguidos (default de fetch).
- **TLS sin verificar**: dispatcher con `rejectUnauthorized: false`
  ("Ping should always work, even with untrusted certificates"). En Go:
  `http.Transport` con `InsecureSkipVerify: true`.
- **Timeout duro de 10 s** ([timeout] default 10000, `AbortController`);
  en Go `context.WithTimeout` por request (campo `Timeout` de
  `v1.Integration`, default `DefaultTimeout`, acortable en tests).
- **`durationMs`** mide desde antes del request hasta llegar los headers de
  la respuesta; el body nunca se lee (`void response.body?.cancel()`, en Go
  `Body.Close()` sin drenar).
- Éxito ⇒ `{statusCode: response.status, durationMs}`; fallo de transporte ⇒
  `{error: extractErrorMessage(error)}` ([common-err]) — **nunca lanza**:
  `Check` devuelve `(CheckResult{Error:...}, nil)` en ese caso, y solo
  `(zero, err)` si el propio check no puede ejecutarse (para ping: nunca).
- Los errores de estado ≥500 NO son fallo del check: el payload lleva
  `statusCode` y el frontend decide (`isError = "error" in pingResult ||
  statusCode >= 500`, [ping-ind]:20).

**Evento por el bus** (contrato con `internal/ws`, mapeo del canal Redis
`ping`):

| Topic | Payload (JSON exacto) |
|---|---|
| `events.ping.<appId>` | `{"url":"...","statusCode":200,"durationMs":12.5}` (éxito) |
| `events.ping.<appId>` | `{"url":"...","error":"..."}` (fallo) |

Es la unión exacta de `pingChannel` ([redis]:31-33), con el `appId` en el
topic en vez del payload (convención `events.<dominio>.<id>` de bus.go).
Upstream publica ese canal desde el cron job ([job-ping]:42) pero hoy no
tiene consumidores (el WS de apps/websocket no lo suscribe; el dot de ping
del widget app usa la QUERY tRPC `widget.app.ping`, [ping-ind]:14). El port
publica igualmente y documenta el topic para que M4 decida si el WS lo
puentea a los widgets (mejora viable sobre upstream).

## 4. Mapeo bus ← Redis (resumen)

| Canal Redis upstream | Topic bus Go | Emisor Go | Consumidor previsto |
|---|---|---|---|
| `pubSub:ping` | `events.ping.<appId>` | job `ping` (`ping.Publish`) | ws → widgets (M4, opcional) |
| `pubSub:cron-job-status:<name>` | `events.cronjob.<name>` | Scheduler (status.go) | ws → `cronJobs.subscribeToStatusUpdates` |
| `list:ping-url` | (sin topic) | — | — (el job lee la tabla `app`) |

## 5. Decisiones y desviaciones documentadas

1. **Ping como pseudo-kind**: no existe upstream como kind (§3); el nombre
   de registro `ping` no colisiona con ningún `integrationKinds` real.
2. **Filtro http(s) de targets** ([job-ping] itera la lista Redis, que nadie
   rellena en el upstream actual): el port toma `ping_url ?? href` y excluye
   esquemas no-HTTP para no publicar errores cada minuto por bookmarks tipo
   `vscode://`. `widget.app.ping` upstream sí intentaría cualquiera.
3. **Dedupe de URLs a nivel de ejecución, no de publicación**: upstream
   publicaba un evento por URL única (sin id); el port ejecuta una comprobación
   por URL y publica un evento por app (para que el topic lleve el appId).
4. **Paralelismo acotado (default 10)**: upstream usa `Promise.allSettled`
   sin límite ([job-ping]:31); el job Go acota con `PingOptions.Parallelism`
   para proteger un proceso SQLite pequeño.
5. **`Timeout` como campo de `v1.Integration`**: upstream lo hardcodea a
   10 s ([timeout]); el campo permite acortarlo en tests y se mantiene el
   default exacto.
6. **Mensajes de error de red no byte-compatibles**: `extractErrorMessage`
   devuelve `error.message` de undici ("fetch failed"...) y Go devuelve
   `Get "<url>": dial tcp ...` — la FORMA (`{url, error}`) es el contrato,
   el string es libre.
7. **Secrets pendientes (M4)**: `Input` no lleva aún `decryptedSecrets`
   ([base-types]); el framework está listo para ampliarlo sin romper las
   factories existentes.

## 6. Cobertura de tests

| Test (paquete) | Cubre |
|---|---|
| `TestRegistryRegisterAndResolve` (integrations) | factory por kind, metadata, inyección de Kind en Input |
| `TestRegistryUnknownKind` (integrations) | `ErrUnknownKind` en New y DefinitionOf |
| `TestRegistryDuplicateKindPanics` (integrations) | pánico por kind duplicado con mensaje |
| `TestRegistryInvalidRegistrationPanics` (integrations) | kind vacío/con punto/espacio, Name vacío, factory nil |
| `TestDefaultRegistryPingKind` (integrations) | autorregistro del kind ping por blank import |
| `TestCheckOnline` (ping/v1) | GET, 200 → statusCode+durationMs>0 |
| `TestCheckServerErrorStillAnswer` (ping/v1) | 500 sigue siendo respuesta definitiva |
| `TestCheckConnectionRefused` (ping/v1) | offline ⇒ `{error}` sin Go-error |
| `TestCheckTimeout` (ping/v1) | timeout duro aborta y reporta error |
| `TestCheckUntrustedCertificate` (ping/v1) | TLS self-signed OK (rejectUnauthorized:false) |
| `TestCheckBadScheme` / `TestCheckParentContextCancel` (ping/v1) | esquema inválido y ctx padre cancelado |
| `TestJSONShape` (ping/v1) | unión JSON exacta de CheckResult |
| `TestEventSuccessShape` / `TestEventFailureShape` (ping) | payload `{url,statusCode,durationMs}` / `{url,error}` exacto |
| `TestPublishTopic` (ping) | topic `events.ping.<appId>`, bus nil = no-op |
| (e2e del job) | `internal/jobs` ping_test.go — ver JOBS.md §8 |

## 7. Preguntas abiertas para M4

1. ¿Puentea `internal/ws` `events.ping.<appId>` hacia los widgets de apps
   (dot en vivo) o se mantiene la query `widget.app.ping` como upstream?
2. Escalado de los ~50 kinds restantes: copiar el patrón del §2; los que
   publiquen eventos necesitan su tabla de topics aquí (§4).
3. La capa de descifrado de `integrationSecret` (@homarr/encryption, AES-GCM
   `"iv.datos"`) debe vivir junto a `internal/db` o en un
   `internal/integrations/secrets` — definir antes de portar factories con
   auth.
4. `Input.ExternalURL` hoy solo se pasa; el uso real (URLs externas para
   OAuth/webhooks) llega con las factories que lo necesiten.
