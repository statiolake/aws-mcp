package rdsdata

import "testing"

func TestCheckReadOnly(t *testing.T) {
	ok := []string{
		"SELECT 1",
		"select * from users where id = :id;",
		"  -- comment\n SELECT name FROM t /* block */ ORDER BY name DESC",
		"WITH x AS (SELECT 1 AS a) SELECT a FROM x",
		"SHOW TABLES",
		"EXPLAIN SELECT * FROM t",
		"EXPLAIN ANALYZE SELECT * FROM t",
		"DESCRIBE users",
		"SELECT 'it''s; DROP TABLE x' AS s",
		"SELECT \"weird;col\" FROM t",
		"SELECT `a;b` FROM t",
		"SELECT $$ ; delete $$, $tag$ ; insert $tag$",
		"SELECT * FROM t WHERE a = $1",
		"SELECT start, comment, \"update\" FROM events",
		"/* nested /* comment */ ; */ SELECT 1",
	}
	for _, q := range ok {
		if err := CheckReadOnly(q); err != nil {
			t.Errorf("%q refused: %v", q, err)
		}
	}
	bad := []string{
		"",
		"  ;",
		"DELETE FROM users",
		"update users set a = 1",
		"INSERT INTO t VALUES (1)",
		"DROP TABLE t",
		"SELECT 1; DROP TABLE t",
		"SELECT 1; SELECT 2",
		"WITH d AS (DELETE FROM t RETURNING *) SELECT * FROM d",
		"EXPLAIN ANALYZE DELETE FROM t",
		"SELECT * INTO newtable FROM t",
		"SELECT * FROM t FOR UPDATE",
		"CALL do_things()",
		"SET ROLE admin",
		"BEGIN",
		"COMMIT",
		"TRUNCATE t",
		"GRANT ALL ON t TO u",
		"VACUUM",
		// PostgreSQL reads 'a\' as a complete string, so this is two statements.
		`SELECT 'a\'; DROP TABLE t; --'`,
		"SELECT 'unterminated",
		"SELECT 1 # comment",
		"/* unterminated SELECT 1",
	}
	for _, q := range bad {
		if err := CheckReadOnly(q); err == nil {
			t.Errorf("%q allowed", q)
		}
	}
}
