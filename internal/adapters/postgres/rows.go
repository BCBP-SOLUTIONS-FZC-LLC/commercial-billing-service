package postgres

import "github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"

// collect reads every row of rows with scan and closes rows. It stands in
// for pgx.CollectRows so the repositories use platform-pgcommon's types
// only (pgcommon.Rows satisfies pgcommon.Row).
func collect[T any](rows pgcommon.Rows, scan func(row pgcommon.Row) (T, error)) ([]T, error) {
	defer rows.Close()
	var out []T
	for rows.Next() {
		v, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// scanOne reads a single-column row into T.
func scanOne[T any](row pgcommon.Row) (T, error) {
	var v T
	err := row.Scan(&v)
	return v, err
}
