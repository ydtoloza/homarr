# WIDGETS.md — router de widgets (`internal/api/router/widgets`)

Documento de referencia del port en Go del dominio de widgets de
`packages/api/src/router/widgets`: hoy `mediaOrganizerRouter` (media-organizer.ts),
`downloadsRouter` (downloads.ts), `mediaServerRouter` (media-server.ts) y
`dnsHoleRouter` (dns-hole.ts) — las superficies de datos de los widgets
media-organizer, downloads, media-server y dns-hole. Complementa
MEDIA_ORGANIZER.md, DOWNLOAD_CLIENT.md, MEDIA_SERVER.md y NETWORK.md
(clientes) y PROTOCOL.md (wire). Fecha: 2026-11-07.

Fuentes citadas (todas en este repo):

- **[mo]** `packages/api/src/router/widgets/media-organizer.ts:9-47`.
- **[dc]** `packages/api/src/router/widgets/downloads.ts:49-194`.
- **[ms]** `packages/api/src/router/widgets/media-server.ts:9-47`.
- **[dns]** `packages/api/src/router/widgets/dns-hole.ts:28-97`.
- **[idx]** `packages/api/src/router/widgets/index.ts` (montaje
  `mediaOrganizer`/`downloads`/`mediaServer`/`dnsHole` bajo `widget`).
- **[settle]** `packages/api/src/settle-integrations.ts`
  (PUBLIC_INTEGRATION_ERROR :7, throw :52-66).
- **[mw]** `packages/api/src/middlewares/integration.ts`
  (createManyWidgetIntegrationMiddleware :218-230, createOne... :241-247,
  NOT_FOUND many :186-192, NOT_FOUND one :113-117, FORBIDDEN :266-296,
  decryptSecret de decryptedSecrets :205-212).
- **[rh-mo]** `packages/request-handler/src/media-organizer.ts`
  (caché 60 s), **[rh-dc]** `packages/request-handler/src/downloads.ts`
  (caché 10 s), **[rh-ms]** `packages/request-handler/src/media-server.ts`
  (caché 10 s por DEFAULT_TTL_MS), **[rh-dns]**
  `packages/request-handler/src/dns-hole.ts` (caché 10 s por DEFAULT_TTL_MS),
  **[rh-core]** `packages/request-handler/src/lib/request-handler.ts`
  (DEFAULT_REQUEST_TIMEOUT_MS 60_000 :35, DEFAULT_TTL_MS 10_000 :34, sin
  stale-if-error).
- **[enc]** `packages/common/src/encryption.ts` (AES-256-CBC, key de
  SECRET_ENCRYPTION_KEY, formato `"<data hex>.<iv hex>"`).
- **[map]** `packages/definitions/src/widget-integration-map.ts:128,129,138,141,148`
  + `packages/definitions/src/integration.ts` (categorías).
- **[cred]** `packages/api/src/trpc.ts:130-135` (credentialProcedure) y
  :163-172 (protectedProcedure).

## 1. Registro

```go
r := trpc.New()
widgets.Register(r, widgets.Options{ResolveIntegrations: ..., /* factories opcionales */})
// chi: r.Handle("/api/trpc/*", r.Handler())
```

Procedimientos registrados (paths exactos de [mo]/[dc]/[ms]/[dns] montados
en el sub-router `widget` de [idx]):

| Path | Tipo | Permiso upstream | Port |
|---|---|---|---|
| `widget.mediaOrganizer.getData` | query | publicProcedure + middleware "query"/mediaMissing | público (sin gate de sesión) |
| `widget.downloads.getJobsAndStatuses` | query | publicProcedure + middleware "query"/downloads | público |
| `widget.mediaServer.getCurrentStreams` | query | publicProcedure + middleware "query"/mediaServer | público |
| `widget.dnsHole.summary` | query | publicProcedure + middleware "query"/dnsHoleSummary | público |
| `widget.downloads.pause` | mutation | protectedProcedure + "interact" | `admin.Writable` + RequireSignedIn |
| `widget.downloads.pauseItem` | mutation | ídem | ídem |
| `widget.downloads.resume` | mutation | ídem | ídem |
| `widget.downloads.resumeItem` | mutation | ídem | ídem |
| `widget.downloads.deleteItem` | mutation | ídem | ídem |
| `widget.dnsHole.enable` | mutation | protectedProcedure + one-middleware "interact"/dnsHoleControls | `admin.Writable` + RequireSignedIn (convención del paquete) |
| `widget.dnsHole.disable` | mutation | ídem | ídem |

Kinds soportados por widget (`widgets.SupportedKinds`, orden de definición
upstream de `getIntegrationKindsByCategory`, con "mock" último):

| Widget kind | Kinds |
|---|---|
| `mediaMissing` | `sonarr,sportarr,radarr,bindery,mock` |
| `downloads` | `sabNzbd,nzbGet,deluge,transmission,qBittorrent,aria2,slskd,mock` |
| `mediaServer` | `jellyfin,emby,plex,navidrome,mock` |
| `dnsHoleSummary` / `dnsHoleControls` | `piHole,adGuardHome,technitiumDns,mock` |

Los kinds sin cliente portado (`navidrome`, `technitiumDns`, los `mock`)
fallan SOLO esa integración (la query responde su fallback público), el
equivalente del fallo en runtime upstream.

Orden de gates de una mutación (el orden upstream de middlewares):
**read-only key** (`admin.Writable`, credProcedure es la base) →
**UNAUTHORIZED** si anónimo (protectedProcedure) → parseo del input
(`integrationIds`) → resolve + permisos interact. Las queries no piden
sesión (publicProcedure) y una API key read-only SÍ puede llamarlas.

A diferencia de los routers admin, `Register` NO recibe `*db.DB`: upstream
accede a la base solo dentro del middleware de integraciones, que este port
inyecta por completo con `Options.ResolveIntegrations`.

## 2. El seam de integraciones (M4)

`Options.ResolveIntegrations` es el port de
`createManyWidgetIntegrationMiddleware("query"|"interact", <widgetKind>)`
([mw]): dados `integrationIds`, carga las filas de `integration` filtradas
por los kinds soportados del widget, verifica el permiso de la acción y
devuelve las filas EN EL ORDEN pedido, con los secretos SIN descifrar
(`SecretValues`, valores crudos de la columna `integrationSecret.value`).
Contrato de errores:

- ids faltantes o de otro kind ⇒ `NOT_FOUND` con el mensaje EXACTO de [mw]:
  `"<n> of the specified integrations not found or not of kinds <kinds>:
  ([<ids>] compared to [<kind:id,…>])"` — el port lo genera con
  `widgets.SupportedKinds(widgetKind)` (mediaMissing ⇒
  `sonarr,sportarr,radarr,bindery,mock`; downloads ⇒
  `sabNzbd,nzbGet,deluge,transmission,qBittorrent,aria2,slskd,mock`, en
  orden de definición upstream). Un `ResolveIntegrations` nil se comporta
  como "cero filas en la base" (NOT_FOUND con ese mensaje).
- permisos ⇒ `FORBIDDEN` con los mensajes exactos de [mw] ("User does not
  have permission to query at least one of the specified integration" —
  singular incluido — y "...to interact with at least one of the specified
  integrations").

El router descifra los secretos con `Options.SecretDecryptor` (default:
`widgets.DefaultSecretDecryptor`, el cifrado upstream — ver §8) y llama a
`Options.NewMediaOrganizer` / `Options.NewDownloadClient` /
`Options.NewMediaServer` / `Options.NewNetwork` (default: dispatch por kind
a los clientes de `internal/integrations/{mediaorganizer,downloadclient,
mediaserver,network}`). `NewNetwork` recibe además el `context.Context` del
request porque la construcción del cliente pi-hole SONDEA la versión del
servidor (v5/v6 — ver NETWORK.md §2.1). Un kind sin cliente (`sportarr`,
`nzbGet`, `navidrome`, `technitiumDns`, `mock`…) falla SOLO esa
integración (la query responde su fallback público), el equivalente del
fallo en runtime upstream. Tests: inyectar factories falsas (ver
widgets_test.go y widgets_mediaserver_network_test.go).

## 3. `widget.mediaOrganizer.getData` ([mo])

Input: `{integrationIds: string[], pageSize?: number 1..50 (default 10)}`
— el parseo replica el orden tRPC: primero el del middleware
(`integrationIds`), luego resolve, luego `pageSize` (un pageSize inválido
con ids desconocidos responde NOT_FOUND, como upstream; zod `z.number()` sin
`.int()` acepta fracciones 1..50 y el port trunca para paginar).

Salida: array con UNA entrada por integración, en el orden pedido:

```jsonc
// éxito (error: undefined se descarta en superjson)
{"integrationId":"…","integrationName":"…",
 "missing":[MissingItem…],"missingCount":42,
 "queued":[QueuedItem…],"queuedCount":7}
// fallo de ESA integración
{"integrationId":"…","integrationName":"…","missing":[],"missingCount":0,
 "queued":[],"queuedCount":0,"error":"INTEGRATION_REQUEST_FAILED"}
```

`missingCount`/`queuedCount` son los `totalCount` server-side. Datos de la
caché (60 s, single-flight, sin stale) de las dos llamadas del cliente en
paralelo. Con `throwOnAllFailures: true` ([settle]): si TODAS las
integraciones fallan (y había al menos una) la query responde
INTERNAL_SERVER_ERROR **"All integration queries failed"**; con
`integrationIds: []` responde `[]`.

## 4. `widget.mediaServer.getCurrentStreams` ([ms])

Input: `{integrationIds: string[], showOnlyPlaying: boolean}` — el
middleware parsea `integrationIds` PRIMERO (un showOnlyPlaying ausente con
ids desconocidos responde NOT_FOUND, como el pageSize de §3), luego el
input del procedimiento (`showOnlyPlaying` boolean OBLIGATORIO, sin
default). Salida por integración, en el orden pedido:

```jsonc
// éxito (error: undefined se descarta en superjson)
{"integrationId":"…","integrationName":"…","integrationKind":"plex",
 "sessions":[StreamSession…]}
// fallo de ESA integración
{"integrationId":"…","integrationName":"…","integrationKind":"plex",
 "sessions":[],"error":"INTEGRATION_REQUEST_FAILED"}
```

A diferencia de downloads/dns-hole NO hay objeto `integration` anidado ni
`updatedAt`: upstream devuelve `integrationKind` como campo plano
([ms] :27-33). `throwOnAllFailures: true` (§3). Caché de **10 s**
(DEFAULT_TTL_MS — [rh-ms] no fija `cacheTtlMs`) con clave
`<integrationId> + hash({showOnlyPlaying})`. El tipo wire `StreamSession`
está pinneado byte a byte en MEDIA_SERVER.md §1 (incluido el tri-estado
ausente|null|valor de `episodeName`/`episodeCount` vía `mediaserver.Optional`).

## 5. `widget.dnsHole.*` ([dns])

### 5.1 summary (query)

Input: `{integrationIds}` (middleware only, sin input de procedimiento).
Salida por integración:

```jsonc
// éxito — updatedAt es el timestamp de la caché (ISO-ms con Z)
{"integrationId":"…","integrationName":"…",
 "integration":{"id":"…","name":"…","kind":"piHole","updatedAt":"…"},
 "summary":{"status":"enabled"?,"domainsBeingBlocked":…,"adsBlockedToday":…,
            "adsBlockedTodayPercentage":…,"dnsQueriesToday":…}}
// fallo — summary null, error constante, integration SIN updatedAt
{"integrationId":"…","integrationName":"…",
 "integration":{"id":"…","name":"…","kind":"piHole"},
 "summary":null,"error":"INTEGRATION_REQUEST_FAILED"}
```

`status` es OPCIONAL en el wire (v6 descarta "failed"/"unknown" — NETWORK.md
§1). `throwOnAllFailures` idéntico a §3. Caché de **10 s** (DEFAULT_TTL_MS).

### 5.2 enable / disable (mutations, one-middleware)

- Input de enable: `{integrationId: string}`; de disable:
  `{integrationId: string, duration?: number}` (segundos; presente y
  no-número ⇒ BAD_REQUEST).
- Cardenalidad UNO: el middleware de una integración tiene mensaje
  NOT_FOUND PROPIO ([mw] :113-117) —
  `Integration with id <id> not found or not of kinds <kinds>` —; el port
  lo modela con `ResolveRequest.Cardinality` ("one"/"many") que el
  resolver real (M4) debe respetar.
- Ejecución: se construye el cliente, corre `Enable`/`Disable(duration)`
  y SOLO después de un éxito se invalida la caché del integrationId
  ([dns] :72-75 — un fallo NO invalida, a diferencia de downloads §6.2).
- Errores: el PRIMER (único) fallo responde INTERNAL_SERVER_ERROR con el
  mensaje crudo del cliente — los Error planos de pi-hole v5/adguard
  ("Failed to enable PiHole for …") y la ResponseError de v6
  ("Response did not indicate success") viajan verbatim (getTRPCError
  FromUnknown upstream). Éxito ⇒ void.

## 6. `widget.downloads.*` ([dc])

### 6.1 getJobsAndStatuses (query)

Input: `{integrationIds, limitPerIntegration?: number (default 50, sin
mínimo), selection?: {sort: enum14, descending, categoryFilter: string[≤100]
cada uno ≤1000, filterIsWhitelist, showCompletedTorrent, activeTorrentThreshold:
number ≥ 0 — TODOS obligatorios dentro de selection}, includeArchivedHistory?:
bool (default false), historyWindowDays?: int ≥1 (default 7)}`. El enum de
sort: name, progress, size, downSpeed, upSpeed, time, added, ratio, received,
sent, index, type, state, category, integration ([dc] :64-80).

Salida por integración:

```jsonc
// éxito — updatedAt es el timestamp de la caché (Date upstream → ISO-ms con Z,
// convención del port, ADMIN.md §4)
{"integrationId":"…","integrationName":"…",
 "integration":{"id":"…","name":"…","kind":"qBittorrent","updatedAt":"2026-11-07T09:00:00.000Z"},
 "data":{"status":{...},"items":[...],"ratioItems":[...]?}}
// fallo — data null, error constante, integration SIN updatedAt
{"integrationId":"…","integrationName":"…",
 "integration":{"id":"…","name":"…","kind":"qBittorrent"},
 "data":null,"error":"INTEGRATION_REQUEST_FAILED"}
```

`ratioItems` solo existe cuando la petición trajo `selection` (§1 de
DOWNLOAD_CLIENT.md). Caché de **10 s** con clave
`<integrationId> + hash(input)` (upstream: hashSharedCacheInput incluye la
fingerprint de secretos; el port equivale porque las mutaciones invalidan).
`throwOnAllFailures` idéntico a §3.

### 6.2 Mutaciones ([dc] :122-194)

- `pause` / `resume`: input `{integrationIds}`; corren
  `PauseQueue`/`ResumeQueue` en TODAS las integraciones resueltas
  (upstream también: `ctx.integrations.map(...)`).
- `pauseItem` / `resumeItem`: input `{integrationIds, item:
  downloadClientItemSchema}` — el item se valida campo a campo (id string,
  type enum torrent/usenet/miscellaneous, state enum, progress 0..1…).
- `deleteItem`: añade `fromDisk: boolean`.
- Ejecución: paralelo acotado (10), se reúnen los errores, se invalida la
  caché de los integrationIds SIEMPRE (upstream invalida antes de lanzar) y
  el PRIMER fallo responde INTERNAL_SERVER_ERROR con el mensaje pinned de la
  taxonomía ("Request to integration failed" / "Response from integration
  did not indicate success" / "An unknown error occured…"). Éxito ⇒ void
  (`{"result":{"data":{}}}` en el wire).

## 7. Paralelismo y timeouts

- Entre integraciones: `Options.Parallelism` (default 10 — upstream NO acota
  `Promise.allSettled`; desviación documentada como en el job ping).
- Dentro de una integración: media-organizer lanza missing+queue en
  paralelo (Promise.all upstream); downloads hace una llamada.
- HTTP: media-organizer/sabnzbd sin timeout por request (solo ctx, hasta los
  60 s de la capa request-handler); los clientes @ctrl traen 5 s por request
  (qbit/transmission/deluge). El `ctx` del procedimiento corta todo.

## 8. Secretos ([enc])

`integrationSecret.value` = `"<data hex>.<iv hex>"` — AES-**256-CBC** con
PKCS#7 (no AES-GCM: la nota de INTEGRATIONS.md §7.3 estaba desactualizada;
el código upstream de packages/common/src/encryption.ts usa
`aes-256-cbc`), IV de 16 bytes aleatorio por cifrado, clave = env
`SECRET_ENCRYPTION_KEY` (64 hex chars, validada upstream) con fallback de 32
bytes a cero cuando la validación de env se salta (`encryption.ts:9`).
`widgets.DecryptSecret(value)` / `DecryptSecretWithKey(value, key)` portan
`decryptSecret`/`decryptSecretWithKey` 1:1 y hay un vector cruzado generado
con Node en secrets_test.go. `EncryptSecret` existe para fixtures (el path
de escritura llega con el router de gestión de integraciones, M4). Si el
descifrado falla, la query responde INTERNAL_SERVER_ERROR (el error del
middleware upstream sería idéntico: decryptSecret lanza dentro del
middleware).

## 9. Cobertura de tests

Harness: `trpc.Router` real sobre httptest con sesión por header
(`X-Test-User`, patrón board) y clientes falsos inyectados por factory.

| Test | Cubre |
|---|---|
| `TestMediaOrganizerGetDataSuccessShape` | entrada de éxito byte-exacto (nombres y orden de claves, `error` ausente), request del resolver (action/kind) |
| `TestMediaOrganizerGetDataDefaultsPageSize`, `TestMediaOrganizerPageSizeValidation` | default 10, 0/51/string ⇒ BAD_REQUEST, fracción 2.5 aceptada (zod sin .int) |
| `TestMediaOrganizerFallbackAndPartialFailure` | fallo parcial: entrada OK + fallback público exacto |
| `TestMediaOrganizerAllFailuresThrow` | todas fallan ⇒ 500 "All integration queries failed" |
| `TestMediaOrganizerEmptyIntegrationIDs` | `[]` ⇒ `[]` |
| `TestResolverNotFoundMessage` | mensaje NOT_FOUND exacto del middleware (resolver nil) |
| `TestResolverForbiddenPassthrough` | FORBIDDEN del resolver atraviesa |
| `TestDownloadsGetJobsAndStatusesSuccessShape` | forma de éxito con updatedAt ISO-ms, data.items byte-exactos |
| `TestDownloadsFailureShape` | entrada de fallo byte-exacta (data null, error constante, integration sin updatedAt) + éxito mezclado |
| `TestDownloadsSelectionValidation` | sort inválido, threshold negativo, selection incompleta, historyWindowDays 0/1.5 ⇒ BAD_REQUEST |
| `TestDownloadsMutationsRequireSession` | anónimo ⇒ UNAUTHORIZED en las 3 formas |
| `TestDownloadsMutationsReadOnlyKeyForbidden` | read-only ⇒ FORBIDDEN "This credential only permits read-only operations" |
| `TestDownloadsMutationsRunOnAllIntegrations` | pause en TODAS, interact al resolver, item validado y transportado, fromDisk, item inválido ⇒ BAD_REQUEST |
| `TestDownloadsMutationFailureSurfacesIntegrationMessage` | primer fallo ⇒ 500 "Request to integration failed" |
| `TestDownloadsCacheInvalidation`, `TestMediaOrganizerCacheTTL` | caché TTL (override de test), single-flight, invalidación por mutación |
| `TestSecretRoundtrip`, `TestDecryptUpstreamVector` (vector Node real), `TestDecryptMalformed`, `TestDecryptDefaultsToZeroKey` | cifrado §8 |

Los tests de media-server y dns-hole viven en
widgets_mediaserver_network_test.go con el MISMO harness (resolver y
clientes falsos por factory):

| Test | Cubre |
|---|---|
| `TestMediaServerGetCurrentStreamsSuccessShape` | entrada de éxito byte-exacto (integrationKind plano, tri-estado de sessions), request del resolver (mediaServer) |
| `TestMediaServerInputValidation` | showOnlyPlaying obligatorio ⇒ BAD_REQUEST; NOT_FOUND del middleware-many gana al boolean ausente (resolver nil) |
| `TestMediaServerFallbackAndAllFailures`, `TestMediaServerEmptyIntegrationIDs` | fallback público exacto, todas fallan ⇒ 500, `[]` ⇒ `[]` |
| `TestMediaServerCacheTTL` | caché 10 s (override), single-flight, showOnlyPlaying parte de la clave |
| `TestDNSHoleSummarySuccessShape`, `TestDNSHoleSummaryFailureShape` | forma de éxito (updatedAt ISO-ms) y de fallo (summary null) byte-exactas + throwOnAllFailures |
| `TestDNSHoleMutationsRequireSession` | anónimo ⇒ UNAUTHORIZED; read-only ⇒ FORBIDDEN |
| `TestDNSHoleMutations` | interact/dnsHoleControls/Cardinality one, integrationId único, duration forwardada y nil, duration inválida ⇒ BAD_REQUEST |
| `TestDNSHoleOneNotFoundMessage` | mensaje NOT_FOUND EXACTO del one-middleware |
| `TestDNSHoleMutationFailureSurfacesExactMessage` | el mensaje crudo del cliente viaja (500) |
| `TestDNSHoleMutationInvalidation` | invalidación SOLO tras éxito (fallo no invalida) |

Ejecución: `go test ./internal/api/router/widgets/...`.

## 10. Pendientes / preguntas para el integrador

1. **Resolver DB-backed**: escribir el `ResolveIntegrations` real (SQL sobre
   `integration` + `integrationSecret` + permisos de
   `integrationUserPermission`/`integrationGroupPermission` con los mensajes
   de §2) cuando M4 cree las tablas en `internal/db` (el DDL ya existe en
   packages/db/schema/sqlite.ts:205+). El router ya consume el contrato.
2. **`mock`**: el kind demo necesita un cliente mock para DEMO_MODE
   (upstream: packages/integrations/src/mock).
3. **superjson Dates**: `updatedAt` viaja como ISO string (convención del
   port); si el frontend exigiera un Date real, la costura es
   `meta.values` en `trpc.WrapJSON` (ADMIN.md §7.1).
4. **Wiring**: añadir `widgets.Register(r, opts)` en el composition root
   junto al resto de routers (httpserver), con el resolver y las factories
   default.
