package coderuntime

import (
	"fmt"
	"regexp"
)

// ReservedBindingGlobals are the binding globals EVERY backend refuses,
// because SOME backend owns the slot in the program's namespace: `console`
// (the log capture), and `__dsh_main__`/`__builtins__`/`__name__`/`__debug__`
// (the Python backend's bootstrap wrapper, seeded module globals, and the
// compile-time `__debug__` constant). One shared set — rather than each
// backend refusing only its own slots — keeps the portability promise real: a
// namespace list valid on one backend is valid on all. `__name__` et al. ARE
// valid portable identifiers, so the identifier rule never rejects them —
// hence this explicit set.
var ReservedBindingGlobals = map[string]struct{}{
	"console":      {},
	"__dsh_main__": {},
	"__builtins__": {},
	"__name__":     {},
	"__debug__":    {},
}

// ReservedErrorMembers are the MemberNameProperty values EVERY backend
// refuses, as one shared contract so a request valid on one backend is valid
// on all: the JS error exclusions (`name`, `message`, `stack`) and Python's
// exception-protocol members (`args`, `with_traceback`, `add_note`).
var ReservedErrorMembers = map[string]struct{}{
	"name":           {},
	"message":        {},
	"stack":          {},
	"args":           {},
	"with_traceback": {},
	"add_note":       {},
}

// dunderMember matches the dunder form (`__x__`, non-empty middle): object
// slots in Python, refused as error members on every backend.
var dunderMember = regexp.MustCompile(`^__.+__$`)

// PortableReservedWords holds the reserved words of every portable target
// language (ECMAScript ∪ Python), refused as binding globals / error-class
// names by all backends. A per-language check would let `lambda` pass one
// backend and fail another; extending the seam with a new language means
// widening this union (a breaking review of existing binding names, by
// design).
var PortableReservedWords = map[string]struct{}{
	// ECMAScript reserved words and reserved-in-strict-mode names.
	"await": {}, "break": {}, "case": {}, "catch": {}, "class": {}, "const": {},
	"continue": {}, "debugger": {}, "default": {}, "delete": {}, "do": {},
	"else": {}, "enum": {}, "export": {}, "extends": {}, "false": {},
	"finally": {}, "for": {}, "function": {}, "if": {}, "import": {}, "in": {},
	"instanceof": {}, "new": {}, "null": {}, "return": {}, "super": {},
	"switch": {}, "this": {}, "throw": {}, "true": {}, "try": {}, "typeof": {},
	"var": {}, "void": {}, "while": {}, "with": {}, "yield": {}, "let": {},
	"static": {}, "implements": {}, "interface": {}, "package": {},
	"private": {}, "protected": {}, "public": {}, "arguments": {}, "eval": {},
	// Python 3.x keywords and soft keywords not already above ("type" and "_"
	// are soft keywords: legal names in practice, reserved here for safety).
	"False": {}, "None": {}, "True": {}, "and": {}, "as": {}, "assert": {},
	"async": {}, "def": {}, "del": {}, "elif": {}, "except": {}, "from": {},
	"global": {}, "is": {}, "lambda": {}, "nonlocal": {}, "not": {}, "or": {},
	"pass": {}, "raise": {}, "match": {}, "type": {}, "_": {},
}

// portableIdentifier matches the LANGUAGE-PORTABLE identifier subset every
// backend enforces on binding globals and error-class names.
var portableIdentifier = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// ValidateBindings rejects malformed binding globals or typed-error
// declarations as Service Definition contract misuse. Every backend enforces
// this same validation — the seam hosts the one shared implementation so the
// "identical everywhere" promise is structural, not a per-backend copy.
func ValidateBindings(request CodeRunRequest) error {
	globals := map[string]struct{}{}
	for _, namespace := range request.Bindings {
		if !portableIdentifier.MatchString(namespace.Global) {
			return fmt.Errorf("dsh-code-runtime: binding global %q is not a usable identifier", namespace.Global)
		}
		if _, reserved := PortableReservedWords[namespace.Global]; reserved {
			return fmt.Errorf("dsh-code-runtime: binding global %q is not a usable identifier", namespace.Global)
		}
		if _, reserved := ReservedBindingGlobals[namespace.Global]; reserved {
			return fmt.Errorf("dsh-code-runtime: reserved binding global %q", namespace.Global)
		}
		if _, duplicate := globals[namespace.Global]; duplicate {
			return fmt.Errorf("dsh-code-runtime: duplicate binding global %q", namespace.Global)
		}
		globals[namespace.Global] = struct{}{}
	}

	errorClassNames := map[string]struct{}{}
	for _, namespace := range request.Bindings {
		descriptor := namespace.ErrorClass
		if descriptor == nil {
			continue
		}
		if !portableIdentifier.MatchString(descriptor.Name) {
			return fmt.Errorf("dsh-code-runtime: binding error class %q is not a usable identifier", descriptor.Name)
		}
		if _, reserved := PortableReservedWords[descriptor.Name]; reserved {
			return fmt.Errorf("dsh-code-runtime: binding error class %q is not a usable identifier", descriptor.Name)
		}
		if _, reserved := ReservedBindingGlobals[descriptor.Name]; reserved {
			return fmt.Errorf("dsh-code-runtime: reserved binding global %q", descriptor.Name)
		}
		if _, clash := globals[descriptor.Name]; clash {
			return fmt.Errorf("dsh-code-runtime: duplicate injected global %q", descriptor.Name)
		}
		if _, clash := errorClassNames[descriptor.Name]; clash {
			return fmt.Errorf("dsh-code-runtime: duplicate injected global %q", descriptor.Name)
		}
		if descriptor.MemberNameProperty == "" {
			return fmt.Errorf("dsh-code-runtime: binding error member property %q is not usable", descriptor.MemberNameProperty)
		}
		if _, reserved := ReservedErrorMembers[descriptor.MemberNameProperty]; reserved {
			return fmt.Errorf("dsh-code-runtime: binding error member property %q is not usable", descriptor.MemberNameProperty)
		}
		if dunderMember.MatchString(descriptor.MemberNameProperty) {
			return fmt.Errorf("dsh-code-runtime: binding error member property %q is not usable", descriptor.MemberNameProperty)
		}
		errorClassNames[descriptor.Name] = struct{}{}
	}
	return nil
}
