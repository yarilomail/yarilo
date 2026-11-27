package guard_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// One render per distinct invocation, in memory only; the key holds every argument and
// the content of every file named, and a failure is cached like an output.
type helmResult struct {
	once sync.Once
	out  []byte
	err  error
}

var (
	helmRenders sync.Map
	helmSlots   = make(chan struct{}, runtime.NumCPU())
)

func helmKey(combined bool, args []string) string {
	var b strings.Builder
	if combined {
		b.WriteString("combined")
	} else {
		b.WriteString("stdout")
	}
	for _, a := range args {
		b.WriteString("\x00")
		b.WriteString(a)
		if st, err := os.Stat(a); err == nil && st.Mode().IsRegular() {
			raw, err := os.ReadFile(a)
			if err != nil {
				b.WriteString("\x00unreadable")
				continue
			}
			sum := sha256.Sum256(raw)
			b.WriteString("\x00sha256:")
			b.WriteString(hex.EncodeToString(sum[:]))
		}
	}
	return b.String()
}

func helmRun(combined bool, args []string) ([]byte, error) {
	v, _ := helmRenders.LoadOrStore(helmKey(combined, args), &helmResult{})
	r := v.(*helmResult)
	r.once.Do(func() {
		helmSlots <- struct{}{}
		defer func() { <-helmSlots }()
		cmd := exec.CommandContext(context.Background(), "helm", args...)
		if combined {
			r.out, r.err = cmd.CombinedOutput()
		} else {
			r.out, r.err = cmd.Output()
		}
	})
	return bytes.Clone(r.out), r.err
}

// helmOutput is exec.Command("helm", args...).Output(), rendered once.
func helmOutput(t *testing.T, args ...string) ([]byte, error) {
	t.Helper()
	return helmRun(false, args)
}

// helmCombined is exec.Command("helm", args...).CombinedOutput(), rendered once.
func helmCombined(t *testing.T, args ...string) ([]byte, error) {
	t.Helper()
	return helmRun(true, args)
}

// helmPrefetch renders the given invocations concurrently into the cache, for
// a row that cannot run in parallel itself.
func helmPrefetch(sets ...[]string) {
	var wg sync.WaitGroup
	for _, args := range sets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = helmRun(false, args)
		}()
	}
	wg.Wait()
}
