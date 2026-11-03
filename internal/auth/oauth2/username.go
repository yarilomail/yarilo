package oauth2

import (
	"fmt"

	"github.com/yarilomail/yarilo/v2/pkg/userfmt"
)

// CompareUsername returns the resolved username when the token's claim matches the
// templated SASL authzid, else ErrUsernameMismatch. An empty authzid (allowed by
// RFC 7628) skips the check and adopts the claim. Comparison is byte-exact after
// expansion; use %Lu for case-insensitive matching.
func CompareUsername(claimUsername, authzid, template string) (string, error) {
	if authzid == "" {
		return claimUsername, nil
	}
	expanded, err := userfmt.Format(template, authzid)
	if err != nil {
		return "", err
	}
	if expanded != claimUsername {
		return "", fmt.Errorf("%w: token=%q authzid=%q (template %q → %q)",
			ErrUsernameMismatch, claimUsername, authzid, template, expanded)
	}
	return claimUsername, nil
}
