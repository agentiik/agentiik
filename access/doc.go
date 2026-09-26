// Package access resolves what a principal may do: the nine permissions, the four roles that
// name sets of them, the grant that binds a principal to a role or denies it one permission at
// one scope, and the rule that turns the grants applying to a principal into the permissions it
// holds.
//
// "Effective permissions are the union of every applying grant: the principal's own and its
// groups', at both scopes." "A workflow-scope grant only adds. Only an explicit deny removes,
// and it wins over any allow at any scope." "An optional expiry ends it without anyone
// remembering to revoke it." Those three sentences are the whole of the rule, and Resolve is
// them and nothing else.
//
// # Why a package of its own
//
// The API answers whether a request may proceed, and the controller answers again, when a run is
// created, whether the principal a trigger records still may: "a trigger armed months ago can
// fire long after the grant that armed it". Two answers to one question drift unless they are one
// function, and the controller cannot import package api, which is an HTTP server. So the rule
// lives here, with no database, no bus and no HTTP behind it: the caller reads the grants from
// wherever it keeps them and hands them over with the time to judge their expiry by, and the same
// grants at the same time always resolve to the same permissions. A test holds the boundary.
//
// The permission vocabulary moved here from package api, which keeps its names for it so that
// every route guard reads as it was written: one type, one list, one place a tenth permission
// would have to be added.
//
// # What it does not decide
//
// Who the principal is, which groups it belongs to and which grants exist are the caller's to
// read. Administration is not a grant either: a grant binds "one scope (a namespace or a single
// workflow)", so a question about the installation resolves to nothing here, and whoever
// administers it is answered for elsewhere.
package access
