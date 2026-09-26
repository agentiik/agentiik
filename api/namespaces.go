package api

import (
	"errors"
	"fmt"
	"strings"

	"github.com/agentiik/agentiik/agk"
)

// NamespaceName refuses a name no namespace can be created under: one outside $defs/namespace,
// lowercase words joined by hyphens, one longer than a name is, or one of the words the API
// routes on.
//
// It is the check agentiik-api namespace create makes until v0.3.0's route makes it here, so that
// the verb and the route refuse the same names.
func NamespaceName(name string) error {
	switch {
	case name == "":
		return errors.New("a namespace has a name, lowercase words joined by hyphens, such as finance or team-ops")
	case len(name) > agk.IdentifierMaxBytes:
		return fmt.Errorf("a namespace's name is at most %d characters and this one is %d: it is written in every path of the API and every key of the object store, and no filesystem holds a longer name", agk.IdentifierMaxBytes, len(name))
	case !givenName.MatchString(name):
		return fmt.Errorf("%.64q is not a namespace: a namespace is named in lowercase words joined by hyphens, such as finance or team-ops", name)
	case agk.IsReservedNamespace(name):
		return fmt.Errorf("%s is a word the API routes on: the first path segment after /api/v1/ decides the route, so %s cannot name a namespace", name, strings.Join(agk.ReservedNamespaces, ", "))
	}
	return nil
}
