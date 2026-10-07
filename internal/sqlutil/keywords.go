package sqlutil

import "strings"

// Keywords is the static list of ClickHouse SQL keywords used for syntax
// highlighting and as the completion fallback when the server's
// system.keywords table is unavailable. Only single-word keywords are listed.
var Keywords = []string{
	"ADD", "AFTER", "ALIAS", "ALL", "ALTER", "AND", "ANTI", "ANY", "ARRAY", "AS", "ASC", "ASOF", "ASYNC",
	"ATTACH", "BETWEEN", "BY", "CASE", "CAST", "CHECK", "CLEAR", "CLUSTER", "CODEC", "COLUMN", "COLUMNS",
	"COMMENT", "CONSTRAINT", "CREATE", "CROSS", "CUBE", "CURRENT", "DATABASE", "DATABASES", "DEDUPLICATE",
	"DEFAULT", "DELETE", "DESC", "DESCRIBE", "DETACH", "DICTIONARIES", "DICTIONARY", "DISTINCT", "DROP",
	"ELSE", "END", "ENGINE", "EPHEMERAL", "EXCEPT", "EXCHANGE", "EXISTS", "EXPLAIN", "FALSE", "FETCH",
	"FILL", "FINAL", "FIRST", "FOLLOWING", "FORMAT", "FREEZE", "FROM", "FULL", "FUNCTION", "GLOBAL",
	"GRANT", "GRANTS", "GROUP", "HAVING", "IDENTIFIED", "IF", "ILIKE", "IN", "INDEX", "INNER", "INSERT",
	"INTERPOLATE", "INTERSECT", "INTERVAL", "INTO", "IS", "JOIN", "KEY", "KILL", "LAST", "LAYOUT", "LEFT",
	"LIFETIME", "LIKE", "LIMIT", "LIVE", "MATERIALIZED", "MODIFY", "MOVE", "MUTATION", "NOT", "NULL",
	"NULLS", "OFFSET", "ON", "OPTIMIZE", "OR", "ORDER", "OUTER", "OUTFILE", "OVER", "PART", "PARTITION",
	"POLICY", "POPULATE", "PRECEDING", "PREWHERE", "PRIMARY", "PROCESSLIST", "PROFILE", "PROJECTION",
	"QUALIFY", "QUOTA", "RANGE", "REFRESH", "RENAME", "REPLACE", "REVOKE", "RIGHT", "ROLE", "ROLES",
	"ROLLUP", "ROW", "ROWS", "SAMPLE", "SELECT", "SEMI", "SET", "SETTINGS", "SHOW", "SOURCE", "STEP",
	"SYNC", "SYSTEM", "TABLE", "TABLES", "TEMPORARY", "THEN", "TIES", "TO", "TOTALS", "TRUE", "TRUNCATE",
	"TTL", "UNBOUNDED", "UNION", "UPDATE", "USE", "USER", "USERS", "USING", "VALUES", "VIEW", "WATCH",
	"WHEN", "WHERE", "WINDOW", "WITH",
}

var keywordSet = func() map[string]struct{} {
	m := make(map[string]struct{}, len(Keywords))
	for _, k := range Keywords {
		m[k] = struct{}{}
	}
	return m
}()

// IsKeyword reports whether word is a known SQL keyword (case-insensitive).
func IsKeyword(word string) bool {
	if len(word) > 16 { // longer than any keyword; skip the allocation
		return false
	}
	_, ok := keywordSet[strings.ToUpper(word)]
	return ok
}
