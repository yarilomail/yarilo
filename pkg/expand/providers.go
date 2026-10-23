package expand

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// builtinProviders answer prefix:name in every template.
var builtinProviders = map[string]Lookup{
	"process":  providerProcess,
	"system":   providerSystem,
	"env":      providerEnv,
	"event":    providerEvent,
	"date":     providerDate,
	"time":     providerTime,
	"generate": providerGenerate,
}

func unsupportedField(f string) error { return fmt.Errorf("Unsupported field '%s'", f) }

func providerProcess(key string) (string, error) {
	switch key {
	case "pid":
		return strconv.Itoa(os.Getpid()), nil
	case "uid":
		return strconv.Itoa(os.Geteuid()), nil
	case "gid":
		return strconv.Itoa(os.Getegid()), nil
	}
	return "", unsupportedField(key)
}

func providerSystem(key string) (string, error) {
	switch key {
	case "cpu_count":
		if v := os.Getenv("NCPU"); v != "" {
			return v, nil
		}
		return strconv.Itoa(runtime.NumCPU()), nil
	case "hostname":
		return os.Hostname()
	case "os":
		return osName()
	case "os-version":
		return osRelease()
	}
	return "", unsupportedField(key)
}

func providerEnv(key string) (string, error) {
	if key == "" {
		return "", errors.New("Missing field")
	}
	return os.Getenv(key), nil
}

// providerEvent has no event to read: yarilo logs records, not events.
func providerEvent(string) (string, error) { return "", errors.New("No event available") }

func providerDate(key string) (string, error) {
	now := time.Now()
	switch key {
	case "year":
		return now.Format("2006"), nil
	case "month":
		return now.Format("01"), nil
	case "day":
		return now.Format("02"), nil
	}
	return "", errUnsupportedKey(key)
}

func providerTime(key string) (string, error) {
	now := time.Now()
	switch key {
	case "unix":
		ns := now.UnixNano()
		return fmt.Sprintf("%d.%09d", ns/1e9, ns%1e9), nil
	case "hour":
		return now.Format("15"), nil
	case "min", "minute":
		return now.Format("04"), nil
	case "sec", "second":
		return now.Format("05"), nil
	case "us", "usec":
		return strconv.Itoa(now.Nanosecond() / 1000), nil
	}
	return "", errUnsupportedKey(key)
}

var guidState struct {
	sync.Mutex
	sec, nsec int64
}

// guid is unique per process and host: a nanosecond counter, the pid and the
// host name.
func guid() string {
	guidState.Lock()
	defer guidState.Unlock()
	if guidState.sec == 0 {
		now := time.Now()
		guidState.sec, guidState.nsec = now.Unix(), int64(now.Nanosecond())
	} else if uint32(guidState.nsec) < ^uint32(0) {
		guidState.nsec++
	} else {
		guidState.sec, guidState.nsec = guidState.sec+1, 0
	}
	host, _ := os.Hostname()
	return fmt.Sprintf("%08x%08x.%x.%s", uint32(guidState.nsec), guidState.sec, os.Getpid(), host)
}

func uuid4() []byte {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return b
}

func uuidRecord(b []byte) string {
	h := hex.EncodeToString(b)
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

func providerGenerate(key string) (string, error) {
	switch {
	case key == "guid":
		return guid(), nil
	case key == "guid128":
		b := make([]byte, 16)
		_, _ = rand.Read(b)
		return hex.EncodeToString(b), nil
	case strings.HasPrefix(key, "uuid"):
		u := uuid4()
		switch rest := strings.TrimPrefix(key, "uuid"); rest {
		case "", ":record":
			return uuidRecord(u), nil
		case ":compact":
			return hex.EncodeToString(u), nil
		case ":microsoft":
			return "{" + uuidRecord(u) + "}", nil
		default:
			return "", errUnsupportedKey(rest)
		}
	}
	return "", errUnsupportedKey(key)
}
