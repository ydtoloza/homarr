# DOCKER.md — dominio Docker (`internal/integrations/docker` + `internal/api/router/docker`)

Documento de referencia del dominio Docker del port: el cliente de la Docker
Engine API (`internal/integrations/docker`) y el router tRPC
(`internal/api/router/docker`). Complementa `server/ARCHITECTURE.md`,
`server/internal/api/PROTOCOL.md` y `server/internal/ws/WEBSOCKET.md`. Todas
las afirmaciones citan fuente upstream; las desviaciones están marcadas.
Fecha: 2026-11-07.

Fuentes citadas (todas en este repo):

- **[router]** `packages/api/src/router/docker/docker-router.ts`
  (procedimientos, gates, mapeo de resultados).
- **[mw]** `packages/api/src/middlewares/docker.ts` (dockerMiddleware).
- **[rh]** `packages/request-handler/src/docker.ts` (listing+stats, logs,
  endpoints, capacidades, timeout de 5 s).
- **[decode]** `packages/request-handler/src/docker-log-decode.ts`
  (demultiplexor del stream de logs).
- **[pkg]** `packages/docker/src/index.ts` (tipos ContainerState,
  DockerContainerTarget, DockerEndpointStatus), **[singleton]**
  `packages/docker/src/singleton.ts` (endpoints desde env + dockerode),
  **[epdesc]** `packages/docker/src/endpoint-descriptor.ts` (descriptores,
  capacidades), **[labels]** `packages/docker/src/labels.ts`,
  **[env]** `packages/docker/src/env.ts`.
- **[defs]** `packages/definitions/src/release-provider.ts`
  (getReleaseSourceFromDockerImage), **[match]**
  `packages/definitions/src/docker-integration-match.ts`
  (extractContainerImageName), **[common]**
  `packages/common/src/encryption.ts` (createKeyedFingerprint),
  **[common-str]** `packages/common/src/string.ts` (bestMatch).
- **[frontend]** `packages/widgets/src/docker/component.tsx` (widget),
  `apps/nextjs/src/app/[locale]/manage/tools/docker/**` (tools admin),
  `apps/nextjs/src/app/[locale]/manage/tools/docker/logs/[containerId]/`
  (terminal xterm).
- **[ws]** `server/internal/ws/subscription.go` + `conn.go` (contrato de
  suscripciones del port).

## 1. Qué es (y qué no es) el dominio Docker

- **NO es una integración** del framework `packages/integrations`: no existe
  en `integrationDefs` ni en la tabla `integration`. El widget docker y las
  tools de administración hablan directamente con el/los daemons Docker vía
  el wrapper `packages/docker` (dockerode) — [router] importa
  `DockerSingleton` a pelo.
- **NO hay tablas docker en la BD**: verificado contra
  `packages/db/schema/sqlite.ts` (cero menciones) y las migraciones del port.
  La tarea original planteaba "docker endpoint table" — no existe upstream.
- Los **endpoints vienen de variables de entorno** ([singleton]:52-100), con
  dockerode instanciado por descriptor. `ENABLE_DOCKER` (default `true`,
  [env]) gatea todo el router vía dockerMiddleware ([mw]); `DEMO_MODE`
  muestra contenedores mock ([rh]:44, 53-227).

## 2. Modelo de endpoints (Go: `docker.Registry`)

`docker.Registry` es el port de `DockerSingleton` ([singleton]): lista de
endpoints construidos + clientes cacheados + fallos de construcción.

Resolución (misma prioridad upstream, `EndpointsFromEnv`):

| Prioridad | Fuente | Formato |
|---|---|---|
| 1 | `DOCKER_ENDPOINTS` | JSON array de descriptores ([epdesc]:30-45): `id`, `name`, `kind` (`docker`\|`podman`), `transport` (`socket`/`tcp` con `allowInsecure:true`/`tls`), `capabilities` (default: todas; debe incluir `inventory`) |
| 2 | `DOCKER_SOCKET_PATHS` | rutas de sockets separadas por coma (legacy) |
| 2 | `DOCKER_HOSTNAMES` + `DOCKER_PORTS` | listas paralelas separadas por coma (legacy; longitudes deben coincidir) |
| 3 | `DOCKER_HOST` | **[extensión fork]** URL estilo docker CLI: `unix://`, `npipe://`, `tcp://` o ruta pelada |
| 4 | default | `/var/run/docker.sock` (unix) / `//./pipe/docker_engine` (Windows, **[extensión fork]** — upstream hardcodea la ruta unix también en Windows, donde nunca funciona) |

Descriptores → `docker.Endpoint{ID, Name, Kind, Transport, Capabilities,
Source, Scope}`. Ids upstream: `socket:<path>`, `tcp:<host>:<puerto>`,
`socket:default` para el fallback ([epdesc]:136-154, [singleton]:89-97).

**Capacidades** ([epdesc]:3-4): `inventory` (listar), `logs`, `lifecycle`
(start/stop/restart), `remove`. El router las comprueba ANTES de tocar el
Engine (assertDockerEndpointCapability, [router]:297-303 → FORBIDDEN
`Docker endpoint does not permit <cap> access`).

**TLS**: parseado y listado (getEndpoints lo reporta), pero
`docker.NewClient` devuelve `ErrTLSNotImplemented` — el transporte TLS queda
marcado NOT_IMPLEMENTED (upstream lo soporta con dockerode +
`readFileSync` de ca/cert/key, [singleton]:143-151). Un CA/cert/key ilegible
falla la CONSTRUCCIÓN del endpoint ([singleton]:35-50) y el endpoint aparece
como `unavailable` en vez de desaparecer.

## 3. Cliente Engine API (`docker.Client`)

`net/http` puro con `DialContext` custom — **sin dependencias nuevas**:

| Transporte | Conexión |
|---|---|
| `socket` unix | `net.Dial("unix", path)` |
| `socket` npipe (Windows) | `os.OpenFile("//./pipe/<name>")` envuelto en `net.Conn` (sin deadlines; CreateFile vía el paquete os) |
| `tcp` | `net.Dialer` contra `host:puerto` (HTTP plano, como upstream: `protocol: "http"`, [singleton]:142) |
| `tls` | `ErrTLSNotImplemented` (§2) |

Rutas **sin prefijo de versión** (`/containers/json`): el Engine sirve el
path sin versionar a su última API y todas las rutas usadas son estables.
Upstream (dockerode) prefija su versión default; la forma sin versión evita
clavar una.

Superficie (params de query EXACTOS del port; upstream serializa booleans
como `true` vía querystring de node):

| Método | Ruta + query | Semántica upstream |
|---|---|---|
| GET | `/containers/json?all=true` | `listContainers({all: true})` sin filters ([rh]:386) |
| GET | `/containers/{id}/json` | inspect — solo existencia ([rh]:241-252; **cualquier** error ⇒ contenedor desconocido) |
| POST | `/containers/{id}/start` \| `stop` \| `restart` | sin `?t=` (dockerode sin opciones) |
| DELETE | `/containers/{id}` | sin `?v=`/`?force=` (dockerode sin opciones) |
| GET | `/containers/{id}/logs?stdout=true&stderr=true&follow=false&tail=N` | `getContainerLogsAsync` ([rh]:260-265) |
| GET | `/containers/{id}/logs?stdout=true&stderr=true&follow=true` | stream del pump (sin `tail` con tail=0) |
| GET | `/containers/{id}/stats?stream=false&one-shot=true` | stats one-shot ([rh]:455-458) |

Timeouts: `DefaultTimeout` = 5 s (`dockerWidgetEndpointTimeoutMs`, [rh]:18)
aplica a llamadas one-shot (`doJSON`) y al dial TCP; el **stream de logs no
lleva ese timeout** (respondería cortado a los 5 s): lo acota
`ResponseHeaderTimeout` (5 s) para las cabeceras y el ctx del llamante.

Errores tipados → mapeo del router:

| Error Go | Origen | Mapeo en el router |
|---|---|---|
| `ErrNotFound` | Engine 404 | NOT_FOUND `Container not found` (vía findContainer) |
| `ErrConflict` | Engine 409 | string dentro del resultado por-target de las actions |
| `ErrUnreachable` | socket ausente / conexión rechazada / deadline | en listing ⇒ endpoint `unavailable` (no lanza); en inspect ⇒ `Container not found`; en tail del subscribe ⇒ NOT_FOUND |
| `ErrTLSNotImplemented` | transporte tls | construcción del endpoint |
| `*RequestError` | otros 4xx/5xx | INTERNAL_SERVER_ERROR con el mensaje del Engine (logs query) o string por-target (actions) |

La semántica "Engine inalcanzable" replica upstream: el listing de un
endpoint fallido NO falla la query — el endpoint sale `unavailable` y sin
contenedores (`Promise.allSettled`, [rh]:383-394, 423-437); un fallo de
stats DEGRADA el endpoint ([rh]:462-467, 513-519).

## 4. Router tRPC (`internal/api/router/docker`)

Paths EXACTOS de [router]:33-257. Gates en el orden upstream:
dockerMiddleware (§2 `Disabled`) → `permissionRequiredProcedure
.requiresPermission("admin")` (anónimo ⇒ UNAUTHORIZED; sin admin ⇒ FORBIDDEN
`Permission denied`) → zod input (BAD_REQUEST) → capacidad → acción. Las
mutaciones van envueltas en `admin.Writable` (credentialProcedure, regla de
casa de `internal/api/router/board/register.go`).

| Procedimiento | Tipo | Input | Output | Estado |
|---|---|---|---|---|
| `docker.getContainers` | query | `{endpointIds?: string[≤100]}` opcional (vacío = todos) | `{containers[], endpoints[], timestamp}` | ✅ |
| `docker.getEndpoints` | query | — | `DockerEndpointStatus[]` (`available`/`unavailable`) | ✅ |
| `docker.startAll` / `stopAll` / `restartAll` / `removeAll` | mutation | `{targets: [{endpointId, id}] 1..100}` | `[{target, success, error?}]` — **nunca lanza** ([router]:261-295); invalida la caché si alguno tuvo éxito | ✅ |
| `docker.logs` | query | `{endpointId, id, tail? 1..1000}` | `{logs: string}` decodificado | ✅ |
| `docker.refreshInventory` | mutation | — | `{}` — reset del Registry + invalidación de caché ([router]:67-70) | ✅ |
| `docker.subscribeLogs` | suscripción (WS) | `{endpointId, id, tail?}` | stream de strings (§5) | ✅ |
| `docker.reconcileServices` | query | — | candidates de reconciliación | ⏸ diferido (§7) |
| `docker.getServiceHealth` | query | — | proyección de salud por capas | ⏸ diferido (§7) |

Detalles de `getContainers` (port de `getContainersWithStatsAsync`,
[rh]:364-520):

- Caché in-process por input (ids ordenados) con TTL de 10 s
  (`DEFAULT_TTL_MS`, `packages/request-handler/src/lib/request-handler.ts:33`);
  la invalidan `refreshInventory` y las actions exitosas.
- Filtra el label `homarr.hide` ([rh]:391, [labels]:2).
- Stats one-shot por contenedor en paralelo (semáforo de 8; upstream usa
  `Promise.all` sin límite); `cpuUsage` = `calculateCpuUsage` con baseline
  delta del poll anterior (caché 10 min, [rh]:341-357 — el one-shot de Docker
  no trae `precpu_stats` utilizable) y `memoryUsage` = usage −
  cache/total_inactive_file/inactive_file.
- `iconUrl`: labels primero (`homarr.icon`; fallback `homepage.icon` con el
  mapeo Dashboard-Icons a PNG, [pkg]/parse-container-labels.ts:28-43), si no
  `bestMatch` sobre la tabla `icon` por nombre de imagen ([rh]:438-445 +
  [common-str]:9-34); `null` si nada.
- `releaseSourceToken`: HMAC (clave derivada de `SECRET_ENCRYPTION_KEY`,
  [common]:9-10) de `["releases-docker-suggestion-v1", provider,
  identifier]` — port de `getReleaseSourceFromDockerImage` ([defs]:90-121).
  Sin token cuando la imagen no mapea (p.ej. lscr con tag no-versión).
- `timestamp`: ISO-8601 string. Upstream serializa un `Date` por superjson;
  `dayjs().to(...)` del frontend acepta string (`packages/common/src/hooks.ts:11-27`) — validar en el e2e del M5.

Consumidores frontend (todos cubiertos): widget `dockerContainers`
(`getContainers` con `options.endpointIds`, `getEndpoints`,
`refreshInventory`, `start/stop/restart/removeAll` con 1 target, invalidate
de los dos procedimientos diferidos), tools admin `docker-table` y
`docker-reconciliation` (estos dos últimos también usan los diferidos), y el
input `multiReleasesRepositories` (`getContainers` para sugerir imágenes).
El flujo de logs: `docker.logs` (tail 1, server-side) + `subscribeLogs`
(tail 200, xterm) — [frontend] `logs/[containerId]/page.tsx:32` y
`terminal.tsx:34`.

## 5. Eventos de logs por el bus (`events.docker.logs.*`)

Upstream NO tiene canal Redis para logs de docker: el observable del
subscribeLogs emite los frames directamente por el WS ([router]:210-256). En
el port el hub WS entrega los data frames EXCLUSIVAMENTE desde el bus
([ws] subscription.go), así que una suscripción activa necesita un productor:

```
subscribeLogs (ws handler)
  ├─ gates (disabled → admin → input → capability → inspect)
  ├─ Initial:  follow=false, tail=N decodificado como UN string
  ├─ pump:     follow=true, tail=0  ──frames──▶  bus
  └─ Subscription{Pattern: "events.docker.logs.<endpointId>.<containerId>",
                  Decode: passthrough}
```

| Topic | Payload (JSON exacto) |
|---|---|
| `events.docker.logs.<endpointId>.<containerId>` | `"texto del frame decodificado"` (string JSON; el frontend hace `terminal.write(data)`, terminal.tsx:24-32) |

- **Topic**: convención `events.<dominio>.<id>` de `internal/bus/bus.go`;
  `docker.LogTopic(endpointID, containerID)`. El id del evento lleva los DOS
  niveles (`<endpointId>.<containerId>`) y la suscripción usa Pattern EXACTO
  (sin wildcard): ese es el filtro por-suscripción (el input solo tiene
  endpoint/id/tail, y el topic ya codifica los dos primeros).
- **Granularidad**: un evento POR FRAME multiplexado (stdout/stderr ya
  demultiplexados y en orden de stream), igual que upstream: el processor de
  [decode]:45-117 llama `onData` por frame y el observable emite cada chunk.
  El frame máximo es 1 MiB (`MAX_MESSAGE_SIZE`, [rh]:289); un frame mayor
  aborta el stream (`ErrFrameTooLarge`, [decode]:90-95 → onError+destroy).
  Streams no multiplexados (TTY) pasan crudos, troceados en frontera UTF-8.
- **Pump compartido**: un stream del Engine por par (endpoint, contenedor)
  con refcount lógico — cada suscriptor se cuelga del MISMO topic; N
  espectadores = 1 conexión al daemon. El Initial por suscriptor repone el
  histórico (`tail` del input, default 200), así el seguidor tardío no se
  queda sin contexto.
- **Backpressure (razonada)**: `bus.Publish` NUNCA bloquea y suelta
  mensajes cuando el buffer del suscriptor (64) se llena. Para logs es
  aceptable y preferible: (a) la vista es un terminal efímero, no una cola —
  nadie replayea 10 000 líneas al volver de un lag; (b) el Initial repone el
  `tail` reciente, así que un cliente lento solo pierde líneas
  intermedias, nunca el contexto; (c) un contenedor que inunda stdout no
  puede bloquear el pump ni crecer la memoria del proceso (upstream, con un
  stream directo por suscriptor, aplicaría backpressure TCP al daemon o
  reventaría igual). Documentado aquí como decisión del port.
- **Ciclo de vida del pump**: el contrato `ws.SubscriptionHandler` no tiene
  callback de unsubscribe ([ws]), así que el refcount exacto es imposible
  hoy. El pump se detiene por: EOF (contenedor borrado), error de stream
  (frame gigante, transporte) o **idle TTL de 30 min sin frames**
  (`Options.LogPumpIdleTimeout`). Un nuevo subscribe reinicia el pump si
  murió. Coste de un leak: una conexión HTTP idle que se auto-libera.
- **Desviación documentada**: tras arrancar el stream, upstream emite un
  error frame INTERNAL_SERVER_ERROR dentro de la suscripción
  ([router]:226-235); el bus no tiene canal de errores, así que el pump
  para en silencio y el terminal se queda quieto (el usuario refresca).
  El fallo en el ARRANQUE sí replica el mapping upstream: NOT_FOUND con el
  mensaje ([router]:237-246).
- El contenedor del terminal real pide `tail: 200` (terminal.tsx:35); el
  pump usa el tail del PRIMER suscriptor y los siguientes reciben su propio
  Initial con su tail — la única discrepancia (menor) es que un pump vivo no
  re-lee el tail del stream.

## 6. Desviaciones y decisiones del port (resumen)

1. **Endpoints por env, no por BD** (§2): upstream no tiene tabla docker;
   la mención "docker endpoint table" del enunciado no existe en el schema.
2. **`DOCKER_HOST` como fuente extra** y default de Windows por named pipe:
   extensiones fork (§2).
3. **Paths sin versión** (§3) en vez del prefijo default de dockerode.
4. **Errores de acción como `error.Error()` de Go** en el campo `error` del
   resultado por-target: upstream ponía `error.message` de dockerode. La
   FORMA (`{target, success, error}`) es el contrato; el string es libre
   (mismo criterio que INTEGRATIONS.md §5.6).
5. **timestamp ISO string** en vez de Date superjson (§4).
6. **Pump con idle TTL** en vez de refcount exacto, y **sin error frames**
   post-arranque (§5) — consecuencia del contrato actual del hub WS.
7. **Paralelismo acotado a 8** en stats (upstream `Promise.all` sin límite)
   para proteger el proceso SQLite pequeño (criterio JOBS/INTEGRATIONS).
8. **Demo mode no portado**: los contenedores mock de [rh]:53-227 son solo
   para homarr.dev; `Options.Disabled` cubre el gate real. Añadir
   `Options.DemoMode` si se necesita.

## 7. Procedimientos diferidos

`docker.reconcileServices` y `docker.getServiceHealth`
([router]:34-55 + `docker-reconciliation.ts`):

- Necesitan el motor de matching de integraciones
  (`matchDockerService`, `buildDockerServiceUrlCandidates` —
  `packages/definitions/src/docker-integration-match.ts:40-56` +
  `docker-service-discovery.ts`) que depende de las ~100 entradas de
  `integrationDefs` (iconUrl, aliases docker, discoverable) — se porta con
  el lote M4 de integraciones.
- Necesitan leer `integration` (+ `items` + `secrets` kinds) y `apps` de la
  BD; el paquete `internal/db` aún no tiene repositorio de integraciones y
  los secretos descifrados llegan con la capa de cifrado (INTEGRATIONS.md
  §7.3). `apps` ya existe (`ListAllApps`), integraciones no.
- Impacto frontend: la página manage/tools/docker muestra los paneles de
  reconciliación vacíos (queries fallan con NOT_FOUND de procedimiento
  inexistente); el widget docker y el flujo de logs funcionan completos.
  Opción intermedia si el integrador lo prefiere antes de M4: registrar
  ambos devolviendo `candidates: []` con los endpoints reales.

## 8. Montaje

```go
r := trpc.New()
bus := bus.New()

registry, err := docker.RegistryFromEnv(os.Getenv.Getenv) // os.Getenv
svc := dockerrouter.Register(r, database, registry, dockerrouter.Options{
    Bus: bus,
})
for path, handler := range svc.SubscriptionHandlers() {
    hub.Register(path, handler) // "docker.subscribeLogs"
}
// chi: r.Handle("/api/trpc/*", r.Handler()); mount hub.Handler() at /websockets
```

## 9. Cobertura de tests

| Test (paquete `integrations/docker`) | Cubre |
|---|---|
| `TestListContainersQuery` | GET /containers/json, `all=true`/ausente, mapeo de ContainerInfo |
| `TestInspectAndActions` | inspect + POST start/stop/restart y DELETE remove SIN query params (dockerode sin opciones) |
| `TestActionErrorMapping` | 409 → ErrConflict con mensaje del Engine; 404 → ErrNotFound |
| `TestLogsQueryAndDecode` / `TestLogsFollowQuery` | query exacta tail/stdout/stderr/follow + DecodeLogs multiplexado |
| `TestStatsOneShotQuery` | `stream=false&one-shot=true`, cpu 20% y memoria 1024−256 |
| `TestUnknownEndpointAndUnreachable` | ErrUnknownEndpoint; socket caído ⇒ ErrUnreachable en containers/logs/stats |
| `TestTLSEndpointNotImplemented` | ErrTLSNotImplemented; CA ausente ⇒ failure de construcción |
| `TestRequestTimeout` | timeout por-request ⇒ ErrUnreachable |
| `TestDecodeLogs*` / `TestFrameReader*` (demux_test.go) | fixtures de docker-log-decode.spec.ts + frames fragmentados, TTY passthrough, límite 1 MiB, UTF-8 |
| `TestCalculateCPUUsage` / `TestCalculateMemoryUsage` / `TestStatsCacheTTL` | casos del spec upstream + baseline previa + TTL 10 min |
| `TestExtractContainerImageName` / `TestGetReleaseSourceFromDockerImage` / `TestReleaseSourceToken` | ports de definitions/release-provider |
| `TestParseEndpointDescriptors` (+ errors, legacy, default, DOCKER_HOST, Registry cache+Reset) | endpoint-descriptor.spec.ts + singleton env |

| Test (paquete `router/docker`) | Cubre |
|---|---|
| `TestPermissionGates` | UNAUTHORIZED anónimo / FORBIDDEN `Permission denied` / admin por GRUPO en los 8 procedimientos |
| `TestDisabledDockerGate` | NOT_FOUND `Docker route is not available` (queries, mutation y subscribe) |
| `TestGetContainersShape` | forma exacta del output (resourceId, host, ports IP/PrivatePort, cpuUsage/memUsage, releaseSourceToken, homarr.hide, iconUrl de label y null) |
| `TestGetContainersEndpointUnavailableAndDegraded` | endpoint caído ⇒ `unavailable`; stats 500 ⇒ `degraded` |
| `TestGetContainersInputValidation` | endpointIds > 100 ⇒ BAD_REQUEST |
| `TestActionResultsAndWritable` | `{target, success, error}` (éxito, 404 inspect, 409 Engine, endpoint desconocido), validación de targets, Writable con API key read-only |
| `TestRemoveAllCapabilityError` | `Endpoint does not permit remove actions` |
| `TestLogsQuery` | `{logs}` decodificado, tail default 200, validación 1..1000, NOT_FOUND, FORBIDDEN por capacidad |
| `TestRefreshInventoryInvalidatesCache` | caché 10 s + invalidación |
| `TestGetEndpoints` / `TestIconFromTable` | statuses + bestMatch de la tabla icon |
| `TestSubscribeLogsGates` | gates del handler WS en orden upstream |
| `TestSubscribeLogsInitialAndBusForward` | Initial = tail decodificado, Pattern exacto, pump tail=0, payload JSON-string, Decode passthrough, sin leakage a otros topics, escaping |
| `TestPumpSharedAcrossSubscribers` | 2 suscriptores ⇒ 1 stream del Engine |
| `TestPumpIdleStop` | pump muere por idle TTL y el siguiente subscribe abre stream nuevo |
| `TestSubscribeTailSnapshotError` | engine muerto ⇒ NOT_FOUND |

## 10. Preguntas abiertas para el integrador

1. **Callback de unsubscribe en `ws.Subscription`**: si el hub añade un
   `Close func()` (o ctx cancelable por suscripción), sustituir el idle TTL
   del pump por refcount exacto (§5) — un cambio local en `pumpRegistry`.
2. **Reconciliación (§7)**: ¿se porta en M4 con el lote de integraciones o
   se registra como stub `candidates: []` antes? La página admin queda
   rota (suave) hasta entonces.
3. **Puente de `events.docker.logs.*` a otros consumidores** (p.ej.
   notificaciones on-error): el topic ya está publicado; hoy solo lo consume
   `docker.subscribeLogs`.
4. **Demo mode** (§6.8): solo relevante si homarr-go debe servir la demo
   pública.
5. **TLS del Engine**: prioridad real para despliegues con daemons remotos;
   el stub está aislado en `NewClient`.
6. **`timestamp` de getContainers** (§4): confirmar en el e2e del M5 que
   `useTimeAgo` pinta bien con string ISO (dayjs acepta ambos).
