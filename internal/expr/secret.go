package expr

import (
	"errors"
	"fmt"
	"reflect"

	"cel.dev/cel-go/cel"
	"cel.dev/cel-go/common/types"
	"cel.dev/cel-go/common/types/ref"
)

// secretTypeName is the type name the checker knows a secret under. It is opaque: the
// checker knows it exists and knows nothing it can be turned into, which is what makes
// "Bearer " + secrets.token a type error rather than a leak.
const secretTypeName = "agentiik.secret"

// secretType is what the secrets root maps to. cel.OpaqueType and not a string, a map or
// dyn: an opaque type has no overload for concatenation, comparison against a string or
// conversion to one, so every way a secret could reach a text position fails in the
// compiler, where a refusal names a rule, rather than at evaluation, where it would name
// a step that is already running.
var secretType = cel.OpaqueType(secretTypeName)

// Secret is an opaque reference to a secret the workflow names. It carries the name and
// never the value behind it, because nothing in this process has the value:
// a secret is resolved by the runner and mounted into the container as a file.
//
// It stringifies to nothing, so a secret that reaches a format verb or a log line prints
// nothing, and it refuses to be marshalled, so a secret that reaches a document is an
// error rather than a field.
type Secret struct {
	// Name is the name the workflow's secrets block lists. It is a reference and not
	// a credential: what it points at is never read here.
	Name string
}

// ErrSecretOpaque is what every attempt to turn a secret into something else unwraps to.
// A caller tells the refusal apart from an evaluation failure with errors.Is rather than
// by reading the text.
var ErrSecretOpaque = errors.New("a secret is opaque")

// Compile-time proof that a secret travels as a CEL value rather than as something the
// default adapter reflects over and unpacks.
var _ ref.Val = Secret{}

// ConvertToNative hands the secret back to Go only as itself. Every other target is
// refused, including string: the one thing a caller could want a secret as is the value,
// and the value is not here.
func (s Secret) ConvertToNative(typeDesc reflect.Type) (any, error) {
	if typeDesc == reflect.TypeOf(Secret{}) || typeDesc.Kind() == reflect.Interface {
		return s, nil
	}
	return nil, fmt.Errorf("%w: it is a reference the runner resolves, and it converts to no %s", ErrSecretOpaque, typeDesc)
}

// ConvertToType answers with the type itself and refuses every conversion. string(secret)
// is the conversion this exists to refuse.
func (s Secret) ConvertToType(typeVal ref.Type) ref.Val {
	if typeVal == types.TypeType {
		return secretType
	}
	return types.NewErr("a secret is opaque: it converts to no %s", typeVal.TypeName())
}

// Equal compares two secrets by the name they reference. Comparing one against anything
// else has no overload, which is what refuses a condition written as secrets.x != "".
func (s Secret) Equal(other ref.Val) ref.Val {
	o, ok := other.(Secret)
	if !ok {
		return types.MaybeNoSuchOverloadErr(other)
	}
	return types.Bool(s.Name == o.Name)
}

// Type gives the opaque type the checker declared secrets under.
func (s Secret) Type() ref.Type { return secretType }

// Value gives the secret itself and never a value behind it.
func (s Secret) Value() any { return s }

// String is empty on purpose. A secret printed by accident, through a format verb or a
// log line that took the whole context, prints nothing at all.
func (s Secret) String() string { return "" }

// MarshalJSON refuses. A secret belongs in the mount list of a task, where it travels as
// a name and a path; a secret written into params.json would be a reference the container
// cannot resolve and a name in a document that had no reason to carry it.
func (s Secret) MarshalJSON() ([]byte, error) {
	return nil, fmt.Errorf("%w: it is mounted at a path and never written into a document", ErrSecretOpaque)
}
