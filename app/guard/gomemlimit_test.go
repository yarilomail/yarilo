package guard_test

import (
	"bytes"
	"errors"
	"io"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// Every container of ours with a memory limit runs with GOMEMLIMIT at 90 % of
// it: without it the runtime lets the heap grow into the kernel's kill, which
// is how the imap OOM ended (#2661).
func TestEveryContainerCollectsBeforeItsMemoryLimit(t *testing.T) {
	t.Parallel()
	units := map[string]int64{"Ki": 1 << 10, "Mi": 1 << 20, "Gi": 1 << 30, "K": 1e3, "M": 1e6, "G": 1e9, "": 1}
	size := regexp.MustCompile(`^(\d+)(\w*)$`)
	for _, values := range []string{"", "../../helm_values/values-sandbox.yaml"} {
		args := []string{"template", "t", "../../helm"}
		if values != "" {
			args = append(args, "-f", values)
		}
		out, err := helmOutput(t, args...)
		if err != nil {
			t.Fatalf("helm template %v: %v", args, err)
		}
		checked := 0
		dec := yaml.NewDecoder(bytes.NewReader(out))
		for {
			var d struct {
				Kind     string
				Metadata struct{ Name string }
				Spec     struct {
					Template struct {
						Spec struct {
							Containers []struct {
								Name      string
								Env       []struct{ Name, Value string }
								Resources struct{ Limits map[string]string }
							}
						}
					}
				}
			}
			if err := dec.Decode(&d); errors.Is(err, io.EOF) {
				break
			} else if err != nil {
				t.Fatalf("values %q: %v", values, err)
			}
			if d.Kind != "Deployment" && d.Kind != "StatefulSet" {
				continue
			}
			for _, c := range d.Spec.Template.Spec.Containers {
				lim := c.Resources.Limits["memory"]
				if !strings.HasPrefix(c.Name, "yarilo-") || lim == "" {
					continue
				}
				m := size.FindStringSubmatch(lim)
				if m == nil {
					t.Fatalf("values %q: %s/%s: memory limit %q", values, d.Metadata.Name, c.Name, lim)
				}
				n, _ := strconv.ParseInt(m[1], 10, 64)
				want := strconv.FormatInt(n*units[m[2]]/10*9, 10)
				var got []string
				for _, e := range c.Env {
					if e.Name == "GOMEMLIMIT" {
						got = append(got, e.Value)
					}
				}
				checked++
				if len(got) != 1 || got[0] != want {
					t.Errorf("values %q: %s/%s with limit %s has GOMEMLIMIT %v, want [%s]", values, d.Metadata.Name, c.Name, lim, got, want)
				}
			}
		}
		if checked == 0 {
			t.Errorf("values %q: no container with a memory limit rendered; the guard looks at nothing", values)
		}
	}
}
