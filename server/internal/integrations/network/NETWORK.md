# NETWORK.md — dominio network / dns-hole (`internal/integrations/network`)

Documento de referencia del port en Go de la abstracción compartida
dns-hole de upstream (`packages/integrations/src/interfaces/dns-hole-summary`
+ los clientes por app de `packages/integrations/src/{pi-hole,adguard-home}`):
el resumen de bloqueo DNS y los controles enable/disable que consumen los
widgets dns-hole vía el router tRPC `widget.dnsHole.*` (portado en
`internal/api/router/widgets`, ver WIDGETS.md). Todas las afirmaciones citan
fuente upstream; las desviaciones están marcadas. Fecha: 2026-11-07.

Fuentes citadas (todas en este repo):

- **[iface]** `packages/integrations/src/interfaces/dns-hole-summary/
  dns-hole-summary-integration.ts:3-7` + `dns-hole-summary-types.ts:1-7`.
- **[factory]** `packages/integrations/src/pi-hole/
  pi-hole-integration-factory.ts:8-30` (sondeo de versión).
- **[v5]** `packages/integrations/src/pi-hole/v5/pi-hole-integration-v5.ts`
  + `pi-hole-schemas-v5.ts`.
- **[v6]** `packages/integrations/src/pi-hole/v6/pi-hole-integration-v6.ts`
  + `pi-hole-schemas-v6.ts` + `packages/integrations/src/base/
  session-store.ts`.
- **[adguard]** `packages/integrations/src/adguard-home/
  adguard-home-integration.ts` + `adguard-home-types.ts`.
- **[resp-err]** `packages/common/src/errors/http/response-error.ts:3-14`
  ("Response did not indicate success").
- **[widget]** `packages/api/src/router/widgets/dns-hole.ts:28-97` +
  `packages/request-handler/src/dns-hole.ts:7-17` (caché 10 s por
  DEFAULT_TTL_MS) + `packages/api/src/settle-integrations.ts`.
- **[mw-one]** `packages/api/src/middlewares/integration.ts:59-127`
  (middleware de UNA integración), **[defs]**
  `packages/definitions/src/integration.ts` (kinds `piHole`/`adGuardHome`/
  `technitiumDns`, secretKinds) + `packages/definitions/src/
  widget-integration-map.ts:128-129`.

## 1. La interfaz y el tipo wire

[iface] — resumen + controles:

```go
type Client interface {
    GetSummary(ctx context.Context) (DnsHoleSummary, error)
    Enable(ctx context.Context) error
    Disable(ctx context.Context, duration *DisableDurationSeconds) error
}
```

`DnsHoleSummary` (dns-hole-summary-types.ts): `status` es OPCIONAL —
pi-hole v6 mapea los estados "failed"/"unknown" de /api/dns/blocking a
undefined (la clave se descarta en superjson) y v5/adguard siempre lo
ponen ("enabled"/"disabled"); el resto son números siempre presentes:
`domainsBeingBlocked`, `adsBlockedToday`, `adsBlockedTodayPercentage`,
`dnsQueriesToday`. El port usa `*string` con `omitempty` para el status y
`float64` para los números.

`Disable` recibe SEGUNDOS en los tres clientes (upstream `duration?:
number`): v5 `?disable=<s>`, v6 `{"timer": <s>}`, adguard multiplica a ms
(`{"duration": <s*1000>}`). nil = desactivación permanente.

## 2. Clientes por app

| Kind upstream | Secretos ([defs] secretKinds) | Puerto | Versiones |
|---|---|---|---|
| `piHole` | `[["apiKey"], []]` — el password es OPCIONAL | `network/pihole` | v5 Y v6 detrás de un sondeo |
| `adGuardHome` | `[["username","password"]]` (HTTP Basic) | `network/adguardhome` | una |
| `technitiumDns` | — | **no portado** (categoría dnsHole upstream; el router de widgets lo marca como fallo por integración) | — |

### 2.1 Pi-hole — un kind, dos versiones ([factory], [v5], [v6])

**Sondeo de versión** (pi-hole-integration-factory.ts:8-30, en la
CONSTRUCCIÓN): `GET {base}/api/info/login` — 404 ⇒ v5 (la api vivía en
`/admin/api.php`), otro estado ⇒ v6 (la api está en `/api`); un servidor
inalcanzable cae a v6 para que el error salga de la llamada. El port lo
implementa en `pihole.New(ctx, input, secrets)` / `NewFromRef(ctx, ref)`
— la factory de widgets (`Options.NewNetwork`) recibe el ctx del request
por eso (la factory upstream es async). `Version()` expone 5/6 para tests.

**v5** (pi-hole-integration-v5.ts):

- **Auth**: el password viaja como el parámetro de QUERY `auth` en TODO
  (getHttpAuthenticationAsync :15-18). Sin apiKey ⇒ sin parámetro (las
  stats de pi-hole 5 son públicas sin password).
- **Summary** (:20-36): `GET /admin/api.php?summaryRaw=&auth=<key>`
  (la serialización `searchParams` convierte el flag en `summaryRaw=` —
  mismo resultado que upstream) → status/ads_blocked_today/
  ads_percentage_today/domains_being_blocked/dns_queries_today.
- **Check** (:38-53): `GET /admin/api.php?status=&auth=...`; un body 2xx
  que NO es objeto (v5 responde un ARRAY vacío con la key errónea) es el
  resultado 401.
- **enable/disable** (:55-74): `GET /admin/api.php?enable=` /
  `?disable[=<segundos>]` — OJO: `duration ? \`=${duration}\` : ""` —
  0 y undefined desactivan SIN valor. Fallos ⇒ Error plano
  `Failed to enable/disable PiHole for <name> (<id>): <status>` cuyo
  MENSAJE llega al wire por las mutaciones (el port lo conserva verbatim;
  `<status>` es http.StatusText, el equivalente de response.statusText).

**v6** (pi-hole-integration-v6.ts):

- **Auth por sesiones** (:28-46, :142-225): `POST /api/auth` con body
  `{"password": <apiKey ?? "">}` y `User-Agent: Homarr` responde
  `{session: {valid, sid, message}}` (schema :3-9). Una sesión INVÁLIDA
  lanza la ResponseError común (401). Las sesiones se REUTILIZAN vía el
  session store (upstream: canal Redis get/set con fingerprint de la
  configuración; el port: memoria por instancia — mismo proceso) y
  `withAuthAsync` (:142-163) re-loguea UNA vez cuando un request responde
  401. Reutilizar en vez de loguear por request evita agotar los slots de
  `webserver.api.max_sessions` (:30-33).
- Cada llamada autenticada lleva el header CUSTOM `sid` (ausente sin
  sesión — `sid ?? undefined`; una sesión válida con sid null, pi-hole sin
  password, tampoco lo envía).
- **Summary** (:48-96, SECUENCIAL): primero `GET /api/stats/summary`
  (queries.total/blocked/percent_blocked + gravity.
  domains_being_blocked) y después `GET /api/dns/blocking`
  (`blocking`: enabled/disabled/failed/unknown + timer). La transform del
  schema (:11-18) descarta "failed"/"unknown" ⇒ `status` ausente.
- **Check** (:98-103): login → `DELETE /api/auth` con el header sid
  (borra la sesión del servidor; fallos solo se logean) → limpia el store.
- **enable/disable** (:105-135): `POST /api/dns/blocking` con
  `{"blocking":true}` / `{"blocking":false[,"timer":<s>]}` — el timer se
  DESCARTA con undefined (JSON.stringify). Fallos ⇒ la ResponseError
  común de @homarr/common/server: **"Response did not indicate success"**
  ([resp-err]:8) — NO es la IntegrationResponseError de la taxonomía
  ("Response from integration did not indicate success"); el port la emite
  como error plano con ese texto porque las mutaciones lo publican en el
  wire.

### 2.2 AdGuard Home ([adguard])

- **Auth**: HTTP BASIC con username/password
  (`Authorization: Basic <base64(user:pass)>`, :146-150).
- **Summary** (:18-92): TRES GET en paralelo (Promise.all): `/control/stats`,
  `/control/status`, `/control/filtering/status`. Errores con mensajes
  EXACTOS por recurso: `Failed to fetch stats/status/filtering status for
  <name> (<id>): <statusText>`; parseo fallido ⇒ `Failed to parse summary
  for <name> (<id>)` (Error plano; la query lo sustituye por el público).
  - "today" (:66-73): con `time_units: "days"` vale la ÚLTIMA entrada de
    `blocked_filtering`/`dns_queries` (`[length-1] ?? 0`); con `"hours"` se
    SUMAN los arrays.
  - `domainsBeingBlocked`: suma de `rules_count` de los filtros con
    `enabled: true` (0 si `filters` es null, :74-79).
  - porcentaje: `blocked/queries*100` solo si `queries > 0`, si no 0.
  - `status`: `protection_enabled ? "enabled" : "disabled"` (:86).
- **Check** (:94-107): `GET /control/status`; un body JSON OBJETO es
  éxito (ParseResult en otro caso).
- **enable/disable** (:109-144): `POST /control/protection` con
  `Content-Type: application/json` y `{"enabled":true}` /
  `{"enabled":false,"duration":<s*1000>}` (duración default 0 =
  permanente). Fallos ⇒ Error plano `Failed to enable/disable AdGuard Home
  for <name> (<id>): <statusText>` — mensaje al wire.

## 3. Flujo de datos y errores

[widget] tres procedimientos montados en el sub-router `dnsHole`:

- **`widget.dnsHole.summary`** (query, publicProcedure + middleware
  "query"/"dnsHoleSummary", SIN input de procedimiento): salida por
  integración

  ```jsonc
  // éxito — updatedAt es el timestamp de la caché (ISO-ms con Z,
  // convención del port)
  {"integrationId":"…","integrationName":"…",
   "integration":{"id":"…","name":"…","kind":"piHole","updatedAt":"…"},
   "summary":{...}}
  // fallo de ESA integración — summary null, integration SIN updatedAt
  {"integrationId":"…","integrationName":"…",
   "integration":{"id":"…","name":"…","kind":"piHole"},
   "summary":null,"error":"INTEGRATION_REQUEST_FAILED"}
  ```

  `throwOnAllFailures: true` (500 "All integration queries failed");
  `integrationIds: []` ⇒ `[]`. Caché de **10 s** (DEFAULT_TTL_MS,
  dns-hole.ts no fija `cacheTtlMs`), single-flight, sin stale; clave
  `<integrationId> + hash({})`.

- **`widget.dnsHole.enable`** (mutation, protectedProcedure +
  `createOneWidgetIntegrationMiddleware("interact", "dnsHoleControls")`):
  input `{integrationId: string}` — UNA integración. Ejecuta
  `enableAsync` y DESPUÉS invalida la caché (dns-hole.ts:72-75 — el
  invalidate va tras la llamada, un fallo NO invalida). El port sigue la
  convención de gates de las mutaciones del paquete (admin.Writable +
  RequireSignedIn) aunque upstream encadena solo protectedProcedure +
  middleware (ver WIDGETS.md §1).

- **`widget.dnsHole.disable`** (mutation, mismos gates): input
  `{integrationId: string, duration?: number}` (segundos, opcional —
  z.number().optional(): presente y no-número ⇒ BAD_REQUEST).

**Errores de mutación**: upstream lanza el error crudo del cliente y
getTRPCErrorFromUnknown lo publica como INTERNAL_SERVER_ERROR con su
mensaje. El port conserva los mensajes EXACTOS: los Error planos de v5/
adguard ("Failed to enable PiHole for …") y la ResponseError de v6
("Response did not indicate success") viajan verbatim; los fallos de
transporte de los clientes usan el mensaje de taxonomía
("Request to integration failed").

**El middleware de UNA integración** ([mw-one]) tiene mensaje NOT_FOUND
PROPIO: `Integration with id <id> not found or not of kinds <kinds>` (el
many: "<n> of the specified integrations not found…"). El port lo modela
con `ResolveRequest.Cardinality` ("one"/"many") que el resolver real (M4)
debe respetar; con resolver nil el router genera el mensaje correcto.

**Sin eventos ni WS**: apps/websocket no tiene canal dns-hole; el frontend
polléa el summary e invalida tras las mutaciones
(packages/widgets/src/dns-hole/*). Este port no añade topics de bus.

## 4. Registro en el framework

- `network/pihole` se auto-registra con el kind `piHole` (mayúscula H) —
  la factory del registry construye con ctx Background y sin secretos, así
  que el sondeo corre a Background y el Check reporta el fallo real del
  servidor.
- `network/adguardhome` con `adGuardHome`. Ambos `Check` siguen la
  convención del framework ({statusCode}/{error} con los mensajes de
  taxonomía; los subtipos de test-connection llegan con ese servicio).
- Los clientes reales se construyen vía `widgets.Options.NewNetwork`
  (`DefaultNetworkFactory`, options.go): piHole ⇒ sondeo v5/v6 con el ctx
  del request; adGuardHome ⇒ directo. `technitiumDns`/`mock` fallan por
  integración (fallback público de la query) hasta M4.

## 5. Cobertura de tests (httptest, fixtures byte-exactos)

| Paquete | Tests | Cubre |
|---|---|---|
| `pihole` | `TestProbeSelectsV5On404` + `TestProbeDefaultsToV6OnTransportError` (sondeo del factory), `TestV5SummaryQueryAndAuth` (auth en la QUERY, `summaryRaw=`), `TestV5CheckWrongKeyReturns401` (array ⇒ 401), `TestV5EnableDisable` (enable/disable=300/disable sin valor), `TestV5EnableFailureMessage` (mensaje EXACTO con statusText), `TestV5StatusDroppedOnUnknown`, `TestV6LoginAndSummary` (POST /api/auth + UA Homarr + header sid + orden stats→blocking), `TestV6SessionReuseAndReLogin` (una sesión para dos llamadas, 401 ⇒ UN re-login), `TestV6FailedBlockingDropsStatus` (failed ⇒ status ausente), `TestV6MutationsAndWireMessages` (bodies POST + timer descartado), `TestV6MutationFailureMessage` ("Response did not indicate success"), `TestV6InvalidPassword`, `TestV6CheckClearsServerSession` (DELETE con sid), `TestV6Passwordless` (sin apiKey no hay login) | ambos flujos completos |
| `adguardhome` | `TestNewFromRefMissingSecret`, `TestCheckOnline` / `TestCheckNonObjectBodyIsParseResult` / `TestCheckBadStatusIsDefinitive`, `TestSummaryDays` (última entrada, filtros enabled, porcentaje), `TestSummaryHours` (suma, filters null, disabled), `TestSummaryZeroQueries`, `TestSummaryFetchFailureMessage` (mensaje EXACTO), `TestEnableDisable` (bodies + Basic + Content-Type), `TestEnableFailureMessage` / `TestDisableFailureMessage`, `TestJSONEncoding` (wire de DnsHoleSummary) | auth Basic, matemática de stats, mutaciones |

## 6. Desviaciones documentadas

1. **Session store en memoria** por instancia (upstream: canal Redis
   cifrado con fingerprint de configuración que sobrevive reinicios del
   proceso). Un solo proceso ⇒ misma semántica; la sesión se recrea ante
   401, así que una pérdida solo cuesta un login extra.
2. **`statusText` de los mensajes de mutación**: upstream usa
   `response.statusText` (reason phrase del servidor, "" en HTTP/2); el
   port usa `http.StatusText` del código. Para los códigos estándar el
   texto coincide.
3. **Fallo de transporte en mutaciones**: upstream propagaría el error
   crudo de fetch/undici ("fetch failed"); el port lo clasifica como
   request error ("Request to integration failed") — mismo tratamiento
   que el resto del port (INTEGRATIONS.md §5.6).
4. **Registry factory con ctx Background**: el sondeo v5/v6 del registro
   automático (sin request) corre a Background; el sondeo real con el ctx
   del request ocurre en `DefaultNetworkFactory`.
5. **Parseo de /control/stats de adguard**: el port decodifica solo los
   campos que consume (time_units, dns_queries, blocked_filtering) — un
   body que omitiera los arrays no usados fallaría el zod upstream pero
   pasaría aquí (irrelevante: los tres campos usados están validados por
   uso).

## 7. Preguntas abiertas para el integrador

1. **Resolver DB-backed con cardinalidad**: el resolver real (M4) debe
   respetar `ResolveRequest.Cardinality` — "one" usa el mensaje
   `Integration with id %s not found or not of kinds %s`
   (middlewares/integration.ts:113-117).
2. **`technitiumDns`**: mismo interface (DnsHoleSummaryIntegration con
   login por apiKey o usuario/contraseña); portarlo añade subpaquete +
   caso en `DefaultNetworkFactory` (el kind ya está en
   `SupportedKinds("dnsHoleSummary"/"dnsHoleControls")`).
3. **`mock`**: el kind demo necesita un cliente mock para DEMO_MODE
   (upstream: packages/integrations/src/mock DnsHoleMockService).
4. **Persistencia de la sesión pi-hole v6**: si se quisiera sobrevivir
   reinicios (como el store Redis upstream), la costura natural es la
   tabla/cache del proceso; hoy cada arranque re-loguea.
5. **TLS verificado contra certificados confiables**: mismas costuras que
   MEDIA_ORGANIZER.md §8.2 (transporte con raíces del sistema detrás de
   `mediaorganizer.HTTPClient`).
