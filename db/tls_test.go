package db

import (
	"crypto/tls"
	"testing"

	"github.com/jackc/pgx/v5"
)

// Every TLS configuration pgx builds from a URL is held to TLS 1.2 at least, for a pool and for
// the one connection migrating opens, whichever sslmode built it and the second host of two included.
func TestEveryConnectionToTheDatabaseHoldsTheTLSFloor(t *testing.T) {
	for _, mode := range []string{"require", "verify-ca", "verify-full", "prefer"} {
		url := "postgres://agentiik@db-1.example.com,db-2.example.com/agentiik?sslmode=" + mode
		pool, err := poolConfig(url)
		if err != nil {
			t.Fatal(err)
		}
		one, err := connConfig(url)
		if err != nil {
			t.Fatal(err)
		}
		for which, c := range map[string]*pgx.ConnConfig{"a pool": pool.ConnConfig, "a connection": one} {
			built := []*tls.Config{c.TLSConfig}
			for _, f := range c.Fallbacks {
				built = append(built, f.TLSConfig)
			}
			held := 0
			for _, b := range built {
				if b == nil {
					continue
				}
				held++
				if b.MinVersion != tls.VersionTLS12 {
					t.Errorf("sslmode=%s built %s a configuration whose floor is %s", mode, which, tls.VersionName(b.MinVersion))
				}
			}
			if held < 2 {
				t.Errorf("sslmode=%s built %s a TLS configuration for one host of two", mode, which)
			}
		}
	}
}
