// Package dbtest provides test-support helpers for counting the SQL
// statements a service or handler issues.
//
// It exists for exactly one reason: a regression test that proves "the number
// of queries does not grow with the number of rows" needs to count queries.
// Nothing on the production path depends on this package.
//
// The counter sits on driver.Conn.Prepare: the SQLite driver (modernc) uses
// database/sql's legacy path (one Prepare per statement), so one Prepare
// corresponds to exactly one statement.
package dbtest

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"sync/atomic"

	"github.com/novaworkbench/backend/internal/db"
)

// countingConnector creates connections that count the statements they run.
type countingConnector struct {
	base  driver.Driver
	dsn   string
	count *int64
}

func (c *countingConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.base.Open(c.dsn)
	if err != nil {
		return nil, err
	}
	return &countingConn{Conn: conn, count: c.count}, nil
}

func (c *countingConnector) Driver() driver.Driver { return c.base }

// countingConn counts one statement per Prepare.
type countingConn struct {
	driver.Conn
	count *int64
}

func (c *countingConn) Prepare(query string) (driver.Stmt, error) {
	atomic.AddInt64(c.count, 1)
	return c.Conn.Prepare(query)
}

// NewCountingDB replaces the pool of d with a counting one and returns the
// counter.
//
// path is the SQLite file d was opened with — the counting pool reopens the
// same file. The schema must already be migrated (db.Init does that) because
// the counting pool takes over afterwards, so only the statements the caller
// triggers are counted.
//
// The returned counter is safe for concurrent use.
func NewCountingDB(d *db.DB, path string) (*int64, error) {
	var count int64
	raw := sql.OpenDB(&countingConnector{
		base:  d.Driver(),
		dsn:   path + "?_journal_mode=WAL&_busy_timeout=5000",
		count: &count,
	})
	raw.SetMaxOpenConns(1)
	raw.SetMaxIdleConns(1)
	// The migration pool hands over to the counting pool.
	_ = d.DB.Close()
	d.DB = raw
	return &count, nil
}
