package httpserver

import (
	"context"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// TestMain removes the Super Admin the suite created for itself (see
// ensureTestSuperAdmin) so a database that started without one ends without one.
func TestMain(m *testing.M) {
	code := m.Run()
	if createdSuperAdmin != uuid.Nil && os.Getenv("DATABASE_URL") != "" {
		ctx := context.Background()
		if conn, err := pgx.Connect(ctx, os.Getenv("DATABASE_URL")); err == nil {
			removeTestSuperAdmin(ctx, conn, createdSuperAdmin)
			conn.Close(ctx)
		}
	}
	os.Exit(code)
}

// removeTestSuperAdmin deletes the user and everything that points at it. The
// referencing tables are discovered from the catalog rather than listed by hand,
// so a new table with a foreign key to users cannot silently leave the admin
// (and the "platform already initialised" state) behind.
func removeTestSuperAdmin(ctx context.Context, conn *pgx.Conn, id uuid.UUID) {
	rows, err := conn.Query(ctx, `
		SELECT c.conrelid::regclass::text, a.attname
		FROM pg_constraint c
		JOIN pg_attribute a ON a.attrelid = c.conrelid AND a.attnum = ANY (c.conkey)
		WHERE c.contype = 'f' AND c.confrelid = 'users'::regclass`)
	if err != nil {
		return
	}
	type ref struct{ table, column string }
	var refs []ref
	for rows.Next() {
		var r ref
		if rows.Scan(&r.table, &r.column) == nil {
			refs = append(refs, r)
		}
	}
	rows.Close()
	for _, r := range refs {
		_, _ = conn.Exec(ctx, "DELETE FROM "+r.table+" WHERE "+pgx.Identifier{r.column}.Sanitize()+" = $1", id)
	}
	_, _ = conn.Exec(ctx, `DELETE FROM users WHERE id = $1`, id)
}
