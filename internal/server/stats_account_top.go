package server

// PostgreSQL account ranking historically uses the minute bucket predicate,
// not the original event timestamp. Full UTC days can be read from canonical
// minute derivatives; at most two partial days retain the original predicate.
func accountTopSourceSQL(from, to int64) (string, []any) {
	firstDay := from / 86400
	if from%86400 > 0 {
		firstDay++
	}
	lastDay := to / 86400
	if to%86400 < 0 {
		lastDay--
	}
	columns := "account_ref,account_name,tokens,requests,errors"
	if firstDay >= lastDay {
		return "SELECT " + columns + " FROM usage_minute WHERE bucket >= $1 AND bucket < $2 AND account_name<>''", []any{from, to}
	}
	// firstDay<lastDay guarantees both complete-day boundaries fit int64,
	// including a range whose endpoints are MinInt64 or MaxInt64.
	return `WITH unsafe AS MATERIALIZED (
 SELECT day FROM usage_account_top_unsafe_day WHERE day >= $3 AND day < $4 AND minute_rows>0
 ) SELECT ` + columns + ` FROM usage_account_top_day d
 WHERE day >= $3 AND day < $4 AND minute_rows>0 AND NOT EXISTS(SELECT 1 FROM unsafe WHERE unsafe.day=d.day)
 UNION ALL SELECT ` + columns + ` FROM usage_minute WHERE bucket >= $1 AND bucket < $5 AND account_name<>''
 UNION ALL SELECT ` + columns + ` FROM usage_minute WHERE bucket >= $6 AND bucket < $2 AND account_name<>''
 UNION ALL SELECT m.* FROM unsafe u CROSS JOIN LATERAL (
 SELECT ` + columns + ` FROM usage_minute
 WHERE bucket >= u.day*86400 AND bucket < (u.day+1)*86400 AND account_name<>'' OFFSET 0
 ) m`, []any{from, to, firstDay, lastDay, firstDay * 86400, lastDay * 86400}
}

func accountTopQuerySQL(from, to int64, limit int) (string, []any) {
	source, args := accountTopSourceSQL(from, to)
	args = append(args, limit)
	placeholder := "$3"
	if len(args) == 7 {
		placeholder = "$7"
	}
	return "SELECT max(account_name),sum(tokens)::bigint,sum(requests)::bigint,sum(errors)::bigint FROM (" + source + ") selected GROUP BY account_ref ORDER BY sum(tokens) DESC,account_ref LIMIT " + placeholder, args
}
