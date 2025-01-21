package template

import (
	"context"
	"database/sql/driver"

	"github.com/motoki317/sc"
	"github.com/traP-jp/h24w-17/domains"
)

type (
	stmtKey struct{}
	argsKey struct{}
)

type cacheWithInfo struct {
	info  domains.CachePlanSelectQuery
	cache *sc.Cache[struct{}, driver.Rows]
}

// NOTE: no write happens to this map, so it's safe to use in concurrent environment
var caches = make(map[string]cacheWithInfo)

var cacheByTable = make(map[string][]*sc.Cache[struct{}, driver.Rows])

type CustomCacheStatement struct {
	inner     driver.Stmt
	query     string
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
	for _, cache := range cacheByTable[table] {
		cache.Purge()
	}
	return s.inner.Exec(args)
}

func (s *CustomCacheStatement) execUpdate(args []driver.Value) (driver.Result, error) {
	// TODO: purge only necessary cache
	table := s.queryInfo.Update.Table
	for _, cache := range cacheByTable[table] {
		cache.Purge()
	}
	return s.inner.Exec(args)
}

func (s *CustomCacheStatement) execDelete(args []driver.Value) (driver.Result, error) {
	table := s.queryInfo.Delete.Table
	for _, cache := range cacheByTable[table] {
		cache.Purge()
	}
	return s.inner.Exec(args)
}

func (s *CustomCacheStatement) Query(args []driver.Value) (driver.Rows, error) {
	ctx := context.WithValue(context.Background(), stmtKey{}, s)
	ctx = context.WithValue(ctx, argsKey{}, args)
	return caches[cacheName(s.query)].cache.Get(ctx, struct{}{})
}

func cacheName(query string) string {
	return query
}

func replaceFn(ctx context.Context, _ struct{}) (driver.Rows, error) {
	stmt := ctx.Value(stmtKey{}).(*CustomCacheStatement)
	args := ctx.Value(argsKey{}).([]driver.Value)
	rows, err := stmt.inner.Query(args)
	if err != nil {
		return nil, err
	}
	return NewCachedRows(rows), nil
}
