package sql

import (
	"context"
	"embed"
	"fmt"
	"sort"
	"strings"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// Migrate applies all embedded *.up.sql migration files in lexicographic order.
// Each file is executed as a single statement block; files that use IF NOT EXISTS
// are safe to run repeatedly (idempotent).
func (s *Store) Migrate(ctx context.Context) error {
	entries, err := migrationFS.ReadDir("migrations")
	if err != nil {
		return fmt.Errorf("sql migrate: read dir: %w", err)
	}

	// Sort ensures deterministic application order (001, 002, …).
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name() < entries[j].Name()
	})

	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".up.sql") {
			continue
		}
		sql, err := migrationFS.ReadFile("migrations/" + e.Name())
		if err != nil {
			return fmt.Errorf("sql migrate: read %s: %w", e.Name(), err)
		}
		if _, err := s.db.ExecContext(ctx, string(sql)); err != nil {
			return fmt.Errorf("sql migrate: exec %s: %w", e.Name(), err)
		}
	}
	return nil
}
