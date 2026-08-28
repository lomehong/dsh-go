// The provider-owned API-key well-formedness definition shared by every
// adapter that puts one in an HTTP header. Port of api-key.ts.
package llm

import (
	"fmt"
	"regexp"
	"strings"
)

// LegalApiKey matches the characters an HTTP header value carries verbatim
// and every known provider key uses: printable ASCII, space excluded. A key
// outside this set cannot reach any provider, so this is a transport
// invariant rather than one provider's policy.
var LegalApiKey = regexp.MustCompile(`^[\x21-\x7E]+$`)

// ApiKeyRejection names why a supplied key cannot be used.
type ApiKeyRejection string

const (
	ApiKeyEmpty            ApiKeyRejection = "empty"
	ApiKeyIllegalCharacter ApiKeyRejection = "illegalCharacters"
)

// CheckApiKey judges one supplied API key, trimming surrounding whitespace
// first. Trimming is silent because a padded key has one unambiguous
// reading; every other defect is reported.
func CheckApiKey(raw string) (string, ApiKeyRejection, bool) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "", ApiKeyEmpty, false
	}
	if !LegalApiKey.MatchString(value) {
		return "", ApiKeyIllegalCharacter, false
	}
	return value, "", true
}

// AssertUsableApiKey accepts one supplied credential or refuses it as
// unusable. The key never enters the message: ref names where to fix it,
// because echoing any part of a secret into a log or UI is the failure this
// diagnosis avoids.
func AssertUsableApiKey(raw, pkg, ref string) (string, error) {
	checked, reason, ok := CheckApiKey(raw)
	if ok {
		return checked, nil
	}
	if reason == ApiKeyEmpty {
		return "", NewError(InvalidCredentialCode,
			fmt.Sprintf("%s: the API key resolved from %s is blank; set %s to the raw key (the web Models page writes it) or export it in the launching environment", pkg, ref, ref),
			nil)
	}
	return "", NewError(InvalidCredentialCode,
		fmt.Sprintf("%s: the API key resolved from %s contains characters no HTTP header can carry; set %s to the raw key alone (the web Models page writes it)", pkg, ref, ref),
		nil)
}
