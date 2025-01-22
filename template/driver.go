package template

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/motoki317/sc"
	"github.com/traP-jp/h24w-17/domains"
	"github.com/traP-jp/h24w-17/normalizer"
)

var queryMap = make(map[string]domains.CachePlanQuery)

// TODO: generate
const cachePlanRaw = ``

func init() {
	sql.Register("mysql+cache", CacheDriver{})

	plan, err := domains.LoadCachePlan(strings.NewReader(cachePlanRaw))
	if err != nil {
		panic(err)
	}

	for _, query := range plan.Queries {
		queryMap[query.Query] = *query
		if query.Type != domains.CachePlanQueryType_SELECT {
			continue
		}

		if query.Select.Cache {
			caches[query.Query] = cacheWithInfo{
				info:  *query.Select,
				cache: sc.NewMust(replaceFn, 10*time.Minute, 10*time.Minute),
			}
		}
	}

	for _, cache := range caches {
		cacheByTable[cache.info.Table] = append(cacheByTable[cache.info.Table], cache.cache)
	}
}

type CacheDriver struct{}

func (d CacheDriver) Open(dsn string) (driver.Conn, error) {
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		return nil, err
	}
	c, err := mysql.NewConnector(cfg)
	if err != nil {
		return nil, err
	}
	conn, err := c.Connect(context.Background())
	if err != nil {
		return nil, err
	}
	return &CacheConn{inner: conn}, nil
}

type CacheConn struct {
	inner driver.Conn
}

func (c *CacheConn) Prepare(rawQuery string) (driver.Stmt, error) {
	normalized, err := normalizer.NormalizeQuery(rawQuery)
	if err != nil {
		return nil, err
	}

	queryInfo, ok := queryMap[normalized.Query]
	if !ok {
		return c.inner.Prepare(rawQuery)
	}

	if queryInfo.Type == domains.CachePlanQueryType_SELECT && !queryInfo.Select.Cache {
		return c.inner.Prepare(rawQuery)
	}

	innerStmt, err := c.inner.Prepare(rawQuery)
	if err != nil {
		return nil, err
	}
	return &CustomCacheStatement{
		inner:           innerStmt,
		rawQuery:        rawQuery,
		query:           normalized.Query,
		extraConditions: normalized.ExtraConditions,
		queryInfo:       queryInfo,
	}, nil
}

func (c *CacheConn) Close() error {
	return c.inner.Close()
}

func (c *CacheConn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}

func (c *CacheConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if i, ok := c.inner.(driver.ConnBeginTx); ok {
		return i.BeginTx(ctx, opts)
	}
	return c.inner.Begin()
}

type CacheRows struct {
	inner   driver.Rows
	cached  bool
	columns []string
	rows    sliceRows
	limit   int

	mu sync.Mutex
}

func (r *CacheRows) Clone() *CacheRows {
	if !r.cached {
		panic("cannot clone uncached rows")
	}
	return &CacheRows{
		inner:   r.inner,
		cached:  r.cached,
		columns: r.columns,
		rows:    r.rows.clone(),
		limit:   r.limit,
	}
}

func NewCachedRows(inner driver.Rows) *CacheRows {
	return &CacheRows{inner: inner}
}

type row = []driver.Value

type sliceRows struct {
	rows []row
	idx  int
}

func (r sliceRows) clone() sliceRows {
	rows := make([]row, len(r.rows))
	copy(rows, r.rows)
	return sliceRows{rows: rows}
}

func (r *sliceRows) append(row row) {
	r.rows = append(r.rows, row)
}

func (r *sliceRows) reset() {
	r.idx = 0
}

func (r *sliceRows) Next(dest []driver.Value, limit int) error {
	if r.idx >= len(r.rows) {
		r.reset()
		return io.EOF
	}
	if limit > 0 && r.idx >= limit {
		r.reset()
		return io.EOF
	}
	row := r.rows[r.idx]
	r.idx++
	copy(dest, row)
	return nil
}

func (r *CacheRows) Columns() []string {
	if r.cached {
		return r.columns
	}
	columns := r.inner.Columns()
	r.columns = make([]string, len(columns))
	copy(r.columns, columns)
	return columns
}

func (r *CacheRows) Close() error {
	if r.cached {
		r.rows.reset()
		return nil
	}
	r.cached = true
	return r.inner.Close()
}

func (r *CacheRows) Next(dest []driver.Value) error {
	if r.cached {
		return r.rows.Next(dest, r.limit)
	}

	err := r.inner.Next(dest)
	if err != nil {
		if err == io.EOF {
			r.cached = true
			return err
		}
		return err
	}

	cachedRow := make(row, len(dest))
	for i := 0; i < len(dest); i++ {
		switch v := dest[i].(type) {
		case int64, float64, string, bool, time.Time, nil: // no need to copy
			cachedRow[i] = v
		case []byte: // copy to prevent mutation
			data := make([]byte, len(v))
			copy(data, v)
			cachedRow[i] = data
		default:
			// TODO: handle other types
			// Should we mark this row as uncacheable?
		}
	}
	r.rows.append(cachedRow)

	return nil
}
