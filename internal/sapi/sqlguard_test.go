package sapi

import "testing"

func TestSqlLeavesState(t *testing.T) {
	for q, want := range map[string]bool{
		"BEGIN":                                true,
		"  begin work":                         true,
		"/* why */ START TRANSACTION":          true,
		"-- note\nstart transaction read only": true,
		"/*!40101 BEGIN */":                    true,
		"SET autocommit = 0":                   true,
		"set session AUTOCOMMIT=0":             true,
		"SET @@session.autocommit = 0":         true,
		"SET @@autocommit=0":                   true,
		"LOCK TABLES t WRITE":                  true,
		"XA START 'x'":                         true,
		"BEGIN NOT ATOMIC SELECT 1; END":       false,
		"SELECT 1":                             false,
		"SET NAMES utf8mb4":                    false,
		"SET @autocommit = 1":                  false,
		"START REPLICA":                        false,
		"INSERT INTO begin_log VALUES (1)":     false,
		"COMMIT":                               false,
		"":                                     false,
		"/* unterminated":                      false,
	} {
		if got := sqlLeavesState(q); got != want {
			t.Errorf("sqlLeavesState(%q) = %v, want %v", q, got, want)
		}
	}
}
