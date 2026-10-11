package ingest

import "testing"

func TestFingerprint(t *testing.T) {
	cases := []struct{ in, want string }{
		{"SELECT * FROM users WHERE id = 42", "SELECT * FROM users WHERE id = ?"},
		{"SELECT * FROM users WHERE id = $1", "SELECT * FROM users WHERE id = ?"},
		{"select *\n  from users\n  where name = 'O''Brien'", "select * from users where name = ?"},
		{"SELECT * FROM t WHERE id IN (1, 2, 3)", "SELECT * FROM t WHERE id IN (?)"},
		{"SELECT * FROM t WHERE id in ($1,$2)", "SELECT * FROM t WHERE id IN (?)"},
		{"SELECT col1 FROM table2 WHERE x = 1.5", "SELECT col1 FROM table2 WHERE x = ?"},
		{"SELECT created_at::date FROM t WHERE a = :name", "SELECT created_at::date FROM t WHERE a = ?"},
		{"SELECT * FROM t WHERE a=@a AND b = :b", "SELECT * FROM t WHERE a=? AND b = ?"},
		{"GET products:category:12", "GET products:category:?"},
		{"GET", "GET"},
	}
	for _, c := range cases {
		if got := Fingerprint(c.in); got != c.want {
			t.Errorf("Fingerprint(%q)\n got %q\nwant %q", c.in, got, c.want)
		}
	}
}
