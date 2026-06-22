package shellsqlite

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
)

func init() { sql.Register("sqlite3", drv{}) }

type drv struct{}

func (drv) Open(name string) (driver.Conn, error) { return conn{name: name}, nil }

type conn struct{ name string }

func (c conn) Prepare(q string) (driver.Stmt, error) { return stmt{c: c, q: q}, nil }
func (c conn) Close() error                          { return nil }
func (c conn) Begin() (driver.Tx, error)             { return tx{}, nil }

type tx struct{}

func (tx) Commit() error   { return nil }
func (tx) Rollback() error { return nil }

type stmt struct {
	c conn
	q string
}

func (s stmt) Close() error                                    { return nil }
func (s stmt) NumInput() int                                   { return strings.Count(s.q, "?") }
func (s stmt) Exec(args []driver.Value) (driver.Result, error) { return s.c.exec(fill(s.q, args)) }
func (s stmt) Query(args []driver.Value) (driver.Rows, error)  { return s.c.query(fill(s.q, args)) }
func (c conn) exec(sqls string) (driver.Result, error) {
	cmd := exec.Command("sqlite3", c.name)
	cmd.Stdin = strings.NewReader(sqls)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("%v: %s", err, out)
	}
	return driver.RowsAffected(0), nil
}
func (c conn) query(sqls string) (driver.Rows, error) {
	cmd := exec.Command("sqlite3", "-batch", "-noheader", "-separator", "\t", c.name, sqls)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("%v: %s", err, out)
	}
	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	if len(lines) == 1 && lines[0] == "" {
		lines = nil
	}
	cols := make([]string, 0)
	if len(lines) > 0 {
		for i := range strings.Split(lines[0], "\t") {
			cols = append(cols, fmt.Sprintf("c%d", i))
		}
	}
	return &rows{cols: cols, lines: lines}, nil
}
func fill(q string, args []driver.Value) string {
	for _, a := range args {
		q = strings.Replace(q, "?", quote(a), 1)
	}
	return q
}
func quote(v any) string {
	switch x := v.(type) {
	case nil:
		return "NULL"
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case bool:
		if x {
			return "1"
		}
		return "0"
	case []byte:
		return "'" + strings.ReplaceAll(string(x), "'", "''") + "'"
	default:
		return "'" + strings.ReplaceAll(fmt.Sprint(x), "'", "''") + "'"
	}
}

type rows struct {
	cols  []string
	lines []string
	i     int
}

func (r *rows) Columns() []string { return r.cols }
func (r *rows) Close() error      { return nil }
func (r *rows) Next(dest []driver.Value) error {
	if r.i >= len(r.lines) {
		return io.EOF
	}
	parts := strings.Split(r.lines[r.i], "\t")
	r.i++
	for i := range dest {
		if i < len(parts) {
			dest[i] = parts[i]
		} else {
			dest[i] = nil
		}
	}
	return nil
}

var _ driver.QueryerContext = conn{}
var _ driver.ExecerContext = conn{}

func (c conn) QueryContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	v := make([]driver.Value, len(args))
	for i, a := range args {
		v[i] = a.Value
	}
	return c.query(fill(q, v))
}
func (c conn) ExecContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Result, error) {
	v := make([]driver.Value, len(args))
	for i, a := range args {
		v[i] = a.Value
	}
	return c.exec(fill(q, v))
}
