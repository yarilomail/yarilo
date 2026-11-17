package locks

// DefaultPrefixes are a Redis backend's prefixes with no option given.
func DefaultPrefixes() (key, channel string) {
	b := NewRedisBackend(nil)
	return b.keyPrefix, b.chPrefix
}
