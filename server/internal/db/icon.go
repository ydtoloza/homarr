package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Repository for the "icon" table, serving app.detectMetadata
// (packages/api/src/router/app.ts:227-239 via getIconForName,
// packages/icons/src/auto-icon-searcher.ts): the auto icon search that backs
// the icon picker fallback of the app creation modal.

// Icon maps to the upstream "icon" table (packages/db/schema/sqlite.ts,
// export icons).
type Icon struct {
	ID   string
	Name string
	URL  string
	// Checksum is the icon repository checksum (unused by the search).
	Checksum string
	// IconRepositoryID is the owning repository row.
	IconRepositoryID string
}

const iconColumns = `id, name, url, checksum, icon_repository_id`

func scanIcon(row *sql.Row) (Icon, error) {
	var i Icon
	err := row.Scan(&i.ID, &i.Name, &i.URL, &i.Checksum, &i.IconRepositoryID)
	return i, err
}

// FindIconByNameLike is getIconForName (auto-icon-searcher.ts:5-9):
// findFirst({where: like(icons.name, `%${name}%`)}). The upstream drizzle
// like() escapes nothing, so % and _ inside the name act as wildcards there
// too; the first row in table order wins (no ORDER BY upstream).
func (d *DB) FindIconByNameLike(ctx context.Context, name string) (Icon, error) {
	query := `SELECT ` + iconColumns + ` FROM "icon" WHERE name LIKE ? LIMIT 1`
	icon, err := scanIcon(d.QueryRowContext(ctx, query, "%"+name+"%"))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Icon{}, ErrNotFound
		}
		return Icon{}, fmt.Errorf("db: find icon by name: %w", err)
	}
	return icon, nil
}
