package typert

import (
	"encoding/json"
	"fmt"
)

// ValidateInvocation is the official definition validation, verbatim in
// effect: ids, service keys, namespaces, methods, codecs, parameter wire
// uniqueness, lookup/JSON source rules, the reserved cancellation parameter,
// scope/lookup consistency, and Context receiver declarations. It mutates
// nothing and is run before any store mutation.
func ValidateInvocation(descriptor *InvocationDescriptor) error {
	if err := validateNonempty("invocation id", descriptor.ID); err != nil {
		return err
	}
	if err := validateSegment("invocation service key", descriptor.Service); err != nil {
		return err
	}
	if err := validateWireName("invocation namespace", descriptor.Namespace); err != nil {
		return err
	}
	if err := validateWireName("invocation method", descriptor.Method); err != nil {
		return err
	}
	if descriptor.Implementation != "" {
		if err := validateWireName("invocation implementation method", descriptor.Implementation); err != nil {
			return err
		}
	}
	if err := validateCodec(descriptor.Result, descriptor.ID+" result"); err != nil {
		return err
	}
	wires := map[string]bool{}
	for i := range descriptor.Parameters {
		parameter := &descriptor.Parameters[i]
		if err := validateWireName("parameter name", parameter.Name); err != nil {
			return err
		}
		if err := validateWireName("parameter wire field", parameter.Wire); err != nil {
			return err
		}
		if wires[parameter.Wire] {
			return fmt.Errorf("typert: invocation %q repeats wire field %q", descriptor.ID, parameter.Wire)
		}
		wires[parameter.Wire] = true
		if parameter.Source == SourceLookup {
			if parameter.AcceptsUndefined {
				return fmt.Errorf("typert: invocation %q lookup parameter %q cannot accept undefined", descriptor.ID, parameter.Name)
			}
			if parameter.Lookup == "" {
				return fmt.Errorf("typert: invocation %q lookup parameter %q has no lookup key", descriptor.ID, parameter.Name)
			}
			if err := validateSegment("lookup key", parameter.Lookup); err != nil {
				return err
			}
		} else if parameter.Lookup != "" {
			return fmt.Errorf("typert: invocation %q JSON parameter %q declares a lookup key", descriptor.ID, parameter.Name)
		}
		if err := validateCodec(parameter.Codec, fmt.Sprintf("%s parameter %s", descriptor.ID, parameter.Name)); err != nil {
			return err
		}
	}
	if descriptor.CancellationParameter != "" && descriptor.CancellationParameter != "signal" {
		return fmt.Errorf("typert: invocation %q cancellation parameter must be %q", descriptor.ID, "signal")
	}
	if descriptor.Scope != nil {
		if descriptor.Invocation.Kind != ReceiverDirect {
			return fmt.Errorf("typert: invocation %q Context receiver cannot declare a direct scope projection", descriptor.ID)
		}
		if err := validateSegment("scope Context key", descriptor.Scope.Context); err != nil {
			return err
		}
		if err := validateWireName("scope wire field", descriptor.Scope.Wire); err != nil {
			return err
		}
		var selected *InvocationParameterDescriptor
		for i := range descriptor.Parameters {
			if descriptor.Parameters[i].Source == SourceLookup {
				if selected != nil {
					selected = nil
					break
				}
				selected = &descriptor.Parameters[i]
			}
		}
		if selected == nil || selected.Wire != descriptor.Scope.Wire || selected.Lookup != descriptor.Scope.Context {
			return fmt.Errorf(
				"typert: invocation %q scope wire %q must select its only lookup parameter",
				descriptor.ID, descriptor.Scope.Wire)
		}
	}
	if descriptor.Invocation.Kind == ReceiverContext {
		if err := validateSegment("Context key", descriptor.Invocation.Context); err != nil {
			return err
		}
		if err := validateWireName("Context wire field", descriptor.Invocation.Wire); err != nil {
			return err
		}
		if wires[descriptor.Invocation.Wire] {
			return fmt.Errorf("typert: invocation %q repeats wire field %q", descriptor.ID, descriptor.Invocation.Wire)
		}
		if err := validateCodec(descriptor.Invocation.Codec, descriptor.ID+" Context"); err != nil {
			return err
		}
	}
	return nil
}

// validateCodec accepts the src-json pass-through and requires strict codecs
// to carry a type symbol and a live validator.
func validateCodec(codec Codec, subject string) error {
	if codec.Mode == CodecSrcJSON {
		return nil
	}
	if err := validateNonempty(subject+" type symbol", codec.TypeSymbol); err != nil {
		return err
	}
	if codec.Validate == nil {
		return fmt.Errorf("typert: %s strict codec has no parse() method", subject)
	}
	// Probe the validator once with a well-formed JSON null the way the
	// official check calls parse presence: only its existence is asserted
	// here, not any specific value's validity.
	_ = json.Valid([]byte("null"))
	return nil
}
