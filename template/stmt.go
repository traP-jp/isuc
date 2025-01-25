package template

import (
	"context"
	"database/sql/driver"
	"fmt"
	"strings"

	"github.com/motoki317/sc"
	"github.com/traP-jp/h24w-17/domains"
	"github.com/traP-jp/h24w-17/normalizer"
)

type (
	queryKey          struct{}
	stmtKey           struct{}
	argsKey           struct{}
	queryerCtxKey     struct{}
	namedValueArgsKey struct{}
)

type cacheWithInfo struct {
	query  string
	info   domains.CachePlanSelectQuery
	pkOnly bool // if true, query is like "SELECT * FROM table WHERE pk = ?"
	cache  *sc.Cache[string, *CacheRows]
}

// NOTE: no write happens to this map, so it's safe to use in concurrent environment
var caches = make(map[string]cacheWithInfo)

var cacheByTable = make(map[string][]cacheWithInfo)

func ExportMetrics() string {
	res := ""
	for query, cache := range caches {
		res += "query: " + query + "\n"
		res += cache.cache.Stats().String() + "\n"
	}
	return res
}

var _ driver.Stmt = &CustomCacheStatement{}

type CustomCacheStatement struct {
	inner    driver.Stmt
	conn     *CacheConn
	rawQuery string
	// query is the normalized query
	query     string
	extraArgs []normalizer.ExtraArg
	queryInfo domains.CachePlanQuery
}

func (s *CustomCacheStatement) Close() error {
	return s.inner.Close()
}

func (s *CustomCacheStatement) NumInput() int {
	return s.inner.NumInput()
}

func (s *CustomCacheStatement) Exec(args []driver.Value) (driver.Result, error) {
	switch s.queryInfo.Type {
	case domains.CachePlanQueryType_INSERT:
		return s.execInsert(args)
	case domains.CachePlanQueryType_UPDATE:
		return s.execUpdate(args)
	case domains.CachePlanQueryType_DELETE:
		return s.execDelete(args)
	}
	return s.inner.Exec(args)
}

func (s *CustomCacheStatement) execInsert(args []driver.Value) (driver.Result, error) {
	table := s.queryInfo.Insert.Table
	// TODO: support composite primary key and other unique key
	for _, cache := range cacheByTable[table] {
		if cache.pkOnly {
			// no need to purge
		} else {
			cache.cache.Purge()
		}
	}
	return s.inner.Exec(args)
}

func (s *CustomCacheStatement) execUpdate(args []driver.Value) (driver.Result, error) {
	// TODO: support composite primary key and other unique key
	table := s.queryInfo.Update.Table
	pk := retrievePrimaryKey(table)

	// if query is like "UPDATE table SET ... WHERE pk = ?"
	updateByPk := len(s.queryInfo.Update.Conditions) == 1 && s.queryInfo.Update.Conditions[0].Column == pk
	if !updateByPk {
		// we should purge all cache
		for _, cache := range cacheByTable[table] {
			cache.cache.Purge()
		}
		return s.inner.Exec(args)
	}

	pkValue := args[s.queryInfo.Update.Conditions[0].Placeholder.Index]

	for _, cache := range cacheByTable[table] {
		if cache.pkOnly {
			// we should forget the cache
			cache.cache.Forget(cacheKey([]driver.Value{pkValue}))
		} else {
			cache.cache.Purge()
		}
	}

	return s.inner.Exec(args)
}

func (s *CustomCacheStatement) execDelete(args []driver.Value) (driver.Result, error) {
	table := s.queryInfo.Delete.Table
	pk := retrievePrimaryKey(table)

	// if query is like "DELETE FROM table WHERE pk = ?"
	deleteByPk := len(s.queryInfo.Delete.Conditions) == 1 && s.queryInfo.Delete.Conditions[0].Column == pk
	if !deleteByPk {
		// we should purge all cache
		for _, cache := range cacheByTable[table] {
			cache.cache.Purge()
		}
		return s.inner.Exec(args)
	}

	pkValue := args[s.queryInfo.Delete.Conditions[0].Placeholder.Index]

	for _, cache := range cacheByTable[table] {
		if cache.pkOnly {
			// query like "SELECT * FROM table WHERE pk = ?"
			// we should forget the cache
			cache.cache.Forget(cacheKey([]driver.Value{pkValue}))
		} else {
			cache.cache.Purge()
		}
	}

	return s.inner.Exec(args)
}

func (s *CustomCacheStatement) Query(args []driver.Value) (driver.Rows, error) {
	ctx := context.WithValue(context.Background(), stmtKey{}, s)
	ctx = context.WithValue(ctx, argsKey{}, args)

	pk := retrievePrimaryKey(s.queryInfo.Select.Table)

	conditions := s.queryInfo.Select.Conditions
	// if query is like "SELECT * FROM table WHERE pk IN (?, ?, ?, ...)"
	if len(conditions) == 1 && conditions[0].Column == pk && conditions[0].Operator == domains.CachePlanOperator_IN {
		return s.inQuery(args)
	}

	rows, err := caches[cacheName(s.query)].cache.Get(ctx, cacheKey(args))
	if err != nil {
		return nil, err
	}

	return rows, nil
}

func (s *CustomCacheStatement) inQuery(args []driver.Value) (driver.Rows, error) {
	// "SELECT * FROM table WHERE pk IN (?, ?, ...)"
	// separate the query into multiple queries and merge the results
	table := s.queryInfo.Select.Table
	pkIndex := s.queryInfo.Select.Conditions[0].Placeholder.Index
	pkValues := args[pkIndex:]

	// find the pkOnly query "SELECT * FROM table WHERE pk = ?"
	var cache cacheWithInfo
	for _, c := range cacheByTable[table] {
		if c.pkOnly {
			cache = c
			break
		}
	}

	allRows := make([]*CacheRows, 0, len(pkValues))
	for _, pkValue := range pkValues {
		// prepare new statement
		stmt, err := s.conn.Prepare(s.query)
		if err != nil {
			return nil, err
		}
		ctx := context.WithValue(context.Background(), stmtKey{}, stmt)
		ctx = context.WithValue(ctx, argsKey{}, []driver.Value{pkValue})
		rows, err := cache.cache.Get(ctx, cacheKey([]driver.Value{pkValue}))
		if err != nil {
			return nil, err
		}
		allRows = append(allRows, rows)
	}

	return mergeCachedRows(allRows), nil
}

func (c *CacheConn) QueryContext(ctx context.Context, rawQuery string, nvargs []driver.NamedValue) (driver.Rows, error) {
	normalized, err := normalizer.NormalizeQuery(rawQuery)
	if err != nil {
		return nil, err
	}

	inner, ok := c.inner.(driver.QueryerContext)
	if !ok {
		return nil, driver.ErrSkip
	}

	queryInfo, ok := queryMap[normalized.Query]
	if !ok {
		return inner.QueryContext(ctx, rawQuery, nvargs)
	}
	if queryInfo.Type != domains.CachePlanQueryType_SELECT || !queryInfo.Select.Cache {
		return inner.QueryContext(ctx, rawQuery, nvargs)
	}

	pk := retrievePrimaryKey(queryInfo.Select.Table)

	conditions := queryInfo.Select.Conditions
	// if query is like "SELECT * FROM table WHERE pk IN (?, ?, ?, ...)"
	if len(conditions) == 1 && conditions[0].Column == pk && conditions[0].Operator == domains.CachePlanOperator_IN {
		return c.inQuery(ctx, rawQuery, nvargs, inner)
	}

	args := make([]driver.Value, len(nvargs))
	for i, nv := range nvargs {
		args[i] = nv.Value
	}

	cache := caches[queryInfo.Query].cache
	cachectx := context.WithValue(ctx, namedValueArgsKey{}, nvargs)
	cachectx = context.WithValue(cachectx, queryerCtxKey{}, inner)
	cachectx = context.WithValue(cachectx, queryKey{}, rawQuery)
	rows, err := cache.Get(cachectx, cacheKey(args))
	if err != nil {
		return nil, err
	}

	return rows, nil
}

func (c *CacheConn) inQuery(ctx context.Context, query string, args []driver.NamedValue, inner driver.QueryerContext) (driver.Rows, error) {
	// "SELECT * FROM table WHERE pk IN (?, ?, ...)"
	// separate the query into multiple queries and merge the results
	normalized, err := normalizer.NormalizeQuery(query)
	if err != nil {
		return nil, err
	}

	queryInfo := queryMap[normalized.Query]
	table := queryInfo.Select.Table
	pkIndex := queryInfo.Select.Conditions[0].Placeholder.Index
	pkValues := args[pkIndex:]

	// find the pkOnly query "SELECT * FROM table WHERE pk = ?"
	var cache cacheWithInfo
	for _, c := range cacheByTable[table] {
		if c.pkOnly {
			cache = c
			break
		}
	}

	allRows := make([]*CacheRows, 0, len(pkValues))
	for _, pkValue := range pkValues {
		cacheCtx := context.WithValue(ctx, queryKey{}, query)
		cacheCtx = context.WithValue(ctx, queryerCtxKey{}, inner)
		cacheCtx = context.WithValue(cacheCtx, namedValueArgsKey{}, args)
		rows, err := cache.cache.Get(cacheCtx, cacheKey([]driver.Value{pkValue.Value}))
		if err != nil {
			return nil, err
		}
		allRows = append(allRows, rows)
	}

	return mergeCachedRows(allRows), nil
}

func cacheName(query string) string {
	return query
}

func cacheKey(args []driver.Value) string {
	var b strings.Builder
	for _, arg := range args {
		switch v := arg.(type) {
		case string:
			b.WriteString(v)
		case []byte:
			b.Write(v)
		default:
			fmt.Fprintf(&b, "%v", v)
		}
		// delimiter
		b.WriteByte(0)
	}
	return b.String()
}

func replaceFn(ctx context.Context, key string) (*CacheRows, error) {
	var res *CacheRows

	queryerCtx, ok := ctx.Value(queryerCtxKey{}).(driver.QueryerContext)
	if ok {
		query := ctx.Value(queryKey{}).(string)
		nvargs := ctx.Value(namedValueArgsKey{}).([]driver.NamedValue)
		rows, err := queryerCtx.QueryContext(ctx, query, nvargs)
		if err != nil {
			return nil, err
		}
		res = NewCacheRows(rows)
	} else {
		stmt := ctx.Value(stmtKey{}).(*CustomCacheStatement)
		args := ctx.Value(argsKey{}).([]driver.Value)
		rows, err := stmt.inner.Query(args)
		if err != nil {
			return nil, err
		}
		res = NewCacheRows(rows)
	}

	if err := res.createCache(); err != nil {
		return nil, err
	}

	return res.Clone(), nil
}

func retrievePrimaryKey(table string) string {
	for name, col := range tableSchema[table].Columns {
		if col.IsPrimary {
			return name
		}
	}
	return ""
}
