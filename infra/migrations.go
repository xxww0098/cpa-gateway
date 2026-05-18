package infra

import (
	"log/slog"

	"gorm.io/gorm"
)

// RunCustomMigrations executes raw SQL migrations that GORM AutoMigrate
// cannot handle (e.g., GIN indexes on JSONB columns).
// It is safe to call repeatedly — all statements use IF NOT EXISTS.
func RunCustomMigrations(db *gorm.DB) error {
	migrations := []struct {
		name string
		sql  string
	}{
		{
			name: "balance_logs_metadata_gin_index",
			sql:  `CREATE INDEX IF NOT EXISTS idx_balance_logs_metadata ON balance_logs USING GIN (metadata)`,
		},
		{
			name: "balance_logs_reference_index",
			sql:  `CREATE INDEX IF NOT EXISTS idx_balance_logs_reference ON balance_logs (reference)`,
		},
	}

	for _, m := range migrations {
		if err := db.Exec(m.sql).Error; err != nil {
			slog.Error("custom migration failed", "name", m.name, "error", err)
			return err
		}
		slog.Info("custom migration applied", "name", m.name)
	}

	return nil
}
