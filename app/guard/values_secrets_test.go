package guard_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"testing"

	"go.yaml.in/yaml/v3"
)

// secretKeys are the keys whose value the loader expands from ${…}: a secret
// lives in a Secret and reaches the config as a reference, never as a value.
// Keys that name a Secret (*_secret_ref, passwordSecret.name) are not here.
var secretKeys = map[string]bool{
	"ring_secret": true, "redis_password": true, "sentinel_password": true,
	"token": true, "websocket_ticket_key": true, "password": true,
	"dsn": true, "static_password": true, "client_secret": true,
	"api_header": true, "hash_nonce": true,
}

var secretRef = regexp.MustCompile(`^\$\{[A-Z0-9_]+\}$`)

// literalSecrets walks a YAML tree and names every secret key whose scalar
// value is neither empty nor a ${…} reference.
func literalSecrets(n *yaml.Node, path string, out *[]string) {
	switch n.Kind {
	case yaml.DocumentNode:
		for _, c := range n.Content {
			literalSecrets(c, path, out)
		}
	case yaml.SequenceNode:
		for i, c := range n.Content {
			literalSecrets(c, fmt.Sprintf("%s[%d]", path, i), out)
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, v := n.Content[i].Value, n.Content[i+1]
			if secretKeys[k] && v.Kind == yaml.ScalarNode && v.Value != "" && !secretRef.MatchString(v.Value) {
				*out = append(*out, fmt.Sprintf("%s.%s (line %d)", path, k, v.Line))
			}
			literalSecrets(v, path+"."+k, out)
		}
	}
}

func literalsIn(t *testing.T, raw []byte) []string {
	t.Helper()
	var root yaml.Node
	if err := yaml.Unmarshal(raw, &root); err != nil {
		t.Fatalf("parse: %v", err)
	}
	var out []string
	literalSecrets(&root, "", &out)
	return out
}

// No values file writes a secret, and no chart render of one carries a secret
// into the ConfigMap: each is empty or a ${…} reference to a Secret (#2507).
func TestValuesHoldOnlySecretReferences(t *testing.T) {
	const sandbox = "../../helm_values/values-sandbox.yaml"
	files := []string{"../../helm/values.yaml"}
	sets := [][]string{nil}
	for _, pattern := range []string{"../../helm_values/values-*.yaml", "../../helm_values/examples/*.yaml"} {
		found, err := filepath.Glob(pattern)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, found...)
		for _, f := range found {
			switch {
			case f == sandbox:
				sets = append(sets, []string{sandbox})
			case filepath.Base(filepath.Dir(f)) == "examples":
				sets = append(sets, []string{f})
			default:
				sets = append(sets, []string{sandbox, f})
			}
		}
	}
	if len(files) < 10 {
		t.Fatalf("found only %d values files: the row would pass by reading nothing", len(files))
	}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, lit := range literalsIn(t, raw) {
			t.Errorf("%s writes a secret as a value: %s; reference a Secret as ${VAR}", filepath.Base(f), lit)
		}
	}
	for _, set := range sets {
		name := "chart defaults"
		if len(set) > 0 {
			name = filepath.Base(set[len(set)-1])
		}
		args := []string{"template", "yarilo", "../../helm"}
		for _, f := range set {
			args = append(args, "-f", f)
		}
		out, err := exec.CommandContext(t.Context(), "helm", args...).Output()
		if err != nil {
			t.Fatalf("helm template %s: %v", name, err)
		}
		for _, lit := range literalsIn(t, []byte(renderedConfig(t, string(out)))) {
			t.Errorf("rendered with %s, the ConfigMap carries a secret value: %s", name, lit)
		}
	}
}
