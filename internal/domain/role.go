package domain

// Role is the kind of agent: the product owner plans, engineers implement,
// testers review.
type Role string

// The three agent roles of the README's "ammo breakdown".
const (
	// RolePO is the product owner / business analyst: researches the repo,
	// writes the plan, arbitrates disputes and handles escalations.
	RolePO Role = "po"
	// RoleEngineer implements a bead in its own worktree and answers feedback.
	RoleEngineer Role = "engineer"
	// RoleTester reviews an engineer's work through one Lens.
	RoleTester Role = "tester"
)

var roleEnum = enum[Role]{kind: "role", values: []Role{RolePO, RoleEngineer, RoleTester}}

// Roles returns every valid Role in declaration order.
func Roles() []Role { return roleEnum.all() }

// ParseRole parses a role name leniently (case, whitespace and hyphens are
// ignored). It returns an error wrapping ErrUnknownValue for anything else,
// including the empty string.
func ParseRole(s string) (Role, error) { return roleEnum.parse(s) }

// String returns the canonical name.
func (r Role) String() string { return string(r) }

// Valid reports whether r is one of the declared roles.
func (r Role) Valid() bool { return roleEnum.valid(r) }

// MarshalText implements encoding.TextMarshaler.
func (r Role) MarshalText() ([]byte, error) { return roleEnum.marshal(r) }

// UnmarshalText implements encoding.TextUnmarshaler.
func (r *Role) UnmarshalText(b []byte) error {
	v, err := roleEnum.unmarshal(b)
	if err != nil {
		return err
	}
	*r = v
	return nil
}

// Lens is the perspective a tester reviews from. Ideally every engineer's
// work is reviewed by one tester per lens.
type Lens string

// The three reviewer lenses.
const (
	// LensUX reviews behaviour, usability, CLI/UI ergonomics and error messages.
	LensUX Lens = "ux"
	// LensQA reviews correctness, edge cases and regressions; runs the suite
	// and reproduces bugs.
	LensQA Lens = "qa"
	// LensTest reviews test coverage and quality and may propose test code.
	LensTest Lens = "test"
)

var lensEnum = enum[Lens]{kind: "lens", values: []Lens{LensUX, LensQA, LensTest}}

// Lenses returns every valid Lens in declaration order.
func Lenses() []Lens { return lensEnum.all() }

// ParseLens parses a lens name leniently; see ParseRole for the rules.
func ParseLens(s string) (Lens, error) { return lensEnum.parse(s) }

// String returns the canonical name.
func (l Lens) String() string { return string(l) }

// Valid reports whether l is one of the declared lenses.
func (l Lens) Valid() bool { return lensEnum.valid(l) }

// MarshalText implements encoding.TextMarshaler.
func (l Lens) MarshalText() ([]byte, error) { return lensEnum.marshal(l) }

// UnmarshalText implements encoding.TextUnmarshaler.
func (l *Lens) UnmarshalText(b []byte) error {
	v, err := lensEnum.unmarshal(b)
	if err != nil {
		return err
	}
	*l = v
	return nil
}
