package backyard

// Unsigned simulation output is a prospective exit-cost input, never current
// custody, a signed build, or a committed position. No account overrides exist.
type phase3KaminoProjection struct {
	Slot          int64              `json:"slot"`
	MessageSHA256 string             `json:"messageSha256"`
	UnitsConsumed uint64             `json:"unitsConsumed"`
	Accounts      []ConfirmedAccount `json:"accounts"`
}
