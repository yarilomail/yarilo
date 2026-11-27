package authtoken

// DefaultKeyPrefix is a Redis store's prefix with no option given.
func DefaultKeyPrefix() string { return NewRedis(nil, 0, WithKeyPrefix("")).keyPrefix }
