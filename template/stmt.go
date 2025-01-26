package template

import (
	"context"
	"database/sql/driver"
	"fmt"
	"log"
	"slices"
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
	query      string
	info       domains.CachePlanSelectQuery
	uniqueOnly bool // if true, query is like "SELECT * FROM table WHERE pk = ?"
	cache      *sc.Cache[string, *cacheRows]
}

// NOTE: no write happens to this map, so it's safe to use in concurrent environment
var caches = make(map[string]cacheWithInfo)

var cacheByTable = make(map[string][]cacheWithInfo)

func ExportMetrics() string {
	res := ""
	for query, cache := range caches {
		stats := cache.cache.Stats()
		progress := "["
		for i := 0; i < 20; i++ {
			if i < int(stats.HitRatio()*20) {
				progress += "#"
			} else {
				progress += "-"
			}
		}
		statsStr := fmt.Sprintf("%s (%.2f%% - %d/%d) (%d replace) (size %d)", progress, stats.HitRatio()*100, stats.Hits, stats.Misses+stats.Hits, stats.Replacements, stats.Size)
		res += fmt.Sprintf("query: \"%s\"\n%s\n\n", query, statsStr)
	}
	return res
}

func PurgeAllCaches() {
	for _, cache := range caches {
		cache.cache.Purge()
	}
}

var _ driver.Stmt = &customCacheStatement{}

type customCacheStatement struct {
	inner    driver.Stmt
	conn     *cacheConn
	rawQuery string
	// query is the normalized query
	query     string
	queryInfo domains.CachePlanQuery
}

func (s *customCacheStatement) Close() error {
	return s.inner.Close()
}

func (s *customCacheStatement) NumInput() int {
	return s.inner.NumInput()
}

func (s *customCacheStatement) Exec(args []driver.Value) (driver.Result, error) {
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

func execInsert(queryInfo domains.CachePlanQuery, args []driver.Value) {
	table := queryInfo.Insert.Table
	normalizedArgs, _ := normalizer.NormalizeArgs(queryInfo.Query)

	rows := slices.Chunk(args, len(queryInfo.Insert.Columns))

	for _, cache := range cacheByTable[table] {
		if cache.uniqueOnly {
			// no need to purge
			continue
		}

		selectConditions := cache.info.Conditions
		if len(selectConditions) != 1 || len(normalizedArgs.ExtraArgs) != 0 || selectConditions[0].Operator != domains.CachePlanOperator_EQ {
			cache.cache.Purge()
			continue
		}

		selectCondition := selectConditions[0]
		insertColumnIdx := slices.Index(queryInfo.Insert.Columns, selectCondition.Column)
		if insertColumnIdx >= 0 {
			// insert query is like "INSERT INTO table (col1, col2, ...) VALUES (?, ?, ...)"
			// select query is like "SELECT * FROM table WHERE col1 = ?"
			// forget the cache
			for row := range rows {
				cache.cache.Forget(cacheKey([]driver.Value{row[insertColumnIdx]}))
			}
		} else {
			cache.cache.Purge()
		}
	}
}

func (s *customCacheStatement) execInsert(args []driver.Value) (driver.Result, error) {
	execInsert(s.queryInfo, args)
	return s.inner.Exec(args)
}

func (s *customCacheStatement) execUpdate(args []driver.Value) (driver.Result, error) {
	// TODO: support composite primary key and other unique key
	table := s.queryInfo.Update.Table

	// if query is like "UPDATE table SET ... WHERE pk = ?"
	var updateByUnique bool
	if len(s.queryInfo.Update.Conditions) == 1 {
		condition := s.queryInfo.Update.Conditions[0]
		column := tableSchema[table].Columns[condition.Column]
		updateByUnique = (column.IsPrimary || column.IsUnique) && condition.Operator == domains.CachePlanOperator_EQ
	}
	if !updateByUnique {
		for _, cache := range cacheByTable[table] {
			if !usedBySelectQuery(cache.info.Targets, s.queryInfo.Update.Targets) {
				// no need to purge because the cache does not contain the updated column
				continue
			}
			// we should purge all cache
			cache.cache.Purge()
		}
		return s.inner.Exec(args)
	}

	uniqueValue := args[s.queryInfo.Update.Conditions[0].Placeholder.Index]

	for _, cache := range cacheByTable[table] {
		if cache.uniqueOnly && usedBySelectQuery(cache.info.Targets, s.queryInfo.Update.Targets) {
			// we should forget the cache
			cache.cache.Forget(cacheKey([]driver.Value{uniqueValue}))
		} else {
			if !usedBySelectQuery(cache.info.Targets, s.queryInfo.Update.Targets) {
				// no need to purge because the cache does not contain the updated column
				continue
			}
			cache.cache.Purge()
		}
	}

	return s.inner.Exec(args)
}

func (s *customCacheStatement) execDelete(args []driver.Value) (driver.Result, error) {
	table := s.queryInfo.Delete.Table

	// if query is like "DELETE FROM table WHERE unique = ?"
	var deleteByUnique bool
	if len(s.queryInfo.Delete.Conditions) == 1 {
		condition := s.queryInfo.Delete.Conditions[0]
		column := tableSchema[table].Columns[condition.Column]
		deleteByUnique = (column.IsPrimary || column.IsUnique) && condition.Operator == domains.CachePlanOperator_EQ
	}
	if !deleteByUnique {
		// we should purge all cache
		for _, cache := range cacheByTable[table] {
			cache.cache.Purge()
		}
		return s.inner.Exec(args)
	}

	uniqueValue := args[s.queryInfo.Delete.Conditions[0].Placeholder.Index]

	for _, cache := range cacheByTable[table] {
		if cache.uniqueOnly {
			// query like "SELECT * FROM table WHERE pk = ?"
			// we should forget the cache
			cache.cache.Forget(cacheKey([]driver.Value{uniqueValue}))
		} else {
			cache.cache.Purge()
		}
	}

	return s.inner.Exec(args)
}

func (c *cacheConn) ExecContext(ctx context.Context, rawQuery string, nvargs []driver.NamedValue) (driver.Result, error) {
	inner, ok := c.inner.(driver.ExecerContext)
	if !ok {
		return nil, driver.ErrSkip
	}

	normalizedQuery := normalizer.NormalizeQuery(rawQuery)

	queryInfo, ok := queryMap[normalizedQuery]
	if !ok {
		log.Println("unknown query:", normalizedQuery)
		PurgeAllCaches()
		return inner.ExecContext(ctx, rawQuery, nvargs)
	}

	switch queryInfo.Type {
	case domains.CachePlanQueryType_INSERT:
		return c.execInsert(ctx, queryInfo, nvargs, inner)
	case domains.CachePlanQueryType_UPDATE:
		return c.execUpdate(ctx, queryInfo, nvargs, inner)
	case domains.CachePlanQueryType_DELETE:
		return c.execDelete(ctx, queryInfo, nvargs, inner)
	}

	return inner.ExecContext(ctx, rawQuery, nvargs)
}

func (c *cacheConn) execInsert(ctx context.Context, queryInfo domains.CachePlanQuery, nvargs []driver.NamedValue, inner driver.ExecerContext) (driver.Result, error) {
	args := make([]driver.Value, 0, len(nvargs))
	for _, nv := range nvargs {
		args = append(args, nv.Value)
	}

	execInsert(queryInfo, args)

	return inner.ExecContext(ctx, queryInfo.Query, nvargs)
}

func usedBySelectQuery(selectTarget []string, updateTarget []domains.CachePlanUpdateTarget) bool {
	for _, target := range updateTarget {
		inSelectTarget := slices.ContainsFunc(selectTarget, func(selectTarget string) bool {
			return selectTarget == target.Column
		})
		if inSelectTarget {
			return true
		}
	}
	return false
}

func (c *cacheConn) execUpdate(ctx context.Context, queryInfo domains.CachePlanQuery, args []driver.NamedValue, inner driver.ExecerContext) (driver.Result, error) {
	table := queryInfo.Update.Table

	// if query is like "UPDATE table SET ... WHERE pk = ?"
	var updateByUnique bool
	if len(queryInfo.Update.Conditions) == 1 {
		condition := queryInfo.Update.Conditions[0]
		column := tableSchema[table].Columns[condition.Column]
		updateByUnique = (column.IsPrimary || column.IsUnique) && condition.Operator == domains.CachePlanOperator_EQ
	}
	if !updateByUnique {
		for _, cache := range cacheByTable[table] {
			if !usedBySelectQuery(cache.info.Targets, queryInfo.Update.Targets) {
				// no need to purge because the cache does not contain the updated column
				continue
			}
			// we should purge all cache
			cache.cache.Purge()
		}
		return inner.ExecContext(ctx, queryInfo.Query, args)
	}

	uniqueValue := args[queryInfo.Update.Conditions[0].Placeholder.Index]

	for _, cache := range cacheByTable[table] {
		if cache.uniqueOnly && usedBySelectQuery(cache.info.Targets, queryInfo.Update.Targets) {
			// we should forget the cache
			cache.cache.Forget(cacheKey([]driver.Value{uniqueValue.Value}))
		} else {
			if !usedBySelectQuery(cache.info.Targets, queryInfo.Update.Targets) {
				// no need to purge because the cache does not contain the updated column
				continue
			}
			cache.cache.Purge()
		}
	}

	return inner.ExecContext(ctx, queryInfo.Query, args)
}

func (c *cacheConn) execDelete(ctx context.Context, queryInfo domains.CachePlanQuery, args []driver.NamedValue, inner driver.ExecerContext) (driver.Result, error) {
	table := queryInfo.Delete.Table

	// if query is like "DELETE FROM table WHERE unique = ?"
	var deleteByUnique bool
	if len(queryInfo.Delete.Conditions) == 1 {
		condition := queryInfo.Delete.Conditions[0]
		column := tableSchema[table].Columns[condition.Column]
		deleteByUnique = (column.IsPrimary || column.IsUnique) && condition.Operator == domains.CachePlanOperator_EQ
	}
	if !deleteByUnique {
		// we should purge all cache
		for _, cache := range cacheByTable[table] {
			cache.cache.Purge()
		}
		return inner.ExecContext(ctx, queryInfo.Query, args)
	}

	uniqueValue := args[queryInfo.Delete.Conditions[0].Placeholder.Index]

	for _, cache := range cacheByTable[table] {
		if cache.uniqueOnly {
			// query like "SELECT * FROM table WHERE pk = ?"
			// we should forget the cache
			cache.cache.Forget(cacheKey([]driver.Value{uniqueValue.Value}))
		} else {
			cache.cache.Purge()
		}
	}

	return inner.ExecContext(ctx, queryInfo.Query, args)
}

func (s *customCacheStatement) Query(args []driver.Value) (driver.Rows, error) {
	ctx := context.WithValue(context.Background(), stmtKey{}, s)
	ctx = context.WithValue(ctx, argsKey{}, args)

	conditions := s.queryInfo.Select.Conditions
	// if query is like "SELECT * FROM table WHERE cond IN (?, ?, ?, ...)"
	if len(conditions) == 1 && conditions[0].Operator == domains.CachePlanOperator_IN {
		return s.inQuery(args)
	}

	rows, err := caches[cacheName(s.query)].cache.Get(ctx, cacheKey(args))
	if err != nil {
		return nil, err
	}

	return rows, nil
}

func (s *customCacheStatement) inQuery(args []driver.Value) (driver.Rows, error) {
	// "SELECT * FROM table WHERE cond IN (?, ?, ...)"
	// separate the query into multiple queries and merge the results
	table := s.queryInfo.Select.Table
	condIdx := s.queryInfo.Select.Conditions[0].Placeholder.Index
	condValues := args[condIdx:]

	// find the query "SELECT * FROM table WHERE cond = ?"
	var cache *cacheWithInfo
	for _, c := range cacheByTable[table] {
		if len(c.info.Conditions) == 1 && c.info.Conditions[0].Column == s.queryInfo.Select.Conditions[0].Column && c.info.Conditions[0].Operator == domains.CachePlanOperator_EQ {
			cache = &c
		}
	}
	if cache == nil {
		return s.inner.Query(args)
	}

	allRows := make([]*cacheRows, 0, len(condValues))
	for _, condValue := range condValues {
		// prepare new statement
		stmt, err := s.conn.Prepare(cache.query)
		if err != nil {
			return nil, err
		}
		ctx := context.WithValue(context.Background(), stmtKey{}, stmt)
		ctx = context.WithValue(ctx, argsKey{}, []driver.Value{condValue})
		rows, err := cache.cache.Get(ctx, cacheKey([]driver.Value{condValue}))
		if err != nil {
			return nil, err
		}
		allRows = append(allRows, rows)
	}

	return mergeCachedRows(allRows), nil
}

func (c *cacheConn) QueryContext(ctx context.Context, rawQuery string, nvargs []driver.NamedValue) (driver.Rows, error) {
	inner, ok := c.inner.(driver.QueryerContext)
	if !ok {
		return nil, driver.ErrSkip
	}

	normalizedQuery := normalizer.NormalizeQuery(rawQuery)

	queryInfo, ok := queryMap[normalizedQuery]
	if !ok {
		return inner.QueryContext(ctx, rawQuery, nvargs)
	} else if strings.Contains(normalizedQuery, "FOR UPDATE") {
		return inner.QueryContext(ctx, rawQuery, nvargs)
	}
	if queryInfo.Type != domains.CachePlanQueryType_SELECT || !queryInfo.Select.Cache {
		return inner.QueryContext(ctx, rawQuery, nvargs)
	}

	conditions := queryInfo.Select.Conditions
	// if query is like "SELECT * FROM table WHERE cond IN (?, ?, ?, ...)"
	if len(conditions) == 1 && conditions[0].Operator == domains.CachePlanOperator_IN {
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

func (c *cacheConn) inQuery(ctx context.Context, query string, args []driver.NamedValue, inner driver.QueryerContext) (driver.Rows, error) {
	// "SELECT * FROM table WHERE cond IN (?, ?, ...)"
	// separate the query into multiple queries and merge the results
	normalizedQuery := normalizer.NormalizeQuery(query)

	queryInfo := queryMap[normalizedQuery]
	table := queryInfo.Select.Table
	condIdx := queryInfo.Select.Conditions[0].Placeholder.Index
	condValues := args[condIdx:]

	// find the query "SELECT * FROM table WHERE cond = ?"
	var cache *cacheWithInfo
	for _, c := range cacheByTable[table] {
		if len(c.info.Conditions) == 1 && c.info.Conditions[0].Column == queryInfo.Select.Conditions[0].Column && c.info.Conditions[0].Operator == domains.CachePlanOperator_EQ {
			cache = &c
		}
	}
	if cache == nil {
		return inner.QueryContext(ctx, query, args)
	}

	allRows := make([]*cacheRows, 0, len(condValues))
	for _, condValue := range condValues {
		nvargs := []driver.NamedValue{condValue}
		cacheCtx := context.WithValue(ctx, queryKey{}, cache.query)
		cacheCtx = context.WithValue(cacheCtx, queryerCtxKey{}, inner)
		cacheCtx = context.WithValue(cacheCtx, namedValueArgsKey{}, nvargs)
		rows, err := cache.cache.Get(cacheCtx, cacheKey([]driver.Value{condValue.Value}))
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

func replaceFn(ctx context.Context, key string) (*cacheRows, error) {
	var res *cacheRows

	queryerCtx, ok := ctx.Value(queryerCtxKey{}).(driver.QueryerContext)
	if ok {
		query := ctx.Value(queryKey{}).(string)
		nvargs := ctx.Value(namedValueArgsKey{}).([]driver.NamedValue)
		rows, err := queryerCtx.QueryContext(ctx, query, nvargs)
		if err != nil {
			return nil, err
		}
		res = newCacheRows(rows)
	} else {
		stmt := ctx.Value(stmtKey{}).(*customCacheStatement)
		args := ctx.Value(argsKey{}).([]driver.Value)
		rows, err := stmt.inner.Query(args)
		if err != nil {
			return nil, err
		}
		res = newCacheRows(rows)
	}

	if err := res.createCache(); err != nil {
		return nil, err
	}

	return res.Clone(), nil
}
