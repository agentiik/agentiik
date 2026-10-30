package config_test

import (
	"testing"

	"github.com/agentiik/agentiik/internal/config"
)

// The connection the API and the controller apply the migrations their database lacks as, before
// they start: AGK_MIGRATE_DATABASE_URL where it is set, the superuser over the local socket the
// application connects through where it is not, and none anywhere else.
func TestTheAPIAndTheControllerReadWhatTheyMigrateAs(t *testing.T) {
	for name, c := range map[string]struct {
		app, admin string
		want       string
		password   bool
	}{
		"the variable set": {
			app:   "postgres://agentiik@db:5432/agentiik?sslmode=verify-full",
			admin: "postgres://postgres@db:5432/agentiik?sslmode=verify-full",
			want:  "postgres://postgres@db:5432/agentiik?sslmode=verify-full", password: true,
		},
		"a local socket, as the Compose file of v0.7.0 and before gives": {
			app:  "postgres://agentiik@/agentiik?host=/run/postgresql",
			want: "postgres://postgres@/agentiik?host=/run/postgresql",
		},
		"several local sockets": {
			app:  "postgres://agentiik@/agentiik?host=/run/postgresql,/var/run/postgresql",
			want: "postgres://postgres@/agentiik?host=/run/postgresql,/var/run/postgresql",
		},
		"an address over the network": {
			app: "postgres://agentiik@db:5432/agentiik?sslmode=verify-full",
		},
		"a socket and a host over the network": {
			app: "postgres://agentiik@/agentiik?host=/run/postgresql,db&sslmode=verify-full",
		},
		"a socket, the role named in a parameter": {
			app: "postgres:///agentiik?host=/run/postgresql&user=agentiik",
		},
	} {
		t.Run(name, func(t *testing.T) {
			i := anInstallation(t)
			i.env[config.DatabaseURL] = c.app
			delete(i.env, config.MigrateDatabaseURL)
			if c.admin != "" {
				i.env[config.MigrateDatabaseURL] = c.admin
			}
			api, err := config.ReadAPI(lookupIn(i.env, nil))
			if err != nil {
				t.Fatal(err)
			}
			delete(i.env, config.MasterKeyFile)
			controller, err := config.ReadController(lookupIn(i.env, nil))
			if err != nil {
				t.Fatal(err)
			}
			for program, m := range map[string]config.Database{"the API": api.Migration, "the controller": controller.Migration} {
				if m.URL != c.want {
					t.Errorf("%s migrates as %q, want %q", program, m.URL, c.want)
				}
				if c.want != "" && m.Role != "postgres" {
					t.Errorf("%s migrates as the role %q, want postgres", program, m.Role)
				}
				if got := m.Password != ""; got != c.password {
					t.Errorf("%s migrates with a password: %v, want %v", program, got, c.password)
				}
			}
		})
	}
}

// AGK_MIGRATE_DATABASE_URL is held to every rule migrate holds it to when the API reads it, and a
// password in it refuses the start, naming it.
func TestTheAPIRefusesAMigrationAddressMigrateWouldRefuse(t *testing.T) {
	i := anInstallation(t)
	i.env[config.MigrateDatabaseURL] = "postgres://postgres:hunter2@db:5432/agentiik?sslmode=verify-full"
	_, err := config.ReadAPI(lookupIn(i.env, nil))
	if names := refused(err); len(names) != 1 || names[0] != config.MigrateDatabaseURL {
		t.Fatalf("the start was refused naming %v: %v", names, err)
	}
	saysNothingOf(t, err, "hunter2")
}
