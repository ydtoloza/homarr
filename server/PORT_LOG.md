# PORT_LOG — registro de ports upstream → Go

Cada entrada documenta un tramo de commits upstream portado al backend Go.
Ver `docs/PORTING.md` para el protocolo completo.

## 2026-11-06 — inicial

- Marcador `server/.ported-upstream` fijado al commit base de la rama
  `go-rewrite` en `upstream/dev` (v2.11.1, `696576c0c`).
- Todo el port del backend está por delante: ver `server/ARCHITECTURE.md`
  para el mapa de fases M0-M5.
