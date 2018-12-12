// Package sql provides SQL implementations of the storage interface.
package sql

import (
	"database/sql"
	"regexp"
	"time"

	"github.com/sirupsen/logrus"

	// import third party drivers
	_ "github.com/lib/pq"
	_ "github.com/mattn/go-sqlite3"
)

// flavor represents a specific SQL implementation, and is used to translate query strings
// between different drivers. Flavors shouldn't aim to translate all possible SQL statements,
// only the specific queries used by the SQL storages.
type flavor struct {
	queryReplacers []replacer

	// Function for locking specific rows in table, which column has specified value. It must
	// return error if locking failed. If underlying database doesn't support locking (sqlite3)
	// just make an empty implementation.
	lockForUpdate func(tx *trans, table, column, value string) error

	// Does the flavor support timezones?
	supportsTimezones bool
}

// A regexp with a replacement string.
type replacer struct {
	re   *regexp.Regexp
	with string
}

// Match a postgres query binds. E.g. "$1", "$12", etc.
var bindRegexp = regexp.MustCompile(`\$\d+`)

func matchLiteral(s string) *regexp.Regexp {
	return regexp.MustCompile(`\b` + regexp.QuoteMeta(s) + `\b`)
}

var (
	// The "github.com/lib/pq" driver is the default flavor. All others are
	// translations of this.
	flavorPostgres = flavor{
		lockForUpdate: func(tx *trans, table, column, value string) error {
			_, err := tx.Exec("SELECT 1 FROM "+table+" WHERE "+column+" = $1 FOR UPDATE NOWAIT;", value)
			return err
		},

		supportsTimezones: true,
	}

	flavorSQLite3 = flavor{
		queryReplacers: []replacer{
			{bindRegexp, "?"},
			// Translate for booleans to integers.
			{matchLiteral("true"), "1"},
			{matchLiteral("false"), "0"},
			{matchLiteral("boolean"), "integer"},
			// Translate other types.
			{matchLiteral("bytea"), "blob"},
			{matchLiteral("timestamptz"), "timestamp"},
			// SQLite doesn't have a "now()" method, replace with "date('now')"
			{regexp.MustCompile(`\bnow\(\)`), "date('now')"},
		},

		// There is no requirement for concurrent access for SQLite3
		lockForUpdate: func(tx *trans, table, column, value string) error {
			return nil
		},
	}
)

func (f flavor) translate(query string) string {
	// TODO(ericchiang): Heavy cashing.
	for _, r := range f.queryReplacers {
		query = r.re.ReplaceAllString(query, r.with)
	}
	return query
}

// translateArgs translates query parameters that may be unique to
// a specific SQL flavor. For example, standardizing "time.Time"
// types to UTC for clients that don't provide timezone support.
func (c *conn) translateArgs(args []interface{}) []interface{} {
	if c.flavor.supportsTimezones {
		return args
	}

	for i, arg := range args {
		if t, ok := arg.(time.Time); ok {
			args[i] = t.UTC()
		}
	}
	return args
}

// conn is the main database connection.
type conn struct {
	db                 *sql.DB
	flavor             flavor
	logger             logrus.FieldLogger
	alreadyExistsCheck func(err error) bool
}

func (c *conn) Close() error {
	return c.db.Close()
}

// conn implements the same method signatures as encoding/sql.DB.

func (c *conn) Exec(query string, args ...interface{}) (sql.Result, error) {
	query = c.flavor.translate(query)
	return c.db.Exec(query, c.translateArgs(args)...)
}

func (c *conn) Query(query string, args ...interface{}) (*sql.Rows, error) {
	query = c.flavor.translate(query)
	return c.db.Query(query, c.translateArgs(args)...)
}

func (c *conn) QueryRow(query string, args ...interface{}) *sql.Row {
	query = c.flavor.translate(query)
	return c.db.QueryRow(query, c.translateArgs(args)...)
}

// ExecTx runs a method which operates on a transaction.
func (c *conn) ExecTx(fn func(tx *trans) error) error {
	sqlTx, err := c.db.Begin()
	if err != nil {
		return err
	}
	if err := fn(&trans{sqlTx, c}); err != nil {
		sqlTx.Rollback()
		return err
	}
	return sqlTx.Commit()
}

type trans struct {
	tx *sql.Tx
	c  *conn
}

// trans implements the same method signatures as encoding/sql.Tx.

func (t *trans) Exec(query string, args ...interface{}) (sql.Result, error) {
	query = t.c.flavor.translate(query)
	return t.tx.Exec(query, t.c.translateArgs(args)...)
}

func (t *trans) Query(query string, args ...interface{}) (*sql.Rows, error) {
	query = t.c.flavor.translate(query)
	return t.tx.Query(query, t.c.translateArgs(args)...)
}

func (t *trans) QueryRow(query string, args ...interface{}) *sql.Row {
	query = t.c.flavor.translate(query)
	return t.tx.QueryRow(query, t.c.translateArgs(args)...)
}
