package director

import (
	"fmt"
	"hash/crc32"
	"sort"
	"strings"

	"github.com/yarilomail/yarilo/internal/cluster/ring"
)

// backendSetHash is a stable hash over the routing fields {ip, port, tag,
// vhosts, up} of the backend set, sorted, so diverged directors differ (#846).
func backendSetHash(backends []ring.Backend) string {
	lines := make([]string, 0, len(backends))
	for _, b := range backends {
		lines = append(lines, fmt.Sprintf("%s:%d|%s|%d|%t", b.IP, b.Port, b.Tag, b.Vhosts, b.Up))
	}
	sort.Strings(lines)
	return fmt.Sprintf("%08x", crc32.ChecksumIEEE([]byte(strings.Join(lines, "\n"))))
}
