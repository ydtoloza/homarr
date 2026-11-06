# Esquema de la base de datos (port Go de `packages/db`)

Este documento describe el esquema SQLite que `server/internal/db` abre y
mantiene: una copia **verbatim** del esquema upstream de Homarr
(`packages/db/schema/sqlite.ts` + migraciones drizzle en
`packages/db/migrations/sqlite/`). La compatibilidad bidireccional con un
`db.sqlite` creado por Homarr upstream (Node/better-sqlite3) es el criterio de
aceptación de este paquete.

Referencias upstream (rama `dev`):

- Esquema drizzle: `packages/db/schema/sqlite.ts` (~41 tablas) + la
  re-exportación `trusted_certificate_hostname` desde
  `packages/core/src/infrastructure/certificates/hostnames/db/sqlite.ts`.
- Driver: `packages/core/src/infrastructure/db/drivers/sqlite.ts`
  (`better-sqlite3` v12.11.1 envuelto en drizzle-orm 0.45.2).
- Casing: `DB_CASING = "snake_case"`
  (`packages/core/src/infrastructure/db/constants.ts`).
- Env por defecto: `DB_URL=/appdata/db/db.sqlite` en producción
  (`packages/core/src/infrastructure/db/env.ts`; también `ENV DB_URL` en el
  `Dockerfile` raíz).
- Migraciones: `packages/db/migrations/sqlite/` (53 archivos `NNNN_*.sql` +
  `meta/_journal.json`), ejecutadas al arrancar el contenedor por
  `scripts/run.sh` → `node ./db/migrations/sqlite/migrate.cjs ./db/migrations/sqlite`
  (bundle de `packages/db/migrations/sqlite/migrate.ts`, que llama a
  `migrate()` de drizzle + `seedDataAsync` + migraciones "custom" en TS).

## 1. Inventario de tablas (41)

Nombres de tabla físicos en camelCase tal cual los genera drizzle (p. ej.
`apiKey`, `serverSetting`); las **columnas** están en snake_case (migro
`0016_change_all_to_snake_case`). Convención de tipos: `text` para ids/strings,
`integer` para booleanos 0/1 y timestamps (epoch en **milisegundos** cuando
drizzle usa `mode: "timestamp_ms"`, en **segundos** con `mode: "timestamp"`);
`options`/`value` en JSON serializado con superjson (string).

### Usuarios / auth (NextAuth + Homarr) — 11 tablas

| Tabla | Propósito | Columnas clave |
|---|---|---|
| `user` | Cuentas de usuario | `id`, `name` (minúsculas), `email`, `email_verified`, `image`, `password` (hash argon2), `provider` (`credentials`/`oidc`/`ldap`, default `credentials`), `home_board_id`, `mobile_home_board_id`, `default_search_engine_id`, `color_scheme`, `header_preferences`, tours… |
| `account` | Identidades OAuth de NextAuth | PK compuesta (`provider`, `provider_account_id`), `user_id` FK cascade |
| `session` | Sesiones de base de datos (`strategy: "database"`, `packages/auth/configuration.ts`) | `session_token` PK, `user_id`, `expires` (ms) |
| `verificationToken` | Tokens de verificación email | PK compuesta (`identifier`, `token`), `expires` |
| `apiKey` | API keys de usuarios | `id`, `api_key`, `read_only`, `oauth_*` (procedencia OAuth), `user_id` |
| `group` | Grupos de usuarios/permisos | `id`, `name` (único), `owner_id`, `home_board_id`, `mobile_home_board_id`, `position` |
| `groupMember` | Membresías grupo↔usuario | PK compuesta (`group_id`, `user_id`) |
| `groupPermission` | Permisos globales de grupo | `group_id`, `permission` |
| `invite` | Invitaciones | `id`, `token` único, `expiration_date`, `creator_id` |
| `media` | Binarios subidos (imágenes) | `id`, `name`, `content` blob, `content_type`, `size`, `created_at`, `creator_id` |
| `onboarding` | Estado del wizard inicial (1 fila) | `id`, `step`, `previous_step` |

### Boards / layout / contenido — 10 tablas

| Tabla | Propósito | Columnas clave |
|---|---|---|
| `board` | Dashboards | `id`, `name` (único), `is_public`, `creator_id`, colores, `opacity`, `custom_css`, `background_image_*`, `item_radius`, `disable_status` |
| `boardUserPermission` | Permiso de board por usuario | PK (`board_id`, `user_id`, `permission`) |
| `boardGroupPermission` | Permiso de board por grupo | PK (`board_id`, `group_id`, `permission`) |
| `layout` | Grillas responsive del board | `id`, `board_id`, `column_count`, `left/right_gutter_column_count`, `breakpoint`, `role` (`desktop`/`mobile`/`custom`), `resize_source` |
| `section` | Secciones del board | `id`, `board_id`, `kind` (`empty`/`category`/`dynamic`), `x_offset`, `y_offset`, `name`, `options` |
| `item_layout` | Posición de un ítem en un layout | PK (`item_id`, `section_id`, `layout_id`), `x_offset`, `y_offset`, `width`, `height` |
| `section_layout` | Posición de una sección en un layout | PK (`section_id`, `layout_id`), `parent_section_id` |
| `section_collapse_state` | Colapso de categoría por usuario | PK (`user_id`, `section_id`), `collapsed` |
| `item` | Widgets colocados | `id`, `board_id`, `kind`, `options`, `advanced_options` |
| `user_item_state` | Estado de un widget por usuario | PK (`user_id`, `item_id`, `key`), `value` |

### Apps / integraciones — 6 tablas

| Tabla | Propósito | Columnas clave |
|---|---|---|
| `app` | Atajos de aplicaciones | `id`, `name`, `description`, `icon_url`, `href`, `ping_url` |
| `integration` | Integraciones externas | `id`, `name`, `url`, `external_url`, `kind`, `app_id` |
| `integrationSecret` | Secretos cifrados (`"iv.datos"`) | PK (`integration_id`, `kind`), `value`, `updated_at` |
| `integrationUserPermission` | Permiso por usuario | PK (`integration_id`, `user_id`, `permission`) |
| `integrationGroupPermissions` | Permiso por grupo | PK (`integration_id`, `group_id`, `permission`) |
| `integration_item` | Vínculo widget↔integración | PK (`item_id`, `integration_id`) |

### Búsqueda, sistema, iconos, secretos de widgets — 7 tablas

| Tabla | Propósito | Columnas clave |
|---|---|---|
| `search_engine` | Motores de búsqueda (bangs DDG) | `id`, `icon_url`, `name`, `short` (único), `url_template`, `type` (`generic`/`from_integration`), `integration_id` |
| `serverSetting` | Ajustes del servidor (clave/valor) | `setting_key` PK, `value` (superjson) |
| `cron_job_configuration` | Override de cron jobs | `name` PK, `cron_expression`, `is_enabled` |
| `trusted_certificate_hostname` | Hostnames TLS confiables (pinning) | PK (`hostname`, `thumbprint`), `certificate` |
| `icon` | Iconos cacheados de repositorios | `id`, `name`, `url`, `checksum`, `icon_repository_id` |
| `iconRepository` | Repositorios de iconos | `id`, `slug` |
| `widget_secret` | Secretos de widgets cifrados | PK (`item_id`, `kind`), `value`, `updated_at` |

### Custom widgets — 4 tablas

| Tabla | Propósito |
|---|---|
| `custom_widget_definition` | Definiciones Custom JSX **v1** (solo lectura desde v2; nombre físico conservado a propósito para que un binario v1 siga leyendo la base) |
| `custom_widget_secret` | Secretos v1 (columna física `value`) |
| `custom_widget_v2_definition` | Definiciones Custom JSX v2 (`sources`, `requests`, `options`, `template`) |
| `custom_widget_v2_secret` | Secretos v2 (columna física `encrypted_value`, PK por `definition_id`+`source_id`+`kind`) |

### Asistente IA — 3 tablas

| Tabla | Propósito |
|---|---|
| `assistant_configuration` | Config global del asistente (proveedor, `base_url`, API key cifrada) |
| `assistant_thread` | Hilos por usuario (`user_id`, `title`, timestamps en **segundos**) |
| `assistant_message` | Mensajes (`thread_id`, `parent_id`, `format` `ai-sdk/v6`, `content`) |

## 2. Apertura de la base y PRAGMAs

Evidencia recopilada:

1. `packages/core/src/infrastructure/db/drivers/sqlite.ts`: la conexión es
   `new Database(dbEnv.URL)` **sin ningún PRAGMA**.
2. `better-sqlite3` v12.11.1 (`src/objects/database.cpp`, `NODE_METHOD(Database::JS_new)`):
   abre con `sqlite3_open_v2(... SQLITE_OPEN_READWRITE|CREATE)` y solo ejecuta
   `sqlite3_busy_timeout(db, timeout)` con default **5000 ms**. No activa WAL ni
   foreign keys.
3. En el código de producción upstream no hay `pragma(` salvo en el export de
   backup (`apps/nextjs/src/app/api/backup/export/route.ts` hace
   `wal_checkpoint(TRUNCATE)`) y en tests. Consecuencia: el journal mode queda
   en el default del archivo (**DELETE** para bases nuevas; WAL solo si el
   archivo ya lo tenía).

Paridad implementada en `Open` (`db.go`):

- `PRAGMA busy_timeout = 5000` (parametrizable con `Options.BusyTimeoutMs`),
  pasada como parámetro DSN `?_pragma=busy_timeout(5000)` de `modernc.org/sqlite`
  para que aplique a **cada** conexión del pool (es estado por conexión).
- **No** se fuerza WAL ni `foreign_keys`: igual que upstream. Matiz: las
  migraciones 0001/0006/0023/0025/0030 ejecutan
  `COMMIT; PRAGMA foreign_keys = OFF; ... COMMIT; PRAGMA foreign_keys = ON; BEGIN;`
  para reconstruir tablas, así que la conexión que **migra** termina con FK ON;
  las conexiones de runtime (nuevas) quedan con FK OFF, exactamente como en
  upstream, donde la conexión de migración (`migrate.cjs`) es un proceso aparte.
- Pool por defecto de **1 conexión** (`Options.MaxOpenConns`): upstream usa una
  única conexión better-sqlite3 síncrona; esto evita `SQLITE_BUSY` entre
  conexiones propias sin WAL. Un segundo handle sobre el mismo archivo funciona
  gracias a `busy_timeout` (upstream hace lo mismo con su lector proxy).
- El directorio padre se crea si falta (`mkdir -p /appdata/db` del entrypoint
  upstream, `scripts/run.sh`).

Sobre `proxy-reader.ts`: upstream mantiene una **segunda conexión** independiente
(pool de 1, `packages/db/proxy/sqlite.ts`) que solo lee `onboarding.step` y
`serverSetting.value WHERE setting_key='culture'` para el flujo auth-proxy de
nginx. No es un réplica ni un pool de lectura: es una conexión dedicada de solo
unas dos lecturas. En el port no hace falta (mismo proceso); si el hub HTTP lo
necesita, basta con otro `db.Open` del mismo archivo.

## 3. Migraciones y tabla de bookkeeping

### Formato on-disk

Los archivos `NNNN_tag.sql` y `meta/_journal.json` son copias byte a byte de
`packages/db/migrations/sqlite/` (verificado con sha256 en
`TestMigrationFilesMatchUpstreamChecksums`). Journal (formato drizzle-kit
`version: "6"`, `dialect: "sqlite"`): una lista `entries` con `idx`, `version`,
`when` (epoch **milisegundos**), `tag` y `breakpoints`. Dentro de cada `.sql`,
las sentencias se separan con el literal `--> statement-breakpoint`.

### Tabla `__drizzle_migrations`

`drizzle-orm` 0.45.2, dialecto SQLite síncrono (`better-sqlite3`,
`drizzle-orm/sqlite-core/dialect.cjs`, clase `SQLiteSyncDialect.migrate`) hace:

1. `CREATE TABLE IF NOT EXISTS "__drizzle_migrations" (id SERIAL PRIMARY KEY, hash text NOT NULL, created_at numeric)`
   (fuera de la transacción). Ojo: `SERIAL` **no** es `INTEGER PRIMARY KEY` en
   SQLite (no es alias de rowid), y drizzle nunca inserta `id`, así que las
   filas quedan con `id = NULL` (quirk histórico de SQLite: PK sin NOT NULL lo
   permite). El DDL se reproduce byte a byte en `migrate.go`.
2. `SELECT id, hash, created_at FROM "__drizzle_migrations" ORDER BY created_at DESC LIMIT 1`.
3. `BEGIN`; para cada entrada del journal **en orden**: si no hay última fila, o
   `Number(ultima.created_at) < entrada.when` (comparación **estricta por
   timestamp**, el `hash` se guarda pero no se consulta), ejecuta las
   sentencias del archivo y hace
   `INSERT INTO "__drizzle_migrations" ("hash", "created_at") VALUES(sha256hex(archivo), when)`;
   al final `COMMIT`.
4. `hash` = **sha256 hex del contenido completo del archivo** (UTF-8, LF),
   calculado por `readMigrationFiles` de drizzle.

`MigrateContext` (`migrate.go`) reproduce 1–4 exactamente, sobre una única
conexión del pool. Particularidad crítica descubierta: los archivos generados
por drizzle-kit para reconstrucciones de tabla (0001, 0006, 0023, 0025, 0030)
**abren con `COMMIT TRANSACTION;`** (cierran la transacción externa de drizzle),
hacen su trabajo con `PRAGMA foreign_keys = OFF/ON` y **terminan con un
`BEGIN TRANSACTION;`** que re-abre la transacción externa. Por eso el runner no
puede asumir que su `BEGIN` sobrevive al primer archivo, y el `COMMIT` final
solo funciona porque la secuencia queda balanceada (igual que en upstream; si
upstream introdujera un archivo desbalanceado, fallaría igual en ambos lados).

### Compatibilidad upstream ↔ Go (confirmada)

- `db.sqlite` creado por upstream: nuestra `Migrate` lee su
  `__drizzle_migrations` con la misma consulta y compara por `created_at`, así
  que aplica solo lo que falte
  (`TestMigrateResumesFromUpstreamBookkeeping` simula una base con las 2
  primeras migraciones aplicadas por "upstream" y verifica que se completan las
  51 restantes).
- `db.sqlite` creado por Go: un upstream (`migrate.cjs` con el mismo drizzle)
  ve las mismas filas/timestamps y no re-aplica nada
  (`TestMigrateIsIdempotentAndRecordsJournalTimestamps` verifica idempotencia
  en caliente y tras cerrar/reabrir).
- El `hash` guardado es idéntico al de drizzle porque los archivos no se
  tocan (checksums en el test).

### Qué NO hace todavía `Migrate` (pendiente, como upstream)

Upstream ejecuta, **después** de `migrate()` (todo dentro de
`packages/db/migrations/sqlite/migrate.ts`): `seedDataAsync`
(`packages/db/migrations/seed.ts`: grupo "everyone", fila de onboarding,
server settings por defecto, buscadores, apps/boards de ejemplo, layouts
protegidos, usuario demo con `SEED_ME`…) y `applyCustomMigrationsAsync`
(`packages/db/migrations/custom/0000..0007`, data-migrations en TS con superjson).
Eso pertenece a la capa de auth/api del port (M2); `Migrate` de `internal/db`
es solo DDL, y así está documentado para quien lo consuma.

## 4. Tablas que usa M1 (login + listado de boards)

| Repo | Tabla | Métodos |
|---|---|---|
| `user.go` | `user` | `CreateUser`, `GetUserByID`, `GetUserByName` (name minúscula + provider, réplica de `authorizeWithBasicCredentialsAsync` en `packages/auth/providers/credentials/authorization/basic-authorization.ts`) |
| `session.go` | `session` | `CreateSession`, `GetSession` (join con `user`, réplica de `getSessionFromTokenAsync`), `DeleteSession`, `DeleteExpiredSessions` |
| `board.go` | `board` (+ `boardUserPermission`, `boardGroupPermission`, `groupMember` en la consulta) | `CreateBoard`, `ListBoardsForUser` (público OR propio OR permiso de usuario OR permiso de grupo, ordenado por `name`, réplica del filtro de `packages/api/src/router/board.ts`), `GetBoardByID`, `CountBoards` |

Convenciones: ids los genera la capa superior (upstream usa cuid2 en la app);
los timestamps van en milisegundos epoch (drizzle `timestamp_ms`); `database/sql`
directo con sentencias preparadas implícitas por el driver, sin ORM; los
métodos reciben `context.Context` y envuelven `sql.ErrNoRows` en
`ErrNotFound`.

## 5. Costura para PostgreSQL futuro

Upstream ya tiene los dos dialectos (`packages/db/schema/postgresql.ts`,
`packages/db/migrations/postgresql/` con su propio `migrate.ts` y pool
`node-postgres`), seleccionados por `DB_DRIVER`
(`packages/core/src/infrastructure/db/mapping.ts`). El port deja la costura
así:

- `Open`/`OpenTest`/repositorios hablan `database/sql`: el driver concreto solo
  aparece en el import blank de `db.go` y en la constante `driverName`.
- `dsn()` es el único punto que compone sintaxis específica de SQLite; una
  implementación PG iría en un `postgres.go` con su propio `OpenPgx` y DDL
  embebido desde `packages/db/migrations/postgresql/` (otro `_journal.json`),
  sin tocar los repos si las consultas se mantienen SQL estándar (los
  placeholders `?` habrían de cambiarse a `$n` — se resolvería con un
  `replacer` por dialecto o compilando las queries por driver).
- Las diferencias conocidas de esquema PG upstream: mismo modelo lógico pero
  DDL/types propios (`0001_increase-secret-kind-length.sql` sube un
  `varchar`), y el runner de drizzle para PG usa `__drizzle_migrations` con
  schema `drizzle` distinto (tabla `drizzle.__drizzle_migrations`), por lo que
  el runner PG NO debe reutilizar el formato SQLite.
- Las migraciones embebidas viven en `migrations/` con su `README.md`
  (origen + Apache-2.0); las de PG se añadirían en `migrations-postgresql/`
  siguiendo el mismo patrón cuando toque.

## 6. Pruebas

`go test ./server/internal/db/...` (solo stdlib):

- `TestMigrationFilesMatchUpstreamChecksums` — los 53 `.sql` embebidos tienen
  el sha256 de upstream (fidelidad de bytes).
- `TestJournalShape` — journal v6/sqlite, 53 entradas, primer/último tag y
  timestamps.
- `TestMigrateCreatesAllTables` — las 41 tablas existen (sqlite_master) y el
  DDL de `__drizzle_migrations` tiene las columnas drizzle.
- `TestMigrateIsIdempotentAndRecordsJournalTimestamps` — segunda pasada y
  re-apertura sin cambios (idempotencia por bookkeeping).
- `TestMigrateResumesFromUpstreamBookkeeping` — continúa desde una base
  parcialmente migrada "por upstream" (formato exacto de drizzle).
- `TestOpenSameDatabaseTwice` — dos handles sobre el mismo archivo, escritura
  concurrente con `busy_timeout`.
- `TestMigrationHashMatchesDrizzleConvention` — sha256 hex del archivo + `when`
  del journal.
- `TestUserRoundtrip`, `TestSessionRoundtrip`, `TestDeleteExpiredSessions`,
  `TestBoardListVisibility`, `TestBoardColumnsKeepUpstreamSnakeCaseNames` —
  CRUD y visibilidad con los nombres físicos de columnas.
