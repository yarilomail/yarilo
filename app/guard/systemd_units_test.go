package guard_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/yarilomail/yarilo/v2/pkg/config"
)

const systemdDir = "../../deploy/systemd"

// unitKeys returns every value of key in a unit file, space-separated values split.
func unitKeys(t *testing.T, path, key string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(line, key+"="); ok {
			out = append(out, strings.Fields(v)...)
		}
	}
	return out
}

func serviceFiles(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(systemdDir, "yarilo-*.service"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no units in %s: %v", systemdDir, err)
	}
	return files
}

// A unit that starts a binary the tree no longer builds fails at boot, not here.
func TestEveryUnitStartsABinaryOfTheTree(t *testing.T) {
	for _, f := range serviceFiles(t) {
		exec := unitKeys(t, f, "ExecStart")
		if len(exec) == 0 {
			t.Errorf("%s has no ExecStart", filepath.Base(f))
			continue
		}
		bin := filepath.Base(exec[0])
		if _, err := os.Stat(filepath.Join("../../app", bin, "main.go")); err != nil {
			t.Errorf("%s starts %s, which is not in app/", filepath.Base(f), bin)
		}
		if want := strings.TrimSuffix(filepath.Base(f), ".service"); bin != want {
			t.Errorf("%s starts %s; the unit is named after its binary", filepath.Base(f), bin)
		}
	}
}

// An Environment= key no code reads is a setting that silently does nothing.
func TestEveryUnitEnvironmentKeyIsReadByTheCode(t *testing.T) {
	read := map[string]bool{}
	getenv := regexp.MustCompile(`os\.(?:Getenv|LookupEnv)\("([A-Z0-9_]+)"\)`)
	for _, root := range []string{"../../app", "../../internal", "../../pkg"} {
		err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return err
			}
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			for _, m := range getenv.FindAllSubmatch(b, -1) {
				read[string(m[1])] = true
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range serviceFiles(t) {
		for _, kv := range unitKeys(t, f, "Environment") {
			if k, _, _ := strings.Cut(kv, "="); !read[k] {
				t.Errorf("%s sets %s, which no code reads", filepath.Base(f), k)
			}
		}
	}
}

// The package installs exactly the binaries the units start, plus the two CLIs.
func TestThePackageCarriesEveryUnitBinary(t *testing.T) {
	b, err := os.ReadFile("../../deploy/nfpm/nfpm.yaml")
	if err != nil {
		t.Fatal(err)
	}
	packaged := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)^    dst: /usr/libexec/yarilo/(\S+)$`).FindAllSubmatch(b, -1) {
		packaged[string(m[1])] = true
	}
	units := map[string]bool{}
	for _, f := range serviceFiles(t) {
		bin := filepath.Base(unitKeys(t, f, "ExecStart")[0])
		units[bin] = true
		if !packaged[bin] {
			t.Errorf("%s starts %s, which deploy/nfpm/nfpm.yaml does not install", filepath.Base(f), bin)
		}
	}
	for bin := range packaged {
		if !units[bin] {
			t.Errorf("deploy/nfpm/nfpm.yaml installs %s, which no unit starts", bin)
		}
	}
}

func TestTheTargetWantsOnlyUnitsThatExist(t *testing.T) {
	wants := unitKeys(t, filepath.Join(systemdDir, "yarilo.target"), "Wants")
	if len(wants) == 0 {
		t.Fatal("yarilo.target wants nothing")
	}
	for _, u := range wants {
		if _, err := os.Stat(filepath.Join(systemdDir, u)); err != nil {
			t.Errorf("yarilo.target wants %s, which deploy/systemd does not ship", u)
		}
	}
}

// A user or group sysusers does not create makes the unit fail with 217/USER.
func TestEveryUnitAccountIsCreatedBySysusers(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(systemdDir, "sysusers.d", "yarilo.conf"))
	if err != nil {
		t.Fatal(err)
	}
	declared := map[string]bool{}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && (f[0] == "u" || f[0] == "g") {
			declared[f[1]] = true
		}
	}
	var missing []string
	for _, f := range serviceFiles(t) {
		for _, key := range []string{"User", "Group", "SupplementaryGroups"} {
			for _, name := range unitKeys(t, f, key) {
				if !declared[name] {
					missing = append(missing, filepath.Base(f)+" "+key+"="+name)
				}
			}
		}
	}
	sort.Strings(missing)
	for _, m := range missing {
		t.Errorf("%s: not created by sysusers.d/yarilo.conf", m)
	}
}

// A login proxy that reaches the master socket can issue itself a session for
// any user: only the backends and the delivery proxy hold its group.
func TestOnlyTheDeliveryProxyAndBackendsReachTheMasterSocket(t *testing.T) {
	cfg, err := config.Load(filepath.Join(systemdDir, "yarilo.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	master := cfg.AuthService.MasterSocketGroup
	if master == "" {
		t.Fatal("deploy/systemd/yarilo.yaml sets no master_socket_group")
	}
	sessionUsers := map[string]bool{}
	for _, u := range cfg.AuthService.MasterSessionUsers {
		sessionUsers[u] = true
	}
	for _, f := range serviceFiles(t) {
		users := unitKeys(t, f, "User")
		if len(users) == 0 {
			continue
		}
		groups := unitKeys(t, f, "SupplementaryGroups")
		if users[0] == "yarilo-login" && slices.Contains(groups, master) {
			t.Errorf("%s runs as yarilo-login with %s, the master socket's group", filepath.Base(f), master)
		}
		if filepath.Base(f) == "yarilo-lmtp-login.service" && !sessionUsers[users[0]] {
			t.Errorf("yarilo-lmtp-login runs as %s, which master_session_users %v does not let issue sessions",
				users[0], cfg.AuthService.MasterSessionUsers)
		}
	}
	b, err := os.ReadFile(filepath.Join(systemdDir, "sysusers.d", "yarilo.conf"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(b), "\n") {
		if f := strings.Fields(line); len(f) == 3 && f[0] == "m" && f[1] == "yarilo-login" && f[2] == master {
			t.Errorf("sysusers.d puts yarilo-login in %s, the master socket's group", master)
		}
	}
}

// A unit whose binary has a services entry finds it enabled in the sample: a
// unit started on a file that leaves its listener out exits at once.
func TestEveryUnitListenerIsInTheSample(t *testing.T) {
	cfg, err := config.Load(filepath.Join(systemdDir, "yarilo.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	entries := map[string]*config.ServiceConfig{}
	for _, e := range cfg.Services.Entries() {
		entries[e.Name] = *e.Svc
	}
	known := map[string]bool{}
	for _, e := range (&config.ServicesConfig{}).Entries() {
		known[e.Name] = true
	}
	for _, f := range serviceFiles(t) {
		name := strings.ReplaceAll(strings.TrimSuffix(strings.TrimPrefix(filepath.Base(f), "yarilo-"), ".service"), "-", "_")
		if !known[name] {
			continue
		}
		if svc := entries[name]; svc == nil || !svc.Enabled {
			t.Errorf("%s: services.%s is not enabled in deploy/systemd/yarilo.yaml", filepath.Base(f), name)
		}
	}
}
