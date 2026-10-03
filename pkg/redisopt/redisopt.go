// Package redisopt builds Redis client options from a URL and a password kept
// apart from it, so the password can come from a Secret, not the ConfigMap.
package redisopt

import (
	"fmt"

	"github.com/redis/go-redis/v9"
)

// Parse reads url and, when password is set, uses it over any password in url.
func Parse(url, password string) (*redis.Options, error) {
	opt, err := redis.ParseURL(url)
	if err != nil {
		return nil, fmt.Errorf("redisopt: %w", err)
	}
	if password != "" {
		opt.Password = password
	}
	return opt, nil
}
