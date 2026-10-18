package jmapcore

// CapVacation is the VacationResponse capability (RFC 8621 §1.3.3).
const CapVacation = "urn:ietf:params:jmap:vacationresponse"

// VacationID is the id of the one VacationResponse an account has (§8).
const VacationID = "singleton"

// VacationCapability is its per-account object, empty in RFC 8621.
type VacationCapability struct{}

// VacationResponse is the object of RFC 8621 §8; the dates are UTCDate strings.
type VacationResponse struct {
	ID        string  `json:"id"`
	IsEnabled bool    `json:"isEnabled"`
	FromDate  *string `json:"fromDate"`
	ToDate    *string `json:"toDate"`
	Subject   *string `json:"subject"`
	TextBody  *string `json:"textBody"`
	HTMLBody  *string `json:"htmlBody"`
}
