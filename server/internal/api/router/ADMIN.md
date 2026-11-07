# ADMIN.md — routers tRPC de administración (user, group, invite, onboard, serverSettings, searchEngine, bangs, apiKeys, info)

Documento de referencia del port en Go de los 9 routers tRPC de
administración de upstream (`packages/api/src/root.ts:7,9,10,20,24,26,30,32` +
`bangs`). Complementa `PROTOCOL.md` (contrato del wire), `SCHEMA.md`
(esquema SQLite) y `BOARDS.md` (routers del dominio de boards, que comparte
convenciones con este paquete). Todas las afirmaciones citan fuente
upstream; las desviaciones están marcadas. Fecha: 2026-11-06.

Fuentes citadas (todas en este repo):

- **[user]** `packages/api/src/router/user.ts` + `router/user/preferences.ts`.
- **[group]** `packages/api/src/router/group.ts`.
- **[invite]** `packages/api/src/router/invite.ts` + `router/invite/checks.ts`.
- **[onboard]** `packages/api/src/router/onboard/onboard-router.ts` + `onboard-queries.ts`, **[claim]** `packages/api/src/onboarding-claim.ts`, **[claim-rest]** `apps/nextjs/src/app/api/onboarding/claim/route.ts`.
- **[settings]** `packages/api/src/router/serverSettings.ts`, **[ss-def]** `packages/server-settings/src/index.ts`, **[ss-q]** `packages/db/queries/server-setting.ts` (+ `queries/group.ts`).
- **[se]** `packages/api/src/router/search-engine/search-engine-router.ts`.
- **[bangs]** `packages/api/src/router/bangs/bangs-router.ts` + `packages/api/src/services/duckduckgo-bangs.ts` + `packages/request-handler/src/duckduckgo-bangs.ts` + `packages/request-handler/src/lib/request-handler.ts`.
- **[keys]** `packages/api/src/router/apiKeys.ts`.
- **[info]** `packages/api/src/router/info.ts`.
- **[cred-access]** `packages/api/src/credential-access.ts`, **[resp-err]** `packages/common/src/errors/http/response-error.ts`, **[ddg-timeout]** `packages/core/src/infrastructure/http/timeout.ts`.
- **[val-user]** `packages/validation/src/user.ts`, **[val-group]** `group.ts`, **[val-se]** `search-engine.ts`, **[val-common]** `common.ts`, **[val-onb]** `onboarding.ts`, **[val-hdr]** `header-preferences.ts`, **[val-email]** `email.ts`.
- **[def]** `packages/definitions/src/{permissions,group,onboarding,user}.ts`.
- **[auth]** `AUTH.md` (bcrypt cost 10, formato de API keys), **[schema]** `SCHEMA.md`, **[trpc]** `../trpc/trpc.go` + `superjson.go`.

## 1. Registro

```go
r := trpc.New()
user.Register(r, database, user.Options{})          // "user.*"        (26)
group.Register(r, database, group.Options{})        // "group.*"       (15)
invite.Register(r, database, invite.Options{})      // "invite.*"      (3)
onboard.Register(r, database, onboard.Options{})    // "onboard.*"     (2 + 4 diferidos)
serversettings.Register(r, database, serversettings.Options{}) // "serverSettings.*" (6)
searchengine.Register(r, database, searchengine.Options{})     // "searchEngine.*"   (9)
bangs.Register(r, database, bangs.Options{})        // "bangs.*"       (1)
apikeys.Register(r, database, apikeys.Options{})    // "apiKeys.*"     (5)
info.Register(r, database, info.Options{})          // "info.*"        (3)
// chi: r.Handle("/api/trpc/*", r.Handler())
```

Además del tRPC, el endpoint REST del claim de onboarding se monta junto a
los demás (`onboard.ClaimHandler`, §3.5):

```go
router.Handle("/api/onboarding/claim", auth.Middleware()(onboard.ClaimHandler(database)))
```

Los paths son los punteados exactos de [root] `packages/api/src/root.ts`
(camelCase: `serverSettings.*`, `searchEngine.*`, `apiKeys.*`). La firma
`Register(r *trpc.Router, database *db.DB, opts Options)` es uniforme; el
pánico por path duplicado lo emite `trpc.Router.Register`.

`Options`:

- `user.Options{DemoMode, OnboardingClaimTokenFromContext}` — `DemoMode`
  replica `env.DEMO_MODE` (upstream compila las ramas demo con el env; el
  fork lo lleva a 0). `OnboardingClaimTokenFromContext` resuelve el token
  del claim de onboarding (cookie `homarr-onboarding-claim`, [claim]
  onboarding-claim.ts:7) que el middleware de httpserver deberá extraer del
  request; nil/vacío = "sin claim".
- `group.Options{OnboardingClaimTokenFromContext}` e
  `onboard.Options{OnboardingClaimTokenFromContext}` — ídem para
  `group.createInitialExternalGroup` y `onboard.nextStep`.
- `bangs.Options{CatalogURL, CacheTTL}` — `CatalogURL` sobreescribe
  `https://duckduckgo.com/bang.js` (`bangs.DefaultCatalogURL`,
  duckduckgo-bangs.ts:11; costura de test); `CacheTTL` sobreescribe los
  10s del request-handler (costura de test, §3.8).
- `info.Options{Version, DemoMode, DemoReadOnly}` — `Version` sobreescribe
  la versión reportada (default `"2.11.1"`, el `version` del package.json
  raíz); `isDemoReadOnly` = `DemoMode && DemoReadOnly` ([info]
  trpc.ts:118).

## 2. Modelo de permisos

Idéntico al del paquete board (BOARDS.md §2): `admin` = expansión del
permiso de grupo `admin` ≙ `ctxauth.Session.IsAdministrator`; los helpers
viven en el paquete compartido `router/internal/admin` (`Caller`,
`ResolveCaller`, `RequireSignedIn`, `RequirePermission`, `ExpandPermissions`,
`CanViewBoard`, `AccessibleBoardIDs`).

| Gate upstream ([trpc] packages/api/src/trpc.ts) | Error del port |
|---|---|
| `protectedProcedure` (anónimo) | `UNAUTHORIZED`, mensaje `"UNAUTHORIZED"` (fallback tRPC v11 al código) |
| `permissionRequiredProcedure.requiresPermission(p)` sin el permiso | `FORBIDDEN "Permission denied"` |
| `onboardingClaimedProcedure` sin admin ni claim válido | `FORBIDDEN "This onboarding session is not claimed."` |
| `onboardingProcedure.requiresStep(s)` con otro paso | `FORBIDDEN "Step denied"` |
| mutación con API key `readOnly` (trpc.ts:130-135) | `FORBIDDEN "This credential only permits read-only operations"` |

⚠️ `permissionRequiredProcedure` extiende `protectedProcedure`
(trpc.ts:195-207): un anónimo recibe **UNAUTHORIZED**, no FORBIDDEN.

### 2.1 API keys read-only (credencial de solo lectura)

`credentialProcedure` ([trpc] trpc.ts:130-135) es la base de TODOS los
procedimientos upstream (public/protected/permissionRequired/
onboardingClaimed/internal): una sesión con `apiKey.readOnly` puede ejecutar
queries pero **toda mutación** responde
`FORBIDDEN` con mensaje fijo — el mensaje exacto es
**"This credential only permits read-only operations"** (trpc.ts:132). La
condición `isReadOnlyProcedure` ([cred-access] credential-access.ts:5-6) es
`type === "query" && meta.readOnly !== false`: mutaciones siempre rechazadas;
ningún router de este dominio declara `meta.readOnly === false` (solo existen
en icons/custom-widget/integration/widgets upstream).

Port en `internal/admin/readonly.go`: `admin.RequireWritable(c Caller)` es el
helper canónico y `admin.Writable(proc)` la costura de registro — envuelve la
mutación en `Register` para que el rechazo conserve el orden upstream
(middleware base: ANTES de los gates de permisos y de la validación de
entrada; una mutación con cuerpo inválido y key read-only responde el error
read-only, no BAD_REQUEST). Solo mira `ctxauth.Session.ReadOnly` (sin SQL):
el middleware de auth lo rellena al resolver la API key (AUTH.md §6).

Mutaciones con el guard aplicado (40 en total):

- **user** (20): `initUser`, `register`, `create`, `setProfileImage`,
  `editProfile`, `delete`, `changePassword`, `updatePreferences`,
  `changeHomeBoards`, `changeDefaultSearchEngine`, `changeSearchPreferences`,
  `changeColorScheme`, `changeByteUnitSystem`,
  `changeEnableRightClickOnWidgets`, `changePingIconsEnabled`,
  `changeHeaderPreferences`, `changeDdgBangs`, `changeFirstDayOfWeek`,
  `completeTour`, `resetTours`. Lecturas sin guard: `getAll`, `selectable`,
  `search`, `getPreferences`, `getById`, `getTourStatus`.
- **group** (10): `createInitialExternalGroup`, `createGroup`,
  `updateGroup`, `savePartialSettings`, `savePositions`, `savePermissions`,
  `transferOwnership`, `deleteGroup`, `addMember`, `removeMember`.
- **invite** (2): `createInvite`, `deleteInvite` (`getAll` es lectura).
- **serverSettings** (2): `updateBoardSettings`, `saveSettings`.
- **searchEngine** (3): `create`, `update`, `delete`.
- **apiKeys** (3): `revokeOwnOAuth`, `create`, `delete`.
- **onboard** (1): `nextStep`.

⚠️ **Pendiente para el integrador**: el paquete `board/**` (otro agente lo
está portando en paralelo) necesita el mismo guard en sus mutaciones —
envolver con `admin.Writable` las que upstream declara `.mutation(...)`
(crear/guardar boards, secciones, items, posiciones, etc.). Hasta entonces
una API key read-only puede mutar boards a través de esos procedimientos.

Checks propios de procedimientos (replicados uno a uno):

- **self-or-admin** (`user.getById/setProfileImage/editProfile/delete`):
  `FORBIDDEN` con el mensaje exacto de cada uno ("You are not allowed to
  view/edit other users details", "…change other users profile images",
  "…delete other users").
- **other-user hidden** (`user.changePassword`, `getPreferences`,
  `updatePreferences` y familia change*): otro usuario sin admin responde
  **NOT_FOUND "User not found"** (user.ts:616-621, preferences.ts:27-29).
- **reserved group "everyone"** ([group] group.ts:420-429):
  `FORBIDDEN "Action is forbidden for reserved group names"` en
  update/delete/transfer/addMember/removeMember sobre el grupo whose name =
  `everyone` ([def] group.ts:1). `groupCreateSchema` además rechaza el
  nombre en la validación (val-group group.ts:14-16 → BAD_REQUEST).
- **similar group name** ([group] group.ts:407-418): `CONFLICT "Found
  group with similar name"` — LIKE con el nombre crudo como patrón (SQLite
  LIKE: ASCII case-insensitive; los `%`/`_` del nombre actúan como
  comodines igual que upstream).
- **guard de board view** (`user.changeHomeBoards` /
  `updatePreferences` → `throwIfActionForbiddenAsync(..., "view")`):
  NOT_FOUND "Board not found" para board inexistente o sin acceso
  (anti-snooping, board-access.ts:45-64) — portado en
  `admin.CanViewBoard`.
- **home boards públicas** (`serverSettings.updateBoardSettings`): los ids
  no públicos (o inexistentes) producen
  `BAD_REQUEST "Board settings home board IDs must reference public
  boards: <ids>"` ([settings] serverSettings.ts:54-69).

## 3. Inventario de procedimientos

**Totales del dominio: 74 upstream — 70 portados, 4 diferidos** (+ el
endpoint REST `POST /api/onboarding/claim`, no-tRPC, portado en §3.5).

### user ([user] user.ts — 26 procedimientos, todos portados)

| Procedimiento | Línea | Permiso | Entrada (zod → JSON) | Salida |
|---|---|---|---|---|
| `user.initUser` | 70 | onboarding paso `user` | username(3-255 trim+lower), password(8-255), confirmPassword==password, email? | void |
| `user.register` | 130 | público | ídem + inviteId, token | void |
| `user.create` | 194 | `admin` | ídem + groupIds[] | void |
| `user.setProfileImage` | 225 | firmado, self-or-admin | `{userId, image: dataURL|null ≤350000}` | void |
| `user.getAll` | 280 | `admin` | void | `[{id,name,email,emailVerified,image}]` |
| `user.selectable` | 318 | firmado | `{providers?}` | `[{id,name,image,email}]` |
| `user.search` | 352 | `admin` | `{query, limit=10 (1-100)}` | ídem (name `""` si null) |
| `user.getPreferences` | 399 | firmado (otro user ⇒ admin, si no NOT_FOUND) | `{userId?}` | preferencias + userId + headerPreferences parseado |
| `user.getById` | 419 | firmado, self-or-admin | `{userId}` | perfil completo (18 campos) |
| `user.editProfile` | 498 | firmado, self-or-admin | `{id, name, email?}` | void |
| `user.delete` | 559 | firmado, self-or-admin | `{userId}` | void |
| `user.changePassword` | 592 | firmado (otro user ⇒ admin, si no NOT_FOUND) | `{previousPassword, password, confirmPassword, userId}` | void |
| `user.updatePreferences` | 674 | firmado | strictObject, ≥1 preferencia | preferencias completas |
| `user.changeHomeBoards` | 705 | firmado | `{homeBoardId?, mobileHomeBoardId?, userId}` | void |
| `user.changeDefaultSearchEngine` | 725 (deprecated) | firmado | `{defaultSearchEngineId?, ddgBangsEnabled, userId}` | void |
| `user.changeSearchPreferences` | 750 | firmado | `{defaultSearchEngineId?, openInNewTab, ddgBangsEnabled, userId}` | void |
| `user.changeColorScheme` | 771 | firmado (self) | `{colorScheme: auto\|light\|dark}` | void |
| `user.changeByteUnitSystem` | 787 | firmado | `{byteUnitSystem: binary\|decimal, id}` | void |
| `user.changeEnableRightClickOnWidgets` | 793 | firmado | `{enableRightClickOnWidgets, id}` | void |
| `user.changePingIconsEnabled` | 810 | firmado | `{pingIconsEnabled, id}` | void |
| `user.changeHeaderPreferences` | 815 | firmado | `{id, headerPreferences (v1/v2/v3)}` | void |
| `user.changeDdgBangs` | 832 (deprecated) | firmado | `{ddgBangs, id}` | void |
| `user.changeFirstDayOfWeek` | 850 | firmado | `{firstDayOfWeek (0-6), id}` | void |
| `user.completeTour` | 867 | firmado | `{tour: manage\|board}` | void |
| `user.resetTours` | 877 | firmado | — | void |
| `user.getTourStatus` | 886 | firmado | — | `{completedManageTour, completedBoardTour}` |

Notas del dominio user:

1. **changePassword** (user.ts:647-664): cambiar la propia exige
   `previousPassword` verificado con **bcrypt cost 10**
   (`auth.ComparePassword`, AUTH.md §1); un admin resetea la de otro sin
   previous. Hash nuevo con `auth.HashPassword` (`$2a$10$…`).
2. **initUser** (user.ts:95-128): transacción única — transición de
   onboarding `user → setup` (rowCount ≠ 1 ⇒ `CONFLICT "The initial
   administrator was already created."`), inserción del usuario, del grupo
   `credentials-admin` ([def] group.ts:2), de su permiso `admin` y de la
   membership. El fork no tiene ldap/oidc, así que `nextStep` es siempre
   `"setup"` (user.ts:93).
3. **register** (user.ts:130-193): la invitación (id + token + no
   expirada) se consume dentro de la transacción que inserta el usuario;
   cualquier fallo ⇒ `FORBIDDEN "Invalid invite"`. Token de invite en
   texto plano (randomBytes(20) hex, invite.ts:86).
4. **Preferencias** (preferences.ts:49-103): el patch se valida entero
   ANTES de la única escritura: merge de headerPreferences por zonas (una
   zona suministrada reemplaza su array), boards del header nuevos deben
   ser accesibles (`BAD_REQUEST "One or more board shortcuts are
   unavailable to this user"`), home boards con view check, default search
   engine existente (`BAD_REQUEST "Search engine not found"`).
5. **headerPreferences** (val-hdr header-preferences.ts): port completo —
   v3 estricta (zonas ≤100 items, items únicos, `user` obligatorio), v1/v2
   migradas en lectura (migrateLegacyHeaderPreferencesV1/V2),
   `includeRequiredHeaderItems` añade `user` a la zona right. La columna
   `user.header_preferences` se serializa con el mismo orden de claves que
   `JSON.stringify` upstream (structs Go en ese orden).

### group ([group] group.ts — 15 procedimientos, todos portados)

| Procedimiento | Línea | Permiso | Salida |
|---|---|---|---|
| `group.getAll` | 24 | `admin` | grupos + `members: [{id,name,email,image}]` |
| `group.getPaginated` | 48 | `admin` | `{items, totalCount}` (search trim LIKE, pageSize ≤100 default 10, page default 1) |
| `group.getById` | 83 | `admin` | grupo + members (+provider) + `permissions: [string]` + `owner: {id,name,image,email}|null`; NOT_FOUND "Group not found" |
| `group.selectable` | 133 | firmado | `[{id,name}]` / con `permissions` si admin y `withPermissions` |
| `group.search` | 160 | `admin` | `[{id,name}]` LIKE %q%, limit 1-100 default 10 |
| `group.createInitialExternalGroup` | 178 | onboarding paso `group` | void; `position = max+1` + permiso `admin`; CONFLICT si la transición ya ocurrió |
| `group.createGroup` | 224 | `admin` | id (string crudo); owner = caller |
| `group.updateGroup` | 242 | `admin` | void |
| `group.savePartialSettings` | 258 | `admin` | void — solo las claves suministradas (drizzle ignora undefined en `.set()`) |
| `group.savePositions` | 272 | `admin` | void — `position = index+1` en transacción |
| `group.savePermissions` | 295 | `admin` | void — reemplazo con dedup (`[...new Set]`); se almacenan tal cual (la expansión es de lectura) |
| `group.transferOwnership` | 339 | `admin` | void |
| `group.deleteGroup` | 353 | `admin` | void |
| `group.addMember` | 362 | `admin` | void — usuario inexistente NOT_FOUND "User not found"; provider no local FORBIDDEN "User's provider is not managed locally" |
| `group.removeMember` | 393 | `admin` | void — borrar un no-miembro es no-op |

`groupPermissionKeys` (val-group group.ts:33, [def] permissions.ts:111):
los 20 keys asignables (`board-*`, `app-*`, `integration-*`,
`search-engine-*`, `media-*`, `other-view-logs`, `admin`); un key
desconocido ⇒ BAD_REQUEST.

### invite ([invite] invite.ts — 3 procedimientos, todos portados)

| Procedimiento | Permiso | Entrada | Salida |
|---|---|---|---|
| `invite.getAll` | `admin` | — | `[{id, expirationDate, creator: {name, id}}]` ORDER BY expirationDate; **sin token** |
| `invite.createInvite` | `admin` | `{expirationDate: ISO con offset}` | `{id, token}` (token 40 hex, única vez) |
| `invite.deleteInvite` | `admin` | `{id}` | void; NOT_FOUND "Invite not found" |

`expirationDate` acepta `z.iso.datetime({offset: true})` con o sin
fracciones (precision -1) — Go: `time.Parse(time.RFC3339, …)`, sin offset ⇒
BAD_REQUEST.

### onboard ([onboard] onboard-router.ts — 2 portados, 4 diferidos)

| Procedimiento | Permiso | Salida | Estado |
|---|---|---|---|
| `onboard.currentStep` | público | `{current, previous\|null}` (normalizeOnboardingStep: desconocido ⇒ "start", settings/integrations ⇒ "setup") | ✅ port |
| `onboard.nextStep` | onboardingClaimedProcedure | void; `PRECONDITION_FAILED "The welcome step is already complete."` si ya no está en "start"; el siguiente paso se resuelve con la tabla user (sin usuarios ⇒ "user", si no "setup" — las ramas ldap/oidc son inalcanzables en el fork) | ✅ port |
| `onboard.testIntegration` | paso `setup` | — | ⏸ diferido (§6) |
| `onboard.detectRuntimeCapabilities` | paso `setup` | — | ⏸ diferido (§6) |
| `onboard.discoverDockerServices` | paso `setup` | — | ⏸ diferido (§6) |
| `onboard.completeSetup` | paso `setup` | — | ⏸ diferido (§6) |

El claim (`__onboarding_claim` en serverSetting, [claim]
onboarding-claim.ts:8): registro JSON crudo `{"hash": sha256hex(token),
"expiresAt": epochMs}` — verificación constant-time (subtle) con
`expiresAt > now`; administrador pasa siempre. La EMISIÓN vive en
`admin.ClaimOnboarding` (claimOnboardingAsync, onboarding-claim.ts:78-117,
§3.5) y el endpoint REST que la consume es `onboard.ClaimHandler`.

### 3.5 Endpoint REST del claim — `POST /api/onboarding/claim`

Port byte-exacto de [claim-rest] `route.ts` (53 líneas) en
`onboard/claimrest.go`: `ClaimHandler(database *db.DB) http.Handler`, que se
monta DETRÁS de `auth.Middleware()` (§1) y lee el caller con
`ctxauth.FromContext`. Flujo:

1. `Sec-Fetch-Site` presente y distinto de `same-origin`/`none` → **403**
   `{"code":"cross_site"}` (route.ts:14-17; callers sin el header pasan).
2. `force` = `session?.user.permissions.includes("admin")` (route.ts:22) ≙
   `ctxauth.Session.IsAdministrator` (el middleware de auth lo deriva del
   permiso de grupo `admin`; un anónimo fuerza `false`).
3. `claimOnboardingAsync(db, {currentToken: <cookie>,
   force})` — la cookie se extrae del header `Cookie` crudo con
   `getOnboardingClaimTokenFromCookieHeader` (onboarding-claim.ts:44-60:
   split `;`, primer `=`, trim, `decodeURIComponent`, escape inválido ⇒ sin
   token). Matriz de estados → HTTP:

   | Estado (claimOnboardingAsync) | HTTP | Body | Extra |
   |---|---|---|---|
   | `finished` (paso raw `finish`) | **409** | `{"code":"finished"}` | Set-Cookie que borra `homarr-onboarding-claim` |
   | `forbidden` (hay usuario y sin `force`) | **403** | `{"code":"administrator_required"}` | — |
   | `locked` (claim ajeno no expirado y sin `force`) | **423** | `{"code":"locked","expiresAt":epochMs}` | `Cache-Control: no-store` |
   | `issued` (nuevo token) / `active` (re-claim del mismo token) | **200** | `{"status":"issued"\|"active","expiresAt":epochMs}` | `Cache-Control: no-store` + Set-Cookie |
   | (default inalcanzable, route.ts:35-37) | **409** | `{"code":"unavailable"}` | — |

4. Set-Cookie de issued/active (route.ts:45-51):
   `homarr-onboarding-claim=<token>; Path=/; Expires=<expiresAt>; HttpOnly;
   SameSite=Strict` + `Secure` si `x-forwarded-proto` (primer valor,
   recortado) es `https` o el request llegó por TLS. Método distinto de POST
   → **405** (route.ts exporta solo POST). Los bodies `finished`/`forbidden`
   NO llevan `no-store` upstream; el port lo replica.

Semántica de `claimOnboardingAsync` (onboarding-claim.ts:78-117, portada en
`admin.ClaimOnboarding`): token = 32 bytes aleatorios base64url (solo su
sha256hex se almacena, con `expiresAt` = now + **4h**
`onboardingClaimTtlMs`); el propio token vigente del caller gana primero
(`active`), el lock (`locked`) y el `forbidden` ceden ante `force` del
administrador (que ROTA el claim); la fila previa se borra solo mientras
conserva el valor leído (borrado optimista, `db.DeleteServerSettingIfValue`)
y si el INSERT pierde la carrera de PK se relee la fila ganadora: no
expirada ⇒ `locked`, otro error ⇒ 500.

Desviaciones documentadas: (1) el Set-Cookie de borrado lleva `Path=/` —
Next `cookies.delete` no emite Path y por RFC 6265 no limpiaría la cookie
`Path=/`; el port emite la cookie vacía con `Expires` en el pasado, `Path=/`,
HttpOnly y SameSite=Strict; (2) montado detrás de `auth.Middleware()` una
cabecera `ApiKey` inválida responde 401 plano (upstream `auth()` no valida
API keys en esta ruta; behavioral igual para el SPA, que nunca manda esa
cabecera aquí); (3) un error interno del claim responde `500
{"code":"internal_error"}` JSON en vez de la página de error de Next.

### serverSettings ([settings] serverSettings.ts — 6 procedimientos, todos portados)

| Procedimiento | Permiso | Salida |
|---|---|---|
| `serverSettings.getCulture` | público | `{defaultLocale}` (merge sobre defaults) |
| `serverSettings.getAll` | `admin` | los 8 keys: analytics, crawlingAndIndexing, board, user, appearance, branding, culture, search ([ss-def] index.ts:6-15) |
| `serverSettings.getBranding` | público | parseBrandingSettings (defaults completos si algo no valida) |
| `serverSettings.getBoardSettings` | `admin` | `{homeBoardId, mobileHomeBoardId, enableStatusByDefault, forceDisableStatus}` |
| `serverSettings.updateBoardSettings` | `admin` | settings resultantes (merge parcial + validación de boards públicas) |
| `serverSettings.saveSettings` | `admin` | void — despacha por settingsKey: `board` (merge + validación), `branding` (merge + legacy keys), `analytics` (schema estricto: solo `enableGeneral`), resto merge genérico `{...current, ...value}` |

Defaults portados de [ss-def] index.ts:130-161 (analytics, crawlingAndIndexing,
board, user, appearance, branding, culture, search), incluidos
`defaultBrandingSettings` y la resolución legacy de authBranding
(`showCustom{AppName,Logo,Greeting}OnLogin` → `authBranding.show{AppName,
Logo,Greeting}`, index.ts:104-128). La validación de branding replica
brandingServerSettingsSchema: appName trim 1-80, greeting ≤160, URLs de
imagen HTTP(S) o path relativo ("/…" no "//…") ≤2048, colores `#hex6`,
overlay 0-0.9, defaultRadius xs..xl, customCss ≤16384; CUALQUIER fallo en
lectura devuelve los defaults íntegros (parseBrandingSettings).

### searchEngine ([se] search-engine-router.ts — 9 procedimientos, todos portados)

| Procedimiento | Línea | Permiso | Notas |
|---|---|---|---|
| `searchEngine.getPaginated` | 17 | firmado | `{search? (trim LIKE name), pageSize ≤100 default 10, page}` → `{items, totalCount}` |
| `searchEngine.getSelectable` | 32 | firmado | `[{value, label}]` ORDER BY name; `withIntegrations` default true |
| `searchEngine.byId` | 47 | firmado | NOT_FOUND "Search engine not found" |
| `searchEngine.getDefaultSearchEngine` | 73 | público | default del usuario → default del server (`search.defaultSearchEngineId`); anónimo no ve engines no-generic; default huérfano se limpia en lectura (UPDATE serverSetting) |
| `searchEngine.catalog` | 136 | público | anónimo solo `generic`; ORDER BY name; cada fila lleva `integration: {id, kind, url}|null` |
| `searchEngine.search` | 158 | público | LIKE `<q.trim().lower()%` sobre `short`; anónimo solo generic (#4132); limit 1-100 default 10 |
| `searchEngine.create` | 179 | `search-engine-create` | `short` se guarda lowercase; unión discriminada `generic` (urlTemplate min1, startsWith http, incluye `%s`) / `fromIntegration` (integrationId opcional) |
| `searchEngine.update` | 194 | `search-engine-modify-all` | NOT_FOUND; **no actualiza `short`** (edit schema lo omite) |
| `searchEngine.delete` | 221 | `search-engine-full-all` | limpia `user.default_search_engine_id` antes de borrar |

### bangs ([bangs] bangs-router.ts — 1 procedimiento, portado)

`bangs.search` (público, `{query, limit 1-50 default 20}`, query) es un
proxy del catálogo de bangs de DuckDuckGo
(`https://duckduckgo.com/bang.js`, ~13k entradas) — NO es un dataset
estático. Port en `bangs/register.go` (el stub vacío fue reemplazado;
firma `Register(r, db, Options)` intacta, la base de datos no se usa):

- **Forma de salida** (`duckDuckGoBangSchema`, duckduckgo-bangs.ts:23-31):
  array de `{t, s, d?, u, c?, sc?, r?}` — `t` token del bang, `s` nombre,
  `u` plantilla URL con `{{{s}}}`, y opcionales `d` (dominio), `c`
  (categoría), `sc` (subcategoría), `r` (rank numérico). Los opcionales se
  omiten cuando el origen no los trae (zod + superjson); claves desconocidas
  del origen se descartan. El spotlight solo consume `t`/`s`/`u`
  (search-engines-search-group.tsx:409-472).
- **Pipeline del fetch** (requestAsync, duckduckgo-bangs.ts:40-72): GET con
  `accept: application/json,text/plain,*/*`; timeout de **10s**
  (`withTimeoutAsync` default, [ddg-timeout] timeout.ts:6); respuesta no-2xx
  ⇒ error con el mensaje exacto de `ResponseError`
  **"Response did not indicate success"** ([resp-err]
  response-error.ts:13) que tRPC envuelve como `INTERNAL_SERVER_ERROR`
  (el mensaje viaja intacto por `getMessageFromUnknownError`); schema
  zod: `t/s/u` requeridos — una entrada inválida rechaza el catálogo entero
  (500); token normalizado a `lowercase+trim`, entradas con token vacío
  descartadas, orden estable ascendente por `t`.
- **Búsqueda** (`searchDuckDuckGoBangsAsync`, services/duckduckgo-bangs.ts:
  35-55): token de consulta normalizado igual; vacío ⇒ `[]`; búsqueda
  binaria del lower bound sobre el catálogo ordenado y recolección de los
  matches de prefijo consecutivos hasta `limit`.
- **Caché** (`createRequestHandler`, request-handler.ts): TTL in-memory de
  **10 segundos** — el handler upstream no fija `cacheTtlMs`, así que aplica
  `DEFAULT_TTL_MS = 10_000` (request-handler.ts:31); sin stale-if-error
  (`fallbackToStaleOnError` sin fijar ⇒ tras expirar, un fallo del upstream
  llega al caller y nada stale se sirve); dedup de fetches concurrentes
  (mapa inflight upstream, single-flight en el port); el fallo no se cachea
  (cada llamada fallida re-intenta).

Desviaciones documentadas: caché solo in-memory por proceso (upstream añade
opcionalmente una capa Redis compartida tras la misma interfaz; el fork es
un proceso único, ARCHITECTURE.md §2); orden byte-wise (`strings.Compare`)
en vez de JS `localeCompare` — sort y búsqueda usan el MISMO comparador, así
que la agrupación por prefijo de la búsqueda binaria se conserva y solo el
ORDEN de resultados alrededor de símbolos/mayúsculas puede diferir; un
payload schema-inválido produce un mensaje descriptivo en vez del dump de
issues de zod (ambos 500); `env.NO_EXTERNAL_CONNECTION` (devuelve `[]` en
upstream) no se porta; un `null` JSON en un campo opcional se trata como
ausente (upstream zod lo rechazaría; bang.js nunca emite nulls).

### apiKeys ([keys] apiKeys.ts — 5 procedimientos, todos portados)

| Procedimiento | Permiso | Notas |
|---|---|---|
| `apiKeys.getOwnOAuth` | firmado | claves propias con `oauth_client_id` IS NOT NULL |
| `apiKeys.revokeOwnOAuth` | firmado | delete id+user+OAuth (no-op silencioso) |
| `apiKeys.getAll` | `admin` | todas las claves + `user: {id, name, image, email}` |
| `apiKeys.create` | `admin` | `{readOnly?}` ⇒ `{apiKey: "<id>.<token>"}`; id = 8 hex (randomBytes(4)), token = 48 hex (generateSecureRandomToken(24)); el token se guarda bcrypt cost 10 en `apiKey.api_key` y el texto claro se devuelve UNA vez ([auth] AUTH.md §6) |
| `apiKeys.delete` | `admin` | `{apiKeyId}`; no-op silencioso como el delete upstream |

### info ([info] info.ts — 3 procedimientos, todos portados)

| Procedimiento | Permiso | Salida |
|---|---|---|
| `info.getInfo` | firmado | `{"version":"2.11.1"}` (Options.Version la sobreescribe) |
| `info.isDemoMode` | público | `false` (Options.DemoMode) |
| `info.isDemoReadOnly` | público | `DemoMode && DemoReadOnly` |

## 4. Formas de salida

- **`void`**: los procedimientos void/renderizan `{"result":{"data":{}}}`
  sin miembro `json` (superjson de `undefined`; trpc.go marshalPlainEnvelope).
- **`null` explícito** (p. ej. `searchEngine.getDefaultSearchEngine` sin
  default): `{"result":{"data":{"json":null}}}` — el procedimiento devuelve
  `json.RawMessage("null")`, distinto de void.
- **Fechas**: `emailVerified`, `expirationDate`, `oauthIssuedAt` viajan
  como **string ISO** (`toISOString()`, UTC con ms) en vez del par
  superjson `{"json": …, "meta": {"values": …["Date"]}}` — convención ya
  fijada por `internal/api/trpc/superjson.go` (los payloads del port son
  JSON plano). Funcional para `new Date(s)` en el frontend; ver §7 para el
  follow-up del integrador.
- **null vs []**: los arrays de relations (`members`, `permissions`,
  `userPermissions`) se emiten siempre como array (vacío si no hay filas,
  nunca null), igual que drizzle findMany; las columnas nullable
  (`email`, `image`, `ownerId`, `description`, `urlTemplate`,
  `integrationId`, `owner`, `integration`, …) emiten `null`.
- **Orden de claves en objetos dinámicos** (serverSettings.getAll y
  records de settings): marshaling Go = alfabético; upstream emite orden
  de inserción. Ningún consumidor depende de ello.

## 5. Decisiones y desviaciones documentadas

1. **Paquete compartido `router/internal/admin`**: los 9 paquetes
   comparten el decodificador de inputs (paridad zod campo a campo,
   BAD_REQUEST en cada violación — estilo board), los gates de permisos,
   la resolución de onboarding/claim y el generador de ids
   (`admin.CreateID`, 24 alfanuméricos lowercase ≙ cuid2 — el generador
   exacto no es load-bearing para el wire). Vive bajo `internal/` para que
   solo los routers lo importen.
2. **Credenciales siempre habilitadas**: `throwIfCredentialsDisabled`
   ([invite] checks.ts) es no-op porque el fork solo embarca el provider
   credentials (`internal/auth`). Lo mismo para
   `canManageGroupMembersLocally()` (siempre true) e
   `isGroupMembershipManagedLocally` (true solo para `credentials`; un
   usuario oidc/ldap de una BD migrada recibe FORBIDDEN "User's provider
   is not managed locally").
3. **Email**: la validación upstream es `z.email()` con el regex unicode
   completo (val-email email.ts:3); el port exige el mínimo estructural
   (un `@`, local y dominio no vacíos con punto, sin espacios). El
   frontend revalida con el mismo schema zod antes de enviar.
4. **`group.getAll` / `getPaginated` sin ORDER BY** (como upstream:
   findMany sin orderBy ⇒ orden rowid de SQLite); `searchEngine.*` ídem
   salvo los ORDER BY explícitos de upstream (catalog/getSelectable por
   name). Determinista en SQLite para el mismo estado de la tabla.
5. **`MaxGroupPosition` coalesce a 1** (queries/group.ts:6-11): con tabla
   vacía el primer grupo creado recibe `position = 2`.
6. **`addMember` duplicado**: el insert crudo upstream hace que la
   violación UNIQUE de `groupMember` burbujee como
   INTERNAL_SERVER_ERROR; el port la replica (sin mapa a CONFLICT).
7. **savePartialSettings distingue ausente de null** (three-state): una
   clave no suministrada no toca la columna; `null` escribe NULL.
8. **updatePreferences**: `strictObject` (claves desconocidas ⇒
   BAD_REQUEST), refine de ≥1 preferencia, merge de header por zonas y
   validación completa del merge antes de la escritura única
   (preferences.ts:98-102). `user.changeHeaderPreferences` acepta los
   formatos legacy v1/v2 (migración en lectura, header-preferences.ts:
   209-219).
9. **parseBrandingSettings total**: cualquier inconsistencia del registro
   almacenado devuelve los defaults íntegros (index.ts:120-127) — un
   branding corrupto nunca rompe la UI.
10. **isDemoMode / ramas demo**: compiladas con `Options.DemoMode=false`;
    los mensajes FORBIDDEN demo ("User deletion is disabled in demo
    mode", "Password changes are disabled in demo mode", "Username changes
    are disabled in demo mode") quedan tras el flag para paridad futura.
11. **Bangs**: sin tabla (el dataset vive en duckduckgo.com); no hay
    `db/bangs.go` porque no hay SQL que portar. El proxy está portado
    (§3 bangs) con caché in-memory TTL 10s y las desviaciones listadas ahí.
12. **Guard read-only como wrapper de registro**: `admin.Writable` envuelve
    la mutación en el punto de `Register` (§2.1) en lugar de inyectar el
    chequeo en cada handler: garantiza completitud (nueva mutación sin
    envolver = visible en diffs del registro) y replica el orden upstream
    (middleware base antes de permisos/validación) sin consultas extra a la
    base (solo mira la sesión del contexto).

## 6. Procedimientos diferidos (y por qué)

| Procedimiento | Motivo |
|---|---|
| `onboard.completeSetup` | 1000+ líneas: tests de conexión de integraciones + cifrado de secretos (`encryptSecret`), defaults de los 67 widgetKinds (`getDefaultWidgetConfig`, `getWidgetIntegrationLimit`), lanes/grids de boards y escrituras de 10 tablas. Requiere el dominio de integraciones (M3). |
| `onboard.testIntegration` | `testConnectionAsync` del router de integraciones (HTTP externo + MissingSecretError). |
| `onboard.discoverDockerServices` | Docker Engine API (`@homarr/docker`, M3). |
| `onboard.detectRuntimeCapabilities` | probes docker/workshop (env + HTTP externo). |

Los 4 de onboarding son los pasos "setup" del wizard: hasta que se porten,
el asistente queda funcional en start → user → group y el paso setup
responderá NOT_FOUND (el resto del wizard de M2 — usuario/grupo/invites —
está completo). `bangs.search` dejó de estar diferido (§3 bangs).

## 7. Preguntas abiertas para el integrador

1. **Fechas como ISO strings**: si algún consumidor del SPA exige un Date
   real (p. ej. date-fns sobre `invite.expirationDate`), la extensión
   natural es soportar `meta.values` en `trpc.WrapJSON`
   (superjson.go:20-24 ya deja la costura anotada); los handlers ya
   producen el ISO exacto que superjson emitiría.
2. **Montaje del endpoint REST del claim** (`POST /api/onboarding/claim`,
   ya portado como `onboard.ClaimHandler`, §3.5): falta el wiring en
   httpserver —
   `router.Handle("/api/onboarding/claim", auth.Middleware()(onboard.ClaimHandler(database)))`.
   Los routers ya verifican el token vía
   `Options.OnboardingClaimTokenFromContext` (el middleware de httpserver
   debe seguir extrayendo la cookie `homarr-onboarding-claim` para esas
   rutas tRPC). Nota: detrás de `auth.Middleware()` una cabecera `ApiKey`
   inválida responde 401 plano en esta ruta (§3.5 desviación 2).
3. **Versionado de `info.getInfo`**: hoy `Options.Version` default
   `"2.11.1"` (package.json raíz upstream); conviene inyectarlo en el
   build (`-ldflags`) cuando el fork lleve su propia versión.
4. **`analytics` CAS**: upstream reintentos con compare-and-swap
   (updateAnalyticsServerSettingAsync, ss-q:66-90); con pool de 1 conexión
   el read-merge-write del port es equivalente, pero si se relaja el pool
   conviene portar el CAS.

## 8. Repositorios nuevos/extendidos en internal/db

- `db/group.go` (nuevo): CRUD de `group`, members con join `user`,
  permisos de grupo (list/replace), posiciones en transacción,
  `MaxGroupPosition` (coalesce 1), `GroupNameSimilarExists` (LIKE crudo),
  `GroupIsEveryone`, `ListGroupPermissionStrings`, transacción de
  `createInitialExternalGroup`.
- `db/invite.go` (nuevo): create/list (ORDER BY expiration, sin token,
  creator join)/get by id y por id+token/delete con rowcount (consumo en
  `user.register`).
- `db/onboard.go` (nuevo): fila de onboarding, transición de paso
  (rowCount para el CONFLICT), `UpdateOnboardingAll` (el UPDATE sin WHERE
  de nextOnboardingStepAsync), `HasAnyUser`.
- `db/onboardingclaim.go` (nuevo): `DeleteServerSettingIfValue` — el
  borrado optimista de `claimOnboardingAsync` (onboarding-claim.ts:97-101):
  solo elimina la fila `__onboarding_claim` mientras conserva el valor
  leído, de modo que la fila de un emisor concurrente sobrevive y el INSERT
  perdedor detecta al ganador (§3.5). Lectura/insert usa los repos de
  `db/serversettings.go`.
- `db/serversettings.go` (nuevo): get/list/insert/update/upsert/delete de
  `serverSetting` (valores superjson crudos; la interpretación es del
  router).
- `db/searchengine.go` (nuevo): CRUD + paginado + selectable + join
  `integration` (`{id, kind, url}`) + búsqueda por prefijo de `short` +
  default de usuario + `ClearUserDefaultSearchEngine`.
- `db/apikey.go` (nuevo): listados OAuth/`getAll` con user, deletes del
  router (creación/lookup ya estaban en `account.go` para la auth).
- `db/user.go` (extendido, como el integrador hizo con `board.go` para el
  dominio board): proyecciones de listado (`ListUsers`,
  `ListSelectableUsers`, `SearchUsers`), `GetUserDetailByID`
  (user.getById), preferencias (get/patch parcial con nil = sin cambio),
  perfil/imagen/borrado, tours, `UsernameTaken`, y las transacciones
  `CreateUserWithAdminGroup` (initUser) y `CreateUserWithInvite`
  (register, con `ErrInviteInvalid`).

SQL context-aware, sentencias preparadas implícitas por database/sql, sin
ORM; escrituras multi-paso en `BeginTx` (pool de 1 conexión, paridad
better-sqlite3). Los getters de fila única devuelven `db.ErrNotFound`
desenvuelto (convención de `board.go`); los routers lo comprueban con
`errors.Is`.

## 9. Cobertura de tests (stdlib + httptest, estilo board)

Cada paquete monta un `trpc.Router` real sobre httptest con inyección de
sesión por header (`X-Test-User`) y de claim (`X-Test-Claim`), así que los
envelopes tRPC son parte de las aserciones.

| Paquete | Tests | Cubre |
|---|---|---|
| `user` | `TestUserCreateAndList` | gates (anónimo/non-admin), create con hash bcrypt, duplicado CONFLICT, validaciones negativas (password corta, confirm distinta, username corta), getAll/selectable/search shapes |
| | `TestUserChangePassword` | verify bcrypt del previous, nuevo hash verifica y el viejo no, NOT_FOUND al cambiar la de otro sin admin, admin reset sin previous |
| | `TestUserPreferences` | shape de getPreferences (defaults + header parseado), NOT_FOUND otro user, merge de updatePreferences, strictObject/empty patch, changeColorScheme/changeFirstDayOfWeek (bounds), changeHomeBoards (pública OK, privada NOT_FOUND), default search engine inexistente BAD_REQUEST, changeHeaderPreferences (v3 + refine de account access), tours |
| | `TestUserGetByIdEditProfileDelete` | self-or-admin, perfil 18 campos, reset de emailVerified al cambiar email, delete self |
| | `TestUserInitUserOnboarding` | gate de claim, transacción initUser (usuario + credentials-admin + admin + member + paso setup), segundo initUser "Step denied" |
| | `TestUserRegisterWithInvite` | invite expirada/token erróneo FORBIDDEN, registro consume la invite, reuso rechazado |
| `group` | `TestGroupCRUDRoundtrip` | gates, createGroup (id crudo, position 2 con tabla vacía), getAll/getById shapes (members/owner/permissions), search, getPaginated, savePermissions (dedup), savePartialSettings (three-state), savePositions (index+1), transferOwnership, rename, add/removeMember, deleteGroup |
| | `TestGroupReservedAndSimilarNames` | "everyone" reservado (validación + FORBIDDEN por procedimiento), CONFLICT por nombre similar (case-insensitive), límites del nombre |
| | `TestGroupCreateInitialExternalGroup` | gate de claim, creación con permiso admin + transición a setup, segundo intento "Step denied" |
| `invite` | `TestInviteLifecycle` | gates, createInvite (token 40 hex), getAll (orden por expiración, creator, SIN token), deleteInvite (NOT_FOUND), formato ISO de expirationDate |
| | `TestInviteValidation` | fecha inválida y sin offset ⇒ BAD_REQUEST |
| `onboard` | `TestOnboardingCurrentStepAndNext` | fila ausente ⇒ start, gate de claim (forbidden sin claim), transición con claim, PRECONDITION_FAILED en el segundo nextStep, claim expirado rechazado |
| | `TestClaimHandlerIssuedActiveLocked` | handler REST §3.5: issued (200 + cookie HttpOnly/SameSite=Strict/Path/Expires + no-store + hash sin token en DB), re-claim active, locked 423 con expiresAt, rotación por admin (token anterior invalidado) |
| | `TestClaimHandlerFinishedForbidden` | finished 409 + cookie borrada (valor vacío, Expires pasado), forbidden tras el primer usuario (también para el navegador con claim: upstream chequea usuario antes del token activo) |
| | `TestClaimHandlerCrossSiteAndMethod` | Sec-Fetch-Site cross-site/same-site 403 cross_site, same-origin/none pasan, Secure por x-forwarded-proto (primer valor), 405 en GET |
| | `TestClaimHandlerUnauthenticatedAdminOverrideIsNotForced` | anónimo tras el primer usuario ⇒ 403 administrator_required |
| | `TestOnboardingClaimTokenFromCookieHeader` | parser del header Cookie (spec onboarding-claim.spec.ts:76-83): match exacto, %2D decodificado, escape inválido, nombre similar, valor vacío |
| | `TestClaimOnboardingExpiredRotation` | claimOnboardingAsync: tras expirar se re-emite y el token viejo no valida (spec :49-60) |
| `serversettings` | `TestServerSettingsReadShapes` | getCulture/getBranding (defaults completos), gates de getBoardSettings/getAll, shape byte-exacto del board default |
| | `TestUpdateBoardSettings` | merge parcial, board privada/inexistente con mensaje exacto, tipos erróneos, gates |
| | `TestSaveSettingsBranches` | merge genérico (culture), analytics estricto (clave desconocida BAD_REQUEST + defaults preservados), branding con legacy keys, settingsKey desconocido |
| `searchengine` | `TestSearchEngineCRUD` | escalera de permisos create/modify/full, shape de fila (nulls), unión discriminada (urlTemplate sin `%s`, nombre largo), update sin tocar short, NOT_FOUND, catalog anónimo (solo generic) vs firmado (con integration), search por prefijo, getPaginated, delete con detach de defaults |
| | `TestGetDefaultSearchEngine` | default de server (anónimo, generic), default de usuario, limpieza del default huérfano en lectura |
| `apikeys` | `TestAPIKeyLifecycle` | gates, formato `id.token` (8+48 hex), bcrypt verify contra el hash almacenado (y rechazo de token erróneo), getAll con user, delete + readOnly roundtrip |
| `user` | `TestReadOnlyAPIKeyEnforcement` | guard §2.1: getTourStatus OK con key read-only, changeColorScheme 403 "This credential only permits read-only operations", sesión normal sin efecto, orden (cuerpo inválido ⇒ error read-only, no BAD_REQUEST), completeTour cubierto |
| `info` | `TestInfoRouter` / `TestInfoOptionsOverride` | UNAUTHORIZED anónimo, shape byte-exacto de la versión, demo flags |
| `bangs` | `TestBangsSearchShapeAndOrder` | proxy portado: matches de prefijo ordenados, forma byte-exacta `{t,s,d,u,c,sc,r}` (opcionales omitidos, clave desconocida `x` descartada), normalización del token, query vacía/sin match `[]`, límite, header accept del fetch |
| | `TestBangsSearchInputValidation` | zod del input: query requerido, limit 1-50 default 20, no-entero/strings ⇒ BAD_REQUEST |
| | `TestBangsSearchCache` | caché TTL: 1 fetch dentro de la ventana, re-fetch tras expirar (TTL real 10s reducido a 30ms en el harness) |
| | `TestBangsSearchUpstreamError` | 500 upstream ⇒ 500 INTERNAL_SERVER_ERROR "Response did not indicate success" (mensaje exacto de ResponseError), fallo no cacheado, re-intento y recuperación |
| | `TestBangsSearchInvalidCatalog` | schema zod del catálogo: falta t/s/u, tipos erróneos, no-array, JSON inválido ⇒ 500 (rechazo del array entero) |
| | `TestBangsSearchDefaultsToRealUpstream` | `bangs.DefaultCatalogURL` = https://duckduckgo.com/bang.js y registro con Options cero (sin contacto de red en tests) |

Ejecución: `go test ./internal/api/router/... ./internal/db/...` (solo
stdlib + las deps del módulo).


## 10. Routers M4: log, updateChecker, home, location, analytics y app.detectMetadata

Seis dominios portados en M4, uno por paquete con la firma uniforme
`Register(r *trpc.Router, database *db.DB, opts Options)`. Complementan este
documento: `PROTOCOL.md` §10 (el formato SSE que sirve `log.subscribe`),
`WEBSOCKET.md` §6/§8 (el mapa de topics del bus y la excepción SSE) y
`BOARDS.md` §6 (el un-defer de `getManageOverview`).

### 10.1 log (`packages/api/src/router/log.ts`) — paquete `log/`

`log.subscribe` es la única suscripción del router (log.ts:19-84):
`permissionRequiredProcedure.requiresPermission("other-view-logs")`, input
`{levels: LogLevel[]}` con default `["info"]` (un array vacío es válido y
filtra todo). Emite `{type:"history", messages}` inicial (la tabla `log` de
`internal/logging`, filtrada y oldest-first como el `getRecentAsync` upstream)
y luego `{type:"message", message}` por evento vivo del bus.

- **Forma de los payloads**: superjson con la anotación
  `{"timestamp": "Date"}` en `meta.values` — el terminal del SPA llama
  `message.timestamp.getTime()` (terminal.tsx:96), así que la fecha DEBE
  reconstruirse como Date en el cliente. El contrato vive en
  `trpc.WrapJSONWithMeta`/`trpc.SuperJSON` (superjson.go) y los handlers
  devuelven `trpc.SuperJSON` en Initial y Decode (el lado WS ya envolvía con
  `trpc.WrapJSON`, que ahora pasa esos envelopes verbatim).
- **Topics del bus**: `events.log.<level>` — el pipeline de `internal/logging`
  publica el envelope superjson del LoggerMessage (lo mismo que upstream
  PUBLISHa); el handler suscribe `events.log.*` y filtra por niveles en el
  `Decode` (el orden armado-de-bus → Initial elimina duplicados; la
  microventana documentada en WEBSOCKET.md §6 se mantiene).
- **Doble transporte**: `Register` registra la suscripción en el registro SSE
  del router tRPC (`RegisterSubscription`) y devuelve `*Service` con
  `SubscriptionHandlers()` para el hub WS (la convención de docker).
- **Errores**: anónimo → UNAUTHORIZED; sin el permiso expandido → FORBIDDEN
  "Permission denied"; nivel inválido → BAD_REQUEST (el enum de
  `zodEnumFromArray`).

### 10.2 updateChecker (`router/update-checker.ts` + `request-handler/src/update-checker.ts`) — paquete `updatechecker/`

`updateChecker.getAvailableUpdates` (admin, query). Porta el request handler
completo: caché fresca 24h + caché stale + single-flight (el lock Redis de
upstream es cross-proceso; en un proceso es single-flight), fetch de
`https://api.github.com/repos/homarr-labs/homarr/releases` (cabeceras octokit:
`accept: application/vnd.github+json`, User-Agent), filtrado semver (tags no
semver descartados, prereleases ocultos salvo que la versión actual lo sea —
flag `prerelease` de GitHub, tag de la versión actual), estrictamente
posteriores a la versión actual, orden descendente. Stale-if-error: un fallo
sirve el payload stale (o vacío) y lo cachea como fresco por otras 24h
("suppressing retries for 24 hours"); el router traga cualquier error en una
lista vacía ("An empty list hides the indicator", update-checker.ts:15-17).
Salida: `[{name, contentHtml?, url, tagName, isPrerelease}]` (contentHtml
omitido cuando la release no trae body_html). `Options{Version, ReleasesURL,
CacheTTL, Now, Client}` son las costuras de test.

### 10.3 home (`router/home.ts`) — paquete `home/`

`home.getStats` (público): las tarjetas de estadísticas del dashboard de
gestión, en el orden exacto de inserción upstream
board→user→invite→integration→app→group→searchEngine→media, cada una solo
cuando la página detrás es alcanzable para el caller
(`getAppManagementAccess`/`getIntegrationManagementAccess` de
definitions/permissions.ts:231-254). Notas: `isProviderEnabled("credentials")`
es siempre true (fork credentials-only); el conteo de boards replica el
filtro `or(isPublic, creatorId, inArray(boardIds))` de home.ts:103-120 con
los boardIds de permisos directos + de grupo; la tarjeta de integration
cuenta los integrations con permiso "full" delegado cuando no hay
`integration-full-all` (`getFullAccessIntegrationIdsAsync`); app cuenta solo
con `app-modify-all` (0 si solo `app-create`); media filtra por `creator_id`
sin `media-view-all`. Salida: `[{titleKey, subtitleKey, count, path}]`.

### 10.4 location (`router/location.ts`) — paquete `location/`

`location.searchCity` (público, `{query}`): proxy de
`https://geocoding-api.open-meteo.com/v1/search?name=<query>` con el timeout
de 10s (`withTimeoutAsync`, igual que bangs). El OUTPUT schema se porta
completo (location.ts:24-41): `{results: [city]}` con las claves exactas
`{id, name, country?, country_code?, latitude, longitude, population?}` —
zod recorta las claves desconocidas de la API y los opcionales ausentes se
omiten; un objeto con solo `generationtime_ms` cae a `{results: []}` (el
refine exige que sea la ÚNICA clave); cualquier otra forma es un fallo de
validación de salida → INTERNAL_SERVER_ERROR (upstream no chequea `res.ok`:
un body de error de la API simplemente no valida). Sin
`env.NO_EXTERNAL_CONNECTION` (misma decisión que bangs).

### 10.5 analytics (`router/analytics.ts` + `packages/analytics/src`) — paquete `analytics/`

`analytics.sendAnalytics` (admin, mutation, guard read-only): devuelve
`{status}` con la semántica exacta de `sendSnapshotAsync`:

| status | condición |
|---|---|
| `disabled` | `analytics.enableGeneral = false` (settings leídos al inicio y re-leídos tras recolectar, send-server-analytics.ts:206-208) |
| `skipped` | la última snapshot exitosa tiene menos de 7 días (`isSnapshotDue`, snapshot-properties.ts:3-9) |
| `sent` | snapshot capturada en PostHog y `lastSuccessfulSnapshotAt` avanzado |
| `failed` | cualquier error — INCLUIDA la fila `analytics` ausente de una instancia fresca (el CAS `updateAnalyticsServerSettingAsync` lanza "Analytics server settings are missing" y el catch degrada, send-server-analytics.ts:223-226) |

La snapshot recolecta las ~25 métricas de send-server-analytics.ts:99-205
via SQL sobre el mismo esquema (counts de 21 tablas, distribuciones por kind
de integration/item/layout/section, buckets de tamaño de board,
`omitZeroCounts` — las claves `count*` en 0 se omiten, y
`countBoardsWithStatusEnabled` desaparece con `forceDisableStatus`), y
captura UN evento `server-analytics` a `https://hog.homarr.dev/batch/` con
la api key de client.ts:5-6, el uuid derivado de
`sha256(instanceId:periodo7d:server-analytics)` (getSnapshotUuid) y
`$geoip_disable`/`$process_person_profile: false`. La actualización de
settings conserva el CAS de 5 intentos upstream (server-setting.ts:66-90);
las llamadas concurrentes comparten la snapshot en vuelo (el
`pendingSnapshot` promise). Desviaciones documentadas: el frame PostHog es
un POST directo de `/batch/` (los internals de posthog-node no son
load-bearing para Homarr), `databaseType` siempre "sqlite", `authProviders`
siempre `["credentials"]`, `env.NO_EXTERNAL_CONNECTION` no portado.

### 10.6 app.detectMetadata (`router/app.ts:227-239`) — paquete `appmeta/`

Mutation `app-create` (guard read-only incluido) con input
`{href: trimmed http(s) URL}` (iconsDetectFaviconSchema). Flujo portado:

1. `fetchWebsiteMetadataAsync` (packages/icons/src/favicon-fetcher.ts,
   favicon.go + guardedfetch.go del paquete): fetch SSRF-guarded de la
   página (Accept text/html, 5s por hop con DNS re-verificado por conexión
   y listas de bloqueo de loopback/link-local/metadata cloud, máx 5
   redirects manuales, head de 256 KiB cortado en `</head`/`<body`),
   `<title>` (entidades decodificadas, whitespace colapsado) y
   `<link rel=icon>` rankeados por relación (apple-touch-icon-precomposed >
   apple-touch-icon > icon > shortcut icon) y área de `sizes`, con el
   fallback `/favicon.ico` del origen que respondió (probe por
   content-type). Nada lanza: sitio inalcanzable → metadatos nulos.
2. `name = (title ?? hostname).slice(0, 64)` (app.ts:236; el slice upstream
   cuenta unidades UTF-16, el port bytes — solo un corte en multibyte lo
   nota).
3. `iconUrl = metadata.iconUrl ?? getIconForName(db, name) ?? defaultIcon`
   (app.ts:238) — la búsqueda de iconos es la primera fila de `icon` con
   `name LIKE %name%` SIN escapar (los %/_ del nombre son comodines también
   upstream, auto-icon-searcher.ts:5-9), repo nuevo `db/icon.go`; el default
   es el icono homarr de jsdelivr (app.ts:21).

Salida `{name, iconUrl}` nunca nulos (el output schema app.ts:232). El
procedimiento vive en su propio paquete (el board package lleva el resto del
app router) con el path punteado intacto; los tests usan un sitio httptest y
filas de icon sembradas.

### 10.7 Registro

```go
log.Register(r, database, log.Options{Store: logStore})          // "log.subscribe" (SSE + WS)
updatechecker.Register(r, database, updatechecker.Options{})     // "updateChecker.getAvailableUpdates"
home.Register(r, database, home.Options{})                       // "home.getStats"
location.Register(r, database, location.Options{})               // "location.searchCity"
analytics.Register(r, database, analytics.Options{})             // "analytics.sendAnalytics"
appmeta.Register(r, database, appmeta.DefaultOptions())          // "app.detectMetadata"
```

`log.Register` devuelve `*Service`: además del registro SSE, el integrador
cablea `for path, handler := range logSvc.SubscriptionHandlers() {
hub.Register(path, handler) }` (WEBSOCKET.md §11). El pipeline de logs se
construye antes: `logStore, _ := logging.NewSQLiteStore(ctx, db)` y
`slog.SetDefault(slog.New(logging.NewHandler(logging.Options{Level:
logLevel, Store: logStore, Bus: eventBus})))` (LOG_LEVEL env, default info).
`r.Bus = eventBus` antes de registrar routers para que las suscripciones SSE
tengan bus.
