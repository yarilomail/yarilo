package jmapcore

// CapQuota is the quota capability of RFC 9425. Its object is empty: the limits
// live in the Quota objects, not in the capability.
const CapQuota = "urn:ietf:params:jmap:quota"

// Quota is the Quota object of RFC 9425 §1.3. Field order follows the RFC so a
// hand-read response matches the spec side by side.
type Quota struct {
	ID           string   `json:"id"`
	ResourceType string   `json:"resourceType"`
	Used         int64    `json:"used"`
	HardLimit    int64    `json:"hardLimit"`
	Scope        string   `json:"scope"`
	Name         string   `json:"name"`
	Types        []string `json:"types"`
	// Nullable per the RFC: a missing soft limit is null, never 0, which a
	// client would read as "no headroom at all".
	WarnLimit   *int64  `json:"warnLimit"`
	SoftLimit   *int64  `json:"softLimit"`
	Description *string `json:"description"`
}

// Resource types of RFC 9425 §1.3. "octets" counts storage, "count" objects.
const (
	QuotaResourceOctets = "octets"
	QuotaResourceCount  = "count"
)

// QuotaScopeAccount is the only scope yarilo has: limits are per account, not
// per domain or installation.
const QuotaScopeAccount = "account"

// QuotaTypeMail names the data type a quota applies to (RFC 9425 §1.3).
const QuotaTypeMail = "Mail"

// AddedItem is one id and its position in a query result (RFC 8620 §5.6).
type AddedItem struct {
	ID    string `json:"id"`
	Index uint   `json:"index"`
}

// QueryChangesResponse is the Foo/queryChanges response of RFC 8620 §5.6, for
// a query whose result set a server can say the difference in.
type QueryChangesResponse struct {
	AccountID     string      `json:"accountId"`
	OldQueryState string      `json:"oldQueryState"`
	NewQueryState string      `json:"newQueryState"`
	Removed       []string    `json:"removed"`
	Added         []AddedItem `json:"added"`
}

// QuotaChangesResponse is Quota/changes (RFC 9425 §4.3): the shared changes
// arguments plus updatedProperties, which is null when the server names none.
type QuotaChangesResponse struct {
	AccountID         string    `json:"accountId"`
	OldState          string    `json:"oldState"`
	NewState          string    `json:"newState"`
	HasMoreChanges    bool      `json:"hasMoreChanges"`
	Created           []string  `json:"created"`
	Updated           []string  `json:"updated"`
	Destroyed         []string  `json:"destroyed"`
	UpdatedProperties *[]string `json:"updatedProperties"`
}
