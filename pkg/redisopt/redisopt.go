// Package redisopt builds Redis clients from a URL and a password kept apart
// from it, so the password can come from a Secret, not the ConfigMap.
package redisopt

import (
	"fmt"
	"strings"

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

// Sentinel names the Sentinels a client asks for the master. The zero value
// is a client on one address.
type Sentinel struct {
	Addrs    []string
	Master   string
	User     string
	Password string
}

// Check refuses half a Sentinel configuration; prefix is the block the keys
// live in, so the error names the key to set.
func (s Sentinel) Check(prefix string) error {
	switch {
	case len(s.Addrs) > 0 && s.Master == "":
		return fmt.Errorf("%ssentinel_master is required with %ssentinel_addrs", prefix, prefix)
	case len(s.Addrs) == 0 && s.Master != "":
		return fmt.Errorf("%ssentinel_addrs is required with %ssentinel_master", prefix, prefix)
	}
	return nil
}

// SentinelFromSettings reads the sentinel_* keys of a redis dict's settings;
// sentinel_addrs is a list or a comma-separated string.
func SentinelFromSettings(settings map[string]any) Sentinel {
	var s Sentinel
	switch v := settings["sentinel_addrs"].(type) {
	case []any:
		for _, a := range v {
			if str, ok := a.(string); ok && str != "" {
				s.Addrs = append(s.Addrs, str)
			}
		}
	case []string:
		s.Addrs = v
	case string:
		for _, a := range strings.Split(v, ",") {
			if a = strings.TrimSpace(a); a != "" {
				s.Addrs = append(s.Addrs, a)
			}
		}
	}
	s.Master, _ = settings["sentinel_master"].(string)
	s.User, _ = settings["sentinel_user"].(string)
	s.Password, _ = settings["sentinel_password"].(string)
	return s
}

// FailoverOptions carries opt's credentials, db, timeouts and pool to a client
// that finds the master through s; opt's own address is not used.
func FailoverOptions(opt *redis.Options, s Sentinel) *redis.FailoverOptions {
	return &redis.FailoverOptions{
		MasterName:            s.Master,
		SentinelAddrs:         s.Addrs,
		SentinelUsername:      s.User,
		SentinelPassword:      s.Password,
		ClientName:            opt.ClientName,
		Protocol:              opt.Protocol,
		Username:              opt.Username,
		Password:              opt.Password,
		DB:                    opt.DB,
		MaxRetries:            opt.MaxRetries,
		MinRetryBackoff:       opt.MinRetryBackoff,
		MaxRetryBackoff:       opt.MaxRetryBackoff,
		DialTimeout:           opt.DialTimeout,
		ReadTimeout:           opt.ReadTimeout,
		WriteTimeout:          opt.WriteTimeout,
		ContextTimeoutEnabled: opt.ContextTimeoutEnabled,
		PoolFIFO:              opt.PoolFIFO,
		PoolSize:              opt.PoolSize,
		PoolTimeout:           opt.PoolTimeout,
		MinIdleConns:          opt.MinIdleConns,
		MaxIdleConns:          opt.MaxIdleConns,
		MaxActiveConns:        opt.MaxActiveConns,
		ConnMaxIdleTime:       opt.ConnMaxIdleTime,
		ConnMaxLifetime:       opt.ConnMaxLifetime,
		TLSConfig:             opt.TLSConfig,
	}
}

// Client returns a client on opt's address, or, with Sentinels named, one that
// follows them to the master. Each mode is one case here (#2505).
func Client(opt *redis.Options, s Sentinel) redis.UniversalClient {
	switch {
	case len(s.Addrs) > 0:
		return redis.NewFailoverClient(FailoverOptions(opt, s))
	default:
		return redis.NewClient(opt)
	}
}
