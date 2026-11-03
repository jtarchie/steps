package sqlite

import "time"

// now is whole-second RFC3339, for columns nothing orders or subtracts finer
// than a second (nodes, versions, approvals, usage, placements, the breaker).
// A column that is ordered or subtracted takes nowNano instead.
func now() string {
	return time.Now().UTC().Format(time.RFC3339)
}

// sortableNano is RFC3339 with a FIXED nine-digit fraction.
//
// time.RFC3339Nano is the obvious choice and is wrong for a stored column: its
// layout uses "9"s, which TRIM trailing zeros, so the strings it produces are
// not comparable in time order. '…:00.5Z' sorts AFTER '…:00.51Z' (because 'Z'
// 0x5A > '1' 0x31), and a timestamp landing exactly on a second has no fraction
// at all and sorts after every fractional timestamp within it.
//
// That matters because sqlite orders these as TEXT: ListRuns, LatestRunByJob
// and FirstRunSince all sort on them, and retention DELETES by that order. With
// the trimming layout, two runs in the same second could be reaped in the wrong
// order — throwing away the newer one, which is the one somebody wants to look
// at. Zero-padding costs a few bytes a row and makes string order time order.
const sortableNano = "2006-01-02T15:04:05.000000000Z07:00"

// nowNano is the sub-second form, for tables whose rows are ordered or
// subtracted: at whole-second resolution every run faster than a second
// reports a duration of zero, which is most task-only runs. RFC3339 parsing
// accepts the fractional form, so reading stays uniform.
func nowNano() string {
	return time.Now().UTC().Format(sortableNano)
}

// parseTimestamp turns a stored RFC3339 string into a Time, yielding the zero
// Time for an empty or unparseable value. Reading history is a diagnostic, so
// a malformed row is better rendered blank than fatal.
func parseTimestamp(value string) time.Time {
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}
	}

	return parsed
}
