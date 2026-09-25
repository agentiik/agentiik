package config_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/agentiik/agentiik/internal/config"
)

// Where the controller exports the audit log: an https sink outside the installation, and the
// credential it is sent with, in a file.

// A sink and its credential are read, and an installation that names none exports nowhere and
// starts.
func TestTheAuditExportIsReadByTheController(t *testing.T) {
	i := anInstallation(t)
	delete(i.env, config.MasterKeyFile)
	c, err := config.ReadController(lookupIn(i.env, nil))
	if err != nil || c.AuditExport != (config.AuditExport{}) {
		t.Fatalf("a controller given no sink reads %+v: %v", c.AuditExport, err)
	}

	i.env[config.AuditExportURL] = "https://siem.example.com/agentiik?source=audit"
	i.env[config.AuditExportTokenFile] = i.write(t, "audit.token", []byte("the sink's token\n"))
	c, err = config.ReadController(lookupIn(i.env, nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.AuditExport.URL != "https://siem.example.com/agentiik?source=audit" || c.AuditExport.Token != "the sink's token" {
		t.Fatalf("the sink is read as %q with the token %q", c.AuditExport.URL, string(c.AuditExport.Token))
	}
	if printed := fmt.Sprintf("%v %+v", c, c); strings.Contains(printed, "the sink's token") {
		t.Fatalf("printing the configuration shows the sink's token: %s", printed)
	}
}

// A sink in plaintext, one carrying a user, and a credential for no sink each refuse the start,
// naming the variable, and the token as a value is refused as every secret is.
func TestAnAuditExportThatCannotBeUsedRefusesTheStart(t *testing.T) {
	for name, c := range map[string]struct {
		env  map[string]string
		file bool
		want string
	}{
		"a sink in plaintext":       {env: map[string]string{config.AuditExportURL: "http://siem.example.com/agentiik"}, want: config.AuditExportURL},
		"a sink with no host":       {env: map[string]string{config.AuditExportURL: "https:///agentiik"}, want: config.AuditExportURL},
		"a sink carrying a user":    {env: map[string]string{config.AuditExportURL: "https://audit:hunter2@siem.example.com/"}, want: config.AuditExportURL},
		"a credential for no sink":  {file: true, want: config.AuditExportTokenFile},
		"the credential as a value": {env: map[string]string{config.AuditExportURL: "https://siem.example.com/", "AGK_AUDIT_EXPORT_TOKEN": "hunter2"}, want: "AGK_AUDIT_EXPORT_TOKEN"},
	} {
		t.Run(name, func(t *testing.T) {
			i := anInstallation(t)
			delete(i.env, config.MasterKeyFile)
			for k, v := range c.env {
				i.env[k] = v
			}
			if c.file {
				i.env[config.AuditExportTokenFile] = i.write(t, "audit.token", []byte("hunter2"))
			}
			_, err := config.ReadController(lookupIn(i.env, nil))
			if names := refused(err); !slices.Equal(names, []string{c.want}) {
				t.Fatalf("the start was refused naming %v: %v", names, err)
			}
			saysNothingOf(t, err, "hunter2")
		})
	}
}
