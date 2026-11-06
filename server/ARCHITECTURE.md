# homarr-go — arquitectura del port

Reescritura del backend de Homarr en Go: **un solo binario** que reemplaza los
4 procesos del runtime oficial (nginx + redis-server + Next.js/Node + apps
websocket/tasks). El frontend React se conserva: se compila como SPA estática
(`next build` con `output: export`) y el binario Go lo embebe con `go:embed`.

Objetivo de recursos: **< 100 MB RSS** (upstream: ~500-800 MB).

## Mapa de porting: paquete TS (upstream) → módulo Go

| Paquete upstream | Rol | Destino en Go |
|---|---|---|
| `apps/nextjs` | UI + SSR + API Next.js | frontend estático embebido (`server/frontend/dist`) + `server/internal/api` |
| `packages/api` | routers tRPC (~33) | `server/internal/api` (wire protocol tRPC compatible) |
| `packages/auth` | credenciales/OIDC/LDAP/sesiones/API keys | `server/internal/auth` |
| `packages/db` | esquema + queries (Drizzle, sqlite/pg) | `server/internal/db` (`modernc.org/sqlite`, DDL portado de `schema/sqlite.ts`) |
| `packages/redis` | pubsub/caché entre procesos | `server/internal/bus` (pubsub in-process; Redis real opcional detrás de la misma interfaz) |
| `apps/websocket` | servidor WS de eventos del board | `server/internal/ws` (mismo protocolo JSON) |
| `apps/tasks` | cron jobs | `server/internal/jobs` |
| `packages/cron-jobs(-core,-status)` | definición/estado de jobs | `server/internal/jobs` |
| `packages/integrations` | 53 integraciones versionadas | `server/internal/integrations/<name>/` (factory + `vN`, espejo del patrón TS) |
| `packages/docker` | dockerode (stats/logs) | `server/internal/integrations/docker` (Docker Engine API) |
| `packages/icons` | proxy de iconos | `server/internal/api/icons` |
| `packages/image-proxy` | proxy de imágenes | `server/internal/api/imageproxy` |
| `packages/spotlight` | búsqueda global | `server/internal/spotlight` |
| `packages/ui`, `widgets`, `boards`, `modals`, `forms*`, `translation`, `onboarding` | UI React | **no se porta** — llega por `git merge upstream/dev` y se recompila |

## Layout del módulo Go

```
server/
  cmd/homarr-go/          main: config, logging, graceful shutdown
  embed.go                go:embed del frontend (frontend/dist)
  frontend/dist/          salida de `next build` (SPA export); placeholder hasta M1
  internal/
    config/               HOMARR_PORT (7575), HOMARR_DATA_DIR (/appdata), HOMARR_GOMEMLIMIT
    httpserver/           chi: health, SPA fallback, logging; mount points tRPC/WS
    api/        (M1+)     wire protocol tRPC + routers (uno por router TS)
    auth/       (M1+)     sesiones cookie + argon2id, OIDC, LDAP, API keys
    db/         (M1+)     SQLite (modernc, sin CGO): DDL de packages/db/schema/sqlite.ts
    bus/        (M3+)     pubsub in-process (reemplaza Redis)
    ws/         (M3+)     hub WebSocket (protocolo de apps/websocket)
    jobs/       (M3+)     scheduler de cron jobs (reemplaza apps/tasks)
    integrations/ (M3+)   <name>/factory.go + <name>/vN/...
  scripts/check-upstream.sh   commits upstream pendientes (backend vs frontend)
  .ported-upstream            sha de upstream/dev ya portado (avanza con --update-marker)
```

## Decisiones técnicas

1. **`modernc.org/sqlite` (pure Go, sin CGO)**: abre **el mismo `db.sqlite`**
   de una instancia Homarr real (formato estándar SQLite) → migración de datos
   sin conversión. Permite cross-compile (amd64/arm64/Windows) e imagen
   distroless.
2. **Redis eliminado**: el pubsub entre los 4 procesos upstream existía porque
   eran procesos separados; en un proceso es un hub con canales Go. Si algún
   día se necesita multi-instancia, se añade un driver Redis tras la misma
   interfaz `bus`.
3. **nginx eliminado**: TLS delegado al reverse proxy externo (Traefik/Caddy/
   nginx del host), como el resto de stacks self-hosted. ACME interno: opcional
   más adelante.
4. **tRPC wire protocol en Go**: el frontend `@trpc/client` habla JSON puro
   sobre HTTP (`/api/trpc/<procedure>`, batch `p1,p2?batch=1`, envelope
   `{"result":{"data":...}}`, errores `{"error":{"json":{"message","code"}}}`).
   Un `trpc` package pequeño lo implementa; el frontend no se entera del
   cambio de lenguaje. Validar contra tráfico real en M1 (captura devtools).
5. **i18n intacta**: los catálogos JSON (`packages/translation`, Crowdin) viajan
   con el frontend; cero trabajo de traducción perdido.
6. **Licencia Apache-2.0 upstream**: se mantiene LICENSE y atribución.

## Fases

- **M0** (hecho): fork, remotes, scaffold, CI, flujo de porting, docs.
- **M1**: spike tRPC en Go contra el frontend real + DDL SQLite + auth
  credentials/sesiones. *Go/no-go del SPA export en este hito.*
- **M2**: routers de boards/administración → editor drag&drop funcional.
- **M3**: WS hub + cron jobs + integraciones núcleo (ping, docker, status).
- **M4**: integraciones por lotes, image-proxy, certificates, invites,
  onboarding, kubernetes, MCP (opcional).
- **M5**: paridad upstream vs fork (diff de respuestas tRPC, e2e Playwright),
  imagen multi-arch, medición final de RAM (`server/BASELINE.md`).

## Riesgos abiertos

- **SPA export de Next.js**: requiere auditar Server Components y el routing
  `[locale]`. Si fuera inviable: (a) migrar frontend a Vite-SPA (mismo React,
  build propio) o (b) Node BFF temporal con tope de memoria. Decisión en M1.
- **Protocolo WS**: replicar el de `apps/websocket/src/main.ts` (M3, se
  documenta aquí al capturarlo).
