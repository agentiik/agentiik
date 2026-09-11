module github.com/agentiik/agentiik

go 1.27

// JSON Schema 2020-12, used by package schema alone and by nothing else in this module.
// The workflow language defines an input's schema as a 2020-12 document, and this
// implementation is the draft itself rather than an older one; it takes a custom
// loader, which is how a $ref is resolved against the commit's tree and refused when it
// leaves it; and it returns a structured error whose keyword and instance location are
// what a refusal message names.
require github.com/santhosh-tekuri/jsonschema/v6 v6.0.3

require golang.org/x/text v0.14.0 // indirect
