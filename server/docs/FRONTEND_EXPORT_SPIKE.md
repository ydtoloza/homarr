# Spike: export estático del frontend (`output: 'export'`) — viabilidad y plan

Análisis estático (sin builds) del árbol upstream TS para decidir si el frontend
Next.js puede compilarse como SPA estática y ser embebida en el binario Go
(plan en `server/ARCHITECTURE.md`). Todas las rutas son relativas a la raíz del
repo. Fecha: 2026-11-06, sobre `upstream/dev` actual del fork.

## Veredicto: **CONDITIONAL-GO**

`output: 'export'` es **técnicamente viable pero no en el estado actual**: el
build falla hoy (layout raíz con `cookies()`/`auth()`, ~32 páginas con
`searchParams`, cero `generateStaticParams`, route handlers POST en `app/api/*`).
Es viable si se acepta convertir la capa de datos de las páginas a client
components (~60 ficheros) y trasladar proxy/manifest/headers a Go. La capa
cliente ya es autosuficiente (154 ficheros `"use client"`, `ClientBoard`,
react-query + tRPC HTTP/WS ya transportan todos los datos en runtime), así que
la conversión es mecánica, pero toca justo los ficheros que upstream cambia más.
Si se prioriza fricción de merge ≈ 0 sobre el objetivo de RAM (<100 MB), usar
los planes alternativos del final.

## 1. Mapa de rutas (`apps/nextjs/src/app`)

### Fuera de `[locale]` (todo server-side, se traslada a Go)

| Ruta | Qué es | Destino en el fork |
|---|---|---|
| `app/api/**` | route handlers (ver lista completa abajo) | endpoints Go (`server/internal/api`, …) |
| `app/manifest.ts` | PWA manifest con branding de DB (`getRscServerSettingsAsync`), `dynamic = "force-dynamic"` | endpoint Go `/manifest.webmanifest` |
| `app/well-known/[...path]/route.ts` | metadata OAuth (authorization-server / protected-resource) del MCP, CORS `*` | endpoint Go `/.well-known/…` |
| rewrites de `next.config.ts` (líneas 91-99) | `/.well-known/:path*` → `/well-known/:path*` y `/api/mcp/:transport/.well-known/:path*` | routing del httpserver Go |
| `public/` (`sw.js`, `favicon.ico`, `images/pwa/*`, `logo/`, `sql-wasm.wasm`) + `public/tabler-icons/<versión>/` generado por `apps/nextjs/scripts/prepare-tabler-icons.mjs` | estáticos | quedan en el export / embebidos |

### Dentro de `[locale]/`

- `(home)/(board)/page.tsx` — board home (Server Component que delega en
  `boards/(content)/(home)/_definition.ts` → `getHomeBoardAsync`).
- `auth/login` (Server Component con `auth()` + `searchParams`), `auth/invite/[id]`,
  `auth/mcp/*` (consentimiento OAuth MCP; CSP propia vía `headers()` del config).
- `boards/(content)/(home)` y `boards/(content)/[name]/(board)` — vista de board
  (factory `boards/(content)/_creator.tsx`); `boards/[name]/settings` — editor;
  `boards/[name]/layout.tsx` (factory `boards/_layout-creator.tsx`).
- `widgets/[kind]` — vista standalone de un widget (RSC + `getHomeBoardAsync`).
- `custom-widget-configuration/[token]` — RSC mínima (auth/permiso) que monta
  `configuration-entry.tsx` (cliente, ya hace fetch propio).
- `init` — onboarding (Server Component que consulta DB directamente).
- `manage/**` — panel de administración completo: `about`, `apps`
  (list/new/edit), `assistant`, `boards`, `custom-widgets`
  (list/new/edit/preview/publish/workshop), `integrations` (list/new/edit),
  `medias`, `search-engines` (list/new/edit), `settings`, `tools/`
  (api, backup, certificates, docker + logs, kubernetes×10 páginas, logs,
  tasks), `users` (list/create/[userId] general+security, groups, invites),
  `workshop`. Todas las `page.tsx` son Server Components con `auth()`,
  `searchParams` y/o `api` (caller tRPC in-process).
- `reference/api` — Scalar (consume `/api/openapi`).
- `_client-providers/` — providers cliente (trpc, session, jotai, mantine, dayjs).
- `not-found.tsx` / `error.tsx` / `loading.tsx` por sección; catch-all
  `manage/[...not-found]/page.tsx`.

### Route handlers a reemplazar por Go (inventario completo de `route.ts`)

```
api/[...trpc]/route.ts              api/trpc/[trpc]/route.ts        (mismo handler tRPC, dos montajes)
api/auth/[...nextauth]/route.ts     Auth.js v5 (credentials/oidc/ldap)
api/assistant/chat/route.ts         chat IA streaming (OpenRouter, tools)
api/backup/export|import|migrations|sql-wasm/route.ts
api/custom-widgets/[resource]/route.ts
api/custom-widgets/configuration-request/[token]/route.ts
api/health/live/route.ts            api/health/ready/route.ts
api/image-proxy/[id]/route.ts       api/user-medias/[id]/route.ts
api/mcp/route.ts  api/mcp/[transport]/route.ts  api/mcp/oauth/{authorize,register,token}/route.ts
api/onboarding/claim/route.ts       api/openapi/route.ts (trpc-to-openapi)
api/about/contributors/{crowdin,github,opencollective}/route.ts   (force-static)
api/about/dependencies/route.ts                                   (force-static)
```

## 2. Auditoría de ejecución server-side (bloqueos del export)

### 2.1 `apps/nextjs/src/proxy.ts` (el middleware de Next 16 se llama `proxy.ts`)

- Redirect a `/init` si el onboarding no terminó: lee DB con una conexión
  propia edge-safe (`packages/db/proxy-reader.ts`: `onboarding.step` y
  `serverSettings.culture`).
- Negocia locale: cookie `homarr-locale` (`@homarr/definitions/cookie`) o
  default de DB; limpia `accept-language`; delega en
  `createI18nMiddleware` (`packages/translation/src/middleware.ts`).
- El matcher excluye `api|static|_next|ficheros|robots.txt`.
- **Es route-based** (redirect + reescritura interna del pathname): el
  equivalente Go es un handler que (1) redirija a `/init` según el step y
  (2) fije la cookie de locale. Como `localePrefix` es `"never"`
  (`packages/translation/src/routing.ts`), no hay URLs por idioma que
  redirigir: la reescritura `/{locale}/...` desaparece si las rutas pasan a
  ser client-rendered. Esfuerzo **M**.

### 2.2 Layout raíz `[locale]/layout.tsx` — bloqueo duro

Server Component async: `auth()`, `getRscServerSettingsAsync()`,
`getRscUserSettingsAsync()`, `getRscAssistantAvailabilityAsync()`,
`getCurrentColorSchemeAsync()` (`apps/nextjs/src/theme/color-scheme.ts`,
`cookies()`), `getI18n("metadata")` en `generateMetadata`/
`generateViewport`. Con export esto no prerrenderiza. La conversión es:
layout cliente que obtiene settings/usuario vía tRPC (`serverSettings.*`,
`user.*` ya existen como routers) e idioma via provider cliente.
**Este fichero es el de mayor conflicto de merge** (upstream lo edita a
menudo); mantenerlo como shell fino y reproducir los providers tal cual.

### 2.3 RSC data fetching (el bloqueo mayor)

- `packages/api/src/rsc-context.ts`: `cache()` + `headers()` + `auth()` →
  `createTRPCContext`. Solo lo usan `packages/api/src/server.ts` (caller
  `api` + `getQueryClient` para dehydrate) y los helpers
  `*-server.ts` (`server-settings-server.ts`, `user-server.ts`,
  `board-server.ts`, `assistant-availability-server.ts`). El wiring es
  profundo pero **confinado al server**; el cliente nunca lo ve.
- ~60 ficheros bajo `apps/nextjs/src/app/[locale]` importan `@homarr/api/server`
  o los `*-server.ts`. Patrón dominante (p. ej.
  `manage/apps/page.tsx`): `auth()` → chequeo de permisos →
  `searchParams` (dinámico) → `api.app.getPaginated` (query directa a DB)
  → `redirect()`/`notFound()` → `getI18n(...)`. **Cada una de esas páginas
  debe volverse client component** con `useQuery`/`useSuspenseQuery` tRPC
  (todos los procedimientos ya existen).
- Boards: `boards/(content)/_creator.tsx` + `boards/_layout-creator.tsx`
  hacen auth + board + `getInitialViewportWidthAsync` (cookie
  `homarr-board-viewport-width` + header `sec-ch-viewport-width` + UA) +
  prefetch de widget queries + `dehydrate`/`HydrationBoundary` (streaming
  SSR). En SPA el board se carga por tRPC client
  (`board.getByNames`/`getHomeBoard`); el prefetch SSR solo era una
  optimización — los widgets ya refetchean por react-query
  (`_client-providers/trpc.tsx`, `widgetQueryRefetchIntervals`).
- 32 ficheros usan `searchParams`; cero `generateStaticParams` en el app.
- Encuestas dinámicas de Next: solo `manifest.ts` usa
  `dynamic = "force-dynamic"`; los 4 de `about/*` son `force-static`.

### 2.4 i18n: next-intl 4.13.1, catálogos server-side hoy

- `packages/translation/src/request.ts`: `getRequestConfig` carga los JSON
  (`lang/<locale>.json`, dynamic import + deepmerge del fallback) en el
  server por request; el plugin `createNextIntlPlugin` de `next.config.ts`
  apunta ahí.
- El cliente recibe los mensajes **serializados en el RSC payload** vía
  `NextIntlClientProvider` sin props (hereda del server).
- `getI18n` (= `getTranslations` de `next-intl/server`,
  `packages/translation/src/server.ts`) se usa en todas las páginas RSC
  convertidas.
- Conversión: cargar mensajes client-side (dynamic import del JSON por
  locale — el paquete ya lo hace para dayjs/mantine-react-table en
  `packages/translation/src/config.ts`) + `useTranslations` en las páginas.
  `localePrefix: "never"` significa que el locale vivo en cookie basta; no
  hay que generar 35 variantes por página. Esfuerzo **L** pero mecánico y
  repartido en los mismos ficheros que 2.3.

### 2.5 `instrumentation.ts` / `instrumentation-node.ts` — no bloquea

En producción arrancan `@homarr/tasks` (cron) y `@homarr/websocket` (WS en
puerto 3001) **embebidos en el proceso Next**
(`apps/nextjs/src/instrumentation-node.ts`). En el fork esos procesos son
módulos Go (`server/internal/jobs`, `server/internal/ws`); para el build
del frontend solo hay que no ejecutarlo (es server-only y con export ni se
empaqueta). En desarrollo ya existe el noop
(`instrumentation-noop.ts` via alias de `next.config.ts`).

### 2.6 `next.config.ts` — flags que interactúan con el export

- `output: "standalone"` → `output: "export"` (+ `images.unoptimized: true`;
  solo 4 ficheros usan `next/image`: `manage/about`, `manage/medias`,
  `kubernetes/.../resource-tile`, `components/layout/logo/logo.tsx`).
- `rewrites()` (well-known) y `headers()` (CSP global, cache de
  `/tabler-icons/*` y `/sw.js`, CSP/X-Frame-Options de MCP) **no se
  aplican en export** → trasladar al httpserver Go (`server/internal/httpserver`).
- `serverExternalPackages` (dockerode, better-sqlite3…): irrelevante en export.
- `turbopack: { root }` y alias de dev: se mantienen.
- Esfuerzo **S**; superficie de conflicto pequeña (un fichero, edits puntuales).

### 2.7 Server actions — se pueden dejar intactos

Único `"use server"`: `packages/common/src/revalidate-path-action.ts`
(`revalidatePath`). ~15 client components llaman `revalidatePathActionAsync`
tras mutar (p. ej. `manage/apps/_app-delete-button.tsx`). En SPA la llamada
POST llegaría al servidor Go: basta que responda **204** a cualquier request
con header `Next-Action` → no-op sin tocar código upstream
(la UI ya refresca vía invalidación react-query). Esfuerzo **S**, fricción 0.

## 3. Contratos cliente ↔ backend (lo que el Go debe hablar)

### 3.1 tRPC (`apps/nextjs/src/app/[locale]/_client-providers/trpc.tsx`)

- `@trpc/client` 11.18.0; URL `/api/trpc` (`packages/api/src/shared.ts`,
  `trpcPath`), transformer **superjson**, header `x-trpc-source`.
- Por defecto **`httpBatchStreamLink`** (batch activo, `?batch=1`,
  `maxURLLength: 2083`, respuesta en stream NDJSON).
- `widget.releases.getLatest` → mismo link con `methodOverride: "POST"`.
- Input no serializable (FormData/uploads) → `httpLink` con transformer
  identidad al serializar (raw multipart/form-data) y superjson al leer.
- Suscripciones → `splitLink`: `widget.beszel.subscribeSystemStats` usa
  **`httpSubscriptionLink` (SSE, `withCredentials`)**; el resto **`wsLink`**
  contra `/websockets` (misma host:port en producción; dev
  `ws://localhost:3001/websockets`).
- `ReactQueryStreamedHydration` (`@tanstack/react-query-next-experimental`)
  queda inerte en SPA (no hay SSR).
- Routers tRPC a portar (29, `packages/api/src/root.ts`): assistant, user,
  userItemState, group, invite, integration, board, section, app,
  searchEngine, widget, location, log, icon, onboard, home, docker,
  kubernetes, serverSettings, cronJobs, apiKeys, media, updateChecker,
  certificates, bangs, analytics, info, customWidget (+ middlewares de
  onboarding/permisos en `packages/api/src/trpc.ts`).

### 3.2 WebSocket (`apps/websocket/src/main.ts`)

- `ws` `WebSocketServer` **puerto 3001**; nginx proxea `/websockets` → 3001
  (`nginx.conf`). El Go servirá `/websockets` directamente.
- Handler tRPC oficial (`applyWSSHandler`): mensajes JSON tRPC
  `{id, method: query|mutation|subscription, params:{path, input}}` con
  superjson, incl. `reconnect`/`stop`/`subscription.state`.
- Auth por cookie de sesión (`getSessionFromToken`); **API keys rechazadas**.
- keepAlive: ping cada 30 s, cierra si no hay pong en 5 s;
  `broadcastReconnectNotification` en SIGTERM.
- Consumidores: docker logs, log de la app, estado de cron jobs, y datos de
  widget vía subscriptions (los datos de widget van sobre este WS por el
  `wsLink`; solo beszel por SSE).

### 3.3 fetch() directo del cliente (fuera de tRPC)

| Llamada | Uso | Fichero |
|---|---|---|
| `GET /api/backup/export` | descargar zip de backup | `manage/tools/backup/_components/backup-export-card.tsx` |
| `POST /api/backup/import` (multipart) | restaurar | `components/backup/database-restore-flow.tsx` |
| `GET /api/backup/migrations`, `GET /api/backup/sql-wasm` | análisis de backup con sql.js (wasm en `public/`) | `components/backup/use-backup-analysis.ts` |
| `GET /api/health/ready` (polling) | esperar fin de restore | `components/backup/database-restore-flow.tsx` |
| `POST /api/custom-widgets/configuration-request/[token]` | preview de custom widget | `custom-widget-configuration/[token]/configuration-entry.tsx` |
| `HEAD <mediaUrl>` | detectar si el fondo del board es vídeo | `components/layout/background.tsx` |
| `GET /api/auth/session` + `/api/auth/*` | sesión Auth.js (`next-auth/react`, `packages/auth/client.ts`) | providers cliente |

Recursos por URL (sin fetch JS, los pide el navegador): `/api/user-medias/[id]`
(`packages/icons/src/local-media-url.ts`), `/api/image-proxy/[id]`
(`packages/image-proxy/src/index.ts`), `/tabler-icons/<v>/*.svg`, `/sw.js`,
`/images/pwa/*`, `/logo/*`, `/manifest.webmanifest`, `/.well-known/*`.

## 4. Notas de versión

- Catálogo raíz (`package.json`): `next 16.4.2`, `react 19.2.9`,
  `next-intl 4.13.1`, `@trpc/client`+`@trpc/server 11.18.0`,
  `@tanstack/react-query 5.101.2`, `superjson 2.2.6`.
- Turbopack: en Next 16 es el bundler por defecto de `next build`
  (el config ya usa la sección `turbopack:` y
  `experimental.turbopackFileSystemCacheForBuild`).
- Restricciones de `output: 'export'` en esta major **[conocimiento, no
  verificado en runtime]**: rewrites/headers/redirects del config
  ignorados o error; `proxy.ts`/middleware no se ejecuta; route handlers
  solo estáticos (GET sin request) y los POST rompen el build;
  `dynamic = "force-dynamic"` y `cookies()`/`headers()`/`searchParams` en
  render fuerzan dinámico → error de prerrender; segmentos dinámicos
  (`[locale]`, `[name]`, `[id]`) exigen `generateStaticParams`;
  `images.unoptimized` obligatorio. Turbopack soporta export en Next 16
  (no hay blockers documentados de combinación).
- Implicación práctica para Homarr: como el locale vive en cookie
  (`localePrefix: "never"`), lo correcto no es prerrenderizar
  `[locale]`×35 sino **una sola copia client-rendered por ruta**; el Go
  sirve el `index.html` del shell para cualquier ruta no-fichero (fallback
  SPA) y el router cliente resuelve.

## 5. Bloqueos rankeados (esfuerzo S/M/L)

| # | Bloqueo | Esfuerzo | Ficheros clave |
|---|---|---|---|
| 1 | Convertir páginas RSC a client components (auth/searchParams/datos → tRPC client) | **L** | ~60 `page.tsx`/layouts bajo `app/[locale]/**`, `boards/(content)/_creator.tsx`, `boards/_layout-creator.tsx` |
| 2 | i18n server → cliente (mensajes por import dinámico, `getI18n` → `useTranslations`) | **L** | `packages/translation/src/*`, mismas páginas que #1 |
| 3 | `proxy.ts` (locale + onboarding redirect + i18n middleware) → Go o guard cliente | **M** | `apps/nextjs/src/proxy.ts`, `packages/db/proxy-reader.ts`, `packages/translation/src/middleware.ts` |
| 4 | Excluir `app/api/**` del build de export (los POST rompen) y servirlos desde Go | **M** | script de build del fork; implementación ya planificada en M1-M4 |
| 5 | Root layout client-side (providers sin RSC) | **M** (incluido en #1 pero es el fichero más caliente) | `app/[locale]/layout.tsx`, `theme/color-scheme.ts` |
| 6 | `next.config.ts`: `output: export`, `images.unoptimized`, quitar rewrites/headers → Go | **S** | `apps/nextjs/next.config.ts` |
| 7 | `manifest.ts` + `well-known` → endpoints Go | **S** | `app/manifest.ts`, `app/well-known/[...path]/route.ts` |
| 8 | Server actions → no-op 204 en Go (header `Next-Action`) | **S** | `packages/common/src/revalidate-path-action.ts` + ~15 llamadores |
| 9 | Auth.js wire (`/api/auth/*`) en Go | **M** | `packages/auth/*` (ya en plan M1; no bloquea el export en sí) |
| 10 | Perder prefetch SSR de widgets (primer render algo más lento) | **S** | aceptable: los widgets ya refetchean client-side |

## 6. Checklist de conversión (orden propuesto)

1. `next.config.ts`: `output: "export"`, `images.unoptimized: true`;
   mover rewrites/headers/CSP al httpserver Go (miro `server/internal/httpserver`).
2. Script fork-only de build que copie `apps/nextjs/src` a un directorio de
   build y **elimine** `app/api/**`, `app/well-known`, `app/manifest.ts`,
   `proxy.ts`, `instrumentation*.ts` antes de `next build` (el árbol
   upstream queda intacto → fricción de merge 0 en ese paso).
3. Root layout → client shell: providers actuales (`_client-providers/*`)
   alimentados por tRPC client (`serverSettings`, `user`, assistant
   availability) y color scheme por cookie leída en `document.cookie`.
4. i18n client-side: provider de next-intl con mensajes importados
   dinámicamente por cookie; eliminar `getRequestConfig` del runtime
   (dejarlo solo para el build si el plugin lo exige).
5. Convertir páginas (de menor a mayor riesgo): `manage/*` (los formularios
   ya son client components; solo cambia la carga inicial y el guard de
   sesión) → `boards/*` (board + layout id por `window.innerWidth` en vez
   de cookie/header) → `(home)/(board)` → `widgets/[kind]` →
   `auth/*`/`init` (el onboarding puede quedar client-side leyendo el step
   por tRPC; el redirect a `/init` lo decide el Go o un guard cliente).
6. Guards de sesión client-side (`useSession`) en lugar de `redirect()` RSC.
7. `revalidatePathActionAsync` → no-op del Go (204 ante `Next-Action`);
   verificar que cada flujo refresca por invalidación de react-query.
8. Endpoints Go fuera del SPA: lista completa en §1 (`api/**`,
   `manifest.webmanifest`, `/.well-known/*`, `/websockets`, `/api/auth/*`,
   `/_next/image` no existe → no aplica).
9. Validar contra el frontend real: batch tRPC (`?batch=1`), superjson,
   SSE beszel, WS handshake en `/websockets`, cookies de sesión.

### Compatibilidad con `git merge upstream/dev`

- **Enfoque overlay** (paso 2): el fork nunca edita los ficheros upstream;
  los reemplazos client-side viven en `server/frontend/overlay/` y el
  script de build los aplica sobre una copia. Los cambios upstream en
  client components internos (el 90% del código) siguen entrando limpios;
  los cambios en el data-layer RSC de las páginas convertidas requieren
  port manual — detectables con `server/scripts/check-upstream.sh`.
- Evitar reescribir componentes: los wrappers deben renderizar los
  componentes cliente upstream tal cual (solo cambia de dónde vienen los
  props iniciales).
- Los ficheros con mayor conflicto esperado: `app/[locale]/layout.tsx`,
  `boards/(content)/_creator.tsx`, `next.config.ts`. Mantener los diffs
  mínimos y agrupados para resolverlos rápido en cada merge.

## 7. Alternativas si se descarta el export

| Opción | Qué es | Pro | Contra |
|---|---|---|---|
| (a) **Vite SPA** | mismo árbol React (`packages/ui`, `widgets`, `modals`… ya se transpilan), entry + router propios (react-router), tRPC/WS idénticos | merges upstream casi intactos (solo llegan a nivel componente); bundle y RAM controlados; sin RSC en ningún sitio | port del shell/layout/i18n al idioma de Vite (**M-L**); perder convenciones de App Router (metadata, next/font); repetir glue que upstream mantiene en `apps/nextjs` |
| (b) **Node BFF temporal** | el binario Go lanza/supervisa el `next start` standalone actual con tope de memoria (`NODE_OPTIONS=--max-old-space-size=…`), y va absorbiendo routers tRPC hacia Go iterativamente | cero cambios de frontend; merges 100% intactos; migración incremental (la que ya define M1-M4) | objetivo de RAM comprometido (~250-450 MB mientras Node viva); dos runtimes en la imagen; complejidad de supervisor |

Recomendación: intentar el export con el enfoque overlay (§6). Si el coste
de port de páginas supera lo estimado en M1, caer a (b) Node BFF y dejar
(a) Vite como reescritura a largo plazo.
