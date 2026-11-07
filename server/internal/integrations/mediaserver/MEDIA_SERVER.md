# MEDIA_SERVER.md — dominio media-server (`internal/integrations/mediaserver`)

Documento de referencia del port en Go de la abstracción compartida
media-server de upstream (`packages/integrations/src/interfaces/media-server`
+ los clientes por app de `packages/integrations/src/{plex,jellyfin,emby}`):
las sesiones "now playing" que consume el widget media-server vía la query
tRPC `widget.mediaServer.getCurrentStreams` (portada en
`internal/api/router/widgets`, ver WIDGETS.md). Todas las afirmaciones citan
fuente upstream; las desviaciones están marcadas. Fecha: 2026-11-07.

Fuentes citadas (todas en este repo):

- **[iface]** `packages/integrations/src/interfaces/media-server/
  media-server-integration.ts:3-6` + `media-server-types.ts:1-57`.
- **[plex]** `packages/integrations/src/plex/plex-integration.ts`
  (getCurrentSessionsAsync :160-264, testingAsync :461-474,
  parseOptionalNumber :22-29, isStreamDirect :153-157) +
  `plex/interface.ts` (formas XML).
- **[jfin]** `packages/integrations/src/jellyfin/jellyfin-integration.ts`
  (auth :87-130, sesiones :150-242, ticksToMs :28-30, parseLocation :33-66)
  + el spec `jellyfin-integration.spec.ts` (casos de parseLocation).
- **[emby]** `packages/integrations/src/emby/emby-integration.ts`
  (headers :93-95, sesiones :115-222, testingAsync :97-113, schemas
  :16-90).
- **[widget]** `packages/api/src/router/widgets/media-server.ts:9-47` +
  `packages/request-handler/src/media-server.ts:7-19` (caché 10 s por
  DEFAULT_TTL_MS, `packages/request-handler/src/lib/request-handler.ts:34`)
  + `packages/api/src/settle-integrations.ts`.
- **[defs]** `packages/definitions/src/integration.ts`
  (secretKinds/httpAuth de plex/jellyfin/emby, categorías "mediaService"),
  `packages/definitions/src/widget-integration-map.ts:138`.

## 1. La interfaz y el tipo wire

[iface] — el widget solo necesita las sesiones activas:

```go
type Client interface {
    GetCurrentSessions(ctx context.Context, input CurrentSessionsInput) ([]StreamSession, error)
}
type CurrentSessionsInput struct{ ShowOnlyPlaying bool }
```

`StreamSession` reproduce `media-server-types.ts` byte a byte (nombres y
orden de claves, null vs clave ausente):

- siempre presentes: `sessionId`, `sessionName`, `user{userId, username,
  profilePictureUrl (string|null)}`, `currentlyPlaying` (null cuando el
  servidor no reporta reproducción),
- `currentlyPlaying`: `type` (audio|video|tv|movie), `name`,
  `seasonNumber/episodeNumber` (number|null), `episodeName`
  (tri-estado), `albumName` (string|null para plex, "" para
  jellyfin/emby), `playback{state (null|"playing"|"paused"|"buffering"),
  positionMs, durationMs}`, `location` (null|lan|wan), `metadata` (null u
  objeto completo),
- `metadata`: `{video{resolution|null, frameRate|null},
  audio{channelCount|null, codec|null}, transcoding{container,
  resolution, target{audioCodec, videoCodec}, isVideoDirect,
  isAudioDirect, containerChanged}, bitrateKbps|null}`.

El **tri-estado** (ausente | null | valor): superjson descarta los
`undefined` y conserva los `null`. `seasonName` es `string|undefined`
(plex pasa `parentTitle` crudo; jellyfin/emby hacen `?? ""` — siempre
presente) ⇒ puntero con `omitempty`. `episodeName` y `episodeCount` son
genuinamente tri-estado en jellyfin/emby (`isEpisode ? EpisodeTitle : null`
con EpisodeTitle ausente ⇒ clave ausente) ⇒ el paquete define `Optional`
(interfaz con `MarshalJSON`: nil = omite, `NullValue()` = null,
`OfValue(v)` = valor; `OptionalNullish(raw)` decodifica el estado del
JSON de entrada). plex nunca deja esas claves ausentes (`?? null`).

Los números del mapeo viajan como `float64` (JSON idéntico para enteros;
`frameRate` 23.976 y porcentajes requieren decimales).

## 2. Clientes por app

| Kind upstream | Secretos ([defs] secretKinds) | Puerto | Timeout por request |
|---|---|---|---|
| `plex` | `[["apiKey"]]` (X-Plex-Token) | `mediaserver/plex` | ninguno (fetch upstream) |
| `jellyfin` | `[["username","password"],["apiKey"]]` | `mediaserver/jellyfin` | ninguno (axios sin timeout) |
| `emby` | `[["apiKey"]]` (X-Emby-Token) | `mediaserver/emby` | ninguno (fetch upstream) |
| `navidrome` | — | **no portado** (implementa IMediaServerIntegration upstream; el router de widgets lo marca como fallo por integración) | — |

Ninguno de los tres verifica `response.ok` igual: plex NO lo verifica ( lee
el XML sea cual sea el estado — un 401 con body no-XML revienta el parseo,
como upstream); jellyfin/emby rechazan los no-2xx (axios/fetch +
ResponseError).

### 2.1 Plex ([plex])

- **Auth**: header `X-Plex-Token: <apiKey>` en TODO (plex solo tiene JSON
  para /identity, /prefs...; /status/sessions es XML).
- **Sesiones**: `GET {url}/status/sessions` — se parsea el XML con
  encoding/xml (upstream: xml2js `parseStringPromise`, ParseError "Invalid
  xml format"; el mensaje nunca llega al wire — la query lo sustituye por
  el público). Se aplana `[Video ?? [], Track ?? []]`; los elementos SIN
  `Player` se descartan; sin sesiones ⇒ `[]`.
- Mapeo (:180-263): todos los escalares son atributos STRING que pasan por
  `parseOptionalNumber` (""/basura ⇒ null), `parsePlaybackState` (solo
  playing/paused/buffering, resto null), `parseLocation` (solo lan/wan) y
  `parseResolution` (ambos números o null). Los atributos ausentes son
  punteros nil en los structs XML — el equivalente del `undefined` de
  xml2js que las cadenas `??` consumen (un atributo presente pero vacío
  gana, `??` no descarta "").
  - `type`: `live === "1"` ⇒ tv; movie/episode/track ⇒ movie/video/audio;
    resto ⇒ video (getCurrentlyPlayingType :491-502).
  - `name`: `grandparentTitle ?? title ?? "Unknown"`.
  - `sessionName`: `` `${product} (${title})` `` del Player.
  - usuario anónimo cuando no hay User (`"Anonymous"`); sessionId
    `"unknown"` sin Session.
  - streams: `Media[0].Part[0].Stream` (solo la PRIMERA Part), video =
    streamType "1", audio = "2".
  - transcode: `isVideoDirect/isAudioDirect = decision !== "transcode"`
    (ausente y "copy" cuentan como directos); `containerChanged` compara
    TranscodeSession.container con Media[0].$.container; bitrate:
    `Session.$.bandwidth ?? Media.$.bitrate` (el ?? salta null, así que un
    bandwidth vacío — que parsea a null — cae al bitrate del Media).
- **Check** (testingAsync :461-474): `GET {url}/prefs` con el token y
  `Accept: application/json`; no-2xx ⇒ resultado con statusCode.
- La superficie `IMediaReleasesIntegration` de la MISMA clase
  (getMediaReleasesAsync + image proxy + merge de episodios,
  plex-integration.ts:266-458) pertenece al widget mediaReleases y NO se
  porta aquí (ver §7).

### 2.2 Jellyfin ([jfin])

- **Auth** (getAccessTokenAsync :100-130): con `apiKey` presente es el
  token directamente; si no, `POST {url}/Users/AuthenticateByName` con
  `Authorization: MediaBrowser Client="Homarr", Device="Homarr",
  DeviceId="homarr", Version="0.0.1"` (prefijo fijo :87), body
  `{"Username":...,"Pw":...}` (orden de claves de JSON.stringify — el port
  usa un struct con ese orden), `redirect: "error"` (una redirección es
  fallo) y TODOS los fallos colapsan en el Error plano "Jellyfin
  authentication failed". Las autenticaciones concurrentes de la misma
  identidad (url+usuario+password) comparten UNA petición en vuelo
  (`pendingAuthentications` :88, a nivel de módulo upstream; por instancia
  aquí — mismo alcance de identidad).
- **Header de cada llamada**: `Authorization: <prefijo>, Token="<token>"`
  (el header del cliente Api del SDK, getHttpAuthenticationAsync :92-98).
- **Sesiones** (:150-242): `GET {url}/Sessions` sin query params. Filtros
  en orden: `UserId !== undefined`, `DeviceId !== "homarr"` (un DeviceId
  nullish SE CONSERVA — null !== "homarr") y `showOnlyPlaying` exige
  NowPlayingItem.
  - `ticksToMs` (:28-30): null ⇒ null, `Math.round(ticks/10000)`.
  - estado: `IsPaused ? "paused" : "playing"` (PlayState nullish ⇒
    playing; nunca null, a diferencia de plex).
  - bitrate (:166-176): `TranscodingInfo.Bitrate ?? MediaSources[0].Bitrate
    ?? (suma de MediaStreams.BitRate || null)` — ojo: un Bitrate 0 de
    MediaSources SE CONSERVA (?? solo salta null/undefined) ⇒
    bitrateKbps 0, no null.
  - `resolution` solo si Width && Height son truthy (0 excluido).
  - `isVideoDirect/isAudioDirect`: `TranscodingInfo.X ?? true`.
  - `containerChanged`: `PlayState.PlayMethod === "DirectStream"` — viene
    del PlayState, INDEPENDIENTE de que haya TranscodingInfo.
  - `location`: jellyfin no reporta lan/wan — se clasifica el
    RemoteEndPoint en cliente (parseLocation :54-66): extractHost (corchetes
    IPv6, o IPv4:puerto por conteo de ":"), isIP de node (::ffff:x.x.x.x es
    6 ⇒ se desenvuelve a IPv4 antes del rango, :58-63) y los rangos
    privados (:34-42: 10/8, 172.16/12, 192.168/16, 127/8, 169.254/16,
    fc00::/7, fe80::/10, ::1) vía net/netip.
  - `convertJellyfinType` (:366-385): Audio/MusicVideo ⇒ audio;
    Episode/Video ⇒ video; Movie ⇒ movie; TvChannel/TvProgram/LiveTv*
    y cualquier desconocido ⇒ tv.
  - `profilePictureUrl`: `externalUrl(/Users/<UserId>/Images/Primary)` con
    fallback a la URL interna.
- **Check** (testingAsync :143-148): con apiKey solo `GET /System/Ping`;
  con credenciales el SDK autentica antes (`authenticateUserByName`
  :354-357) — el port encadena POST AuthenticateByName (con la taxonomía
  axios: request/response) + GET /System/Ping. La diferencia con la query:
  el fallo de login del Check reporta los mensajes de taxonomía, mientras
  la query envuelve TODO en "Jellyfin authentication failed".

### 2.3 Emby ([emby])

- **Auth**: TODA llamada lleva `X-Emby-Token: <apiKey>` Y
  `Authorization: Emby Client="Dashboard", Device="Homarr",
  DeviceId="homarr-emby-integration", Version="0.0.1"` (:93-95). Sin login
  por credenciales (apiKey obligatoria).
- **Sesiones** (:115-222): `GET {url}/emby/Sessions` (prefijo /emby).
  Errores con mensajes EXACTOS: no-2xx ⇒ `Emby server <id> returned a non
  successful status code: <status>`; fallo de zod ⇒ `Emby server <id>
  returned an unexpected response: ...` — ambos Error plano (taxonomía
  unknown); el port los emite como errores planos con ese texto (la query
  los sustituye por el público igualmente).
- Filtros idénticos a jellyfin salvo el DeviceId: `"homarr-emby-integration"`.
- El mapeo reutiliza los helpers del módulo jellyfin VERBATIM (upstream
  importa `convertJellyfinType, parseLocation, ticksToMs` :13; el port
  llama `jellyfin.ConvertType/ParseLocation/TicksToMs`).
- El schema zod (:29-63) es nullish: `EpisodeTitle`/`EpisodeCount` null se
  mantienen null y ausentes se descartan (tri-estado, §1).
- **Check** (testingAsync :97-113): `GET {url}/emby/System/Ping` con ambos
  headers.

## 3. Flujo de datos y errores

[widget] `widget.mediaServer.getCurrentStreams` (query, publicProcedure +
middleware "query"/"mediaServer"): entrada `{integrationIds: string[],
showOnlyPlaying: boolean}` — el middleware parsea `integrationIds` PRIMERO
(un showOnlyPlaying ausente con ids desconocidos responde NOT_FOUND, como
el pageSize de media-organizer), luego el input del procedimiento
(`showOnlyPlaying` boolean OBLIGATORIO, sin default). Salida por
integración, en el orden pedido:

```jsonc
// éxito (error: undefined se descarta en superjson)
{"integrationId":"…","integrationName":"…","integrationKind":"plex",
 "sessions":[StreamSession…]}
// fallo de ESA integración
{"integrationId":"…","integrationName":"…","integrationKind":"plex",
 "sessions":[],"error":"INTEGRATION_REQUEST_FAILED"}
```

Nótese que NO hay objeto `integration` anidado ni `updatedAt` (a diferencia
de downloads/dns-hole): upstream devuelve `integrationKind` como campo
plano (:27-33). `throwOnAllFailures: true`: si TODAS fallan ⇒
INTERNAL_SERVER_ERROR "All integration queries failed"; `integrationIds:
[]` ⇒ `[]`.

Caché del request-handler: **10 s** (DEFAULT_TTL_MS, media-server.ts no
fija `cacheTtlMs`), single-flight, sin stale-if-error; la clave del port es
`<integrationId> + hash({showOnlyPlaying})` (equivalente del
hashSharedCacheInput con la fingerprint de secretos, que en un solo
proceso solo cambia por mutaciones). El frontend re-polling cada 10 s
(`packages/widgets/src/refetch-intervals.ts:18`) hace el resto.

**Sin eventos ni WS**: apps/websocket no tiene canal de media y las sesiones
viajan SOLO por tRPC — este port no añade topics de bus para el dominio.

## 4. Registro en el framework

Cada subpaquete se auto-registra con el kind EXACTO upstream (`plex`,
`jellyfin`, `emby`). Igual que en media-organizer, las factories del
registry no tienen secretos aún (INTEGRATIONS.md §5.7): su `Check` responde
el mensaje exacto de missing-secret; los clientes reales se construyen vía
`widgets.Options.NewMediaServer` (`DefaultMediaServerFactory`,
options.go) con secretos descifrados. El estado mutables por instancia:
jellyfin guarda el token de acceso y el mapa de logins en vuelo (un
proceso de larga vida reutiliza el token; no hay invalidación por 401
upstream en la query path — el fallo se propaga).

## 5. Cobertura de tests (httptest, fixtures byte-exactos)

| Paquete | Tests | Cubre |
|---|---|---|
| `plex` | `TestCheckOnline` (/prefs + token + Accept), `TestCheckMissingSecretReportsUpstreamMessage`, `TestCheckTransportFailureIsRequestError`, `TestGetCurrentSessionsShape` (XML byte-exacto: episodio con transcode (videoDecision transcode/copy, containerChanged), track sin Session (sessionId unknown, usuario Anonymous, albumName, seasonName del parentTitle), sin status check), `TestGetCurrentSessionsLiveIsTV` (live="1" ⇒ tv), `TestGetCurrentSessionsEmpty` ([] no nil), `TestGetCurrentSessionsBadXML` (parse), `TestGetCurrentSessionsMissingSecret` | todo el flujo XML |
| `jellyfin` | `TestCheckWithAPIKey` (ping con Token), `TestCheckWithCredentials` (login + ping, prefijo y orden del body), `TestGetCurrentSessionsShapeAndFilters` (JSON byte-exacto: ticks→ms, bitrateKbps 0 conservado, lan/wan, containerChanged por PlayMethod, filtros homarr/sin UserId), `TestGetCurrentSessionsShowOnlyPlaying`, `TestGetCurrentSessionsPausedAndTriState` (episodeName null vs ausente), `TestSessionsTransportErrorClassified`, `TestSessionsBadStatus`, `TestLoginFailuresCollapseToPinnedMessage`, `TestSecretsFromMapPicksEitherSet`, `TestConcurrentLoginsShareOneRequest` (dedupe en vuelo), `TestParseLocation` (casos del spec upstream incl. ::ffff) | auth doble, mapeo, clasificación |
| `emby` | `TestCheckOnline`, `TestCheckNon2xxIsDefinitive`, `TestGetCurrentSessionsShape` (headers dobles, zod nullish, currentlyPlaying null, transcoding completo, ejemplo.com ⇒ location null), `TestGetCurrentSessionsBadStatus` / `BadJSON` (mensajes EXACTOS "Emby server …"), `TestGetCurrentSessionsMissingSecret` | headers + mapeo + mensajes |

## 6. Desviaciones documentadas

1. **Mensaje del ParseError de plex**: upstream "Invalid xml format"
   (plex-integration.ts:480); el port usa el mensaje de taxonomía
   compartido — nunca llega al wire (queries ⇒ INTEGRATION_REQUEST_FAILED).
2. **Atributos XML presentes pero vacíos**: el port trata `""` como ausente
   en las cadenas `??` de plex (id/title del User, product/title del
   Player). Plex no emite atributos vacíos para esos campos.
3. **Errores de transporte de las mutaciones** (no hay mutaciones en este
   dominio) y textos de axios/undici: la FORMA `{sessions:[],
   error: público}` es el contrato, el string interno es libre
   (INTEGRATIONS.md §5.6).
4. **`sessionName` con Client/DeviceName nullish**: upstream renderizaría
   "undefined (undefined)"; el port `"" ()` — caso no alcanzable con
   servidores reales.
5. **pendingAuthentications por instancia** en vez de módulo: mismo
   alcance efectivo (la identidad incluye url+usuario+password).

## 7. Preguntas abiertas para el integrador

1. **`mediaReleases`**: el widget de estrenos consume
   `IMediaReleasesIntegration` de estas MISMAS integraciones
   (`widget.mediaReleases.getMediaReleases`, media-release.ts + request
   handler con merge de episodios e image proxy). Portarlo es un paquete
   hermano (media-release) con la misma mecánica; los clientes plex/
   jellyfin/emby añadirían el método `GetMediaReleases`.
2. **`navidrome`**: mismo interface (IMediaServerIntegration) — portar el
   cliente en M4 y añadir el caso a `DefaultMediaServerFactory` (el kind
   ya está en `SupportedKinds("mediaServer")`).
3. **`mock`**: el kind demo necesita un cliente mock para DEMO_MODE
   (upstream: packages/integrations/src/mock/data/media-server.ts, 5
   sesiones fijas).
4. **TLS verificado contra certificados confiables**: mismas costuras que
   MEDIA_ORGANIZER.md §8.2 (Options/HTTP del dominio mediaorganizer se
   reutiliza; el transporte usa las raíces del sistema).
5. **Check subtipos**: los TestConnectionError upstream (statusCode/
   authorization/request con metadata) llegan con el port del servicio
   test-connection; hoy `Check` devuelve la unión {statusCode}/{error} del
   framework.
