package main

import (
	"database/sql"
	"fmt"
	"io/ioutil"
	"os"
	"strings"
	"time"

	"github.com/lib/pq"
	lib "github.com/lukaszgryglicki/calcmetric"
)

const (
	gPrefix          = "V3_"
	gMaxPlaceholders = 0x8000
)

var (
	gRequired = []string{
		"CONN",
		"METRIC",
		"TABLE",
		"PROJECT_SLUG",
		"TIME_RANGE",
	}
	// -1 - error
	// 0 - ok, no calculations needed
	// 1 - calculated
	gFinalState = 0
)

func toDBIdentifier(arg string) string {
	return strings.Replace(strings.ToLower(arg), "-", "_", -1)
}

func isCalculated(db *sql.DB, table, projectSlug, timeRange string, debug bool, env map[string]string, dtf, dtt time.Time) (bool, error) {
	dtf = lib.DayStart(dtf)
	// dtt = lib.NextDayStart(dtt)
	dtt = lib.DayStart(dtt)
	sqlQuery := fmt.Sprintf(
		`select last_calculated_at from "%s" where project_slug = $1 and time_range = $2 and date_from = $3 and date_to = $4`,
		table,
	)
	args := []interface{}{projectSlug, timeRange, dtf, dtt}
	if debug {
		lib.Logf("executing sql: %s\nwith args: %+v\n", sqlQuery, args)
	}
	rows, err := db.Query(sqlQuery, args...)
	if err != nil {
		switch e := err.(type) {
		case *pq.Error:
			errName := e.Code.Name()
			if errName == "undefined_table" {
				lib.Logf("table '%s' does not exist yet, so we need to calculate this metric.\n", table)
				return false, nil
			}
			lib.QueryOut(sqlQuery, args...)
			return false, err
		default:
			lib.QueryOut(sqlQuery, args...)
			return false, err
		}
	}
	defer func() { _ = rows.Close() }()
	var (
		lastCalc time.Time
		fetched  bool
	)
	for rows.Next() {
		err := rows.Scan(&lastCalc)
		if err != nil {
			return false, err
		}
		fetched = true
	}
	err = rows.Err()
	if err != nil {
		return false, err
	}
	if fetched {
		lib.Logf("table '%s' was last computed at %+v for (%s, %s, %+v, %+v), so calculation is not needed\n", table, lastCalc, projectSlug, timeRange, dtf, dtt)
		return true, nil
	}
	lib.Logf("table '%s' present, but it needs calculation for (%s, %s, %+v, %+v)\n", table, projectSlug, timeRange, dtf, dtt)
	return false, nil
}

func dbTypeName(column *sql.ColumnType, env map[string]string) (string, error) {
	_, guess := env["GUESS_TYPE"]
	name := strings.ToLower(column.DatabaseTypeName())
	switch name {
	case "text", "bool", "date", "interval", "numeric":
		return name, nil
	case "varchar":
		return "text", nil
	case "timestamptz":
		return "timestamp", nil
	case "int8", "int16", "int32", "int64":
		return "bigint", nil
	case "float8":
		return "numeric", nil
	default:
		if guess {
			return name, nil
		}
		return "error", fmt.Errorf("unknown type: '%s' in %+v", name, column)
	}
}

func supportCleanup(db *sql.DB, table, timeRange, projectSlug string, dtf, dtt time.Time, debug bool, env map[string]string) {
	cl, clOK := env["CLEANUP"]
	if !clOK || cl == "" {
		return
	}
	dtf = lib.DayStart(dtf)
	dtt = lib.DayStart(dtt)
	delQuery := fmt.Sprintf(
		`delete from "%s" where time_range = $1 and project_slug = $2 and date_from < $3 and date_to < $4 and date(last_calculated_at) < date(now())`,
		table,
	)
	args := []interface{}{timeRange, projectSlug, dtf, dtt}
	if debug {
		lib.Logf("cleanup: delete from table:\n%s\n%+v\n", delQuery, args)
	}
	res, err := db.Exec(delQuery, args...)
	if err != nil {
		lib.Logf("error: %+v\n", err)
		lib.QueryOut(delQuery, args...)
		return
	}
	rows, err := res.RowsAffected()
	if err == nil && rows > 0 {
		lib.Logf("cleanup %d rows from \"%s\"(%s, %s, <%+v, <%+v)\n", rows, table, projectSlug, timeRange, dtf, dtt)
	}
	return
}

func supportDelete(db *sql.DB, table, timeRange, projectSlug string, dtf, dtt time.Time, debug bool, env map[string]string) bool {
	del, delOK := env["DELETE"]
	if !delOK || del == "" {
		return false
	}
	delAry := strings.Split(del, ",")
	delMap := make(map[string]struct{})
	for _, k := range delAry {
		delMap[k] = struct{}{}
	}
	if len(delMap) <= 0 {
		lib.Logf("unconditioned DELETE is not suppored - you probably mean something else, use DROP to do a full table delete instead\n")
		return false
	}
	args := []interface{}{}
	delQuery := fmt.Sprintf(`delete from "%s"`, table)
	// tr,ps,df,dt
	conds := []string{}
	cond := ""
	i := 0
	_, tr := delMap["tr"]
	if tr {
		i++
		conds = append(conds, fmt.Sprintf("time_range = $%d", i))
		args = append(args, timeRange)
	}
	_, ps := delMap["ps"]
	if ps {
		i++
		conds = append(conds, fmt.Sprintf("project_slug = $%d", i))
		args = append(args, projectSlug)
	}
	_, df := delMap["df"]
	if df {
		i++
		conds = append(conds, fmt.Sprintf("date_from = $%d", i))
		args = append(args, dtf)
	}
	_, dt := delMap["dt"]
	if dt {
		i++
		conds = append(conds, fmt.Sprintf("date_to = $%d", i))
		args = append(args, dtt)
	}
	if len(conds) > 0 {
		cond = strings.Join(conds, " and ")
		delQuery += " where " + cond
	}
	if debug {
		lib.Logf("delete from table:\n%s\n%+v\n", delQuery, args)
	}
	res, err := db.Exec(delQuery, args...)
	if err != nil {
		lib.Logf("error: %+v\n", err)
		lib.QueryOut(delQuery, args...)
		return false
	}
	rows, err := res.RowsAffected()
	if err == nil {
		return rows > 0
	}
	return false
}

func calculate(db *sql.DB, sqlQuery, table, projectSlug, timeRange, dtFrom, dtTo string, ppt, debug bool, env map[string]string) error {
	rows, err := db.Query(sqlQuery)
	if err != nil {
		lib.QueryOut(sqlQuery, []interface{}{}...)
		return err
	}
	defer func() { _ = rows.Close() }()
	columns, err := rows.ColumnTypes()
	if err != nil {
		return err
	}
	if debug {
		lib.Logf("columns: %d\n", len(columns))
		for _, column := range columns {
			lib.Logf("%+v\n", column)
		}
	}
	indicesAry := []string{}
	indices, ok := env["INDEXED_COLUMNS"]
	if ok && indices != "" {
		indicesAry = strings.Split(indices, ",")
		if debug {
			lib.Logf("extra indices requested: %+v\n", indicesAry)
		}
	}
	createTable := fmt.Sprintf(`create table if not exists "%s"(
  time_range varchar(6) not null,
  project_slug text not null,
  last_calculated_at timestamp not null,
  date_from date not null,
  date_to date not null,
  row_number int not null,
`,
		table,
	)
	l := len(columns) - 1
	colNames := []string{}
	namesMap := make(map[string]struct{})
	for i, column := range columns {
		tp, err := dbTypeName(column, env)
		if err != nil {
			return err
		}
		colName := column.Name()
		_, ok := namesMap[colName]
		if ok {
			return fmt.Errorf("non unique column name '%s'", colName)
		}
		namesMap[colName] = struct{}{}
		colNames = append(colNames, colName)
		createTable += fmt.Sprintf(`  %s %s`, colName, tp)
		nullable, ok := column.Nullable()
		if ok && !nullable {
			createTable += ` not null`
		}
		if i < l {
			createTable += ",\n"
		} else {
			createTable += `,
  primary key(time_range, project_slug, date_from, date_to, row_number)
);
`
		}
	}
	createTable += fmt.Sprintf(`create index if not exists "%s_time_range_idx" on "%s"(time_range);
`,
		table,
		table,
	)
	if !ppt {
		createTable += fmt.Sprintf(`create index if not exists "%s_project_slug_idx" on "%s"(project_slug);
`,
			table,
			table,
		)
	}
	for _, index := range indicesAry {
		createTable += fmt.Sprintf(`create index if not exists "%s_%s_idx" on "%s"(%s);
`,
			table,
			index,
			table,
			index,
		)
	}
	if debug {
		lib.Logf("create table:\n%s\n", createTable)
	}
	_, err = db.Exec(createTable)
	if err != nil {
		lib.QueryOut(createTable, []interface{}{}...)
		return err
	}
	i := 0
	nColumns := len(columns)
	pValues := make([]interface{}, nColumns)
	for i := range columns {
		pValues[i] = new(sql.RawBytes)
	}
	calcDt := time.Now()
	p := 0
	ep := 0
	changes := false
	// This is the type of query that we will be using (UPSERT):
	// insert into t(a, b, c) values (1, 2, 30), (4, 5, 60) on conflict(a, b) do update set (b, c) = (excluded.b, excluded.c);
	queryRoot := fmt.Sprintf(`insert into "%s"(time_range, project_slug, last_calculated_at, date_from, date_to, row_number, `, table)
	query := ""
	args := []interface{}{}
	batches := 0
	for rows.Next() {
		err := rows.Scan(pValues...)
		if err != nil {
			return err
		}
		i++
		args = append(args, []interface{}{timeRange, projectSlug, calcDt, dtFrom, dtTo, i}...)
		for _, pValue := range pValues {
			args = append(args, string(*pValue.(*sql.RawBytes)))
		}
		if ep == 0 {
			ep = len(pValues)
		}
		if query == "" {
			query = queryRoot
			for j, colName := range colNames {
				query += colName
				if j < l {
					query += ", "
				}
			}
			query += fmt.Sprintf(`) values ($%d, $%d, $%d, $%d, $%d, $%d, `, p+1, p+2, p+3, p+4, p+5, p+6)
		} else {
			query += fmt.Sprintf(`, ($%d, $%d, $%d, $%d, $%d, $%d, `, p+1, p+2, p+3, p+4, p+5, p+6)
		}
		for j := range colNames {
			query += fmt.Sprintf("$%d", p+j+7)
			if j < l {
				query += ", "
			}
		}
		query += ")"
		p += 6 + ep
		if p >= gMaxPlaceholders-(6+ep) {
			query += " on conflict(time_range, project_slug, date_from, date_to, row_number) do update set "
			if l > 0 {
				query += "("
			}
			for j, colName := range colNames {
				query += colName
				if j < l {
					query += ", "
				}
			}
			if l > 0 {
				query += ") = ("
			} else {
				query += " = "
			}
			for j, colName := range colNames {
				query += "excluded." + colName
				if j < l {
					query += ", "
				}
			}
			if l > 0 {
				query += ")"
			}
			if debug {
				lib.Logf("flush at %d\n", p)
				lib.Logf("query:\n%s\n", query)
				lib.Logf("args(%d):\n%+v\n", len(args), args)
			}
			var rslt sql.Result
			rslt, err = db.Exec(query, args...)
			if err != nil {
				lib.QueryOut(query, args...)
				return err
			}
			nRows, err := rslt.RowsAffected()
			if err != nil {
				lib.QueryOut(query, args...)
				return err
			}
			if !changes && nRows > 0 {
				changes = true
			}
			query = ""
			args = []interface{}{}
			p = 0
			batches++
		}
	}
	if len(args) > 0 {
		query += " on conflict(time_range, project_slug, date_from, date_to, row_number) do update set "
		if l > 0 {
			query += "("
		}
		for j, colName := range colNames {
			query += colName
			if j < l {
				query += ", "
			}
		}
		if l > 0 {
			query += ") = ("
		} else {
			query += " = "
		}
		for j, colName := range colNames {
			query += "excluded." + colName
			if j < l {
				query += ", "
			}
		}
		if l > 0 {
			query += ")"
		}
		if debug {
			lib.Logf("final flush at %d\n", p)
			lib.Logf("query:\n%s\n", query)
			lib.Logf("args(%d):\n%+v\n", len(args), args)
		}
		var rslt sql.Result
		rslt, err = db.Exec(query, args...)
		if err != nil {
			lib.QueryOut(query, args...)
			return err
		}
		nRows, err := rslt.RowsAffected()
		if err != nil {
			lib.QueryOut(query, args...)
			return err
		}
		if !changes && nRows > 0 {
			changes = true
		}
		batches++
	}
	err = rows.Err()
	if err != nil {
		return err
	}
	if changes {
		gFinalState = 1
	}
	lib.Logf("completed in %d batches\n", batches)
	return nil
}

func currentTimeRange(timeRange string, debug bool, env map[string]string) (time.Time, time.Time) {
	now := time.Now()
	dtf, dtt := now, now
	switch timeRange {
	case "7d", "7dp":
		_, daily := env["CALC_WEEK_DAILY"]
		if daily {
			dtt = lib.DayStart(now)
			dtf = dtt.AddDate(0, 0, -7)
		} else {
			dtt = lib.WeekStart(now)
			dtf = dtt.AddDate(0, 0, -7)
		}
		if timeRange == "7dp" {
			dtf = dtf.AddDate(0, 0, -7)
			dtt = dtt.AddDate(0, 0, -7)
		}
	case "30d", "30dp":
		_, daily := env["CALC_MONTH_DAILY"]
		if daily {
			dtt = lib.DayStart(now)
			dtf = dtt.AddDate(0, 0, -30)
			if timeRange == "30dp" {
				dtf = dtf.AddDate(0, 0, -30)
				dtt = dtt.AddDate(0, 0, -30)
			}
		} else {
			dtt = lib.MonthStart(now)
			dtf = dtt.AddDate(0, -1, 0)
			if timeRange == "30dp" {
				dtf = dtf.AddDate(0, -1, 0)
				dtt = dtt.AddDate(0, -1, 0)
			}
		}
	case "q", "qp":
		_, daily := env["CALC_QUARTER_DAILY"]
		if daily {
			dtt = lib.DayStart(now)
			dtf = dtt.AddDate(0, -3, 0)
			if timeRange == "qp" {
				dtf = dtf.AddDate(0, -3, 0)
				dtt = dtt.AddDate(0, -3, 0)
			}
		} else {
			dtt = lib.QuarterStart(now)
			dtf = dtt.AddDate(0, -3, 0)
			if timeRange == "qp" {
				dtf = dtf.AddDate(0, -3, 0)
				dtt = dtt.AddDate(0, -3, 0)
			}
		}
	case "ty", "typ":
		dtt = lib.DayStart(now)
		dtf = lib.YearStart(now)
		if timeRange == "typ" {
			diff := dtt.Sub(dtf)
			dtf = dtf.Add(-diff)
			dtt = dtt.Add(-diff)
		}
	case "y", "yp":
		_, daily := env["CALC_YEAR_DAILY"]
		if daily {
			dtt = lib.DayStart(now)
			dtf = dtt.AddDate(-1, 0, 0)
			if timeRange == "yp" {
				dtf = dtf.AddDate(-1, 0, 0)
				dtt = dtt.AddDate(-1, 0, 0)
			}
		} else {
			dtt = lib.YearStart(now)
			dtf = dtt.AddDate(-1, 0, 0)
			if timeRange == "yp" {
				dtf = dtf.AddDate(-1, 0, 0)
				dtt = dtt.AddDate(-1, 0, 0)
			}
		}
	case "2y", "2yp":
		_, daily := env["CALC_YEAR2_DAILY"]
		if daily {
			dtt = lib.DayStart(now)
			dtf = dtt.AddDate(-2, 0, 0)
			if timeRange == "2yp" {
				dtf = dtf.AddDate(-2, 0, 0)
				dtt = dtt.AddDate(-2, 0, 0)
			}
		} else {
			dtt = lib.YearStart(now)
			if now.Year()%2 == 1 {
				dtt = dtt.AddDate(-1, 0, 0)
			}
			dtf = dtt.AddDate(-2, 0, 0)
			if timeRange == "2yp" {
				dtf = dtf.AddDate(-2, 0, 0)
				dtt = dtt.AddDate(-2, 0, 0)
			}
		}
	case "a":
		dtt, _ = lib.TimeParseAny("2100")
		dtf, _ = lib.TimeParseAny("1970")
		if timeRange == "typ" {
			diff := dtt.Sub(dtf)
			dtf = dtf.Add(-diff)
			dtt = dtt.Add(-diff)
		}
	}
	lib.Logf("checking for time range %s - %s\n", lib.ToYMDQuoted(dtf), lib.ToYMDQuoted(dtt))
	return dtf, dtt
}

func needsCalculation(db *sql.DB, table, projectSlug, timeRange string, debug bool, env map[string]string) (bool, time.Time, time.Time, error) {
	var tm time.Time
	switch timeRange {
	case "7d", "7dp", "30d", "30dp", "q", "qp", "ty", "typ", "y", "yp", "2y", "2yp", "a":
		dtf, dtt := currentTimeRange(timeRange, debug, env)
		isCalc, err := isCalculated(db, table, projectSlug, timeRange, debug, env, dtf, dtt)
		if err != nil {
			return true, dtf, dtt, err
		}
		return !isCalc, dtf, dtt, nil
	case "c":
		dtFrom, ok := env["DATE_FROM"]
		if !ok {
			return true, tm, tm, fmt.Errorf("you must specify %sDATE_FROM when using %sTIME_RANGE=c", gPrefix, gPrefix)
		}
		dtTo, ok := env["DATE_TO"]
		if !ok {
			return true, tm, tm, fmt.Errorf("you must specify %sDATE_TO when using %sTIME_RANGE=c", gPrefix, gPrefix)
		}
		dtf, err := lib.TimeParseAny(dtFrom)
		if err != nil {
			return true, tm, tm, err
		}
		dtt, err := lib.TimeParseAny(dtTo)
		if err != nil {
			return true, dtf, tm, err
		}
		dtf = lib.DayStart(dtf)
		dtt = lib.DayStart(dtt)
		isCalc, err := isCalculated(db, table, projectSlug, timeRange, debug, env, dtf, dtt)
		if err != nil {
			return true, dtf, dtt, err
		}
		return !isCalc, dtf, dtt, nil
	default:
		return true, tm, tm, fmt.Errorf("unknown time range: '%s'", timeRange)
	}
}

func calcMetric() error {
	env := make(map[string]string)
	prefixLen := len(gPrefix)
	for _, pair := range os.Environ() {
		if strings.HasPrefix(pair, gPrefix) {
			ary := strings.Split(pair, "=")
			if len(ary) < 2 {
				continue
			}
			key := ary[0]
			val := strings.Join(ary[1:], "=")
			env[key[prefixLen:]] = val
		}
	}
	_, debug := env["DEBUG"]
	if debug {
		lib.Logf("map: %+v\n", env)
	}
	for _, key := range gRequired {
		_, ok := env[key]
		if !ok {
			msg := fmt.Sprintf("you must define %s%s environment variable to run this", gPrefix, key)
			lib.Logf("env: %s\n", msg)
			err := fmt.Errorf("%s", msg)
			return err
		}
	}
	connStr, _ := env["CONN"]
	db, err := sql.Open("postgres", connStr)
	if err != nil {
		return err
	}
	defer func() { db.Close() }()
	if debug {
		lib.Logf("db: %+v\n", db)
	}
	table, _ := env["TABLE"]
	_, drop := env["DROP"]
	if drop {
		dropTable := fmt.Sprintf(`drop table if exists "%s"`, table)
		if debug {
			lib.Logf("drop table:\n%s\n", dropTable)
		}
		_, err = db.Exec(dropTable)
		if err != nil {
			lib.QueryOut(dropTable, []interface{}{}...)
			return err
		}
	}
	projectSlug, _ := env["PROJECT_SLUG"]
	// Per Project Tables
	_, ppt := env["PPT"]
	if ppt {
		table += "_" + toDBIdentifier(projectSlug)
	}
	timeRange, _ := env["TIME_RANGE"]
	needsCalc, dtf, dtt, err := needsCalculation(db, table, projectSlug, timeRange, debug, env)
	if err != nil {
		return err
	}
	deleted := supportDelete(db, table, timeRange, projectSlug, dtf, dtt, debug, env)
	if deleted {
		needsCalc, dtf, dtt, err = needsCalculation(db, table, projectSlug, timeRange, debug, env)
	}
	if !needsCalc {
		_, ok := env["FORCE_CALC"]
		if ok {
			needsCalc = true
			lib.Logf("table '%s' doesn't need calculation but it was requested to calculate anyway\n", table)
		}
	}
	if !needsCalc {
		if debug {
			lib.Logf("table '%s' doesn't need calculation now\n", table)
		}
		return nil
	}
	metric, _ := env["METRIC"]
	path, ok := env["SQL_PATH"]
	if !ok {
		path = "./sql/"
	}
	fn := path + metric + ".sql"
	contents, err := ioutil.ReadFile(fn)
	if err != nil {
		return err
	}
	sql := string(contents)
	sql = strings.Replace(sql, "{{project_slug}}", projectSlug, -1)
	limit, _ := env["LIMIT"]
	if limit != "" {
		sql = strings.Replace(sql, "{{limit}}", limit, -1)
	}
	offset, _ := env["OFFSET"]
	if offset != "" {
		sql = strings.Replace(sql, "{{offset}}", offset, -1)
	}
	for k, v := range env {
		if strings.HasPrefix(k, "PARAM_") {
			n := k[6:]
			sql = strings.Replace(sql, "{{"+n+"}}", v, -1)
		}
	}
	dtfs := lib.ToYMDQuoted(dtf)
	dtts := lib.ToYMDQuoted(dtt)
	sql = strings.Replace(sql, "{{date_from}}", dtfs, -1)
	sql = strings.Replace(sql, "{{date_to}}", dtts, -1)
	if debug {
		lib.Logf("generated SQL:\n%s\n", sql)
	}
	err = calculate(db, sql, table, projectSlug, timeRange, dtfs, dtts, ppt, debug, env)
	if err != nil {
		return err
	}
	supportCleanup(db, table, timeRange, projectSlug, dtf, dtt, debug, env)
	return nil
}

func main() {
	dtStart := time.Now()
	rCode := 0
	err := calcMetric()
	if err != nil {
		lib.Logf("calcMetric error: %+v\n", err)
		rCode = 1
		gFinalState = -1
	}
	dtEnd := time.Now()
	lib.Logf("time: %v, final state: %d\n", dtEnd.Sub(dtStart), gFinalState)
	if rCode != 0 {
		os.Exit(rCode)
	}
	if rCode == 0 && gFinalState == 0 {
		// This is to mark that calculations were not needed
		os.Exit(66)
	}
}
