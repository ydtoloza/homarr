# WebSocket — port Go de `apps/websocket`

Documento de referencia del paquete `server/internal/ws`: el endpoint
`/websockets` que habla tRPC v11 sobre WebSocket con el SPA. Todas las
afirmaciones están verificadas contra las fuentes upstream (rama `dev` de
este repo) o contra tRPC `11.18.0` (versión clavada en `package.json:108-112`).
Como `node_modules` no está instalado en el workspace, los ficheros de tRPC se
verificaron descargándolos en crudo de GitHub en el tag `v11.18.0`; las rutas
citadas son relativas a `packages/` del repo trpc/trpc. Fecha: 2026-11-06.

Citas:

- **[main.ts]** `apps/websocket/src/main.ts` (la app entera son 89 líneas).
- **[ws.ts]** `packages/server/src/adapters/ws.ts` (adaptador WS oficial de
  tRPC, montado por [main.ts] con `applyWSSHandler`).
- **[wsClient.ts]** `packages/client/src/links/wsLink/wsClient/wsClient.ts`,
  **[wsConnection.ts]** `.../wsConnection.ts`, **[options.ts]** `.../options.ts`,
  **[utils.ts]** `.../utils.ts` — el cliente `createWSClient` del SPA.
- **[envelopes.ts]** `packages/server/src/unstable-core-do-not-import/rpc/envelopes.ts`,
  **[parseTRPCMessage.ts]** `.../rpc/parseTRPCMessage.ts`,
  **[transformer.ts]** `.../transformer.ts`,
  **[getErrorShape.ts]** `.../error/getErrorShape.ts`,
  **[TRPCError.ts]** `.../error/TRPCError.ts`,
  **[router.ts]** `.../router.ts`.
- **[trpc.tsx]** `apps/nextjs/src/app/[locale]/_client-providers/trpc.tsx`
  (cómo el SPA reparte operaciones entre links).
- **[websocket.ts]** `packages/api/src/websocket.ts` (re-exporta `appRouter`
  y `createTRPCContext` — el "websocket router" es EL router tRPC completo;
  lo subscribable son los procedimientos `.subscription()`).

## 1. Qué es y qué no es

`apps/websocket` es un `WebSocketServer` del paquete `ws` en el **puerto
3001** ([main.ts]:13-15) al que se le enchufa el adaptador WS oficial de
tRPC ([main.ts]:35-75). El router que sirve es el **router tRPC raíz
completo** ([websocket.ts]:1-2) — no hay un router separado para WS. En la
práctica, el SPA solo manda **suscripciones** por WS: el `splitLink` de
[trpc.tsx]:193-205 dirige `type === "subscription"` al `wsLink`, salvo
`widget.beszel.subscribeSystemStats` que va por SSE (§8). Las queries y
mutations viajan por HTTP (`httpBatchStreamLink`) — jamás por el WebSocket.

El `wsLink` procesa TODOS los tipos de operación que le lleguen
([wsLink.ts]:20-54 → `client.request` manda `method: type`), y [ws.ts]
ejecutaría también `query`/`mutation` (`type !== 'subscription'` en
[ws.ts]:187-201), pero el cliente Homarr nunca los manda. homarr-go los
rechaza con el error "no procedure" del router (§6, desviación documentada).

## 2. Ruta y montaje

- nginx proxea `location /websockets → http://127.0.0.1:3001` con
  `Upgrade`/`Connection` (`nginx.conf:12-19`); todo lo demás va al Next.js
  del 3000. El binario Go montará `Hub.Handler()` directamente en
  `/websockets` (el integrador lo cablea en `httpserver`; hoy el mount point
  queda como seam).
- URL del cliente: `ws(s)://<host>:<puerto>/websockets` en producción
  ([trpc.tsx]:57-68, misma host:port que la página); en dev
  `ws://localhost:3001/websockets`. El cliente se crea con
  `createWSClient({ url, lazy: { enabled: true, closeMs: 30_000 } })`
  ([trpc.tsx]:108-115): conexión perezosa, se abre con la primera
  suscripción y se cierra tras 30 s sin actividad
  ([wsClient.ts]:67-77, [options.ts]:72-75).
- **`connectionParams`**: el SPA no pasa `connectionParams` al cliente, así
  que nunca se envía `?connectionParams=1` ni el primer mensaje
  `{"method":"connectionParams","data":...}` ([utils.ts]:74-96: solo se
  añade cuando `urlOptions.connectionParams` existe; [ws.ts]:157-159 crea el
  contexto inmediatamente en ese caso). homarr-go no implementa el modo
  connectionParams (documentado; upstream solo lo activa si la URL lo pide).

## 3. Protocolo del wire

Codificación: **JSON en text frames** (`jsonEncoder`, `wsEncoder.ts:24-35`).
Superjson envuelve `result.data` y el shape de error completo; los frames
`started`/`stopped` **no llevan clave `data`**
([transformer.ts]:`transformTRPCResponseItem`:92-113 — solo envuelve cuando
existe `'data' in item.result`, y el error entero va envuelto). Con payloads
planos, superjson 2.x produce exactamente `{"json": <valor>}` sin `meta`
(ver PROTOCOL.md §8).

### 3.1 Cliente → servidor

```jsonc
// suscribirse (parseTRPCMessage.ts:51-89 valida: id no-null, method ∈
// {query, mutation, subscription}, params.path string; input superjson)
{"id":1,"method":"subscription","params":{"path":"log.subscribe","input":{"json":{"levels":["info"]}}}}
// cancelar por id de la suscripción (wsClient.ts:229-233;
// parseTRPCMessage.ts:61-67 deja pasar solo id+method)
{"id":1,"method":"subscription.stop"}
```

- **Batch**: el cliente puede mandar un **array** de mensajes
  ([wsClient.ts]:407-414 codifica array si hay varios en el flush;
  [ws.ts] `client.on('message')`: `Array.isArray(msgJSON) ? msgJSON :
  [msgJSON]`). El parseo es all-or-nothing: un mensaje estructuralmente
  inválido produce UN solo frame `PARSE_ERROR` con `id: null` y se descarta
  el lote entero ([ws.ts]:488-509, el try/catch envuelve todo el `.map`).
- **`id`**: número o string JSON, nunca null — `` `id` is required `` con
  `PARSE_ERROR` si es null ([ws.ts]:135-155). homarr-go normaliza "id
  ausente" al mismo error (upstream respondería con un frame sin `id`,
  comportamiento de-facto muerto: el cliente siempre manda id).
- **`lastEventId`**: si viene en `params`, el adaptador lo fusiona en el
  input ([ws.ts]:171-180): objeto → se añade la clave; input ausente → se
  sustituye por `{"lastEventId": ...}`; otro valor → se pierde. Es la
  reanudación de suscripciones tras reconexión (solo la usa el link SSE con
  `tracked()`; el wsLink la manda si el servidor dio `result.id`).
- **Keepalive**: text frames `PING`/`PONG` **fuera** del protocolo JSON-RPC;
  [ws.ts]:466-470 responde `PONG` a un `PING` del cliente; [wsConnection.ts]:178-182
  (cliente) responde `PONG` al `PING` del servidor. Ambos lados los saltan
  en el dispatch JSON ([wsClient.ts]:323-324).

### 3.2 Servidor → cliente

```jsonc
{"id":1,"result":{"type":"started"}}                                   // suscripción aceptada
{"id":1,"result":{"type":"data","data":{"json":<payload superjson>}}}  // cada valor
{"id":1,"result":{"type":"stopped"}}                                   // fin (stop o completion)
{"id":1,"error":{"json":{"message":"...","code":-32004,"data":{"code":"NOT_FOUND","httpStatus":404,"path":"..."}}}}
{"id":null,"error":{"json":{...}}}            // errores de protocolo/creación de contexto
{"id":null,"method":"reconnect"}              // broadcast en SIGTERM ([ws.ts]:617-627)
```

Semántica de la suscripción ([ws.ts] `handleRequest`, rama subscription):

1. Se ejecuta el procedimiento; si lanza → frame de error con el `id` de la
   petición y **la conexión sigue abierta** (el catch de [ws.ts]:379-390
   responde y no cierra).
2. Si el `id` ya tiene una suscripción viva → `BAD_REQUEST` `Duplicate id
   <id>` ([ws.ts]:245-253).
3. Al arrancar se manda `started`; cada valor del observable/generador es un
   frame `data` ([ws.ts]:286-335); si el `id` llega duplicado **después** de
   que el procedimiento devuelva, es `BAD_REQUEST` — por eso el chequeo de
   duplicados precede al `started` en homarr-go.
4. Al recibir `subscription.stop` se aborta la suscripción y se responde
   `stopped` ([ws.ts]:157-162 aborta; el bucle emite `stopped` tras
   `iterator.return()`, [ws.ts]:337-345). Stop de un id desconocido: **no-op
   silencioso** (`clientSubscriptions.get(id)?.abort()`).
5. En cierre de conexión se abortan todas las suscripciones sin frames de
   despedida ([ws.ts]:587-594, `client.once('close')`).

El shape de error es el `getErrorShape` estándar ([getErrorShape.ts]:21-35):
`{message, code: <numérico>, data: {code: <clave>, httpStatus, path?}}` —
`path` solo si el servidor conoce la ruta; `data.stack` solo en dev (nunca
en homarr-go; ver PROTOCOL.md §5 para la tabla completa códigos↔HTTP).

### 3.3 Errores de protocolo (frames con `id: null`)

| Situación | Código | Mensaje | Fuente |
|---|---|---|---|
| `id: null` en la petición | PARSE_ERROR (-32700) | `` `id` is required `` | [ws.ts]:135-155 |
| JSON del frame inválido | PARSE_ERROR (-32700) | mensaje del parseo (V8; en Go el de `encoding/json`) | [ws.ts]:488-509 — **el mensaje no es byte-estable, el código sí** |
| method no es query/mutation/subscription | PARSE_ERROR (-32700) | `Invalid procedure type` | [parseTRPCMessage.ts]:14-18 + catch de [ws.ts]:494-509 (el mensaje sale de `cause?.message ?? code`, [TRPCError.ts]:76) |
| params no es objeto | PARSE_ERROR (-32700) | `Not an object` | [parseTRPCMessage.ts]:7-11 |
| path/lastEventId no string | PARSE_ERROR (-32700) | `Invalid string` | [parseTRPCMessage.ts]:35-39 |
| binario con el encoder JSON | PARSE_ERROR (-32700) | mensaje del encoder (`jsonEncoder received binary data...`) | [wsEncoder.ts]:26-34 |
| createContext lanza (API key) | UNAUTHORIZED (-32001) | `API keys are not supported over WebSockets` | [main.ts]:41-43 |

En todos los casos **la conexión permanece abierta** salvo el último
(createContext), que responde el frame y cierra ([ws.ts]:99-135:
`respond(...)` + `client.close()` en el siguiente tick).

## 4. Autenticación

El chequeo ocurre en `createContext` **después** del upgrade HTTP (el
handshake 101 ya se completó; [main.ts]:40-66):

1. **Header `ApiKey` presente** → `throw new TRPCError({ code:
   "UNAUTHORIZED", message: "API keys are not supported over WebSockets" })`
   ([main.ts]:41-43). El adaptador convierte el throw en un frame de error
   `{id: null, ...}` inmediato y **cierra la conexión** en el siguiente tick
   ([ws.ts]:99-135). Nota: un navegador no puede poner headers custom en el
   handshake WS, así que esta rama solo la disparan clientes no-browser.
2. **Cookie de sesión**: se lee `homarr.session-token`
   (`AUTH_COOKIE_PREFIX` + `.session-token`, `packages/auth/session.ts:11`)
   y se resuelve con `getSessionFromToken` (`packages/auth/session.ts:21-55`):
   token ausente/desconocido/expirado → `null` → **contexto anónimo**.
3. **Error interno del resolver** (p.ej. DB caída): el try/catch de
   [main.ts]:59-65 lo registra y prosigue con `session: null` — de nuevo
   anónimo. (homarr-go replica: `Validate` con error → log + anónimo.)

**No hay rechazo a nivel HTTP**: un upgrade sin cookie se acepta igual que
uno autenticado; el contexto anónimo hace que cada suscripción falle por su
permiso (los procedimientos upstream usan `permissionRequiredProcedure`,
`packages/api/src/trpc.ts`) con un frame `UNAUTHORIZED` por petición. Por
eso no hay "close code de upgrade rechazado": no existe tal camino upstream.

Cierre post-rechazo-de-API-key: upstream llama a `client.close()` sin
código → close frame **sin status** (el peer ve 1005). coder/websocket no
puede emitir un close sin código, así que homarr-go cierra con
`GoingAway` (1001). El frame de error anterior es lo que el cliente
procesa; el código de cierre es cosmético (desviación documentada).

## 5. Keepalive

Configuración upstream ([main.ts]:67-74): `pingMs: 30000`, `pongWaitMs:
5000`. Algoritmo (`handleKeepAlive`, [ws.ts]:541-577):

- Cada 30 s el servidor manda el text frame `PING` y arma un temporizador
  de 5 s.
- **Cualquier** mensaje entrante (PONG, suscripción, lo que sea) reinicia
  ambos temporizadores — el `on('message')` de keepalive se dispara con
  todo ([ws.ts]:560-564).
- Si a los 5 s del PING no llegó nada → `client.terminate()` (cierre
  abrupto, sin close frame → el peer ve 1006/abnormal).
- El cliente del SPA **no** tiene keepalive propio (`createWSClient` sin
  `keepAlive` → `keepAliveDefaults.enabled: false`, [options.ts]:84-88);
  solo responde PONG a los PING del servidor ([wsConnection.ts]:178-182).

homarr-go: `Options.KeepAlive` (nil = desactivado) con `UpstreamKeepAlive()`
= 30 s/5 s; tests usan intervalos cortos. El terminate es `CloseNow()`.

## 6. Suscripciones que fluyen por WS (y su mapeo a bus)

Consumidores `useSubscription` del frontend (grep sobre apps/packages):

| tRPC path | Input | Emite | Fuente upstream |
|---|---|---|---|
| `log.subscribe` | `{levels: LogLevel[]}` (default `["info"]`) | `{type:"history", messages}` inicial y luego `{type:"message", message}` | `packages/api/src/router/log.ts:19-84` |
| `docker.subscribeLogs` | `{endpointId, id, tail?}` (admin) | chunks de texto del log del contenedor | `packages/api/src/router/docker/docker-router.ts:201-256` |
| `cronJobs.subscribeToStatusUpdates` | ninguno (admin) | `TaskStatus` por cada job | `packages/api/src/router/cron-jobs.ts:62-82` |
| `widget.beszel.subscribeSystemStats` | `{integrationIds, systemId}` | stats en vivo | **SSE, no WS** (§8) |

`widget.app.ping` NO es suscripción: es una query que el widget refreshea
por polling (`packages/api/src/router/widgets/app.ts:11-50`; el canal Redis
`ping` que publica el cron `ping` — `packages/cron-jobs/src/jobs/ping.ts:42`,
`packages/redis/src/index.ts:31-33` — no tiene consumidor en el frontend
hoy; queda mapeado por si los jobs lo reviven).

Mapeo a topics del bus (`events.<domain>.<id>`, convención de
`server/internal/bus/bus.go`), publicado por los productores Go (jobs /
integraciones) y consumido por los handlers de `internal/ws`:

| Suscripción | Topic/patrón bus | Payload (JSON crudo) | Notes |
|---|---|---|---|
| `log.subscribe` | `events.log.*` (publicador: el logger del binario) | `LoggerMessage` | el filtro por `levels` lo aplica el `Decode` del handler; la parte "history" es el `Initial` (seam: ring buffer de logs del binario) |
| `docker.subscribeLogs` | `events.docker.logs.<endpointId>.<containerId>` (publicador: `internal/integrations/docker`) | chunk de texto | patrón exacto por petición |
| `cronJobs.subscribeToStatusUpdates` | `events.cronjobs.status.<jobName>` (publicador: `internal/jobs`) | `TaskStatus` | una suscripción escucha `events.cronjobs.status.*` entero |
| (futuro) ping por app/integración | `events.ping.<id>` | `{url, statusCode, durationMs}` / `{url, error}` | mismo shape del `pingChannel` upstream |

Diferencias semánticas que asume el transporte: upstream suscribe al canal
Redis DENTRO del observable y deduplica el historial con los mensajes
bufferizados (log.ts:44-69). En homarr-go la suscripción al bus se arma
después de resolver el `Initial` y antes de arrancar el pump: los mensajes
publicados en la microventana entre resolución inicial y `bus.Subscribe`
se pierden (el bus ya es "drop on slow" — eventos efímeros, no cola). El
handler de logs de M3 puede re-sincronizar desde el ring buffer si le
importa la ventana.

## 7. Reconexión y apagado

- **Cliente**: al cerrarse el socket con suscripciones pendientes,
  reconecta con backoff exponencial 0/2/4/…/30 s ([wsClient.ts]:250-280,
  [options.ts]:95-97) y **reenvía las suscripciones pendientes** en la
  nueva conexión ([wsClient.ts]:265-271). El frame `reconnect` del servidor
  fuerza ese mismo camino ([wsClient.ts]:387-395).
- **Servidor SIGTERM** ([main.ts]:84-88):
  `handler.broadcastReconnectNotification()` → `{"id":null,"method":"reconnect"}`
  a cada cliente abierto ([ws.ts]:617-627) y `wss.close()`.
  `Hub.Close()` en Go replica la secuencia: frame `reconnect`, cierre de
  cada conexión (`GoingAway`; upstream termina sin código), y las upgrades
  nuevas reciben 503 mientras el proceso vive. La señal SIGTERM la cablea
  el integrador (en upstream lo instala el proceso websocket; en homarr-go
  el binario único la recibe él mismo).

## 8. Excepción SSE: `widget.beszel.subscribeSystemStats`

- El cliente lo enruta a **`httpSubscriptionLink`** con
  `eventSourceOptions: { withCredentials: true }` sobre la URL tRPC normal
  `/api/trpc` ([trpc.tsx]:194-200) — es una petición HTTP GET con
  `Accept: text/event-stream` al endpoint tRPC estándar, **no** pasa por
  `/websockets`.
- Formato del stream (productor `sse.streamProducer`,
  `packages/server/src/unstable-core-do-not-import/stream/sse.ts`): eventos
  SSE con nombres `connected` (data con el envelope `started`), `data`
  (superjson), `serialized-error` y `ping`; el cliente los consume en
  `httpSubscriptionLink.ts:80-237` y puede reanudar con `lastEventId` si el
  procedimiento usa `tracked()`.
- **Decisión: NO se implementa en `internal/ws` (queda stub/documentado).**
  Razones: (1) vive en el lado HTTP tRPC (`/api/trpc/...`), cuyo handler es
  `internal/api/trpc` — no en este paquete ni en este puerto; (2) el
  procedimiento requiere la integración beszel (M4); (3) el coste del
  formato SSE + `tracked()`/`lastEventId` no compensa hasta que exista el
  procedimiento. Cuando toque: añadir en `internal/api/trpc` la rama
  `Accept: text/event-stream` para procedimientos subscription (ver
  PROTOCOL.md §7, que ya lo lista como fuera de alcance) y registrar el
  procedimiento en el router de widgets. Mientras tanto, si algo pregunta
  por ese path por WS recibirá el `NOT_FOUND` estándar — situación
  irrelevante porque el SPA nunca lo manda por WS.

## 9. Implementación Go

```
server/internal/ws/
  hub.go          Hub: registro, conexiones, auth del upgrade, Close (SIGTERM)
  conn.go         ciclo de vida por conexión (1 lector + 1 escritor + pump por suscripción)
  protocol.go     parseo de frames y builders byte-exactos (§3)
  subscription.go contrato SubscriptionHandler / Subscription / Request
  ws_test.go      suite (ver §10)
```

- **`Hub`**: `New(eventBus *bus.Bus, sessions SessionValidator, opts Options)`.
  `SessionValidator` es la interfaz mínima `Validate(ctx, token)
  (*ctxauth.Session, error)`; el integrador la adapta a `auth.Auth`
  (ValidateSession + permisos de `callerSession`), los tests usan un fake.
  Semántica upstream: token vacío/desconocido/expirado → `(nil, nil)` =
  anónimo; error interno → log + anónimo ([main.ts]:59-65).
- **Registro**: `Register(path string, s SubscriptionHandler)` con paths
  punteados como `root.ts` (`log.subscribe`, `docker.subscribeLogs`,
  `cronJobs.subscribeToStatusUpdates`). El handler devuelve por petición una
  `Subscription{Initial any, Pattern string, Decode func(bus.Message) (any, bool)}`:
  `Initial` (opcional) sale justo tras `started`, `Pattern` es el patrón del
  bus que se reenvía ("" = nada) y `Decode` filtra/transforma (nil = passthrough
  raw, cero copia con `json.RawMessage`). Un handler que requiere permisos
  devuelve un `*trpc.Error` (→ frame de error, conexión viva), igual que un
  procedimiento tRPC que lanza.
- **Goroutines**: una goroutine lectora (dueña del mapa de suscripciones),
  una escritora (la única que toca el socket para escribir; frames por canal
  FIFO, cola de 256; cliente lento → se suelta la conexión, coherente con el
  drop del bus), una de keepalive y un pump por suscripción con bus. Sin
  escrituras concurrentes sobre el mismo conn (regla de coder/websocket).
- **Superjson**: reutiliza `internal/api/trpc` (`WrapJSON`/`UnwrapInput`) —
  mismos envelopes que el lado HTTP.
- **Límite de lectura**: 1 MiB por frame (upstream no pone límite; red de
  seguridad).

### Desviaciones respecto a upstream (todas documentadas, ninguna observable para el SPA)

1. `query`/`mutation` por WS → `NOT_FOUND` `No "<type>"-procedure on path`
   ([router.ts]:`callProcedure`:411-419) en vez de ejecutarse: el SPA nunca
   los manda por WS (§1) y el registro es subscription-only.
2. Códigos de cierre: rechazo de API key y apagado usan 1001 (upstream
   cierra sin código → peer 1005, o termina abrupto con `wss.close()`).
3. `id` ausente (no solo `null`) → `` `id` is required ``.
4. Los mensajes de `PARSE_ERROR` por JSON inválido son los de
   `encoding/json`, no los de V8 (el código -32700 y el shape son exactos).
5. Falta el modo `connectionParams=1` (el SPA no lo usa, §2).

## 10. Tests

`go test ./internal/ws/ -count=1` (stdlib + httptest + el cliente de
coder/websocket; el bus de las pruebas es el `bus.Bus` real):

- `TestAnonymousUpgradeAcceptedAndSubscriptionUnauthorized` — upgrade sin cookie aceptado; la suscripción falla con frame UNAUTHORIZED; la conexión sigue viva (§4).
- `TestSessionCookieValidatedOnUpgrade` — la sesión validada llega al handler como `*ctxauth.Session`.
- `TestInvalidOrFailingTokenIsAnonymous` — token inválido y error interno del validador → anónimo (§4.3).
- `TestAPIKeyRejected` — frame `{"id":null,...UNAUTHORIZED...}` byte-exacto + cierre (§4.1).
- `TestSubscribeInitialBusForwardingAndStop` — started → initial → frame de bus → stop → stopped → silencio; stop de id desconocido no-op (§3.2).
- `TestDecodeFilterDropsMessages` — el filtro del handler (estilo `levels`) descarta mensajes del bus.
- `TestUnknownSubscriptionPath` — `No "subscription"-procedure on path "missing.sub"` byte-exacto (§6).
- `TestQueryAndMutationOverWSAreNotFound` — desviación 1.
- `TestDuplicateSubscriptionID` — `Duplicate id 1` BAD_REQUEST (§3.2.2).
- `TestBatchArrayFrames` — array de dos suscripciones → dos `started` (§3.1).
- `TestMalformedJSONFrame` — PARSE_ERROR -32700 con id null y conexión utilizable después (§3.3).
- `TestProtocolErrorFrames` — tabla §3.3 byte-exacta (id null, method inválido, params no-objeto, path no-string).
- `TestLastEventIDMergedIntoInput` — fusión `lastEventId` en el input preservando orden de claves (§3.1).
- `TestKeepalivePingPongExchange` — PING/PONG corto y conexión viva (§5).
- `TestKeepalivePongTimeoutTerminates` — silencio tras PING → cierre abrupto (§5).
- `TestCloseBroadcastsReconnectNotification` — `{"id":null,"method":"reconnect"}` + cierre a todos, 503 a upgrades nuevas (§7).
- `TestDisconnectUnsubscribesFromBus` — desconexión limpia: la conexión sale del hub y publicar tras ella es seguro.

Nota de entorno: `-race` no ejecutable en esta máquina (sin gcc para CGO en
Windows); la concurrencia se validó con 30 corridas consecutivas de la
suite completa.

## 11. Pendiente para el integrador

1. **Montaje**: `r.Handle("/websockets", hub.Handler())` en
   `internal/httpserver` (hoy hay un comentario con mount point `/api/ws`
   desactualizado; la ruta upstream es `/websockets`, §2).
2. **Adaptador de auth**: implementar `SessionValidator` sobre `auth.Auth`
   (reusar `ValidateSession` + `callerSession`, ambos ya en `internal/auth`).
3. **Cableado del apagado**: llamar `hub.Close()` en el camino de SIGTERM
   del binario, después de los productores del bus (§7).
4. **Productores del bus**: los handlers de este paquete ya consumen los
   topics del §6; `internal/jobs` (cron status, log publisher) e
   `internal/integrations/docker` (stream de logs) deben publicarlos.
5. **SSE beszel**: decidir M4+ (§8) — requiere el lado HTTP de
   `internal/api/trpc`, no este paquete.
