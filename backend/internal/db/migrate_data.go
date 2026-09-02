package db

import (
	"fmt"
	"strings"
)

// TableStat reports the outcome of copying one table.
type TableStat struct {
	Table    string   `json:"table"`
	Inserted int      `json:"inserted"`
	Skipped  int      `json:"skipped"`           // rows already present in the destination (PK conflict)
	Dropped  []string `json:"dropped,omitempty"` // source columns not present in the destination schema
}

// copyOrder lists every table, parents before children so foreign keys
// (declared ON DELETE CASCADE) are satisfied during the copy.
var copyOrder = []string{
	"projects",
	"memories",
	"requirements",
	"knowledge",
	"conversations",
	"refinement_chats",
	"project_run_configs",
	"platform_tokens",
	"roles",
	"settings",
	"claude_configs",
	"weekly_reports",
	"job_logs",
	"token_usage",
	// RBAC tables (parents before join tables).
	"users",
	"acl_roles",
	"permissions",
	"acl_role_permissions",
	"acl_user_roles",
	"user_projects",
	"sessions",
	"sub_tasks",
}

// Migrate copies all data from src into dst (which must already be migrated —
// Init does that). IDs are preserved verbatim. Rows that conflict with an
// existing destination row are counted as skipped, so re-running a partial
// migration is safe.
func Migrate(src, dst *DB, logf func(string)) ([]TableStat, error) {
	if src.dialect == dst.dialect && src.dialect != SQLite {
		return nil, fmt.Errorf("source and destination are both %s", src.dialect)
	}
	stats := make([]TableStat, 0, len(copyOrder))
	for _, table := range copyOrder {
		st, err := copyTable(src, dst, table)
		if err != nil {
			return stats, err
		}
		if logf != nil {
			msg := fmt.Sprintf("%-22s %d inserted, %d skipped", table, st.Inserted, st.Skipped)
			if len(st.Dropped) > 0 {
				msg += fmt.Sprintf(" (legacy columns dropped: %s)", strings.Join(st.Dropped, ", "))
			}
			logf(msg)
		}
		stats = append(stats, st)
	}
	return stats, nil
}

// tableColumns returns the column names of a table without reading rows —
// portable across all three drivers.
func tableColumns(d *DB, table string) ([]string, error) {
	rows, err := d.Query("SELECT * FROM " + table + " WHERE 1=0")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return rows.Columns()
}

func copyTable(src, dst *DB, table string) (TableStat, error) {
	st := TableStat{Table: table}

	srcCols, err := tableColumns(src, table)
	if err != nil {
		// A table added in a later release won't exist in an older source
		// database (e.g. token_usage when migrating from a pre-usage DB).
		// Skip it instead of aborting the whole migration.
		if isNoTableError(err) {
			return st, nil
		}
		return st, fmt.Errorf("read %s columns: %w", table, err)
	}
	dstCols, err := tableColumns(dst, table)
	if err != nil {
		return st, fmt.Errorf("read %s columns: %w", table, err)
	}

	// Copy only columns the destination schema knows about. Source databases
	// may carry legacy columns from removed features (e.g. ad-hoc ALTERs from
	// old branches); those are reported as dropped rather than failing.
	inDst := make(map[string]bool, len(dstCols))
	for _, c := range dstCols {
		inDst[c] = true
	}
	var cols []string
	for _, c := range srcCols {
		if inDst[c] {
			cols = append(cols, c)
		} else {
			st.Dropped = append(st.Dropped, c)
		}
	}

	srcQuoted := make([]string, len(cols))
	for i, c := range cols {
		srcQuoted[i] = src.Ident(c)
	}
	rows, err := src.Query("SELECT " + strings.Join(srcQuoted, ", ") + " FROM " + table)
	if err != nil {
		return st, fmt.Errorf("read %s: %w", table, err)
	}
	defer rows.Close()

	// Quote every identifier for the destination dialect (the `key` column is
	// reserved in MySQL), then let the DB wrapper rebind `?` as needed.
	quoted := make([]string, len(cols))
	for i, c := range cols {
		quoted[i] = dst.Ident(c)
	}
	insert := fmt.Sprintf("INSERT INTO %s (%s) VALUES (?%s)",
		table, strings.Join(quoted, ", "), strings.Repeat(", ?", len(cols)-1))

	// Rows are inserted in autocommit mode (no wrapping transaction) so a
	// skippable row error doesn't poison subsequent inserts — on PostgreSQL a
	// failed statement aborts the whole transaction. One-shot migrations over
	// small tables don't need batch speed.
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return st, fmt.Errorf("scan %s: %w", table, err)
		}
		// []byte → string so text columns land as text on every driver.
		for i, v := range vals {
			if b, ok := v.([]byte); ok {
				vals[i] = string(b)
			}
		}
		if _, err := dst.Exec(insert, vals...); err != nil {
			if isSkippableRowError(err) {
				st.Skipped++
				continue
			}
			return st, fmt.Errorf("insert into %s: %w", table, err)
		}
		st.Inserted++
	}
	if err := rows.Err(); err != nil {
		return st, fmt.Errorf("read %s: %w", table, err)
	}
	return st, nil
}

// isNoTableError reports whether err is a "table does not exist" error from
// the source driver. The migration source is always SQLite, so we only match
// SQLite's "no such table" spelling; this lets copyTable skip tables that were
// added in a release newer than the source database.
func isNoTableError(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(strings.ToLower(err.Error()), "no such table")
}

// isSkippableRowError reports whether a row-level insert error is safe to
// skip during a one-shot copy: duplicates (re-running a partial migration)
// and FK violations (orphaned rows — SQLite doesn't enforce FKs by default,
// so old databases accumulate them; strict targets reject the insert).
func isSkippableRowError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "duplicate entry") || // MySQL 1062
		strings.Contains(msg, "duplicate key") || // Postgres 23505
		strings.Contains(msg, "unique constraint") || // SQLite
		strings.Contains(msg, "foreign key constraint") // MySQL 1452 / Postgres 23503
}
