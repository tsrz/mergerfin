package db

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/tsrz/mergerfin/internal/idgen"
	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// DB wraps a SQLite connection pool configured for WAL mode and foreign key enforcement.
type DB struct {
	sqlDB *sql.DB
}

// Open opens (or creates) the SQLite database at dbPath, runs embedded migrations,
// and seeds default settings and virtual libraries.
func Open(ctx context.Context, dbPath string, defaultServerName string) (*DB, error) {
	if dbPath == "" {
		return nil, fmt.Errorf("dbPath must not be empty")
	}

	if dbPath != ":memory:" && !strings.HasPrefix(dbPath, "file:") {
		dir := filepath.Dir(dbPath)
		if dir != "" && dir != "." {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return nil, fmt.Errorf("create db directory %q: %w", dir, err)
			}
		}
	}

	dsn := buildDSN(dbPath)
	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite %q: %w", dbPath, err)
	}

	sqlDB.SetMaxOpenConns(4)
	sqlDB.SetMaxIdleConns(4)

	if err := sqlDB.PingContext(ctx); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("ping sqlite %q: %w", dbPath, err)
	}

	d := &DB{sqlDB: sqlDB}
	if err := d.migrate(ctx); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("run migrations: %w", err)
	}

	if err := d.seed(ctx, defaultServerName); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("seed defaults: %w", err)
	}

	return d, nil
}

// Close closes the underlying SQLite connection pool.
func (d *DB) Close() error {
	if d == nil || d.sqlDB == nil {
		return nil
	}
	return d.sqlDB.Close()
}

// SqlDB returns the underlying *sql.DB handle.
func (d *DB) SqlDB() *sql.DB {
	return d.sqlDB
}

func buildDSN(dbPath string) string {
	pragmas := "_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(ON)&_pragma=busy_timeout(5000)&_txlock=immediate"

	if dbPath == ":memory:" {
		return "file::memory:?mode=memory&cache=shared&" + pragmas
	}
	if strings.HasPrefix(dbPath, "file:") {
		if strings.Contains(dbPath, "?") {
			return dbPath + "&" + pragmas
		}
		return dbPath + "?" + pragmas
	}

	u := url.URL{
		Scheme:   "file",
		Path:     dbPath,
		RawQuery: pragmas,
	}
	return u.String()
}

func (d *DB) migrate(ctx context.Context) error {
	if _, err := d.sqlDB.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version TEXT PRIMARY KEY,
			applied_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		);
	`); err != nil {
		return fmt.Errorf("ensure schema_migrations table: %w", err)
	}

	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		return fmt.Errorf("read embedded migrations: %w", err)
	}

	var files []string
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".sql") {
			files = append(files, entry.Name())
		}
	}
	sort.Strings(files)

	for _, name := range files {
		var exists int
		err := d.sqlDB.QueryRowContext(ctx, `SELECT 1 FROM schema_migrations WHERE version = ?`, name).Scan(&exists)
		if err == nil {
			continue
		}
		if err != sql.ErrNoRows {
			return fmt.Errorf("check migration %s: %w", name, err)
		}

		rawSQL, err := fs.ReadFile(migrationsFS, "migrations/"+name)
		if err != nil {
			return fmt.Errorf("read migration %s: %w", name, err)
		}

		tx, err := d.sqlDB.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin migration %s tx: %w", name, err)
		}

		if _, err := tx.ExecContext(ctx, string(rawSQL)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("exec migration %s: %w", name, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations(version) VALUES(?)`, name); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("record migration %s: %w", name, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %s: %w", name, err)
		}
	}

	return nil
}

func (d *DB) seed(ctx context.Context, defaultServerName string) error {
	serverName := strings.TrimSpace(defaultServerName)
	if serverName == "" {
		serverName = "MergerFin"
	}

	tx, err := d.sqlDB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin seed tx: %w", err)
	}
	defer func() {
		_ = tx.Rollback()
	}()

	settingsSeeds := []struct {
		key   string
		value string
	}{
		{key: "server_id", value: idgen.NewRandomHex32()},
		{key: "server_name", value: serverName},
		{key: "encryption_salt", value: idgen.NewRandomHex32()},
	}
	for _, s := range settingsSeeds {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO settings(key, value) VALUES(?, ?)`, s.key, s.value); err != nil {
			return fmt.Errorf("seed setting %s: %w", s.key, err)
		}
	}

	libraries := []struct {
		id             string
		name           string
		collectionType string
	}{
		{id: idgen.MoviesLibraryID(), name: "Movies", collectionType: "movies"},
		{id: idgen.TVShowsLibraryID(), name: "TV Shows", collectionType: "tvshows"},
	}
	for _, lib := range libraries {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO virtual_libraries(id, name, collection_type) VALUES(?, ?, ?)`, lib.id, lib.name, lib.collectionType); err != nil {
			return fmt.Errorf("seed virtual library %s: %w", lib.name, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit seed tx: %w", err)
	}
	return nil
}
