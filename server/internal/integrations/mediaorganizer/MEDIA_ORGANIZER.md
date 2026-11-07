# MEDIA_ORGANIZER.md — dominio media-organizer (`internal/integrations/mediaorganizer`)

Documento de referencia del port en Go de la abstracción compartida
media-organizer de upstream (`packages/integrations/src/interfaces/media-organizer`
+ los clientes por app de `packages/integrations/src/media-organizer`): los
datos de faltantes/cola que consume el widget media-organizer vía la query
tRPC `widget.mediaOrganizer.getData` (portada en
`internal/api/router/widgets`, ver WIDGETS.md). Todas las afirmaciones citan
fuente upstream; las desviaciones están marcadas. Fecha: 2026-11-07.

Fuentes citadas (todas en este repo):

- **[iface]** `packages/integrations/src/interfaces/media-organizer/media-organizer-integration.ts`
  + `media-organizer-types.ts` (IMediaOrganizerIntegration, MissingMediaItem,
  QueuedMediaItem).
- **[sonarr]** `packages/integrations/src/media-organizer/sonarr/sonarr-integration.ts`
  (getMissingAsync :192-229, getMediaQueueAsync :231-272, testingAsync
  :281-290, schemas :293-340).
- **[radarr]** `.../media-organizer/radarr/radarr-integration.ts`
  (getMissingAsync :150-181, getMediaQueueAsync :183-221, testingAsync
  :230-239, schemas :250-283).
- **[bindery]** `.../media-organizer/bindery/bindery-integration.ts`
  (getMissingAsync :22-50, getMediaQueueAsync :52-93, testingAsync :101-110,
  schemas :113-140).
- **[base]** `packages/integrations/src/base/integration.ts` (createUrl
  :70-88, externalUrl :93-98, getSecretValue :58-64).
- **[handler]** `packages/request-handler/src/media-organizer.ts:14-34`
  (cacheNamespace "media-organizer:summary", cacheTtlMs 60_000, las dos
  llamadas en Promise.all).
- **[widget]** `packages/api/src/router/widgets/media-organizer.ts:9-47` +
  `packages/api/src/settle-integrations.ts`.
- **[defs]** `packages/definitions/src/integration.ts` (categorias y
  secretKinds de sonarr/sportarr/radarr/lidarr/readarr/bindery),
  `packages/definitions/src/widget-integration-map.ts:148`.
- **[prio]** `packages/integrations/src/media-organizer/media-organizer.ts:6-17`
  (mediaOrganizerPriorities).

## 1. La interfaz y los tipos

[iface] :3-6 — solo dos métodos, con `pageSize = 10` por defecto:

```go
type Client interface {
    GetMissing(ctx context.Context, pageSize int) (MissingResult, error)
    GetMediaQueue(ctx context.Context, pageSize int) (QueueResult, error)
}
```

`MissingItem`/`QueuedItem` replican `MissingMediaItem`/`QueuedMediaItem`
([iface] media-organizer-types.ts) con el JSON exacto del wire:

- opcionales ausentes ⇒ clave ausente (`year`, `seasonNumber`,
  `episodeNumber`, `seriesTitle` — punteros con `omitempty`),
- `imageUrl` ⇒ **siempre presente** (`null` cuando no hay imagen: todos los
  mappers upstream escriben `?? null` — [sonarr] :222/:265, [radarr]
  :176/:214, [bindery] :45/:86),
- `timeLeft` ⇒ siempre presente en cola (`null` si no aplica), `string`,
- `percentComplete` ⇒ entero 0..100 (`Math.round` del porcentaje de bytes,
  [sonarr] :252-254; el denominador ≤ 0 ⇒ 0).

Orden de claves del wire: superjson respeta el orden de inserción del objeto
literal del mapper upstream (p. ej. sonarr pone `seasonNumber` antes de
`year`, radarr solo `year`). El port usa UN struct común en el orden de la
declaración TS (id, title, type, year, seasonNumber, episodeNumber,
seriesTitle, imageUrl, link): el orden de claves JSON no es semántico para el
frontend y una sola forma evita structs por app.

## 2. Qué apps implementan el dominio

| Kind upstream | ¿Media organizer? | Puerto |
|---|---|---|
| `sonarr` | sí ([defs] categoria `["calendar","mediaOrganizer","serviceVersion"]`, secretKinds `[["apiKey"]]`) | `mediaorganizer/sonarr` |
| `radarr` | sí (`["calendar","mediaOrganizer","serviceVersion"]`) | `mediaorganizer/radarr` |
| `bindery` | sí (`["mediaOrganizer","serviceVersion"]`) | `mediaorganizer/bindery` |
| `sportarr` | **NO** — categoria `mediaOrganizer` pero NO implementa `IMediaOrganizerIntegration` (solo calendar); upstream fallaría en runtime (`getMissingAsync is not a function`) | no portado |
| `lidarr` / `readarr` | **NO** — categoria solo `["calendar","serviceVersion"]` | no portado |
| `mock` | sí (datos demo, `packages/integrations/src/mock`) | diferido (M4 demo mode) |

El widget declara `supportedIntegrations =
getIntegrationKindsByCategory("mediaOrganizer")` ([widget-map] :148); un kind
sin la interfaz produce la entrada de error pública por integración (§5), el
mismo resultado que upstream.

## 3. Clientes por app (semántica exacta)

Auth común: header **`X-Api-Key: <apiKey>`** en todas las llamadas
(`apiKeyAuth`; [bindery] declara además httpAuth
`{type: "apiKeyHeader", name: "X-Api-Key"}`). Un secret ausente produce el
error upstream EXACTO de `getSecretValue` ([base] :58-64):
**`No secret of kind apiKey was found`** (como `CheckResult.Error` del
framework y como error de las llamadas de dominio).

URLs: `url(path, query)` = baseUrl sin slash final + path ([base] :70-88);
los links de UI usan `externalUrl(path)` = `externalUrl ?? url` ([base]
:93-98).

### 3.1 sonarr ([sonarr])

- `GetMissing`: `GET /api/v3/wanted/missing?page=1&pageSize=<n>&sortKey=airDateUtc&sortDirection=ascending&includeSeries=true`.
- `GetMediaQueue`: `GET /api/v3/queue?page=1&pageSize=<n>&includeSeries=true&includeEpisode=true`.
- Respuesta paginada `{totalRecords, records}`; items:
  `type: "episode"`, título de cola = `episode.title ?? title`, link
  `externalUrl(/series/<titleSlug>)` con fallbacks `/wanted/missing`
  (missing) y `/activity/queue` (cola) cuando no hay `series.titleSlug`
  (:223-226, :266-268); imagen = primera image con `coverType === "poster"`
  (`remoteUrl ?? null`).
- `Check`: `GET {url}/api` con el header (:281-290); 2xx ⇒ éxito.

### 3.2 radarr ([radarr])

- `GetMissing`: `GET /api/v3/wanted/missing?page=1&pageSize=<n>&sortKey=title&sortDirection=ascending`.
- `GetMediaQueue`: `GET /api/v3/queue?page=1&pageSize=<n>&includeMovie=true`.
- items `type: "movie"`, link `externalUrl(/movie/<titleSlug>)` (missing
  siempre tiene slug; cola cae a `/activity/queue`), `year` de la película.
- `Check`: `GET {url}/api` (:230-239).

### 3.3 bindery ([bindery])

Sin paginación server-side: ambos endpoints traen TODO y el port pagina
client-side (`slice(0, pageSize)`), pero `totalCount` cuenta el array COMPLETO
en missing (:33) y los activos en cola (:66-69).

- `GetMissing`: `GET /api/v1/wanted/missing` — **array desnudo** (sin
  `{items,total}`). `year` = `new Date(releaseDate).getUTCFullYear()` (null ⇒
  ausente). `imageUrl` SOLO si empieza por `http` (las demás URLs son rutas
  proxied con auth que darían 401 en el navegador, :40-45). Link
  `externalUrl(/book/<id>)`.
- `GetMediaQueue`: `GET /api/v1/queue` — unión `array | {items}` (z.union,
  :58-61); se EXCLUYEN los `status === "imported"` (el endpoint devuelve el
  histórico completo, :62-66). `percentComplete = Math.round(parseFloat(percentage))`
  (0 si falta); un item al 100% NO muestra el `timeLeft` stale (:70-76).
  Link `/book/<book.id>` si hay, si no `/queue`.
- `Check`: `GET {url}/api/v1/system/status` (:101-110) — Bindery no tiene
  `/api` desnudo.

## 4. HTTP y errores

Transporte: upstream usa `fetchWithTrustedCertificatesAsync`
(`packages/core/src/infrastructure/http/request.ts:111-137`) — TLS verificado
(con los certificados de confianza del dominio certificates, no portado aún;
el port usa los roots del sistema — costura `HTTPClient.Client` para
inyectar), redirects seguidos y **sin timeout por request** (el deadline
viene de la capa request-handler: `DEFAULT_REQUEST_TIMEOUT_MS = 60_000`,
`packages/request-handler/src/lib/request-handler.ts:35`; el port lo expone
como `mediaorganizer.DefaultRequestTimeout` y los clientes honran el
`context`).

Taxonomía de errores replicada 1:1 (`packages/integrations/src/base/errors`):

| Clase upstream | `mediaorganizer.Error.Kind` | Mensaje (pinned) |
|---|---|---|
| `IntegrationRequestError` | `ErrKindRequest` | `Request to integration failed` |
| `IntegrationResponseError` (no 2xx) | `ErrKindResponse` | `Response from integration did not indicate success` |
| `IntegrationParseError` (JSON/zod) | `ErrKindParse` | `Failed to parse integration data` |
| `IntegrationUnknownError` | `ErrKindUnknown` | `An unknown error occured while executing Integration method` (typo incluido) |

Los mensajes solo llegan al wire en MUTACIONES (downloads) y como texto de
`CheckResult.Error`; las QUERIES los reemplazan por la constante pública
`INTEGRATION_REQUEST_FAILED` (WIDGETS.md §3). El "unavailable vs error" del
enunciado vive aquí: una caída de red es `ErrKindRequest` (cliente
unavailable) y una respuesta 4xx/5xx o un JSON inválido son
`ErrKindResponse`/`ErrKindParse` (cliente disponible pero roto) — distinción
que upstream también solo usa para logs/test-connection.

## 5. Flujo de datos del widget

[handler] media-organizer.ts: `getMissingAsync` + `getMediaQueueAsync` en
paralelo por integración, aplanado a
`{missing, missingCount, queued, queuedCount}`, caché in-memory **60 s**
(`cacheTtlMs: 60_000`) con single-flight y sin stale-if-error. La query tRPC
(`widget.mediaOrganizer.getData`) ejecuta las integraciones en paralelo
(`Promise.allSettled` SIN límite upstream) con fallback por integración
`{integrationId, integrationName, missing: [], missingCount: 0, queued: [],
queuedCount: 0, error: "INTEGRATION_REQUEST_FAILED"}` y
`throwOnAllFailures: true` ⇒ si TODAS fallan, INTERNAL_SERVER_ERROR
`"All integration queries failed"`. El port acota el paralelismo a 10
(desviación documentada, INTEGRATIONS.md §5.4). No hay cron jobs ni eventos
del bus para este dominio: el widget refresca por react-query.

## 6. Registro en el framework

Cada subpaquete se auto-registra (`integrations.Register`) con el kind EXACTO
de la columna `integration`.`kind` ([defs]): `sonarr`, `radarr`, `bindery`.
Como `integrations.Input` aún no lleva secretos (INTEGRATIONS.md §5.7), las
factories del registry construyen el cliente SIN apiKey y su `Check` responde
el mensaje exacto de missing-secret; la construcción real (con secretos
descifrados) pasa por la factory del router de widgets
(`Options.NewMediaOrganizer`, WIDGETS.md §2) hasta que M4 amplíe `Input`.
Desviación menor documentada: los clientes no revalidan `z.string().url()`
sobre `remoteUrl` (upstream sí); un remoteUrl inválido viajaría igual — el
frontend solo lo renderiza.

## 7. Cobertura de tests (httptest, fixtures byte-exactos)

| Paquete | Tests | Cubre |
|---|---|---|
| `sonarr` | `TestCheckOnline`, `TestCheckUnauthorizedStatusIsDefinitive`, `TestCheckMissingSecretReportsUpstreamMessage`, `TestCheckConnectionRefused`, `TestCheckSendsAPIKeyHeader` | Check: 2xx, 401 definitivo, missing-secret, transporte caído, header |
| | `TestGetMissingShapeAndQuery`, `TestGetMediaQueueShape`, `TestGetMediaQueueServerError`, `TestGetMissingBadJSON`, `TestExternalURLOverride` | query params exactos, JSON de items byte-exacto (incluye `imageUrl: null`, fallback de links), errores response/parse, externalUrl |
| | `TestRegistryRegistration` | autorregistro kind `sonarr` + Check sin secretos |
| `radarr` | `TestCheckOnlineAndHeader`, `TestCheckNon2xxIsDefinitive`, `TestGetMissingShape` (sortKey=title, sin includeSeries), `TestGetMediaQueueShape` (includeMovie, override de título/año, fallback), `TestPercentCompleteRounding` (66.67 ⇒ 67), `TestMissingSecret`, `TestQueueServerErrorPath` | ídem radarr |
| `bindery` | `TestCheckUsesSystemStatus`, `TestGetMissingBareArrayAndPaging` (array desnudo, totalCount completo, filtro http de imágenes), `TestGetMediaQueueUnionAndFiltering` (unión array|items, exclusión imported, drop de timeLeft al 100%), `TestGetMediaQueueBareArray`, `TestMissingSecret`, `TestServerErrorPath` | ídem bindery |

Ejecución: `go test ./internal/integrations/mediaorganizer/...`.

## 8. Preguntas abiertas para el integrador

1. **Secretos en `integrations.Input`**: ampliar el struct del framework
   (INTEGRATIONS.md §5.7) y mover la resolución de las factories del registry
   de "missing secret" a clientes reales; la factory del router de widgets
   ya consume secretos descifrados y no cambia.
2. **TLS con certificados de confianza** (dominio certificates, M4):
   inyectar un `*http.Client` con el CA pool ampliado vía `HTTPClient.Client`
   — la costura ya existe en cada cliente.
3. **Sportarr**: si upstream le añade `IMediaOrganizerIntegration`, portar
   aquí (subclass de sonarr con paths propios) y añadirlo a
   `widgets.DefaultMediaOrganizerFactory`.
4. **mock**: el kind demo (`packages/integrations/src/mock`) falta; el
   DEMO_MODE del fork lo necesitará para el widget sin integraciones reales.
