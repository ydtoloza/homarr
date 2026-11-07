# Autenticación — port Go de `packages/auth`

Documento de referencia del paquete `server/internal/auth`: verificación de
credenciales, sesiones de base de datos compatibles con Auth.js v5, la
superficie REST que habla el SPA y el middleware que resuelve el caller.
Todas las afirmaciones están verificadas contra las fuentes upstream (rama
`dev` de este repo) o contra next-auth `5.0.0-beta.32` (versión clavada en
`package.json:205`), citadas como `[next-auth]` con rutas relativas a
`packages/` del repo nextauthjs/next-auth. Fecha: 2026-11-06.

## 1. Contraseñas: bcrypt cost 10 (NO argon2)

**La contraseña se verifica con bcrypt, coste 10** — `packages/auth/security.ts:1-15`:

```ts
import bcrypt from "bcrypt";
export const hashPasswordAsync = async (password: string) => {
  return bcrypt.hash(password, 10);
};
export const comparePasswordsAsync = async (password: string, hash: string) => {
  return await bcrypt.compare(password, hash);
};
```

El flujo de login lo consume `authorizeWithBasicCredentialsAsync`
(`packages/auth/providers/credentials/authorization/basic-authorization.ts:18-41`):
lookup por `name = input.toLowerCase()` AND `provider = "credentials"` y
`comparePasswordsAsync(credentials.password, user.password)`.

**Compatibilidad con bases de datos existentes: directa.** El paquete npm
`bcrypt` y `golang.org/x/crypto/bcrypt` implementan el mismo formato
crypt modular (`$2a$10$…` / `$2b$10$…`, 60 caracteres): el coste viaja dentro
del hash y `CompareHashAndPassword` acepta prefijos `$2a$/$2b$/$2y$`. Verificado
contra el hash real del usuario demo que siembra la migración upstream
(`packages/db/migrations/seed.ts:955`, `$2b$10$odRXt5e95kSQV5Axmk/FeO6GVOxuRQQ8NnRcBA78Wg4V3kZxPY68u`
— contraseña `demo`, la de demo.homarr.dev): `x/crypto/bcrypt` lo valida.
Los tests fijan ese fixture (`password_test.go`,
`TestComparePasswordUpstreamFixture`).

`HashPassword` (Go) emite `$2a$10$…`, exactamente lo que upstream escribe en
`user.password` para cambio de contraseña
(`packages/api/src/router/user.ts:78,153,666,910`) y en `apiKey.api_key`
para las API keys (`packages/api/src/router/apiKeys.ts:73-74`).

Nota: `ARCHITECTURE.md` y `SCHEMA.md` mencionan "argon2id" — es un error
heredado de un análisis previo; la fuente real es bcrypt.

## 2. Sesiones

**Estrategia de base de datos** (`packages/auth/configuration.ts:109-113`):
`session: { strategy: "database", maxAge: AUTH_SESSION_EXPIRY_TIME }`. La tabla
`session` (`packages/db/schema/sqlite.ts:107-119`, DDL portado en
`server/internal/db/migrations/0016`): `session_token` **PK** (sin columna
`id`), `user_id` FK cascade, `expires` (epoch ms).

- **Token**: `generateSessionToken()` = `generateSecureRandomToken(48)` =
  **48 bytes aleatorios en hex = 96 caracteres**
  (`packages/auth/session.ts:17-19`, `packages/common/src/security.ts:8-10`).
- **Duración**: `AUTH_SESSION_EXPIRY_TIME` con default `"30d"`
  (`packages/auth/env.ts:29`). **No es rolling**: se fija en la creación y
  nunca se refresca; la validación es `expires.getTime() <= Date.now()` → nula
  (`packages/auth/session.ts:44`). La fila caducada se queda en la tabla (no
  hay cron de limpieza upstream; el borrado ocurre en sign-out).
- **Creación**: la hace el callback `signIn` de la configuración, no el
  adapter (`packages/auth/configuration.ts:73-103`): inserta la fila y setea
  la cookie manualmente.
- **La sesión tRPC**: `getSessionFromTokenAsync` (`packages/auth/session.ts:21-55`)
  hace join con el user y `createSessionCallback`
  (`packages/auth/callbacks.ts:38-62`) añade `colorScheme` y `permissions`
  (permisos de grupo con expansión de hijos,
  `packages/definitions/src/permissions.ts:97-101`).

### Cookies (names pinned)

Homarr sobreescribe los nombres de cookie de Auth.js con prefijo
`AUTH_COOKIE_PREFIX` (default `homarr`) para aislar su jarra en un host
compartido (`packages/auth/configuration.ts:23-39`; issue homarr#5773):

| Cookie | Nombre http | Nombre https | Atributos |
|---|---|---|---|
| **session-token** | `homarr.session-token` | `homarr.session-token` (¡sin `__Secure-`!) | `Path=/; Expires=<expires de la sesión>; HttpOnly; SameSite=Lax; Secure*` (`configuration.ts:94-100`) |
| **csrf-token** | `homarr.csrf-token` | `__Host-homarr.csrf-token` | `Path=/; HttpOnly; SameSite=Lax; Secure*` (defaults de [next-auth] `core/src/lib/utils/cookie.ts`, sin `maxAge` → cookie de sesión) |
| color-scheme | `homarr.color-scheme` | idem | `Path=/; Expires=+1 año`, sin HttpOnly (`packages/auth/events.ts:88-91`, clave en `packages/definitions/src/cookie.ts:1`) |

\* `Secure` depende de que la URL del request sea https
(`apps/nextjs/src/app/api/auth/[...nextauth]/route.ts:25-28`); detrás de proxy
se reescribe el origin con `x-forwarded-proto`/`x-forwarded-host`
(`route.ts:57-69`).

Esos son los tres que el flujo credentials del SPA observa. Auth.js además
gestiona `homarr.callback-url` / `__Secure-homarr.callback-url`
(`configuration.ts:34`), pkce/state/nonce para OIDC y el chunking de la cookie
de sesión por encima de ~3.9 KiB ([next-auth] `core/src/lib/utils/cookie.ts`
`SessionStore`, `CHUNK_SIZE = 4096-160`) — irrelevante para un token de 96
caracteres; **no portado** (gap documentado, §7).

## 3. CSRF

Port de [next-auth] `core/src/lib/actions/callback/oauth/csrf-token.ts`:

- **Token**: `randomString(32)` → 32 caracteres hex (16 bytes aleatorios).
- **Cookie**: `token|sha256hex(token + secret)` — el secret (`AUTH_SECRET`,
  generado al arrancar el contenedor en `scripts/run.sh:65`) actúa de sal y
  prueba que la cookie la puso este servidor.
- **Verificación en POST**: doble envío — la parte token de la cookie debe
  validar contra su hash **y** ser igual al `csrfToken` del body
  (`csrfTokenVerified = isPost && csrfToken === bodyValue`). Aplica a
  `signout`, `signin` y al `callback` de providers credentials
  ([next-auth] `core/src/lib/index.ts`, POST switch).
- **Fallo**: `MissingCSRF` → el catch de [next-auth] `core/src/index.ts`
  responde **400 con body JSON `null`**.

## 4. Superficie REST (`/api/auth/*`) — contrato exacto

El SPA llama via `next-auth/react` (`packages/auth/client.ts:1` re-exporta
`signIn/signOut/useSession` de [next-auth] `next-auth/src/react.tsx`). El
route handler upstream es
`apps/nextjs/src/app/api/auth/[...nextauth]/route.ts` (catch-all, GET+POST).

| Endpoint | Método | Petición | Respuesta |
|---|---|---|---|
| `/api/auth/csrf` | GET | — | 200 `{"csrfToken":"…"}` + `Set-Cookie` csrf. Cabeceras `Cache-Control: private, no-cache, no-store`, `Expires: 0`, `Pragma: no-cache` ([next-auth] `core/src/lib/pages/index.ts` renderPage) |
| `/api/auth/providers` | GET | — | 200 JSON por id con whitelist `{id, name, type, signinUrl, callbackUrl}` (renderPage `render.providers`) |
| `/api/auth/session` | GET | cookie de sesión | 200 con la sesión (`{"user":{id,name,email,image,colorScheme,permissions},"expires"}`) o **body JSON `null`** (v5 devuelve null, no `{}`; [next-auth] `core/src/lib/actions/session.ts` + `createSessionCallback` de `packages/auth/callbacks.ts:38-62`). Sesión caducada/desconocida → `null` **+** cookie limpiada (`sessionStore.clean()`) |
| `/api/auth/callback/credentials` | POST | form-urlencoded: **`csrfToken`, `callbackUrl`, `name`, `password`** (el form del login envía también `provider`, ignorado por el schema `userSignInSchema` = `{name, password}` en `packages/validation/src/user.ts:91-94`) | Éxito: fila de sesión + `Set-Cookie` session-token + `{url: callbackUrl}` (con cabecera `X-Auth-Return-Redirect: 1`, que `react.tsx` siempre envía) o 302 sin ella. Fallo: `{url: "<origin>/auth/login?error=CredentialsSignin&code=credentials"}` |
| `/api/auth/signout` | POST | form-urlencoded: `csrfToken`, `callbackUrl` | Borra la fila de sesión, `Set-Cookie homarr.session-token=; Max-Age=0`, `{url: callbackUrl}` / 302 ([next-auth] `core/src/lib/actions/signout.ts`) |

Detalles pinados:

- **Nombres de campo del login**: `name` y `password`. El `<input name="username">`
  del form (`apps/nextjs/src/app/[locale]/auth/login/_login-form.tsx:123-131`)
  es solo cosmético (autocomplete); el submit real va por
  `signIn(provider, credentials)` de react.tsx, que mete en el body los campos
  del objeto `{name, password, provider}` + `csrfToken` + `callbackUrl`
  ([next-auth] `react.tsx` signIn: `Content-Type:
  application/x-www-form-urlencoded`, `X-Auth-Return-Redirect: 1`, body
  `URLSearchParams({...signInParams, csrfToken, callbackUrl})`).
- **CallbackUrl**: el callback `redirect` por defecto ([next-auth]
  `core/src/lib/init.ts` defaults) fuerza same-origin: URLs relativas se
  resuelven contra el origin y absolutas de otro origin caen al origin.
- **Fallo de login**: `authorize` devuelve null → `CredentialsSignin` (code
  por defecto `"credentials"`, [next-auth] `core/src/errors.ts`) → redirect a
  `pages.error = "/auth/login"` (`packages/auth/configuration.ts:114-117`)
  con `?error=CredentialsSignin&code=credentials`. El SPA lee `error` de la
  query de la url devuelta (`react.tsx`).
- **Efectos del signIn event** (`packages/auth/events.ts:47,88-91,95-112`):
  añadir el usuario al grupo `everyone` (best-effort: los errores del event
  handler se tragan con `eventsErrorHandler`) y setear la cookie
  `homarr.color-scheme`. Portados en `applySignInEvent`.
- **Desviación menor documentada**: campos `name`/`password` vacíos upstream
  fallarían en el parse zod dentro del callback (→ error Configuration);
  homarr-go responde como `CredentialsSignin` (mismo efecto práctico: el SPA
  valida con el mismo schema antes de enviar).
- **Path desconocido bajo /api/auth**: upstream responde **400 con body
  `"Bad request."`** ([next-auth] `core/src/index.ts`:
  `if (!internalRequest) return Response.json(\`Bad request.\`, { status: 400 })`),
  no 404. El handler Go replica eso (fallback del mux).

## 5. Middleware de Go y su mapeo con upstream

`Auth.Middleware()` replica la resolución del caller que hace el route
handler tRPC upstream (`apps/nextjs/src/app/api/trpc/[trpc]/route.ts:34-46`):

1. Si hay cabecera `ApiKey` (`packages/auth/api-key/constants.ts:1`) y es
   inválida → **401 con body plano `{"error":"UNAUTHORIZED"}`**
   (`route.ts:41-45`), sin envelope tRPC y sin caer al flujo de sesión. La
   cabecera CORS de esa respuesta la añade el route tRPC upstream
   (`setCorsHeaders`); en Go la añade el montaje de /api/trpc, no este
   middleware.
2. Si no, cookie de sesión válida (tabla `session`, no caducada) →
   `ctxauth.WithSession` en el request context.
3. Anónimo → pasa sin sesión: los procedimientos deciden su authz, igual que
   upstream donde `publicProcedure` no exige nada y los guards son middlewares
   tRPC (`packages/api/src/trpc.ts`): `protectedProcedure`/`enforceUserIsAuthed`
   (`trpc.ts:163-172`) ≙ `ctxauth.FromContext(ctx) == nil` → UNAUTHORIZED;
   `permissionRequiredProcedure.requiresPermission("admin")`
   (`trpc.ts:195-207`) ≙ `!ctxauth.Session.IsAdministrator` → FORBIDDEN.
   `IsAdministrator` ≙ permiso de grupo `admin`
   (`trpc.ts:149` usa `permissions.includes("admin")`; expansión de permisos
   portada en `permissions.go`).

Montaje previsto (integrador): `router.Handle("/api/trpc/*", a.Middleware()(trpcHandler))`.
**No** montarlo sobre `/api/auth/*` (upstream no valida API keys ahí).

## 6. API keys

Port de `packages/auth/api-key/get-api-key-session.ts:22-82`:

- **Formato de la cabecera `ApiKey`**: `"<id>.<token>"` (split por punto,
  exactamente 2 partes). `<id>` = `randomBytes(4).toString("hex")` (8 hex) y
  `<token>` = `generateSecureRandomToken(24)` = 48 hex, generado en
  `packages/api/src/router/apiKeys.ts:73-74`; el `<token>` se guarda
  **bcrypt-haseado** en `apiKey.api_key` (misma función que las contraseñas,
  coste 10) y la clave se devuelve al usuario una sola vez como `id.token`.
- **Validación**: row por `id` → `comparePasswordsAsync(token, api_key)`
  (línea 65). Formato inválido / id desconocido / token incorrecto →
  `null` → el route responde el 401 plano (§5.1).
- **Sesión resultante**: upstream **no crea fila de sesión**; construye un
  objeto en memoria con `expires = now+1d` y añade
  `apiKey: {readOnly, oauthClientId}` (`packages/auth/callbacks.ts:23-36`).
  El flag `readOnly` rechaza mutaciones en tRPC
  (`packages/api/src/trpc.ts:130-135` + `isReadOnlyProcedure`). **Seam**: el
  `ctxauth.Session` de Go no lleva `readOnly` aún; cuando se porten los
  routers de credenciales se añadirá el campo y el flag de la tabla
  (`apiKey.read_only`).
- Existen también API keys de proveniencia OAuth (`oauth_*`, migración 0049)
  — no afectan a la validación.

## 7. Fuera de alcance en M1 (gap documentado)

- **OIDC/LDAP**: providers alternativos (`filterProviders`) — el handler solo
  anuncia `credentials` en GET providers. El login del SPA con esos providers
  requiere sus endpoints (`signin/oidc`, `callback/oidc`, redirectProxy) — M4.
- **Cookie `homarr.callback-url`**: Auth.js la mantiene en cada request
  ([next-auth] `core/src/lib/utils/callback-url.ts`); el SPA siempre envía
  `callbackUrl` explícito, así que se omite.
- **Chunking de cookies** y el modo sin `X-Auth-Return-Redirect` de las
  páginas HTML (flows no-SPA): el 302 sí está implementado.
- **`jwt.encode/decode` override** (`packages/auth/configuration.ts:118-128`):
  existe para que Auth.js deje pasar el token crudo con strategy database; en
  Go no hay capa JWT que silenciar.
- **`AUTH_LOGOUT_REDIRECT_URL`** (`packages/auth/env.ts:28`): el SPA redirige
  en cliente tras `signOut({redirect:false})`
  (`apps/nextjs/src/components/user-avatar-menu.tsx:64-65`); el server no
  participa.

## 8. Implementación Go

| Fichero | Contenido |
|---|---|
| `password.go` | `HashPassword` / `ComparePassword` (bcrypt cost 10) |
| `csrf.go` | mint/verify del par csrf (token + `token\|sha256(token+secret)`) |
| `permissions.go` | expansión de permisos de grupo (`groupPermissionParents`) |
| `auth.go` | `Auth`, `Options`, sesiones (create/validate/delete + cookies), `Middleware()` (sesión o API key), resolución de API key |
| `handler.go` | `Handler()` — los 5 endpoints REST + fallback 400 `"Bad request."` |
| `internal/db/account.go` | repos `apiKey`, `UserHasPermission`, `UserPermissions`, `AddUserToGroup`, `GroupIDByName` |
| `internal/db/user.go` | + `UpdateUserPassword` |

Tests (`go test ./internal/auth/... ./internal/db/...`):
`TestComparePasswordUpstreamFixture` (hash `$2b$` real del seed upstream),
`TestHashPasswordFormat`, `TestComparePasswordMalformedHash`,
`TestSessionCreateValidateExpire`, `TestSessionDelete`,
`TestMiddlewareSessionCookie`, `TestMiddlewareAnonymous`,
`TestMiddlewareExpiredSession`, `TestMiddlewareAPIKey` (válido / token
incorrecto / id desconocido / malformado / key sobre sesión),
`TestHandlerLoginFlow` (csrf → login → session → signout → null),
`TestHandlerLoginEveryoneGroupSideEffect`, `TestHandlerLoginWrongPassword`,
`TestHandlerLoginLowercasesName`, `TestHandlerLoginRequiresCSRF` (sin cookie /
doble envío fallido / cookie re-firmada con otro secret),
`TestHandlerSessionAnonymousNull`, `TestHandlerSessionExpiredClearsCookie`,
`TestHandlerSessionAdminPermissions`, `TestHandlerCSRFCookieAndNoCache`,
`TestHandlerProviders`, `TestHandlerUnknownAction`,
`TestHandlerRedirectWithoutHeader`, `TestHandlerCrossOriginCallbackRejected`,
`TestHandlerSignoutRequiresCSRF`, `TestExpandPermissionsAdmin`,
y en db: `TestAPIKeyRoundtrip`, `TestUserPermissionsAndGroups`,
`TestUpdateUserPassword`.

## 9. Montaje

```go
a := auth.New(database, auth.Options{
    Secret: cfg.AuthSecret,      // AUTH_SECRET (se genera si viene vacío: crypto/rand)
    Secure: cfg.ExternalHTTPS,   // scheme visible: https ⇒ Secure + __Host- csrf
})
router.Handle("/api/auth/*", a.Handler())
router.Handle("/api/trpc/*", a.Middleware()(trpcRouter.Handler()))
```
