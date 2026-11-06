# Protocolo tRPC v11 — contrato exacto del wire y su implementación en Go

Documento de referencia del paquete `server/internal/api/trpc` (implementación)
y del protocolo HTTP que habla el frontend Homarr con el backend. Todas las
afirmaciones están verificadas contra las fuentes upstream salvo las marcadas
como **[no verificado en fuente]**. Fecha: 2026-11-06.

Fuentes citadas:

- **[homarr]** este repo: `apps/nextjs`, `packages/api`, `package.json`.
- **[trpc]** `trpc/trpc` tag `v11.18.0` (la versión clavada en
  [homarr] `package.json:108-112`). `node_modules` no está instalado en el
  workspace: los ficheros se verificaron descargándolos de GitHub en crudo;
  las rutas citadas son relativas a `packages/` del repo tRPC.
- **[superjson]** `flightcontrolhq/superjson` `src/index.ts` (tag `v2.2.5`;
  [homarr] clava `superjson 2.2.6` en `apps/nextjs/package.json` — la función
  `serialize` es idéntica en la rama 2.x **[verificado en v2.2.5]**).

## 1. Stack del cliente

- `@trpc/client` / `@trpc/server` **11.18.0**, transformer **superjson**
  ([homarr] `package.json:108-112`, `packages/api/src/trpc.ts:79-93`:
  `initTRPC...create({ transformer: superjson, errorFormatter: ... })`).
- Cliente React configurado en [homarr]
  `apps/nextjs/src/app/[locale]/_client-providers/trpc.tsx:186-241`:
  - Por defecto **`httpBatchStreamLink`** (`trpc.tsx:230-235`):
    `url = /api/trpc` (`packages/api/src/shared.ts:49`, `trpcPath`),
    `maxURLLength: 2083`.
  - `widget.releases.getLatest` usa el mismo link con
    **`methodOverride: "POST"`** (`trpc.tsx:222-229`) — en el wire es un POST
    de query; el server lo acepta porque la ruta monta
    `allowMethodOverride: true` ([homarr]
    `apps/nextjs/src/app/api/trpc/[trpc]/route.ts:51`).
  - Input no JSON-serializable (FormData/uploads) → `httpLink` con
    transformer identidad al serializar (`trpc.tsx:207-219`), body
    `multipart/form-data`.
  - Suscripciones → `splitLink`: `widget.beszel.subscribeSystemStats` via
    **`httpSubscriptionLink`** (SSE, `withCredentials`, `trpc.tsx:196-200`);
    el resto via **`wsLink`** a `/websockets` (`trpc.tsx:201-204`).
  - Header **`x-trpc-source`** en todas las peticiones:
    `"nextjs-react (json)"` (links batch), `"nextjs-react (form-data)"`
    (uploads), `"fetch"` (`packages/api/src/client.ts:11-19`), `"rsc"`
    (`packages/api/src/rsc-context.ts:10`). El server solo lo lee para log
    (`packages/api/src/trpc.ts:55`) — **cualquier valor o su ausencia es
    válido**.
- Segundo cliente no-React: `fetchApi` con `httpLink` + superjson
  (`packages/api/src/client.ts:11-19`) — respuestas JSON clásicas, nunca
  batch.
- Router raíz con **28 routers** ([homarr] `packages/api/src/root.ts:5-34`;
  28 entradas `lazy(() => …)` — el spike FRONTEND_EXPORT_SPIKE.md §3.1 dice
  "29 routers", cifra ligeramente desactualizada).

## 2. URL y método (petición)

Verificado en [trpc] `client/src/links/internals/httpUtils.ts:84-141`
(`getInput`, `getUrl`, `getBody`) y `httpBatchStreamLink.ts:68-99`:

| Caso | Método | URL | Body |
|---|---|---|---|
| Single no-batch (httpLink, query) | GET | `/api/trpc/<proc>?input=<JSON urlencodeado>` | — |
| Single no-batch (httpLink, mutation) | POST | `/api/trpc/<proc>` | `<input serializado>` directo |
| Batch (httpBatchStreamLink/httpBatchLink, **incluido batch de 1**) | GET (query) | `/api/trpc/<p1>,<p2>?batch=1&input={"0":<in0>,"1":<in1>}` | — |
| Batch (mutation) | POST | `/api/trpc/<p1>,<p2>?batch=1` | `{"0":<in0>,"1":<in1>}` |
| Batch de 1 con `methodOverride: "POST"` | POST | `/api/trpc/<proc>?batch=1` | `{"0":<in0>}` |

Detalles que se desviaron del análisis previo (FRONTEND_EXPORT_SPIKE.md §3.1):

1. **`httpBatchStreamLink` SIEMPRE agrupa por `dataLoader`**: incluso una
   operación sola viaja como **batch de 1** — la URL lleva `?batch=1` y el
   input va empaquetado como `{"0":...}` (`httpBatchStreamLink.ts:43-99`,
   `httpUtils.ts:120-122`: `batch=1` se añade siempre que hay lista de
   `inputs`). No existe la forma "single GET sin batch" para ese link; solo
   `httpLink` produce single puro.
2. El parámetro **`?batch=1` es el marcador de batch en el server**
   (`http/contentType.ts:84`: `isBatchCall = searchParams.get('batch') === '1'`;
   las comas del path solo separan procedimientos).
3. `methodOverride` **no añade ninguna marca extra al wire**: simplemente
   cambia el método HTTP a POST y omite el `input` de la URL
   (`httpUtils.ts:125-127,136-140`, `fetchHTTPResponse` método
   `opts.methodOverride ?? METHOD[type]` en `:201`). El server exige
   `allowMethodOverride: true` para aceptar POST sobre queries
   (`http/resolveResponse.ts:55-68,234-236`).
4. Superjson envuelve cada input: `{"json": <valor>, "meta": {...}?}`.
   Para valores sin tipos no-nativos de JSON, **`meta` se omite por
   completo** ([superjson] `serialize`: `res.meta` solo se rellena si el
   walker produjo anotaciones de valores o igualdades referenciales, y luego
   se marca `meta.v = 1`).

Headers de petición (httpBatchStreamLink, `httpBatchStreamLink.ts:81-83` +
`httpUtils.ts:209-217`): `content-type: application/json` (solo en POST),
`trpc-accept: application/jsonl` (ver §4), `x-trpc-source` y cookies del
callback de `createHeadersCallbackForSource` ([homarr]
`packages/api/src/shared.ts:11-24`).

## 3. Respuesta clásica (httpLink / httpBatchLink sin stream)

Verificado en [trpc] `http/resolveResponse.ts:411-457,673-744` y
`transformer.ts:87-108` (`transformTRPCResponseItem`: el transformer de
salida envuelve `result.data` y **el shape de error completo**):

- Éxito single, HTTP 200:
  `{"result":{"data":{"json":<output>}}}` (sin `meta` si el output es plano).
- Error single, HTTP = estado del código (§5):
  `{"error":{"json":{"message":"...","code":-32004,"data":{"code":"NOT_FOUND","httpStatus":404,"path":"board.nothing"}}}}`
  — el `{"json": ...}` viene de superjson envolviendo el shape entero, no de
  un sobre JSON-RPC. `code` es el **numérico**; `data.code` la clave de
  string; `data.path` solo si el server conoce el path
  (`error/getErrorShape.ts:21-35`). `data.stack` solo en dev
  (`getErrorShape.ts:29-31`) — homarr-go nunca lo emite.
- Éxito batch: **array JSON** `[{"result":...},{"error":...}]` — también para
  batch de 1 (la rama batch de `resolveResponse` siempre serializa el array;
  la rama single es `if (!info.isBatchCall)` en `resolveResponse.ts:411`).
- **HTTP status del batch**: si todos los elementos comparten status (o todos
  son 200) → ese status; si difieren → **`207 Multi-Status`**
  (`http/getHTTPStatusCode.ts:71-94`: `httpStatuses.size !== 1 → 207`). Esto
  corrigió una suposición previa ("máximo de los status").
- Content-Type: `application/json`; además **todas** las respuestas tRPC
  llevan `vary: trpc-accept, accept` (`resolveResponse.ts:222`) y las cabeceras
  CORS del route handler ([homarr] `route.ts:19-24`: `Access-Control-Allow-Origin: *`,
  `Access-Control-Request-Method: *`, `Access-Control-Allow-Methods: OPTIONS, GET, POST`,
  `Access-Control-Allow-Headers: *`).
- Errores fatales pre-ejecución (batch desactivado, stream sin batch, fallo de
  content-type, body inválido antes del lazy-parse) → **un único envelope de
  error** con el status del código (`resolveResponse.ts:745-774`). En
  upstream, el fallo de parseo de inputs es en realidad **por operación**
  (el parse es perezoso y ocurre dentro de cada call vía `getRawInput`,
  `contentType.ts:53-61,100-137`) — homarr-go lo replica así.

## 4. Respuesta en stream (httpBatchStreamLink) — el modo por defecto

Petición: header **`trpc-accept: application/jsonl`** (o el token
`application/jsonl` dentro del header estándar `accept`;
`http/contentType.ts:8-18`, `httpBatchStreamLink.ts:82-83`).

Respuesta (`resolveResponse.ts:573-584,667-670`):

- **HTTP 200 siempre** (aunque todas las ops fallen — los errores viajan
  dentro del stream), `Content-Type: application/json` (¡no
  `application/jsonl`!) y `transfer-encoding: chunked`.
- Body: **JSON Lines** (protocolo `jsonlStreamProducer`,
  `stream/jsonl.ts:282-313`):
  1. **Línea head**: objeto cuyas claves son los índices de las ops
     (`"0"`, `"1"`, ...) y cada valor es un `EncodedValue` = tupla
     `[[<data>], <chunkDef>...]`. Sin valores asíncronos pendientes queda
     `[[<payload>]]`.
  2. **Líneas chunk** posteriores: para promises/async-iterables que el
     producer aún no resolvió (`[chunkId, status, EncodedValue]`).
  - Cada línea va envuelta por el transformer de salida (superjson) **a nivel
    de línea** (`jsonl.ts:286-298`): para payloads planos, la línea es
    `{"json": {<head/chunk sin envolver>}}` y el `{"json": ...}` interno de
    `result.data` NO existe (el envoltorio de línea lo cubre todo).
  - Pings (opcional): un espacio `" "` como keep-alive.
- El cliente consume con `jsonlStreamConsumer`
  (`stream/jsonl.ts:516-654`): la primera línea es el head; `decode`
  (`jsonl.ts:588-602`) soporta de forma nativa entradas **sin chunks**
  (`[[data]]` → `data`); el link resuelve `head[key]` por índice y desempaqueta
  promises anidadas si las hay (`httpBatchStreamLink.ts:125-165`).
- Si la respuesta **no es 2xx**, el cliente ni intenta el stream: parsea el
  body como JSON clásico de error y propaga el mismo error a todas las ops
  (`httpBatchStreamLink.ts:103-123`).

**Subconjunto implementado en homarr-go (M1)**: ejecución secuencial y
payloads JSON planos → se emite **una única línea head** con todas las ops
codificadas `[[{"result":{"data":<output crudo>}}]]` /
`[[{"error":{<shape>}}]]`, sin líneas chunk. Es una codificación válida para
`jsonlStreamConsumer` (los chunks solo existen porque el producer oficial
puede resolver tarde; homarr-go resuelve todo antes de escribir). Los tests
fijan el byte exacto de estas líneas.

Ejemplo — batch de 2 (op0 `echo` con `{"a":1}`, op1 procedimiento inexistente):

```
HTTP/1.1 200 OK
Content-Type: application/json
Vary: trpc-accept, accept

{"json":{"0":[[{"result":{"data":{"a":1}}}]],"1":[[{"error":{"message":"No procedure found on path \"missing\"","code":-32004,"data":{"code":"NOT_FOUND","httpStatus":404,"path":"missing"}}}]]}}\n
```

Restricciones upstream relevantes (`resolveResponse.ts:305-325`): un call con
`trpc-accept: application/jsonl` **que no sea batch** (sin `?batch=1`) es un
`BAD_REQUEST` ("Streaming requests must be batched (you can do a batch of 1)")
— replicado.

## 5. Tabla de errores (tRPC 11.18.0)

Numéricos: [trpc] `rpc/codes.ts:11-44` (`TRPC_ERROR_CODES_BY_KEY` — "se copian
los últimos dígitos de los HTTP 4XX"). Status: `http/getHTTPStatusCode.ts:7-32`
(`JSONRPC2_TO_HTTP_CODE`), desconocido → 500 (`getStatusCodeFromKey:59-63`).
Cinco códigos comparten `-32603` (INTERNAL_SERVER_ERROR, NOT_IMPLEMENTED,
BAD_GATEWAY, SERVICE_UNAVAILABLE, GATEWAY_TIMEOUT).

| Clave | num | HTTP | | Clave | num | HTTP |
|---|---|---|---|---|---|---|
| PARSE_ERROR | -32700 | 400 | | TIMEOUT | -32008 | 408 |
| BAD_REQUEST | -32600 | 400 | | CONFLICT | -32009 | 409 |
| UNAUTHORIZED | -32001 | 401 | | PRECONDITION_FAILED | -32012 | 412 |
| PAYMENT_REQUIRED | -32002 | 402 | | PAYLOAD_TOO_LARGE | -32013 | 413 |
| FORBIDDEN | -32003 | 403 | | UNSUPPORTED_MEDIA_TYPE | -32015 | 415 |
| NOT_FOUND | -32004 | 404 | | UNPROCESSABLE_CONTENT | -32022 | 422 |
| METHOD_NOT_SUPPORTED | -32005 | 405 | | PRECONDITION_REQUIRED | -32028 | 428 |
| | | | | TOO_MANY_REQUESTS | -32029 | 429 |
| | | | | CLIENT_CLOSED_REQUEST | -32099 | 499 |
| INTERNAL_SERVER_ERROR | -32603 | 500 | | NOT_IMPLEMENTED | -32603 | 501 |
| BAD_GATEWAY | -32603 | 502 | | SERVICE_UNAVAILABLE | -32603 | 503 |
| GATEWAY_TIMEOUT | -32603 | 504 | | | | |

Mensajes upstream replicados en homarr-go: `No procedure found on path "<p>"`
(`resolveResponse.ts:341-345`), `Batch call exceeds maximum size`
(`contentType.ts:93-96`), `"input" needs to be an object when doing a batch
call` (`contentType.ts:121-126`), `Unsupported content-type "<ct>"` /
`Missing content-type header` (`contentType.ts:307-312`).

Notas del errorFormatter de [homarr] (`packages/api/src/trpc.ts:84-92`):
upstream añade **siempre** `data.zodError` (null si no hay), `data.error`
(null) e `isBoardLayoutBoundsError` (false) dentro de `data`. homarr-go no los
emite por defecto (el cliente react-query solo consume `message`,
`data.code`, `data.httpStatus`); un handler Go puede añadirlos con
`Error.Data` si algún consumidor los necesita. Sin `data.stack` (upstream solo
en dev).

## 6. Errores de petición (decisiones replicadas)

| Situación | Respuesta |
|---|---|
| `?batch=1` con más ops que el máximo (homarr-go: 100, configurable) | fatal 400 `Batch call exceeds maximum size` |
| stream sin batch | fatal 400 "Streaming requests must be batched…" |
| POST/DELETE/PUT sin `Content-Type` | fatal 415 `Missing content-type header` |
| `Content-Type` no JSON/multipart/octet-stream | fatal 415 `Unsupported content-type "<ct>"` |
| `multipart/form-data` u `octet-stream` (uploads) | fatal 415 en homarr-go (gap M1; upstream pasa el FormData como input, `contentType.ts:214-288`) |
| JSON de body/input inválido | error **por operación** BAD_REQUEST "Failed to parse input: …" |
| batch con input que no es objeto | error por operación BAD_REQUEST `"input" needs to be an object…` |
| índice ausente en el registro batch | esa operación recibe input indefinido (null en Go) |
| procedimiento inexistente | por operación NOT_FOUND |
| método distinto de GET/POST (con Content-Type JSON) | por operación METHOD_NOT_SUPPORTED (upstream lo resuelve contra el tipo del procedimiento; homarr-go aún no tipa procedimientos) |
| body > límite (homarr-go: 10 MiB) | fatal 413 PAYLOAD_TOO_LARGE (`http.MaxBytesReader`) |
| panic en un handler | por operación INTERNAL_SERVER_ERROR; el resto del batch sigue |
| output no serializable | por operación INTERNAL_SERVER_ERROR |
| HEAD | `204` (warmup, `resolveResponse.ts:227-232`) |
| OPTIONS | `204` + CORS ([homarr] `route.ts:26-33`) |

`PARSE_ERROR` (-32700) existe en la tabla pero el handler HTTP de v11 no lo
genera para parseo de inputs (usa BAD_REQUEST); queda reservado/aceptado.

## 7. Fuera de alcance en M1 (documentado, no implementado)

- **WebSocket** `/websockets` ([homarr] `apps/websocket/src/main.ts`): tRPC WS
  `{id, method: query|mutation|subscription, params: {path, input}}` con
  superjson, `reconnect`/`stop`/`subscription.state`; keepAlive ping 30 s.
  Se porta en M3 (`server/internal/ws`).
- **SSE** (`httpSubscriptionLink`) para `widget.beszel.subscribeSystemStats`:
  `Content-Type: text/event-stream`, eventos tRPC con superjson y
  `withCredentials`. Requiere procedimientos subscription (M3+).
- **multipart/form-data y octet-stream** (uploads de media): se responde 415
  con mensaje explícito hasta portar los routers de medias.
- **Mezcla de tipos de procedimiento en un batch**: upstream la rechaza con
  BAD_REQUEST (`contentType.ts:180-192`) porque conoce el tipo de cada
  procedimiento; homarr-go no tipa procedimientos todavía (seam).
- **`connectionParams`**, `lastEventId`, abort signals por operación,
  ejecución concurrente de ops del batch (homarr-go es secuencial en M1).

## 8. Comprobación con superjson real (lo que debe esperar el integrador)

Para valores planos (los que producen los handlers Go), superjson 2.x emite
exactamente `{"json": <valor>}` — nunca `{"json":..., "meta":{}}`
([superjson] `serialize`). Al recibir inputs, `meta` (con `values`,
`referentialEqualities` y `v: 1`) puede venir presente; homarr-go lo ignora:
las fechas llegan como strings ISO dentro de `json` y los handlers Go
trabajan con JSON puro. Si algún día un router necesita reconstruir tipos
(`meta.values`), la extensión natural es interpretar `meta` en
`UnwrapInput`/`WrapJSON` (`superjson.go`).

## 9. Montaje e integración

```go
r := trpc.New()
r.Register("board.getByNames", boardGetByNames) // paths punteados como root.ts
// chi:
router.Handle("/api/trpc/*", r.Handler())
```

Auth (seam, no construido): upstream resuelve sesión/API-key en el route
handler antes de tRPC ([homarr] `route.ts:35-47`; un API key inválido
responde 401 JSON plano `{"error":"UNAUTHORIZED"}`, no-envelope) y construye
el contexto tRPC con la sesión ([homarr] `packages/api/src/trpc.ts:53-71`).
En homarr-go lo equivalente es un middleware httpserver que guarde el caller
en el `request.Context()`; los `Procedure` ya reciben ese `ctx`.

Validación pendiente del integrador (M1): capturar tráfico real del frontend
(devtools) contra el server Go y comparar — los tests fijan el wire byte a
byte según las fuentes citadas, pero el SPA real añade cookies/sesión y
combinaciones de batch que conviene verificar en caliente.
