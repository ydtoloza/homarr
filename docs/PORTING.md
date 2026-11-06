# Porting de updates upstream al backend Go

Cómo recibir las actualizaciones de [homarr-labs/homarr](https://github.com/homarr-labs/homarr)
y traducirlas a este fork. La regla de oro: **port por release, no por commit**.

## Remotes y ramas

| Remote | Repo | Uso |
|---|---|---|
| `origin` | `ydtoloza/homarr` | el fork (rama `go-rewrite`) |
| `upstream` | `homarr-labs/homarr` | el proyecto original (rama `dev`) |

## Protocolo (cada release upstream vX.Y.Z)

```bash
git fetch upstream dev --tags
./server/scripts/check-upstream.sh          # 1. informe: backend vs frontend pendientes
git merge upstream/dev                      # 2. frontend: resolver conflictos (casi siempre apps/nextjs)
# 3. portar cada commit listado en BACKEND → server/ (ver reglas abajo)
go test ./server/... && cd apps/nextjs && bun run build   # 4. verificación
./server/scripts/check-upstream.sh --update-marker        # 5. avanzar marcador + PORT_LOG.md
git tag go-vX.Y.Z                           # 6. tag alineado con la release upstream
git push origin go-rewrite --tags
```

El workflow `upstream-check.yml` ejecuta el paso 1 **cada noche a las 04:00 UTC**
y abre/actualiza un issue si hay commits de backend pendientes. No hay que
acordarse de mirar.

## Reglas de traducción TS → Go

| Upstream | Destino | Nota |
|---|---|---|
| `packages/api/src/router/<x>.ts` | `server/internal/api/router/<x>.go` | mismos procedure paths (la URL es el contrato) |
| Schema Zod de un procedure | validación en el handler Go | mismos códigos de error tRPC (BAD_REQUEST=400, NOT_FOUND=404...) |
| `packages/db` queries (Drizzle) | `server/internal/db/queries` | mismo SQL; SQLite es el mismo motor |
| Integración en `packages/integrations/<name>` | `server/internal/integrations/<name>` | factory + subpaquete `vN` por versión de API |
| `apps/websocket` protocolo | `server/internal/ws` | mensajes JSON idénticos |
| Cron job en `packages/cron-jobs` | `server/internal/jobs` | mismas frecuencias |

## Qué NO se porta

- **Todo lo de UI** (`packages/ui`, `widgets`, `boards`, `modals*`, `forms*`,
  `translation`, `onboarding`, `spotlight` cliente): llega con el `git merge`
  y se recompila como SPA estática.
- Workflows/docs/config de CI upstream: ignorar salvo cambios de protocolo.

## Criterios de aceptación de un port

1. `go test ./server/...` en verde.
2. La UI (SPA export servida por Go) funciona contra los routers portados.
3. `PORT_LOG.md` anota qué se portó y el marcador avanza.
4. Si el commit toca protocolo (tRPC envelope, WS, esquema DB): se documenta
   en `server/ARCHITECTURE.md` y se añade un test con fixture real.
