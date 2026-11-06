#!/usr/bin/env bash
# check-upstream.sh — lista los commits de upstream pendientes de portar al
# backend Go, separados en BACKEND (port manual a server/) y FRONTEND
# (llegan con `git merge upstream/dev`).
#
# Uso:
#   ./server/scripts/check-upstream.sh                  # informe
#   ./server/scripts/check-upstream.sh --update-marker  # informe + avanzar marcador
#
# Emita una línea de resumen `##SUMMARY last=... backend=N frontend=N`
# que consume .github/workflows/upstream-check.yml.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$REPO_ROOT"

MARKER="server/.ported-upstream"
PORT_LOG="server/PORT_LOG.md"

# Directorios upstream que implementan el backend y deben portarse a Go.
# Añadir aquí los paquetes nuevos que se consideren backend.
BACKEND_PATHS=(
  apps/tasks
  apps/websocket
  packages/analytics
  packages/api
  packages/auth
  packages/cron-job-status
  packages/cron-jobs
  packages/cron-jobs-core
  packages/db
  packages/definitions
  packages/docker
  packages/icons
  packages/image-proxy
  packages/integrations
  packages/notifications
  packages/ping
  packages/redis
  packages/request-handler
  packages/server-settings
  packages/settings
  packages/spotlight
)

if [[ ! -f "$MARKER" ]]; then
  echo "ERROR: falta $MARKER (sha de upstream/dev ya portado)." >&2
  exit 1
fi
LAST_SHA=$(cat "$MARKER")

if ! git rev-parse -q --verify "${LAST_SHA}^{commit}" >/dev/null 2>&1; then
  echo "ERROR: $LAST_SHA no existe en este clone. Ejecuta: git fetch upstream dev --tags" >&2
  exit 1
fi

git fetch -q upstream dev
UPSTREAM_SHA=$(git rev-parse upstream/dev)
RANGE="$LAST_SHA..upstream/dev"

BACKEND_LOG=$(git log --oneline --no-decorate "$RANGE" -- "${BACKEND_PATHS[@]}" || true)
EXCLUDES=()
for p in "${BACKEND_PATHS[@]}"; do EXCLUDES+=(":(exclude)$p"); done
FRONTEND_LOG=$(git log --oneline --no-decorate "$RANGE" -- . "${EXCLUDES[@]}" || true)

BACKEND_COUNT=$(git rev-list --count "$RANGE" -- "${BACKEND_PATHS[@]}" || true)
FRONTEND_COUNT=$(git rev-list --count "$RANGE" -- . "${EXCLUDES[@]}" || true)

echo "# Informe upstream → go-rewrite"
echo
echo "- Marcador: \`$LAST_SHA\`"
echo "- upstream/dev: \`$UPSTREAM_SHA\`"
echo "- Backend pendiente (portar a Go): **$BACKEND_COUNT**"
echo "- Frontend pendiente (llega con git merge): **$FRONTEND_COUNT**"
echo
echo "## BACKEND (portar a mano)"
echo '```'
if [[ -n "$BACKEND_LOG" ]]; then echo "$BACKEND_LOG"; else echo "(nada pendiente)"; fi
echo '```'
echo
echo "## FRONTEND (merge git)"
echo '```'
if [[ -n "$FRONTEND_LOG" ]]; then
  echo "$FRONTEND_LOG" | head -50
  if (( FRONTEND_COUNT > 50 )); then
    echo "... (+$((FRONTEND_COUNT - 50)) commits frontend no mostrados)"
  fi
else
  echo "(nada pendiente)"
fi
echo '```'
echo
echo "##SUMMARY last=$LAST_SHA upstream=$UPSTREAM_SHA backend=$BACKEND_COUNT frontend=$FRONTEND_COUNT"

if [[ "${1:-}" == "--update-marker" ]]; then
  printf '%s\n' "$UPSTREAM_SHA" > "$MARKER"
  {
    echo
    echo "## $(date -u +%F) — marcador avanzado a \`$UPSTREAM_SHA\`"
    echo "- Commits backend pendientes al momento del port: $BACKEND_COUNT."
  } >> "$PORT_LOG"
  echo
  echo "Marcador actualizado a $UPSTREAM_SHA y $PORT_LOG anotado."
fi
