package jmapcore

// CapSieve is the capability of RFC 9661.
const CapSieve = "urn:ietf:params:jmap:sieve"

// SieveSession is what a server advertises for RFC 9661: Implementation at the
// session level, Account in each account's capabilities.
type SieveSession struct {
	Implementation string
	Account        SieveCapability
}

// SieveCapability is the per-account urn:ietf:params:jmap:sieve object
// (RFC 9661 §1.2.1). A nil limit or list is sent as null.
type SieveCapability struct {
	MaxSizeScriptName   int      `json:"maxSizeScriptName"`
	MaxSizeScript       *int     `json:"maxSizeScript"`
	MaxNumberScripts    *int     `json:"maxNumberScripts"`
	MaxNumberRedirects  *int     `json:"maxNumberRedirects"`
	SieveExtensions     []string `json:"sieveExtensions"`
	NotificationMethods []string `json:"notificationMethods"`
	ExternalLists       []string `json:"externalLists"`
}

// SieveScript is the object of RFC 9661 §2.1.
type SieveScript struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	BlobID   string `json:"blobId"`
	IsActive bool   `json:"isActive"`
}

// SetError types of RFC 9661 §2.4.
const (
	SetErrInvalidSieve  = "invalidSieve"
	SetErrSieveIsActive = "sieveIsActive"
)
