# DOWNLOAD_CLIENT.md — dominio download-client (`internal/integrations/downloadclient`)

Documento de referencia del port en Go de la abstracción compartida
download-client de upstream (`packages/integrations/src/interfaces/downloads`
+ los clientes por app de `packages/integrations/src/download-client`): los
datos y mutaciones que consume el widget downloads vía el router tRPC
`widget.downloads.*` (portado en `internal/api/router/widgets`, ver
WIDGETS.md). Todas las afirmaciones citan fuente upstream; las desviaciones
están marcadas. Fecha: 2026-11-07.

Fuentes citadas (todas en este repo salvo las librerías @ctrl, verificadas
por versión exacta desde unpkg):

- **[iface]** `packages/integrations/src/interfaces/downloads/download-client-integration.ts:13-28`
  + `download-client-items.ts:16-46` + `download-client-status.ts:3-12` +
  `download-client-data.ts:4-9`.
- **[select]** `packages/integrations/src/download-client/select-torrents.ts:8-50`.
- **[qbit]** `.../download-client/qbittorrent/qbittorrent-integration.ts` +
  **@ctrl/qbittorrent 9.14.0** (catálogo pnpm del package.json raíz:
  `qbittorrentSession.js` login/authHeaders/requestWithAuth/checkVersion,
  `qbittorrent.js` listTorrents/stopTorrent/startTorrent/removeTorrent).
- **[trans]** `.../download-client/transmission/transmission-integration.ts` +
  **@ctrl/transmission 7.4.0** (`transmission.js` request/409 handshake,
  torrent-get fields :222+).
- **[deluge]** `.../download-client/deluge/deluge-integration.ts` +
  **@ctrl/deluge 8.0.0** (`deluge.js` login/request/web.update_ui fields
  :345-371, body.error :557-559).
- **[sab]** `.../download-client/sabnzbd/sabnzbd-integration.ts` +
  `sabnzbd-schema.ts` + `sabnzbd-history.ts` (fetch directo, sin librería).
- **[widget]** `packages/api/src/router/widgets/downloads.ts:49-194` +
  `packages/request-handler/src/downloads.ts:7-23` (caché 10 s) +
  `packages/api/src/settle-integrations.ts`.
- **[defs]** `packages/definitions/src/integration.ts` (kinds
  `sabNzbd`/`nzbGet`/`deluge`/`transmission`/`qBittorrent`/`aria2`/`slskd`),
  `packages/definitions/src/widget-integration-map.ts:141`.

## 1. La interfaz y los tipos

[iface] — la unión normalizada torrent/usenet que habla el widget:

```go
type Client interface {
    GetJobsAndStatus(ctx context.Context, input JobsInput) (JobsAndStatus, error)
    PauseQueue(ctx context.Context) error
    PauseItem(ctx context.Context, item Item) error
    ResumeQueue(ctx context.Context) error
    ResumeItem(ctx context.Context, item Item) error
    DeleteItem(ctx context.Context, item Item, fromDisk bool) error
}
```

- `Item` (download-client-items.ts): `id/index/name/type/size/time/` +
  `state/progress` obligatorios; `sent/received/downSpeed/upSpeed/added`
  opcionales (ausentes ⇒ clave ausente en el JSON) y `category` como
  `string | string[]` (absente ⇒ clave ausente; `""` y `[]` SÍ viajan — el
  port usa `any` con `omitempty`, que solo suelta la interfaz nil).
  `time`: positivo = eta ms (0 = infinito), negativo = ms desde la
  finalización (máximo -1). `progress` 0..1. `added` = epoch **ms**.
- `Status`: `paused` (bool), `rates: {down, up?}` (bytes/s; `up` ausente
  solo en SABnzbd), `types: ["torrent"|"usenet"|...]`.
- `JobsAndStatus`: `{status, items, ratioItems?}` — `ratioItems`
  (`Pick<item,"sent"|"size"|"progress"|"category">[]`) SOLO se emite cuando
  la petición trae `selection` (los tres clientes torrent upstream lo
  condicionan igual: [qbit] :70, [trans] :93, [deluge] :132).

`JobsInput` trae `Limit` (el `limitPerIntegration` del input zod, default 50,
sin mínimo — un número zod), `Selection` (opcional), y los dos campos
SABnzbd (`IncludeArchivedHistory` default false, `HistoryWindowDays` default
7). La selección NO se replica tal cual: los tres clientes torrent la
interpretan como upstream (qbit server-side, transmission/deluge local vía
`SelectTorrents`).

## 2. Clientes por app

| Kind upstream | Secretos ([defs] secretKinds) | Puerto | Timeout por request |
|---|---|---|---|
| `qBittorrent` | `[["apiKey"]]` o `[["username","password"]]` | `downloadclient/qbittorrent` | 5 s (@ctrl) |
| `transmission` | `[["username","password"]]` | `downloadclient/transmission` | 5 s (@ctrl) |
| `deluge` | `[["password"]]` | `downloadclient/deluge` | 5 s (@ctrl) |
| `sabNzbd` | `[["apiKey"]]` | `downloadclient/sabnzbd` | ninguno (fetch upstream) |
| `nzbGet`, `aria2`, `slskd` | — | **no portado** (M4; el router de widgets los marca como fallo por integración) | — |

Los 5 métodos de mutación son idempotentes respecto de la sesión del
cliente: cada llamada pública repite el preamble de auth que la librería
upstream haría al reconstruir el cliente (`getClientAsync` se llama en cada
método upstream).

### 2.1 qBittorrent ([qbit] + @ctrl/qbittorrent 9.14.0)

Auth (qbittorrentSession.js):

- **apiKey** ⇒ header `Authorization: Bearer <key>` en TODO, sin login.
- **username/password** ⇒ `POST {base}/api/v2/auth/login`,
  `Content-Type: application/x-www-form-urlencoded`, body
  `username=..&password=..` (redirect manual, sin reintentos); el SID es el
  primer par nombre=valor del primer `Set-Cookie` (getAuthCookieName);
  expiración de `Expires`/`Max-Age` con fallback +1h. Cada request lleva
  `Cookie: SID=<sid>` **más** `Authorization: Basic ...` si hay
  usuario/contraseña (compatibilidad 5.2+). Re-login de un disparo cuando un
  request responde 401/403 (requestWithAuth; el modo apiKey nunca re-loguea).
- **Versión**: `GET /api/v2/app/version` (respuesta TEXTO, no JSON); se
  recorta el prefijo `v` y el sufijo `-x`; `isVersion5OrHigher` = comparación
  numérica ≥ 5.0.0. Upstream llama `getAppVersion()` en CADA construcción
  del cliente ([qbit] :216-217) ⇒ cada operación del port refresca la
  versión si es desconocida. La versión decide:
  - pausa: `POST /torrents/stop` (v5+) vs `/torrents/pause` (v4), con body
    form `hashes=all|<hash>`;
  - reanudar: `/torrents/start` vs `/torrents/resume`;
  - el filtro de `/torrents/info`: v5 reescribe `paused→stopped` y
    `resumed→running`; v4 `stopped→paused`, `running→resumed`.
- Lectura: `GET /api/v2/torrents/info` con query
  `limit/offset/sort/reverse/category/filter` (offset/limit como strings,
  `reverse` como `"true"/"false"` — JSON.stringify upstream).
- Mapeo del widget ([qbit] :33-173): sin selección,
  `listTorrents({limit})` + rates sumadas + `paused` = ningún estado mapea a
  algo distinto de paused (lista vacía ⇒ paused=true). Con selección:
  paginado de hasta **100 páginas de 100** (`pageSize = min(limit,100)`,
  forzado a 100 si hay filtros), dedupe por hash, cortes
  `batch.length < pageSize` o sin hashes nuevos (qBittorrent 4 rebobina los
  offsets fuera de rango), sort server-side solo para
  name/progress/size/added→added_on/index→priority/upSpeed→upspeed/downSpeed→dlspeed/sent→uploaded;
  `category` solo cuando `filterIsWhitelist && 1 filtro`; completados
  retenidos solo con `showCompletedTorrent` y
  `upspeed >= threshold*1024`. Estado global con selección: `/transfer/info`
  GET (`dl_info_speed/up_info_speed`) + `listTorrents({filter:"running",
  limit:1})` + ratioItems paginados con offset SIEMPRE a paso 100 y
  `limit = min(restante,100)` (:88-111).
- Tiempos: `progress === 1 ⇒ min(completion_on*1000 - now, -1)`;
  `eta === 8640000 ⇒ 0`; si no `max(eta*1000, 0)` (:57-63). `downSpeed`
  solo con `progress !== 1`.

### 2.2 Transmission ([trans] + @ctrl/transmission 7.4.0)

- Todo es `POST {base}/transmission/rpc`, body JSON
  `{"method": <rpc>, "arguments": {...}}`, `Content-Type: application/json`.
- Auth: `Authorization: Basic` con usuario/contraseña; token de sesión en el
  header `X-Transmission-Session-Id` — handshake: cualquier request que
  responda **409** guarda el header de respuesta
  `x-transmission-session-id` y reintenta UNA vez; cuando no hay sesión,
  `session-get` corre primero (es el único método que la librería manda sin
  sesión previa).
- `torrent-get` con lista explícita de campos (la librería pide un superset;
  el port pide exactamente los mapeados: id, name, hashString, totalSize,
  percentDone, rateDownload, rateUpload, eta, addedDate, doneDate,
  queuePosition, uploadedEver, downloadedEver, status, labels).
- Mapeo ([trans] :31-129): rates y `paused` sobre la lista COMPLETA;
  `status` numérico → 0 paused, 1/3 stalled, 2/4 leeching, 5/6 seeding
  (:140-156); `category` = `labels` (array); tiempo
  `percentDone===1 ⇒ min(doneDate*1000-now,-1)` si no `max(eta*1000,0)`.
  Selección: `SelectTorrents` local (§3) sobre la respuesta completa;
  `ratioItems = torrents.slice(0, limit)`.
- Mutaciones: pausa/reanudar globales = recolectar todos los hashString y
  `torrent-stop`/`torrent-start` con el ARRAY de ids; por item = array de 1
  (`_handleNormalizedIds` convierte el string); delete =
  `torrent-remove` con `delete-local-data` bool.

### 2.3 Deluge ([deluge] + @ctrl/deluge 8.0.0)

- Todo es `POST {base}/json`, body `{"method","params","id": msgId++}`
  (msgId cicla a 0 al llegar a 4096), header `Cookie` con la sesión.
- Auth: `auth.login` con `params: [password]` (sin cookie previa) ⇒ la
  PRIMERA cookie del Set-Cookie es la sesión (con expiración). Cada request
  autenticado primero valida (`_validateAuth`): cookie presente y no a
  punto de expirar (< 5 s) y `auth.check_session` true, si no re-login;
  después auto-conecta el daemon: `web.connected` → si false
  `web.get_hosts` → `web.connect` con el primer host.
- Lectura: `web.update_ui` con `[[campos], {}]` — la lista fija de la
  librería (deluge.js:345-371) + `"label"` (si el plugin no existe, no
  falla) + los extras del caller (homarr añade `"completed_time"`). La
  respuesta trae `{torrents: {id: {...}}, stats: {download_rate,
  upload_rate}, filters}`.
- Mapeo ([deluge] :62-141): rates `Math.floor`; `progress/100`;
  `category` = `label`; el id viene de la CLAVE del dict; `received` (para
  sort/ratio) = `floor(total_wanted * progress/100)`; tiempo
  `progress===100 ⇒ min((completed_time - now)*1000, -1)` si no
  `max(eta*1000,0)` (now en SEGUNDOS). `ratioItems =
  torrents.slice(0, limit)`.
- Errores: un `error` en el BODY de la RPC lanza un Error plano en la
  librería (deluge.js:557-559) ⇒ upstream lo clasifica como
  **UNKNOWN** ("An unknown error occured…"), no como response error — el
  port replica la clasificación.
- Mutaciones: pausa/reanudar globales = listar y `core.pause_torrent` /
  `core.resume_torrent` con `[[id]]` por torrent (Promise.all upstream,
  acotado a 10 en el port); delete = `core.remove_torrent` con
  `[id, removeData]`.

### 2.4 SABnzbd ([sab])

Fetch directo (sin librería): `GET {base}/api?output=json&mode=<m>&apikey=<key>&...`
— la api key viaja como **query param** ([defs] httpAuth
`{type: "apiKeyQuery", name: "apikey"}`). No-2xx ⇒ ResponseError ⇒
`ErrKindResponse`. Sin timeout por request upstream (la capa del widget
impone los 60 s).

- `GetJobsAndStatus` ([sab] :27-111): `mode=queue&limit` y
  `mode=history&limit` en paralelo. Estado: `paused = queue.paused`,
  `rates.down = floor(Number(kbpersec) * 1024)` (kbpersec es KiB/s y STRING)
  — sin `up`. Items de cola: `id = nzo_id`, `index = slot.index`,
  `name = filename`, `size = ceil(parseFloat(mb) * 1024 * 1024)`,
  `downSpeed = index > 0 ? 0 : status.rates.down`,
  `timeleft "d:hh:mm:ss"` invertido y convertido a ms,
  `progress = parseFloat(percentage)/100`, SIN `added` (única integración
  sin timestamp). Items de historia (concatenados tras la cola, luego
  `slice(0, limit)`): `index` = posición en la lista, `size = bytes`,
  `time = completed*1000 - now`, `added = (completed - download_time -
  postproc_time) * 1000`, `progress = 1`. Estados de cola: Queued→queued,
  Paused→paused, resto→downloading; de historia: Completed→completed,
  Failed→failed, resto→processing.
- Historia archivada (`includeArchivedHistory`, sabnzbd-history.ts):
  paginación `mode=history&archive=1&start&limit` en páginas de **100,
  máximo 100 páginas**, corte cuando la página viene incompleta o TODOS sus
  slots son anteriores al corte (`now/1000 - historyWindowDays*86400`).
  Resultado = slots en proceso primero (no Completed/Failed), luego los
  completados/fallados de la ventana ordenados por `completed` DESC,
  dedupe por `nzo_id`.
- Mutaciones ([sab] :114-140): pausa/reanudar globales = `mode=pause` /
  `mode=resume`; por item = `mode=queue&name=pause|resume&value=<nzo_id>`;
  delete: `progress !== 1` ⇒ `mode=queue` si no `mode=history`, con
  `name=delete`, `archive = fromDisk ? "0" : "1"`,
  `del_files = fromDisk ? "1" : "0"` (borrar del disco mantiene el archivo
  en la historia; https://github.com/sabnzbd/sabnzbd/issues/2754).
- `Check`: `mode=translate&value=ping` (la llamada más ligera que exige api
  key, :21-25).

## 3. `SelectTorrents` (select-torrents.ts:8-50)

Port exacto del filtro/orden/ventana local de transmission y deluge:
filtro por categoría (whitelist/blacklist — un torrent sin categoría cuenta
como `""`), completados solo con `showCompletedTorrent &&
upSpeed >= threshold*1024`; orden por el campo de la selección (string
extractor → `localeCompare` upstream, **comparación byte-wise en Go** —
misma desviación documentada que el port de bangs; numérico → resta ×
multiplicador descendente), con `time` tratando el 0 como Infinity;
`slice(0, limit)` final. Sorts sin extractor (`type`, `integration`,
`category` como campo) NO reordenan, como upstream.

## 4. Flujo de datos y errores

[handler] downloads.ts: `getClientJobsAndStatusAsync` directo con caché
in-memory de **10 s** (`cacheTtlMs: 10 * 1000`), single-flight, sin
stale-if-error; `updatedAt` del wire es el timestamp de la entrada de caché
(upstream serializa un Date por superjson; el port emite el ISO-ms con `Z`
según la convención del port, ADMIN.md §4). Las mutaciones corren sobre
TODAS las integraciones resueltas (no solo la dueña del item — [widget]
:140-151), invalidan la caché y lanzan el PRIMER fallo como
INTERNAL_SERVER_ERROR con el mensaje de la IntegrationError (por eso la
taxonomía de MEDIA_ORGANIZER.md §4 importa aquí). Queries con el mismo
`settleIntegrationQueries` del media-organizer (fallback público
`INTEGRATION_REQUEST_FAILED`, throwOnAllFailures). Sin cron jobs ni bus:
todo es query + refetch del cliente.

## 5. Registro en el framework

Cada subpaquete se auto-registra con el kind EXACTO upstream: `qBittorrent`
(mayúscula B), `transmission`, `deluge`, `sabNzbd` (mayúscula N). Igual que
en media-organizer, las factories del registry no tienen secretos aún, así
que su `Check` responde el mensaje exacto de missing-secret; los clientes
reales se construyen vía `widgets.Options.NewDownloadClient` con secretos
descifrados. Nota del re-login qBittorrent: al ser estado por instancia, un
proceso de larga vida con el cliente persistente (el port reutiliza la
instancia por request del widget vía la caché de clientes implícita en el
factory del router) mantiene el SID y lo refresca solo ante 401/403.

## 6. Cobertura de tests (httptest, fixtures byte-exactos)

| Paquete | Tests | Cubre |
|---|---|---|
| `qbittorrent` | `TestLoginCookieFlow` (POST form + Set-Cookie SID + version probe + cookies en cada request), `TestAPIKeyBearer` (Bearer sin login), `TestCheckRejectedLogin` / `TestCheckTransportFailure`, `TestV4VersusV5MutationEndpoints` (pause/resume vs stop/start, `hashes=all|hash`, delete con deleteFiles), `TestFilterTranslation` (running→resumed v4 / running v5), `TestItemsShapeAndStatus` (JSON byte-exacto: eta 8640000⇒0, completion negativa, downSpeed solo activo, category "" presente), `TestSelectionPagingAndRatioItems` (2 páginas de 100, category/reverse, transfer/info + running + ratioItems), `TestReloginOn401` (403 ⇒ un re-login) | auth por cookie y por apiKey, v4/v5, paginado |
| `transmission` | `TestSessionHandshakeAndTorrentGet` (409 + header de sesión + Basic auth + fields de torrent-get + JSON de items), `TestSelectionAndRatioItems` (whitelist + sort progress desc + slice), `TestMutations` (torrent-stop/start/remove con arrays de ids y delete-local-data), `TestCheckSessionGet`, `TestTransportFailure` | handshake 409, mapeo, mutaciones |
| `deluge` | `TestLoginCookieAndConnectFlow` (auth.login params, cookie, web.connected→update_ui, rates floored, JSON de items con id de la clave del dict y orden de inserción preservado), `TestConnectWhenDetached` (get_hosts→connect), `TestUpdateUIFields` (lista fija + label + completed_time + filtro `{}`), `TestSelectionAndRatioItems`, `TestMutations` ([[id]], remove [id,bool]), `TestErrorBodySurfaces` (body.error ⇒ UNKNOWN pinned), `TestCheckLogin` | flujo completo del RPC |
| `sabnzbd` | `TestGetJobsAndStatusShape` (apikey/output/mode/limit en query, kbpersec×1024 floor, mb ceil, timeleft parse, estados, slice tras concat, rates.up ausente), `TestFullQueueAndHistoryShapes` (historia: index reinicia, added derivado, progress 1), `TestArchivedHistory` (archive paging 100×100, corte por cutoff, merge processing→ventana desc, dedupe), `TestMutations` (pause/resume/pauseItem/delete queue-vs-history con archive/del_files), `TestMissingSecretAndErrorPath`, `TestCheckUsesTranslatePing` | todo el modo api |

Ejecución: `go test ./internal/integrations/downloadclient/...`.

## 7. Preguntas abiertas para el integrador

1. **nzbGet / aria2 / slskd**: mismos interfaces (`IDownloadClientIntegration`);
   portarlos en M4 añade subpaquetes + entradas en
   `widgets.DefaultDownloadClientFactory` (los kinds ya están listados en
   `widgets.SupportedKinds`).
2. **mock**: ídem MEDIA_ORGANIZER.md §8.4.
3. **TLS**: mismas costuras que MEDIA_ORGANIZER.md §8.2.
4. **`Selection.sort` byte-wise vs localeCompare**: si algún día el orden
   de dos torrents con acentos cambia el render, el fix es portar
   `localeCompare` con collation CLDR en `SelectTorrents` (y en el sort de
   bangs, misma deuda).
