package handlers

import "regexp"

var (
	// irPhonePattern matches Iranian mobile numbers in the 09xxxxxxxxx form.
	irPhonePattern = regexp.MustCompile(`^09\d{9}$`)
	// postalCodePattern matches Iranian 10-digit postal codes.
	postalCodePattern = regexp.MustCompile(`^\d{10}$`)
	// orderIDPattern matches the TDJ-XXXXXX order ID format (A-Z or 0-9).
	orderIDPattern = regexp.MustCompile(`^TDJ-[A-Z0-9]{6}$`)
	// sessionIDPattern matches the 32-hex-character session cookie value.
	sessionIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)
)

// validIranianPhone reports whether s is a well-formed Iranian mobile number.
// Enforcing the whitelist here both rejects malformed input and makes the
// value safe to reflect into HTML fragments (it can only contain digits).
func validIranianPhone(s string) bool {
	return irPhonePattern.MatchString(s)
}

// validPostalCode reports whether s is a 10-digit Iranian postal code.
func validPostalCode(s string) bool {
	return postalCodePattern.MatchString(s)
}

// validOrderID reports whether s matches the TDJ-XXXXXX format. All order IDs
// used in URLs/paths and HTML attributes must pass this check to prevent
// attribute-injection and IDOR probing on arbitrary strings.
func validOrderID(s string) bool {
	return orderIDPattern.MatchString(s)
}

// validSessionID reports whether s matches the server-generated session ID
// format. Validating before any map lookup prevents abuse of oversized or
// malformed cookie values.
func validSessionID(s string) bool {
	return sessionIDPattern.MatchString(s)
}
