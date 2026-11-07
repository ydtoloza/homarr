# BOARDS.md — routers tRPC del dominio de boards (board, section, app, userItemState)

Documento de referencia del paquete `server/internal/api/router/board`: el
port en Go de los 4 routers tRPC del dominio de dashboards de upstream.
Complementa `server/internal/api/PROTOCOL.md` (contrato del wire) y
`server/internal/db/SCHEMA.md`. Todas las afirmaciones citan fuente upstream;
las desviaciones están marcadas. Fecha: 2026-11-06.

Fuentes citadas (todas en este repo):

- **[board]** `packages/api/src/router/board.ts` (boardRouter, 3103 líneas).
- **[access]** `packages/api/src/router/board/board-access.ts` (throwIfActionForbiddenAsync).
- **[bounds]** `packages/validation/src/board-layout-bounds.ts` + `board-layout-bounds-error.ts`.
- **[custom]** `packages/api/src/router/board/custom-widget-placement-access.ts`.
- **[section]** `packages/api/src/router/section/section-router.ts`.
- **[app]** `packages/api/src/router/app.ts` + `router/app/app-access-control.ts`.
- **[uis]** `packages/api/src/router/user-item-state.ts`.
- **[val-board]** `packages/validation/src/board.ts`, **[val-shared]** `packages/validation/src/shared.ts`, **[val-app]** `packages/validation/src/app.ts`, **[val-common]** `packages/validation/src/common.ts`, **[val-perms]** `packages/validation/src/permissions.ts`.
- **[perms-def]** `packages/definitions/src/permissions.ts` (groupPermissionParents, getPermissionsWithChildren), **[board-def]** `packages/definitions/src/board.ts`, **[section-def]** `packages/definitions/src/section.ts`, **[widget-def]** `packages/definitions/src/widget.ts`.
- **[auth-board]** `packages/auth/permissions/board-permissions.ts` (constructBoardPermissions), **[auth-int]** `packages/auth/permissions/integration-permissions.ts`, **[auth-cb]** `packages/auth/callbacks.ts` (getCurrentUserPermissionsAsync).
- **[schema]** `packages/db/schema/sqlite.ts`, **[seed]** `packages/db/migrations/seed.ts`.
- **[common]** `packages/common/src/grid-algorithm.ts` + `packages/common/src/layout-resize-source.ts`.
- **[int-access]** `packages/api/src/router/integration/integration-access.ts`.

## 1. Registro

```go
r := trpc.New()
board.Register(r, database, board.Options{})
// chi: r.Handle("/api/trpc/*", r.Handler())
```

`Register` (register.go) registra los 4 routers con sus **paths punteados
exactos** de [root] `packages/api/src/root.ts:8,12-14`: `board.*` (26
procedimientos: 25 portados + 1 diferido), `section.changeCollapsed`,
`app.*` (11: 10 portados + 1 diferido) y `userItemState.*` (2). Pánicos por
path duplicado (los detecta `trpc.Router.Register`).

`Options` (register.go):

- `DeviceTypeFromContext func(ctx) string` — el tipo de dispositivo para
  `board.getHomeBoard`. Upstream lo resuelve del header User-Agent
  ([board] trpc context `packages/api/src/trpc.ts:65`); el middleware de
  httpserver deberá inyectarlo en el contexto de la petición. Nil o valor
  distinto de `"mobile"`/`"desktop"` ⇒ `"desktop"`.

## 2. Modelo de permisos

**Permisos globales.** Upstream viaja en la sesión
(`session.user.permissions` = permisos de los grupos del usuario expandidos
con `getPermissionsWithChildren`, [auth-cb] callbacks.ts:10-21). La sesión Go
(`ctxauth.Session`) es mínima: el paquete resuelve los permisos desde la BD
(`ListGroupPermissionsForUser`, `SELECT DISTINCT ... groupPermission ⋈
groupMember`) y aplica la misma expansión transitiva sobre
`groupPermissionParents` ([perms-def] permissions.ts:57-71), incluida la
cadena `admin → board-full-all → board-modify-all → board-view-all` y
`board-create`. `ctxauth.Session.IsAdministrator` (extensión del fork) equivale
al permiso `admin` — para un upstream fiel, el usuario debe pertenecer a un
grupo con la fila `groupPermission('admin')`.

**Procedimientos protegidos** ([board] packages/api/src/trpc.ts:163-207):

| Gate | Error |
|---|---|
| `protectedProcedure` (anónimo) | `UNAUTHORIZED`, mensaje `"UNAUTHORIZED"` (tRPC v11 hace fallback al código: `message ?? cause?.message ?? opts.code`, TRPCError.ts:64) |
| `permissionRequiredProcedure.requiresPermission(p)` sin el permiso | `FORBIDDEN "Permission denied"` |
| Guard de board sin el nivel requerido o board inexistente | `NOT_FOUND "Board not found"` (anti-snooping, [access] board-access.ts:45-64,84-89) |

⚠️ `permissionRequiredProcedure` extiende `protectedProcedure` ([board]
trpc.ts:195-207): un anónimo en `board.exists`/`app.*` con permiso recibe
**UNAUTHORIZED**, no FORBIDDEN.

**Matriz de acceso a boards** (`constructBoardPermissions`, [auth-board]
board-permissions.ts:24-47; port en perms.go):

| Nivel | Lo otorga |
|---|---|
| full | ser creator · fila user/board perm `full` · permiso de grupo `full` · global `board-full-all` |
| modify | full ∧ (creator · `modify`/`full` user/grupo · global `board-modify-all`/`board-full-all`) |
| view | modify ∧ (creator · cualquier fila user/grupo · board público · global `board-view-all`) |

Escalera de `throwIfActionForbiddenAsync` ([access] board-access.ts:49-63):
`full` cubre todo; `modify` cubre modify+view; `view` solo view.

**Filtro de visibilidad** (`getAccessibleBoardsWhere`, [board] board.ts:2932-2939):
`is_public = 1 OR creator_id = userId OR id ∈ boardIds`, donde `boardIds`
son los boards compartidos por `boardUserPermission` + `boardGroupPermission`
de los grupos del usuario (`getBoardAccessContextAsync`, board.ts:2868-2902).
Con `board-view-all` en la sesión: sin filtro.

## 3. Inventario de procedimientos

### board (boardRouter, [board] board.ts:265-2485)

| Procedimiento | Línea | Permiso | Entrada (zod → JSON) | Salida | Estado |
|---|---|---|---|---|---|
| `board.exists` | 266 | `board-create` | `string` (sin validación de nombre) | `boolean` | ✅ port |
| `board.getPublicBoards` | 280 | público | — | `[{id,name,logoImageUrl}]` | ✅ port |
| `board.getBoardsForGroup` | 290 | `admin` | `{groupId}` | `[{id,name,logoImageUrl}]` | ✅ port |
| `board.getAllBoards` | 321 | público | `void` | `boardSummary[]` (§4.1) | ✅ port |
| `board.getManageOverview` | 375 | público (con `userId` solo admin) | `{fullPreview?,previewRowLimit?,userId?}` opcional | `board+preview[]` | ✅ port (M4, manageoverview.go) |
| `board.catalog` | 640 | público | — | `[{id,name,logoImageUrl,permissions,isHome,isMobileHome}]` | ✅ port |
| `board.search` | 643 | público | `{query, limit=10 (1-100)}` | ídem | ✅ port |
| `board.createBoard` | 652 | `board-create` | `{name, columnCount(1-24), isPublic}` | `{boardId,name,layoutId}` | ✅ port |
| `board.duplicateBoard` | 731 | `board-create` + view | `{id, name}` | `{boardId}` | ✅ port |
| `board.renameBoard` | 1039 | full | `{id, name}` | `void` | ✅ port |
| `board.changeBoardVisibility` | 1064 | full | `{id, visibility: public\|private}` | `void` | ✅ port |
| `board.deleteBoard` | 1102 | full | `{id}` | `void` | ✅ port |
| `board.setHomeBoard` | 1126 | view | `{id}` | `void` | ✅ port |
| `board.setMobileHomeBoard` | 1149 | view | `{id}` | `void` | ✅ port |
| `board.getHomeBoard` | 1172 | público | — | full board (§4.3) | ✅ port |
| `board.getBoardByName` | 1195 | view | `{name}` (nombre case-insensitive) | full board (§4.3) | ✅ port |
| `board.getBoardSettings` | 1210 | modify | `{id}` | settings (§4.4) | ✅ port |
| `board.saveLayouts` | 1269 | modify | `{id, layouts[]}` | `layouts[]` | ✅ port |
| `board.resetLayout` | 1555 | modify | `{boardId, layoutId}` | `void` | ✅ port |
| `board.savePartialBoardSettings` | 1636 | modify | `{id, ...settings parcial}` | `void` | ✅ port |
| `board.saveBoard` | 1688 | modify | `{id, sections[], items[]}` (§4.5) | `void` | ✅ port¹ |
| `board.getBoardPermissions` | 2123 | full | `{id}` | `{inherited,users,groups}` | ✅ port |
| `board.saveUserBoardPermissions` | 2195 | full | `{entityId, permissions:[{principalId,permission}]}` | `void` | ✅ port |
| `board.saveGroupBoardPermissions` | 2234 | full | ídem | `void` | ✅ port |
| `board.addItem` | 2275 | modify | `{boardId, kind, options?, size?, integrationIds?}` | `{itemId}` | ✅ port² |
| `board.updateItemLayout` | 2399 | modify | `{boardId,itemId,layoutId,xOffset,yOffset,width,height}` | eco de la entrada | ✅ port |

¹ Valida timetable diferida (§5.3). ² Mapa de compatibilidad widget↔integración diferido (§5.2).

### section (sectionRouter, [section] section-router.ts:12-72)

| Procedimiento | Permiso | Entrada | Salida | Estado |
|---|---|---|---|---|
| `section.changeCollapsed` | firmado (sin check de board) | `{sectionId, collapsed}` | `void` | ✅ port |

Errores exactos: `NOT_FOUND "Section not found id=<id>"`, `BAD_REQUEST
"Section cannot be collapsed id=<id>"` (no contenedor o `collapsible` falso
tras `containerSectionOptionsSchema`, [val-shared] shared.ts:67-77).

### app (appRouter, [app] app.ts:23-330)

| Procedimiento | Línea | Permiso | Entrada | Salida | Estado |
|---|---|---|---|---|---|
| `app.getPaginated` | 24 | `app-modify-all` | `{search?, pageSize=10 (≤100), page=1}` | `{items: App[], totalCount}` | ✅ port |
| `app.all` | 60 | `app-modify-all` | `void` | `App[]` | ✅ port |
| `app.search` | 80 | firmado + (`app-modify-all` ∨ `board-modify-all`) | `{query, limit=10}` | `App[]` | ✅ port |
| `app.selectable` | 120 | firmado | `void` | `App[]` (mismas columnas) | ✅ port |
| `app.byId` | 158 | público + AppAccessControl | `{id}` | `App` | ✅ port |
| `app.byIds` | 186 | público + AppAccessControl | `string[]` | `App[]` | ✅ port |
| `app.create` | 190 | `app-create` | `{name,description,iconUrl,href,pingUrl}` | `{appId, ...App}` | ✅ port |
| `app.detectMetadata` | 227 | `app-create` | `{href}` | `{name,iconUrl}` | ✅ port M4 en `router/appmeta` (ADMIN.md §10) — no en este paquete |
| `app.createMany` | 241 | `app-create` | `App[]` (iconUrl nullable, sin trim) | `void` | ✅ port |
| `app.update` | 256 | `app-modify-all` | `{...app, id}` | `void` | ✅ port³ |
| `app.delete` | 307 | `app-full-all` | `{id}` | `void` | ✅ port³ |

³ Invalidación de caché de integraciones no-op (M3, sin Redis/caché aún).

`App` = `selectAppSchema` ([schema] validationSchemas.ts:7): `{id, name,
description: string|null, iconUrl, href: string|null, pingUrl: string|null}`.
`pingUrl` exige URL http(s) ([val-app] app.ts:22-29); `""` ⇒ null. `name`
trim 1-64; `description` trim ≤512, `""` ⇒ null. `app.byId` anónimo solo
resuelve apps situadas en boards públicos (`AppAccessControl`, [app]
app-access-control.ts:20-46: items `kind='app'` aportan `options.appId`;
`bookmarks` su legado `options.items`).

### userItemState (userItemStateRouter, [uis] user-item-state.ts:22-76)

| Procedimiento | Permiso | Entrada | Salida | Estado |
|---|---|---|---|---|
| `userItemState.get` | firmado + view del board del item | `{itemId (1-64)}` | `{<key>: <valor JSON>}` | ✅ port |
| `userItemState.set` | ídem | `{itemId, key (1-255), value (≤4096, JSON válido)}` | `void` | ✅ port |

`set` rechaza claves nuevas pasando el tope de 500 por usuario/item con
`BAD_REQUEST "Each user can save at most 500 state keys per item. Existing
keys can still be updated."` (upsert sobre PK `user_id+item_id+key`). El
gate `itemStateProcedure` ([uis] :12-20) devuelve `NOT_FOUND "Item not
found"` / `NOT_FOUND "Board not found"`.

**Totales: 40 procedimientos en el dominio — 40 portados** (M4 completa el
dominio: `board.getManageOverview` en manageoverview.go y
`app.detectMetadata` en el paquete `router/appmeta`).

## 4. Formas de salida (lo que consume el frontend)

### 4.1 `board.getAllBoards` — `boardSummarySchema` ([val-board] board.ts:159-175)

```jsonc
{
  "id": "...", "name": "...", "logoImageUrl": null, "isPublic": false,
  "creator": {"id": "...", "name": "alice", "image": null},   // null si no hay creator
  "isHome": true, "isMobileHome": false,                       // vs user.homeBoardId/mobileHomeBoardId
  "userPermissions": [{"permission": "view"}],                 // SOLO filas del caller; arrays vacíos, nunca null
  "groupPermissions": [{"permission": "modify"}]               // ídem (el schema recorta boardId/groupId)
}
```

### 4.2 `board.catalog` / `board.search`

Cada board: `{id, name, logoImageUrl, permissions:
{hasFullAccess, hasChangeAccess, hasViewAccess}, isHome, isMobileHome}`
([board] searchAccessibleBoardsAsync, board.ts:255-262 — aquí
`constructBoardPermissions` usa `creatorId`, no la relación creator).
`ORDER BY name`; `search` filtra con `LIKE '%q%'` (SQLite: ASCII
case-insensitive, igual que el `like()` de drizzle).

### 4.3 Full board (`getBoardByName`, `getHomeBoard`) — `getFullBoardWithWhereAsync` ([board] board.ts:2941-3060)

- **Columnas del board completas** + `creator` + `userPermissions:
  [{permission}]` (solo del caller) + `groupPermissions: [{boardId, groupId,
  permission}]` (solo grupos del caller; SIN recortar).
- **`layouts`**: `{id, name, columnCount, leftGutterColumnCount,
  rightGutterColumnCount, breakpoint, role}` ordenados por breakpoint;
  `resizeSource` solo presente si no es null (spread condicional, board.ts:3017-3023).
- **`sections`** (unión con claves recortadas — ¡distintas por kind!):
  - empty: `{"id","kind":"empty","xOffset","yOffset"}` — sin options/collapsed/layouts.
  - container (y el legado `"dynamic"` transformado): `{"id","kind":"container","options","collapsed","layouts":[{layoutId,xOffset,yOffset,width,height,parentSectionId}]}`;
    `options` = blob superjson de la BD al que `containerSectionOptionsSchema`
    aplica defaults (`title:"", customCssClasses:[], borderColor:"",
    showLabel:true, collapsible:false, showOpenAll:false, scrollable:false`)
    y recorta claves desconocidas; `collapsed` del `section_collapse_state`
    del caller (default false).
- **`items`**: `{id, kind, options (record crudo tras unwrap superjson),
  layouts:[{layoutId,xOffset,yOffset,width,height,sectionId}],
  integrationIds, advancedOptions:{title:null|string, customCssClasses:[],
  borderColor:""}}` — items que no validan el unión de 67 widgetKinds se
  **saltan** con warning (`parseItem`, board.ts:3086-3094), no fallan.

### 4.4 `board.getBoardSettings` — `boardSettingsSchema` ([val-board] board.ts:78-81)

Columnas nullable renderizadas **`""`** (no null): pageTitle, metaTitle,
logoImageUrl, faviconImageUrl, backgroundImageUrl, customCss, iconColor. Más
`backgroundImageAttachment|Repeat|Size` (enums fixed/scroll,
repeat|repeat-x|repeat-y|no-repeat, cover|contain), primaryColor,
secondaryColor, opacity (0-100), itemRadius (xs..xl), disableStatus, id, name.

### 4.5 `board.saveBoard` — diff semántico ([board] board.ts:1707-2121)

Entrada: `{id, sections: [emptySection | containerSection], items:
[commonItem]}` (§4.3 para las formas; en la entrada las grids son
`xOffset/yOffset 0-32767`, `width/height 1-32767`). Efectos:

1. `getBoardLayoutBoundsIssue` ([bounds] :53-82) contra los layouts de BD →
   `BAD_REQUEST` con `data.isBoardLayoutBoundsError: true` (el errorFormatter
   upstream añade ese flag, packages/api/src/trpc.ts:90).
2. `validateWidgetConfigurationsAsync` (board.ts:164-219): integración
   inexistente o sin permiso `use` ([int-access] integration-access.ts:12-75)
   → `NOT_FOUND "Integration not found"`.
3. Guard de Custom Widgets ([custom] :49-79): crear/alterar `kind=customApi`
   sin admin → `FORBIDDEN "Only administrators can add or configure Custom Widgets"`.
4. `normalizeAppWidgetOptions` ([val-app] app.ts:67-79) para kind `app`.
5. Diff: insert/update/delete de sections, section_layout (solo containers),
   items, item_layout y relations `integration_item`. `name` de sections se
   pone siempre a NULL; offsets solo para empty (containers NULL).
   **No hay cascade** (FK OFF, ver SCHEMA.md): como upstream better-sqlite3,
   los item_layout huérfanos quedan en la tabla.

### 4.6 `board.addItem` — colocación automática ([board] board.ts:2313-2397)

Primera sección `empty` del carril main ordenada por `(yOffset, id)`; para
**cada layout** busca el primer hueco libre (`findFirstAvailableBoardItemPosition`,
board.ts:121-139, escaneo y↑ x→ considerando items y containers hijos) con
tamaño `size ?? widgetDefaultSizes[kind] ?? {1,1}` ([widget-def] widget.ts:72-98)
y `width = min(columnCountDelCarril, width)`; sin hueco → `BAD_REQUEST "Board
section is full, no free grid position available"`; sin sección main →
`BAD_REQUEST "Board has no main section to place items in"`. Serializado por
board con mutex por clave (equivalente del `boardItemPlacementTails`, board.ts:95-119).

### 4.7 `void` en el wire

Las salidas `z.void()` llegan como `{"result":{"data":{}}}` (superjson de
`undefined` colapsa a `{}`); sin miembro `json`.

## 5. Decisiones y desviaciones documentadas

1. **columnCount exige entero** (createBoard/saveLayouts): upstream acepta
   `z.number()` (¡flotantes!) — un flotante rompería los scans int64 del port.
   El slider del frontend siempre manda enteros.
2. **Mapa widget↔integración diferido**: `getWidgetIntegrationIssue`
   ([widget-def] widget-integration-map.ts, 342 líneas de configuración) solo
   produce un `BAD_REQUEST` de UX; el control de seguridad (existencia +
   permiso `use`) SÍ está portado.
3. **Validación SSRF de timetable diferida**: `validateTimetableOptionsChangeAsync`
   (packages/api/src/router/widgets/timetable.ts:168-186) hace resolución DNS
   contra blocklists; los items timetable se guardan sin esa verificación
   hasta M3.
4. **deviceType**: inyectable vía `Options.DeviceTypeFromContext`; default
   desktop. La cadena de home board (`getHomeIdBoardAsync`, board.ts:2500-2546)
   está completa: user[settingKey] → user.homeBoardId → grupo de menor
   position (≠ everyone, con ambos home columns NOT NULL) → grupo everyone →
   serverSetting `board` (`homeBoardId`/`mobileHomeBoardId`, defaults null).
5. **Reparación de layouts protegidos**: `boardLayoutsNeedRepair`
   (board.ts:3062-3074) + `seedProtectedBoardLayoutsAsync` ([seed] seed.ts:98-190)
   portados completos, incluida la proyección de posiciones al layout mobile
   (`insertMissingProjectedPositionsAsync`, seed.ts:209-303, sobre el
   algoritmo de grid de grid.go). Solo afecta boards legacy pre-migración
   0029/0044. Como upstream, la reparación corre en cada lectura de full
   board y en `saveLayouts`/`resetLayout`; `duplicateBoard` NO repara
   (usa la query relacional cruda de board.ts:756-773).
6. **Permisos de integración** (`constructIntegrationPermissions`, [auth-int]):
   leídos de `integrationUserPermission`/`integrationGroupPermissions` +
   globales `integration-{use,interact,full}-all`.
7. **createBoard** fija el home board del creator si no tenía ninguno
   (board.ts:725-727) y crea sección raíz `empty(0,0)` + layouts Mobile(3, bp0)
   y Base(columnCount, bp768).
8. **`boardNameSchema`**: `^[A-Za-z0-9-_]*$`, 1-255 ([val-board] :25-29) —
   los nombres con espacios son rechazados por upstream también.
9. **`board.exists`**: entrada `z.string()` SIN validación de nombre
   (board.ts:268) — el port lo respeta.

## 6. Procedimientos diferidos (y por qué)

| Procedimiento | Motivo |
|---|---|
(Ninguno: `board.getManageOverview` se portó en M4 — manageoverview.go,
§3 — y `app.detectMetadata` vive en el paquete `router/appmeta`, ADMIN.md
§10. Los diferidos restantes del dominio son los 4 de onboarding, que
viven en ADMIN.md §6.)

### 6.1 Algoritmo de grid responsivo (portado)

`generateResponsiveGridFor` ([common] `packages/common/src/grid-algorithm.ts:23-138`)
está portado en `grid.go` (del paquete `board`, no en un `internal/grid`
separado: es el único consumidor y comparte los helpers de lanes de
definitions.go). El oráculo es [board] `board/test/grid-algorithm.spec.ts`,
portado completo en `grid_test.go` (5 fixtures de ítems 16→16/18/10/6/3
columnas, 4 fixtures con containers anidados y el grafo recursivo
corrupto). Quirks preservados deliberadamente:

- Al ensanchar (`width >= previousWidth`) se conservan todos los offsets y
  solo se recortan los widths a `min(columnCount, width)`; las secciones
  anidadas SÍ se reproyectan antes del early-return (grid-algorithm.ts:61-81).
- Al estrechar se empaqueta en orden `(yOffset, xOffset)` de las posiciones
  originales (sort estable) y la altura de un container es
  `max(alturaProyectada, alturaDeclarada)` (grid-algorithm.ts:87); en el
  early-return, en cambio, la altura declarada gana.
- `nextFreeSpot` escanea y<99999 y devuelve `null` → `Error "No free spot
  available"` → 500; inalcanzable en la práctica porque `normalizeItems`
  recorta todo width al column count (portado igualmente como
  `errNoFreeSpot`).
- El guard de ciclos `ancestorSectionIds` detiene grafos de secciones
  recursivos corruptos (grid-algorithm.ts:50).

Sobre el algoritmo vive `getUpdatedBoardLayout` (board.ts:2603-2715): remapea
los carriles (roots left/main/right, carriles sin columnas → main), proyecta
por carril con `previousWidth = MAX_SAFE_INTEGER` cuando varias lanes se
fusionan, y produce las filas `item_layout`/`section_layout` más el
`resizeSource` (`packages/common/src/layout-resize-source.ts`: el historial
pre-resize solo se conserva mientras las posiciones proyectadas no se hayan
tocado; al coincidir geometría y elementos se "rebobina" al arreglo original
y una vuelta a la geometría original borra el historial).

### 6.2 `board.saveLayouts` (board.ts:1269-1554)

Entrada `{id, layouts[]}` (responsiveBoardLayoutsSchema: mínimo 2, exactamente
un Mobile y un Base, breakpoints/ids únicos, gutters 0-3 que dejan ≥1 columna,
Mobile sin sidebars — los mensajes exactos de los `superRefine` se replican).
Permiso modify. Efectos en orden: `ensureGutterRootSectionsAsync` crea los
roots de carril left/right que falten (múltiples roots = Error → 500); la
validación de roles (`New layouts must use the custom role` / `Mobile and
Base layouts cannot be removed` / `Layout roles cannot be changed` — las dos
últimas inalcanzables por el esquema, portadas igualmente); el Base se
proyecta una vez si cambian columnCount/gutters y los layouts custom nuevos
se proyectan EN CADENA desde el Base ya redimensionado
(`getElementsForProjectedLayout`); los layouts redimensionados actualizan sus
`item_layout`/`section_layout` por `(itemId|sectionId, layoutId)` y guardan
el `resizeSource`; los layouts custom eliminados se borran (sin cascade).
Salida: los layouts enviados con ids generados para los añadidos, roles de BD
y ordenados por breakpoint.

### 6.3 `board.resetLayout` (board.ts:1555-1635)

Entrada `{boardId, layoutId}`. Permiso modify. Errores: `NOT_FOUND "Layout
not found"`, `INTERNAL_SERVER_ERROR "Board must have a Base layout"`,
`BAD_REQUEST "The Base layout cannot be reset"`. Proyecta Base → layout
objetivo (`getUpdatedBoardLayout`) y en una transacción: `resize_source =
NULL` del objetivo, borrado de sus `item_layout`/`section_layout` e inserción
de las filas proyectadas. Los items sin colocación en Base pierden su
colocación en el objetivo (semántica upstream). Salida `void`.

### 6.4 `board.duplicateBoard` (board.ts:731-1038)

Entrada `{id, name}` (boardNameSchema). Gates en orden: `board-create`
(FORBIDDEN "Permission denied"), view del board (NOT_FOUND anti-snooping),
`noBoardWithSimilarNameAsync` (CONFLICT "Board with similar name already
exists", case-insensitive). Carga cruda (sin reparación de layouts);
`throwIfCustomWidgetBoardDuplicationForbidden` (FORBIDDEN si hay
`kind=customApi` sin admin) y `validateWidgetConfigurationsAsync` (todas las
integraciones deben existir y dar `use`). Clona en una transacción: board
(todas las columnas, name=input, creatorId=caller), layouts normalizados
(`normalizeBoardLayoutRoles` para ≥2; con 1 layout genera Mobile(3, bp0) y
convierte el existente en Base(768) proyectando sus posiciones al mobile con
`getUpdatedBoardLayout`; con 0 genera el par Mobile/Base), sections,
section_layout (+ proyección), section_collapse_state (mismo usuario, nueva
section), items, item_layout (+ proyección) e integration_item — todos con
ids nuevos, `resizeSource` NULL. Salida `{boardId}`.

## 7. Repositories nuevos en internal/db

- `db/board.go` (extendido): `BoardFull`, updates parciales
  (`BoardPartialSettings` — nil = sin cambio, "" ⇒ NULL como el zod
  `trimmedNullableString`), `BoardNameTaken` (comparación case-insensitive
  en app code, como upstream), permisos de board user/grupo (list + replace),
  lecturas de groupMember/groupPermission, home boards de user y grupo,
  `GetBoardServerSettings` (unwrap superjson + defaults).
- `db/layout.go`: CRUD de `layout` + orden por breakpoint.
- `db/section.go`: CRUD de `section` (+ `section_collapse_state`).
- `db/item.go`: CRUD de `item`, `item_layout`, `section_layout`,
  `integration_item`, lecturas de permisos de integración.
- `db/useritemstate.go`: list/count/upsert de `user_item_state`.
- `db/app.go`: CRUD de `app`, paginación/búsqueda, apps en boards públicos,
  `FindIconURLByName` (getIconForName, packages/icons/src/auto-icon-searcher.ts:5-10).

SQL context-aware, prepared implícito por database/sql, sin ORM. El pool es
de 1 conexión (paridad better-sqlite3); las escrituras multi-paso van en
`BeginTx`.

## 8. Cobertura de tests (package board_test)

`harness_test.go` monta un `trpc.Router` real + httptest con inyección de
sesión por header (equivalente del middleware auth). Tests:

| Test | Cubre |
|---|---|
| `TestBoardVisibilityMatrix` | matriz owner/grupo/user-perm/admin/anónimo + catalog + search |
| `TestCreateBoardShapeAndGuards` | UNAUTHORIZED anónimo, FORBIDDEN sin permiso, forma exacta de getAllBoards y del full board (layouts/section raíz) |
| `TestBoardMutationGuards` | rename: anónimo UNAUTHORIZED, sin acceso/modify/view NOT_FOUND, full + CONFLICT por nombre, visibility, home board privado → BAD_REQUEST, setHomeBoard, exists, delete |
| `TestBoardSettingsRoundtrip` | savePartial/getSettings con ""-for-null y enums inválidos |
| `TestBatchCallSanity` | batch clásico (array) y stream JSONL (`trpc-accept`) mezclando 2 procedimientos |
| `TestSectionItemCRUDAndLayoutPersistence` | saveBoard roundtrip completo, updateItemLayout (eco, overlap CONFLICT, bounds con marker), diff de borrado, addItem colocación |
| `TestSectionCollapse` | changeCollapsed: mensajes exactos, estado por usuario, flag `collapsed` en el read |
| `TestUserItemState` | set/get JSON crudo, aislamiento por usuario, tope 500, "Value must be JSON" |
| `TestGetHomeBoardFallbacks` | cadena de home board (serverSettings → user) |
| `TestGetBoardsForGroup` | gate admin + boards del grupo |
| `TestAppRouter` | gates por permiso, formas de App, AppAccessControl anónimo (público/privado), search, getPaginated, selectable, update, delete, createMany con fallback de icono |
| `TestGridAlgorithmItemFixtures` | oráculo [board] grid-algorithm.spec.ts: 5 fixtures de texto 16→16/18/10/6/3 columnas |
| `TestGridAlgorithmContainerFixtures` | oráculo: 4 fixtures con containers anidados (outer "b", inner "f") |
| `TestGridAlgorithmStopsRecursiveSectionGraph` | oráculo: grafo recursivo corrupto |
| `TestGridAlgorithmWideningKeepsOffsetsAndClampsWidth`, `TestGridAlgorithmNarrowingPacksInScanOrder`, `TestGridAlgorithmNestedContainerReflowsWhileRootWidens`, `TestGridAlgorithmPackingUsesProjectedSectionHeight`, `TestGridAlgorithmEmptySection` | quirks extra del algoritmo (grid_test.go) |
| `TestSaveLayoutsRoundtrip` | saveLayouts: guards UNAUTHORIZED/NOT_FOUND, mensajes exactos de validación, ensanche con resizeSource, rebobinado al volver a la geometría original, alta/baja de layout custom con proyección |
| `TestSaveLayoutsNarrowingProjectsPlacements` | empaquetado del estrechamiento a través del procedimiento |
| `TestResetLayoutRestoresFromBase` | resetLayout: guards ("Layout not found", "The Base layout cannot be reset"), proyección Base→Mobile, Base intacto, resizeSource NULL |
| `TestDuplicateBoardClonesBoard` | duplicateBoard: gates (UNAUTHORIZED/FORBIDDEN/NOT_FOUND/CONFLICT/nombre inválido), clon completo con ids nuevos (board, layouts, sections, placements, items, integrations, collapse states), original intacto |
| `TestDuplicateBoardSingleLayoutGeneratesMobile` | ruta legacy de 1 layout: Mobile generado con la proyección Base→3 columnas |
| `TestDuplicateBoardCustomWidgetGuard` | FORBIDDEN con Custom Widget sin admin; admin sí duplica |
