# WIDGETS.md — router de widgets (`internal/api/router/widgets`)

Documento de referencia del port en Go del dominio de widgets de
`packages/api/src/router/widgets`: hoy `mediaOrganizerRouter` (media-organizer.ts)
y `downloadsRouter` (downloads.ts), las dos superficies de datos de los
widgets media-organizer y downloads. Complementa MEDIA_ORGANIZER.md y
DOWNLOAD_CLIENT.md (clientes) y PROTOCOL.md (wire). Fecha: 2026-11-07.

Fuentes citadas (todas en este repo):

- **[mo]** `packages/api/src/router/widgets/media-organizer.ts:9-47`.
- **[dc]** `packages/api/src/router/widgets/downloads.ts:49-194`.
- **[idx]** `packages/api/src/router/widgets/index.ts` (montaje
  `mediaOrganizer`/`downloads` bajo `widget`).
- **[settle]** `packages/api/src/settle-integrations.ts`
  (PUBLIC_INTEGRATION_ERROR :7, throw :52-66).
- **[mw]** `packages/api/src/middlewares/integration.ts`
  (createManyWidgetIntegrationMiddleware :218-230, NOT_FOUND :186-192,
  FORBIDDEN :266-296, decryptSecret de decryptedSecrets :205-212).
- **[rh-mo]** `packages/request-handler/src/media-organizer.ts`
  (caché 60 s), **[rh-dc]** `packages/request-handler/src/downloads.ts`
  (caché 10 s), **[rh-core]** `packages/request-handler/src/lib/request-handler.ts`
  (DEFAULT_REQUEST_TIMEOUT_MS 60_000 :35, sin stale-if-error).
- **[enc]** `packages/common/src/encryption.ts` (AES-256-CBC, key de
  SECRET_ENCRYPTION_KEY, formato `"<data hex>.<iv hex>"`).
- **[map]** `packages/definitions/src/widget-integration-map.ts:141,148`
  + `packages/definitions/src/integration.ts` (categorías).
- **[cred]** `packages/api/src/trpc.ts:130-135` (credentialProcedure) y
  :163-172 (protectedProcedure).

## 1. Registro

```go
r := trpc.New()
widgets.Register(r, widgets.Options{ResolveIntegrations: ..., /* factories opcionales */})
// chi: r.Handle("/api/trpc/*", r.Handler())
```

Procedimientos registrados (paths exactos de [mo]/[dc] montados en el
sub-router `widget` de [idx]):

| Path | Tipo | Permiso upstream | Port |
|---|---|---|---|
| `widget.mediaOrganizer.getData` | query | publicProcedure + middleware "query"/mediaMissing | público (sin gate de sesión) |
| `widget.downloads.getJobsAndStatuses` | query | publicProcedure + middleware "query"/downloads | público |
| `widget.downloads.pause` | mutation | protectedProcedure + "interact" | `admin.Writable` + RequireSignedIn |
| `widget.downloads.pauseItem` | mutation | ídem | ídem |
| `widget.downloads.resume` | mutation | ídem | ídem |
| `widget.downloads.resumeItem` | mutation | ídem | ídem |
| `widget.downloads.deleteItem` | mutation | ídem | ídem |

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
`widgets.DefaultSecretDecryptor`, el cifrado upstream — ver §6) y llama a
`Options.NewMediaOrganizer` / `Options.NewDownloadClient` (default:
dispatch por kind a los clientes de `internal/integrations/{mediaorganizer,
downloadclient}`). Un kind sin cliente (`sportarr`, `nzbGet`, `mock`…) falla
SOLO esa integración (la query responde su fallback público), el equivalente
del fallo en runtime upstream. Tests: inyectar factories falsas (ver
widgets_test.go).

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

## 4. `widget.downloads.*` ([dc])

### 4.1 getJobsAndStatuses (query)

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

### 4.2 Mutaciones ([dc] :122-194)

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

## 5. Paralelismo y timeouts

- Entre integraciones: `Options.Parallelism` (default 10 — upstream NO acota
  `Promise.allSettled`; desviación documentada como en el job ping).
- Dentro de una integración: media-organizer lanza missing+queue en
  paralelo (Promise.all upstream); downloads hace una llamada.
- HTTP: media-organizer/sabnzbd sin timeout por request (solo ctx, hasta los
  60 s de la capa request-handler); los clientes @ctrl traen 5 s por request
  (qbit/transmission/deluge). El `ctx` del procedimiento corta todo.

## 6. Secretos ([enc])

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

## 7. Cobertura de tests

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
| `TestSecretRoundtrip`, `TestDecryptUpstreamVector` (vector Node real), `TestDecryptMalformed`, `TestDecryptDefaultsToZeroKey` | cifrado §6 |

Ejecución: `go test ./internal/api/router/widgets/...`.

## 8. Pendientes / preguntas para el integrador

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
