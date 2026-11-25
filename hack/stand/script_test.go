// Package stand holds the sandbox measurement scripts. The rows here are what
// CI can check about them without a cluster.
package stand

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

func scripts(t *testing.T) []string {
	t.Helper()
	found, err := filepath.Glob("*.sh")
	if err != nil || len(found) == 0 {
		t.Fatalf("no scripts found: %v", err)
	}
	return found
}

// A window that dies on the tooling costs the slot, and the slot is the scarce
// thing: the scripts at least have to parse.
func TestEveryStandScriptParses(t *testing.T) {
	for _, s := range scripts(t) {
		t.Run(s, func(t *testing.T) {
			if out, err := exec.CommandContext(t.Context(), "bash", "-n", s).CombinedOutput(); err != nil {
				t.Errorf("bash -n: %v\n%s", err, out)
			}
		})
	}
}

// An empty array is an unbound variable under `set -u` on bash 3.2: the arm
// with no overlay died at the deploy line on a laptop that runs it.
func TestAnEmptyArrayExpansionSurvivesSetU(t *testing.T) {
	const snippet = `set -euo pipefail
a=()
printf '%s\n' cmd ${a[@]+"${a[@]}"} tail
a=(-f over.yaml)
printf '%s\n' cmd ${a[@]+"${a[@]}"} tail`
	out, err := exec.CommandContext(t.Context(), "bash", "-c", snippet).CombinedOutput()
	if err != nil {
		t.Fatalf("the guarded expansion did not run: %v\n%s", err, out)
	}
	got := strings.Fields(string(out))
	want := []string{"cmd", "tail", "cmd", "-f", "over.yaml", "tail"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("argv = %v, want %v", got, want)
	}
}

// The arm's deploy is the line that bit: it must not expand an array without
// the guard, or the next windowless arm dies at the same place.
func TestTheArmDeployGuardsItsOverlay(t *testing.T) {
	raw, err := os.ReadFile("ab-arm.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if !strings.Contains(line, "[@]}\"") || strings.Contains(line, "[@]+") {
			continue
		}
		// A for-loop over a list built in the same function is fine; what is
		// not is an array that can legitimately be empty.
		if strings.Contains(line, "overlay_args") || strings.Contains(line, "curls") ||
			strings.Contains(line, "pids") || strings.Contains(line, "files") {
			t.Errorf("unguarded expansion, empty is a legitimate value here:\n  %s", strings.TrimSpace(line))
		}
	}
}

// The chart an arm deploys comes from one place, checked like the tooling: a
// release's arm must not run this checkout's chart, nor a dirty one.
func TestTheArmDeploysTheChartItNames(t *testing.T) {
	src := armSource(t)
	for _, want := range []string{
		`CHART="${YARILO_ARM_CHART:-$REPO}"`,
		`git -C "$CHART" status --porcelain`,
		`echo "-- chart: $(git -C "$CHART" describe`,
		`stand_deploy "$OUT/deploy-$ARM" "$TAG" "$CHART/helm"`,
		`-f "$CHART/helm_values/values-sandbox.yaml"`,
		`OVERLAY="$CHART/helm_values/`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("ab-arm.sh lacks %s", want)
		}
	}
	for _, stale := range []string{`"$REPO/helm"`, `"$REPO/helm_values/`} {
		if strings.Contains(src, stale) {
			t.Errorf("ab-arm.sh still deploys from %s", stale)
		}
	}
}

// armSource is the arm as CI can read it: what it greps for, what it prints,
// and which steps a carried arm skips.
func armSource(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("ab-arm.sh")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// A counter the arm collects and never prints is a number nobody reads, which
// is how the CRC price went missing from a whole window (#1964).
func TestEveryCollectedCounterHasAReader(t *testing.T) {
	src := armSource(t)
	for _, metric := range []string{
		"maildir_uidlist_read_total",
		"maildir_dir_read_total",
		"maildir_listing_miss_total",
		"maildir_cache_stat_total",
		"maildir_window_closed_total",
		"index_cache_record_crc_mismatch_total",
		"mailbox_message_opened_total",
		"fileindex_journal_write_failed_total",
		"mailbox_write_failed_total",
		"maildir_partial_pass_empty_total",
		"quota_folders_opened_total",
		"maildir_lock_hold_seconds",
		"imap_thread_record_seconds",
		"yarilo_threads_save_not_recorded_total",
	} {
		if strings.Count(src, metric) < 2 {
			t.Errorf("%s is collected but never summed into a line an operator reads", metric)
		}
	}
}

// A carried arm must not wipe, seed or fill: those are what a window about
// state written by the version before cannot survive (#1714).
func TestACarriedArmSkipsTheWipe(t *testing.T) {
	src := armSource(t)
	if !strings.Contains(src, `KEEP_STORE="${YARILO_ARM_KEEP_STORE:-0}"`) {
		t.Fatal("the carried mode is gone; a window over old state cannot be run")
	}
	carry := strings.Index(src, `if [ "$KEEP_STORE" = "1" ]; then`)
	wipe := strings.Index(src, `step "wipe"`)
	if carry < 0 || wipe < 0 || carry > wipe {
		t.Fatal("the wipe is not inside the branch the carried mode skips")
	}
	if !strings.Contains(src, "messages-last-arm.txt") {
		t.Error("nothing records what an arm ended on, so a carried start cannot be proven")
	}
}

// The CPU profile is what prices a read, so it is taken on every arm and its
// absence fails the arm rather than passing quietly.
func TestTheCPUProfileIsTakenOnEveryArm(t *testing.T) {
	src := armSource(t)
	if !strings.Contains(src, "cpu_profile \"$ARM-$name\"") {
		t.Fatal("no arm takes a CPU profile")
	}
	idx := strings.Index(src, "cpu_profile \"$ARM-$name\"")
	before := src[:idx]
	if strings.LastIndex(before, `if [ "$BLOCKPROFILE" = "1" ]; then`) > strings.LastIndex(before, "\n  backend_counters") {
		t.Error("the CPU profile is inside the overlay branch, so an ordinary arm takes none")
	}
	if !strings.Contains(src, "this arm cannot price a read") {
		t.Error("a missing CPU profile does not fail the arm")
	}
}

// An overlay may carry only the keys its own question needs; the guard reads
// two levels, because a sibling one level down is what quietly moves in.
func TestEveryOverlayCarriesOnlyItsOwnKeys(t *testing.T) {
	cases := []struct {
		name     string
		allow    string
		notAnArm bool
	}{
		{name: "blockprofile", allow: `^telemetry(\.pprof)?$`},
		{name: "fsync-never", allow: `^storage(\.mail_fsync)?$`},
		{name: "notls", allow: `^internalTLS(\.enabled)?$`},
		// The OAuth2 matrix overlays: every login would reach the IdP, so never an arm (#2231).
		{name: "oauth2-local", allow: `^components(\.auth)?$`, notAnArm: true},
		{name: "oauth2-introspection", allow: `^components(\.auth)?$`, notAnArm: true},
		{name: "oauth2-discovery", allow: `^components(\.auth)?$`, notAnArm: true},
		// The policy overlay: every login would reach wforce, so never an arm (#2317).
		{name: "wforce", allow: `^(components(\.auth)?|protocol(\.imap)?)$`, notAnArm: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join("..", "..", "helm_values", "values-sandbox-"+tc.name+".yaml")
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("the %s overlay is missing: %v", tc.name, err)
			}
			allow := regexp.MustCompile(tc.allow)
			var top string
			for _, line := range strings.Split(string(raw), "\n") {
				trimmed := strings.TrimSpace(line)
				if trimmed == "" || strings.HasPrefix(trimmed, "#") {
					continue
				}
				indent := len(line) - len(strings.TrimLeft(line, " "))
				if indent > 2 {
					continue // the guard reads two levels, as the overlays are two deep
				}
				key := strings.TrimSuffix(strings.Fields(trimmed)[0], ":")
				path := key
				if indent == 2 {
					path = top + "." + key
				} else {
					top = key
				}
				if !allow.MatchString(path) {
					t.Errorf("%s carries %q, which its question does not need", tc.name, path)
				}
			}
			// And the arm must know the name, or the overlay is unreachable.
			if named := strings.Contains(armSource(t), tc.name+")"); named == tc.notAnArm {
				t.Errorf("ab-arm.sh naming the %s overlay is %v, want %v", tc.name, named, !tc.notAnArm)
			}
		})
	}
}

// An overlay nobody named is refused rather than deployed silently.
func TestTheArmRefusesAnUnknownOverlay(t *testing.T) {
	if !strings.Contains(armSource(t), "no overlay is named") {
		t.Error("an unknown overlay name does not stop the arm")
	}
}

// bash resolves a function when the line runs: a helper defined below its
// first top-level caller is "command not found", and cost win406 an arm.
func TestEveryHelperIsDefinedBeforeItIsCalled(t *testing.T) {
	src := armSource(t)
	lines := strings.Split(src, "\n")
	def := regexp.MustCompile(`^([a-z_][a-z0-9_]*)\(\)\s*\{`)

	defined := map[string]int{}
	for i, line := range lines {
		if m := def.FindStringSubmatch(line); m != nil {
			defined[m[1]] = i
		}
	}
	if len(defined) == 0 {
		t.Fatal("no helpers found, so this asserts nothing")
	}

	inFunc := false
	for i, line := range lines {
		if def.MatchString(line) {
			inFunc = true
			continue
		}
		if inFunc {
			if line == "}" {
				inFunc = false
			}
			continue
		}
		for name, at := range defined {
			if at <= i {
				continue
			}
			call := regexp.MustCompile(`(^|[^\w.-])` + regexp.QuoteMeta(name) + `($|[^\w.-])`)
			if call.MatchString(line) && !strings.Contains(line, "#") {
				t.Errorf("line %d calls %s, which is defined at line %d:\n  %s",
					i+1, name, at+1, strings.TrimSpace(line))
			}
		}
	}
}

// A definition inside a branch exists only when that branch runs: the carried
// arm skipped the wipe and lost two helpers defined between its steps.
func TestNoHelperIsDefinedInsideABranch(t *testing.T) {
	def := regexp.MustCompile(`^([a-z_][a-z0-9_]*)\(\)\s*\{`)
	depth, inFunc := 0, false
	for i, line := range strings.Split(armSource(t), "\n") {
		trimmed := strings.TrimSpace(line)
		if def.MatchString(line) {
			if depth > 0 {
				t.Errorf("line %d defines %s inside a branch, so an arm that takes the other one has no such helper",
					i+1, strings.TrimSuffix(trimmed, "() {"))
			}
			inFunc = true
			continue
		}
		if inFunc {
			if line == "}" {
				inFunc = false
			}
			continue
		}
		// Only top-level control counts: what is inside a function travels
		// with it.
		switch {
		case strings.HasPrefix(trimmed, "if ") || trimmed == "if", strings.HasPrefix(trimmed, "for "), strings.HasPrefix(trimmed, "while "), strings.HasPrefix(trimmed, "case "):
			depth++
		case trimmed == "fi" || trimmed == "done" || trimmed == "esac":
			if depth > 0 {
				depth--
			}
		}
	}
}

// A forward that never came up is a flake and is retried once; a pod that
// cannot be profiled still stops the arm.
func TestAFlakyForwardIsRetriedOnce(t *testing.T) {
	src := armSource(t)
	if !strings.Contains(src, `/pprof.sh"`) {
		t.Fatal("ab-arm.sh does not source pprof.sh, where the capture lives")
	}
	lib, err := os.ReadFile("pprof.sh")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(lib), "retry_capture() {") {
		t.Fatal("no retry around the capture; one flaky forward ends an arm")
	}
	for _, kind := range []string{"block_profile", "cpu_profile"} {
		if !regexp.MustCompile(kind + `\(\) \{ retry_capture`).MatchString(src) {
			t.Errorf("%s does not go through the retry", kind)
		}
	}
	// The retry must tell the two causes apart, or a pod with no pprof at all
	// is tried twice and still fails the arm twice as slowly.
	if !strings.Contains(string(lib), `[ "$rc" = 2 ] || return "$rc"`) {
		t.Error("the retry does not distinguish a forward that never came up from a capture that failed")
	}
}

// The method is read in the README, so that is where the carried mode's
// limits have to be: a window is set up from it, not from the shell (#1714).
func TestTheReadmeSaysWhatTheCarriedModeIsFor(t *testing.T) {
	raw, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatalf("the stand has no README: %v", err)
	}
	doc := string(raw)
	for _, want := range []string{
		"YARILO_ARM_KEEP_STORE",
		"first listing",
		"not** a throughput comparison",
		"bigger store",
		"two arms that both wipe and fill",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("the README does not say %q, so the next window can read the mode as a general A/B", want)
		}
	}
	// And the script points at it rather than repeating it.
	if !strings.Contains(armSource(t), "README.md") {
		t.Error("the arm does not point at the README")
	}
}

// One run per type cannot show a cold listing: there is nothing to compare
// the first run with.
func TestTheArmCanRepeatARunPerType(t *testing.T) {
	src := armSource(t)
	if !strings.Contains(src, `REPEATS="${YARILO_ARM_REPEATS:-1}"`) {
		t.Fatal("an arm cannot repeat a run, so cold and warm cannot be told apart")
	}
	if !strings.Contains(src, `name="$type-run$run"`) {
		t.Error("the runs of one type share a name, so their counters are one number")
	}
	// Default stays one run, or every window pays three times over.
	if !strings.Contains(src, "YARILO_ARM_REPEATS:-1") {
		t.Error("the default is not one run")
	}
}

// A quick arm is one storage type, and it has to prove its own start only:
// a wipe check over ranges the arm never touched reads somebody else's state.
func TestTheQuickArmTouchesOnlyItsOwnType(t *testing.T) {
	src := armSource(t)
	if !strings.Contains(src, `TYPES="${YARILO_ARM_TYPES:-mdbox maildir sdbox}"`) {
		t.Fatal("an arm cannot be narrowed to one storage type")
	}
	// Every place that walks the types walks the ones this arm runs.
	if n := strings.Count(src, "for t in mdbox maildir sdbox"); n != 0 {
		t.Errorf("%d loops still walk all three types, so a one-type arm wipes, fills and proves the other two", n)
	}
	if strings.Contains(src, `for pair in "mdbox 1-20"`) || !strings.Contains(src, "for type in $TYPES") {
		t.Error("the runs are a fixed list, so a one-type arm still runs three")
	}
	// And the arm says which type and which fill it was, beside the tag.
	if !strings.Contains(src, `types=[$TYPES] fill=$FILL`) {
		t.Error("the arm does not print its knobs, so a window cannot say what kind of arm it is")
	}
}

// The first-seen count is the start's, not the image's: 20 accounts log in per
// run, so a smaller fill must not move it.
func TestTheArmFlagsAFirstSeenOutsideItsRange(t *testing.T) {
	src := armSource(t)
	if !strings.Contains(src, "first_seen_off=1") {
		t.Error("a first-seen outside 20-21 is not marked, so a different start reads as a result")
	}
	if !strings.Contains(src, "cold + 0 < 20 || cold + 0 > 21") {
		t.Error("the first-seen control does not name the range it checks")
	}
}

func acceptSource(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("accept.sh")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// smokeLog is a smoketest log as slog writes it: one line per skip, then the summary.
func smokeLog(t *testing.T, checks, passed, failed int, skips map[string]string) string {
	t.Helper()
	var b strings.Builder
	for name, area := range skips {
		fmt.Fprintf(&b, `{"level":"WARN","msg":"smoke: SKIP","check":%q,"reason":"needs x","area":%q,"exempt":false}`+"\n", name, area)
	}
	fmt.Fprintf(&b, `{"level":"INFO","msg":"smoke: summary","checks":%d,"passed":%d,"failed":%d,"skipped":%d,"exempt":0}`+"\n",
		checks, passed, failed, len(skips))
	p := filepath.Join(t.TempDir(), "smoke.log")
	if err := os.WriteFile(p, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// Acceptance skips exactly the oauth2 rows, named; a count of three made of
// anything else is a failure (#2231).
func TestTheSmoketestVerdictSkipsOnlyTheOAuth2RowsByName(t *testing.T) {
	src := acceptSource(t)
	checks := regexp.MustCompile(`(?m)^SMOKE_CHECKS=\d+$`).FindString(src)
	names := regexp.MustCompile(`(?ms)^SMOKE_OAUTH2_SKIPS='.*?'$`).FindString(src)
	fn := regexp.MustCompile(`(?ms)^smoke_judge\(\) \{.*?^\}`).FindString(src)
	if checks == "" || names == "" || fn == "" {
		t.Fatal("accept.sh lost SMOKE_CHECKS, SMOKE_OAUTH2_SKIPS or smoke_judge")
	}
	oauth := map[string]string{}
	for _, n := range strings.Split(strings.Trim(strings.TrimPrefix(names, "SMOKE_OAUTH2_SKIPS="), "'"), "\n") {
		oauth[n] = "oauth2"
	}
	passed, _ := strconv.Atoi(strings.TrimPrefix(checks, "SMOKE_CHECKS="))
	if len(oauth) != 3 {
		t.Fatalf("SMOKE_OAUTH2_SKIPS names %d rows, want 3", len(oauth))
	}
	swapped := map[string]string{"imap FTS (SEARCH BODY/TEXT/HEADER/FROM)": "imap"}
	plusOne := map[string]string{"imap FTS (SEARCH BODY/TEXT/HEADER/FROM)": "imap"}
	n := 0
	for name := range oauth {
		plusOne[name] = "oauth2"
		if n < 2 {
			swapped[name] = "oauth2"
		}
		n++
	}
	tests := []struct {
		name string
		log  string
		ok   bool
	}{
		{"the oauth2 rows skipped", smokeLog(t, passed+3, passed, 0, oauth), true},
		{"three skips, one of them not oauth2", smokeLog(t, passed+3, passed, 0, swapped), false},
		{"another row skipped as well", smokeLog(t, passed+4, passed, 0, plusOne), false},
		{"nothing skipped", smokeLog(t, passed+3, passed+3, 0, nil), false},
		{"a failure", smokeLog(t, passed+3, passed-1, 1, oauth), false},
		{"a row more than SMOKE_CHECKS", smokeLog(t, passed+4, passed+1, 0, oauth), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := exec.CommandContext(t.Context(), "bash", "-c", checks+"\n"+names+"\n"+fn+"\nsmoke_judge "+tt.log).CombinedOutput()
			if err != nil {
				t.Fatalf("smoke_judge: %v\n%s", err, out)
			}
			if got := strings.HasPrefix(string(out), "ok "); got != tt.ok {
				t.Errorf("verdict %q, want ok=%v", out, tt.ok)
			}
		})
	}
}

// The names acceptance expects are the names the binary registers.
func TestTheAcceptedSkipsAreTheSmoketestRows(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "app", "smoketest", "oauth2.go"))
	if err != nil {
		t.Fatal(err)
	}
	var code []string
	for _, m := range regexp.MustCompile(`oauth2\w+Check\s+= "([^"]+)"`).FindAllStringSubmatch(string(raw), -1) {
		code = append(code, m[1])
	}
	names := regexp.MustCompile(`(?ms)^SMOKE_OAUTH2_SKIPS='(.*?)'$`).FindStringSubmatch(acceptSource(t))
	if len(code) != 3 || names == nil || strings.Join(code, "\n") != names[1] {
		t.Errorf("smoketest names %q, accept.sh expects %q", code, names)
	}
}

// An APPEND whose connection a domain move closed is not a failed append; any
// other APPEND error, or one outside the kick's two seconds, still is (#2388).
func TestAKickedAppendIsLeftOut(t *testing.T) {
	fn := regexp.MustCompile(`(?ms)^not_kicked\(\) \{.*?^\}`).FindString(acceptSource(t))
	if fn == "" {
		t.Fatal("accept.sh lost not_kicked")
	}
	line := func(at, msg string) string {
		return fmt.Sprintf(`{"time":"2026-10-29T%sZ","level":"ERROR","msg":%q,"service":"imap"}`, at, msg)
	}
	pipe := "handling APPEND command: write tcp 10.1.114.43:10143->10.1.114.39:44570: write: broken pipe"
	kicks := `{"time":"2026-10-29T14:57:40.504577421Z","level":"INFO","msg":"director: domain moved, kicking its sessions off the old backend"}` + "\n"
	cases := []struct {
		name string
		line string
		kept bool
	}{
		{"broken pipe just after the kick", line("14:57:40.551953302", pipe), false},
		{"reset within two seconds", line("14:57:42.100000000", "handling APPEND command: read: connection reset by peer"), false},
		{"broken pipe after the window", line("14:57:43.000000000", pipe), true},
		{"broken pipe before the kick", line("14:57:40.100000000", pipe), true},
		{"another error at the kick", line("14:57:40.600000000", "handling APPEND command: quota exceeded"), true},
	}
	dir := t.TempDir()
	kf := filepath.Join(dir, "kicks")
	if err := os.WriteFile(kf, []byte(kicks), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lf := filepath.Join(t.TempDir(), "lines")
			if err := os.WriteFile(lf, []byte(tc.line+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			out, err := exec.CommandContext(t.Context(), "bash", "-c", fn+"\nnot_kicked "+kf+" "+lf).CombinedOutput()
			if err != nil {
				t.Fatalf("not_kicked: %v\n%s", err, out)
			}
			if kept := strings.TrimSpace(string(out)) != ""; kept != tc.kept {
				t.Errorf("kept = %v, want %v (output %q)", kept, tc.kept, out)
			}
		})
	}
}

// The SRS domain must be dkim-milter-fwd's sign_domain; either missing fails.
func TestSRSDomainIsTheSignDomain(t *testing.T) {
	fn := regexp.MustCompile(`(?ms)^srs_judge\(\) \{.*?^\}`).FindString(acceptSource(t))
	if fn == "" {
		t.Fatal("accept.sh lost srs_judge")
	}
	for _, tc := range []struct {
		srs, sign, want string
	}{
		{"fwd.d00001.test", "fwd.d00001.test", "ok fwd.d00001.test"},
		{"fwd.d00001.test", "fwd.d00002.test", "fail SRS domain 'fwd.d00001.test', sign_domain 'fwd.d00002.test'"},
		{"mail-sb.seconddns.com", "fwd.d00001.test", "fail SRS domain 'mail-sb.seconddns.com', sign_domain 'fwd.d00001.test'"},
		{"fwd.d00001.test", "", "fail SRS domain 'fwd.d00001.test', sign_domain 'none'"},
		{"", "", "fail SRS domain 'none', sign_domain 'none'"},
	} {
		out, err := exec.CommandContext(t.Context(), "bash", "-c", fn+"\nsrs_judge \"$1\" \"$2\"", "-", tc.srs, tc.sign).CombinedOutput()
		if err != nil {
			t.Fatalf("srs_judge: %v\n%s", err, out)
		}
		if got := strings.TrimSpace(string(out)); got != tc.want {
			t.Errorf("srs_judge %q %q = %q, want %q", tc.srs, tc.sign, got, tc.want)
		}
	}
}

// The MAILCRYPT window compares write modes alone: the A and B overlays may
// differ in storage.mail_crypt.write and nothing else, or B would measure
// another change too. Both must be overlays ab-arm.sh accepts.
func TestMailCryptOverlaysDifferInWriteAlone(t *testing.T) {
	lines := func(name string) []string {
		b, err := os.ReadFile(filepath.Join("..", "..", "helm_values", "values-sandbox-"+name+".yaml"))
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, l := range strings.Split(string(b), "\n") {
			if s := strings.TrimSpace(l); s != "" && !strings.HasPrefix(s, "#") {
				out = append(out, l)
			}
		}
		return out
	}
	a, b := lines("mailcrypt-keys"), lines("mailcrypt-write")
	if len(a) != len(b) {
		t.Fatalf("the overlays have %d and %d lines", len(a), len(b))
	}
	diff := 0
	for i := range a {
		if a[i] != b[i] {
			diff++
			if strings.TrimSpace(a[i]) != `write: "off"` || strings.TrimSpace(b[i]) != "write: age" {
				t.Errorf("line %d: %q against %q; only write may differ", i+1, a[i], b[i])
			}
		}
	}
	if diff != 1 {
		t.Errorf("%d lines differ, want the write line alone", diff)
	}
	arm, err := os.ReadFile("ab-arm.sh")
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`(?m)^\s*mailcrypt-keys\|mailcrypt-write\)`).Match(arm) {
		t.Error("ab-arm.sh does not name the mailcrypt overlays")
	}
}

// The window's disk growth is B's message bytes against A's, per type, after
// the same fill: every arm must write them, quoted so the pod's shell does not
// expand the patterns against the domain's own directory.
func TestEveryArmWritesItsMessageBytes(t *testing.T) {
	arm, err := os.ReadFile("ab-arm.sh")
	if err != nil {
		t.Fatal(err)
	}
	s := string(arm)
	if !strings.Contains(s, `"disk: arm=$ARM type=$t message_bytes=`) {
		t.Error("ab-arm.sh writes no per-type message bytes after the fill")
	}
	for _, p := range []string{`'*/Maildir/cur/*'`, `'*/Maildir/new/*'`, `'*/dbox-Mails/u.*'`, `'*/storage/m.*'`} {
		if !strings.Contains(s, p) {
			t.Errorf("pattern %s is not quoted for the pod's shell", p)
		}
	}
}

// The merge watcher's clock: busybox awk's %d is 32-bit and printed
// -2147483648 for an uptime in milliseconds, which timed no merge (d2621h).
func TestTheMergeWatchersClockDoesNotOverflow(t *testing.T) {
	raw, err := os.ReadFile("thread-merge-watch.sh")
	if err != nil {
		t.Fatal(err)
	}
	var now string
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "now()") {
			now = line
		}
	}
	if now == "" {
		t.Fatal("no now() in the watcher")
	}
	up := filepath.Join(t.TempDir(), "uptime")
	if err := os.WriteFile(up, []byte("7901857.25 1234.50\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.CommandContext(t.Context(), "sh", "-c", "UPTIME="+up+"\n"+now+"\nnow").CombinedOutput()
	if err != nil {
		t.Fatalf("now: %v\n%s", err, out)
	}
	if got := strings.TrimSpace(string(out)); got != "790185725" {
		t.Errorf("now() = %q, want 790185725 hundredths", got)
	}
}

// The summary of a run reads the merge time and the trigger off the files the
// run leaves, and a run with every delivery 250 exits 0: grep -c counting none
// exits 1, which failed a summary after d2621d.
func TestTheMergeSummaryReadsARun(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"run.log": "2026-11-20T20:10:53Z u60@x merge-2 yarilo-backend-0: trigger_seen_cs=790190444 base_seen_cs=790190498 base_mtime=x base_size=6360058 log_after=215\n",
		"phases.txt": "== u60@x merge-2\n" +
			"phase: user=u60@x n=1 noop_ms=1 rcpt_ms=2 data_ms=70 rcpt=250 data=250 log_before=130900 log_after=131000\n" +
			"phase: user=u60@x n=1 noop_ms=1 rcpt_ms=2 data_ms=72 rcpt=250 data=250 log_before=131000 log_after=131199\n",
		"fill.txt": "phase: user=u60@x n=1 noop_ms=1 rcpt_ms=2 data_ms=60 rcpt=250 data=250\n",
		"mem.txt":  "u60@x yarilo-backend-0 peak=1 anon=2 file=3\n",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	out, err := exec.CommandContext(t.Context(), "bash", "thread-merge-summary.sh", dir).CombinedOutput()
	if err != nil {
		t.Fatalf("summary exited %v\n%s", err, out)
	}
	for _, want := range []string{"u60@x merge-2 yarilo-backend-0 merge 0.54 s", "u60@x merge-2 trigger data_ms=72", "deliveries not 250: 0"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("the summary lacks %q:\n%s", want, out)
		}
	}
}

// A run with a delivery not answered 250 exits non-zero, as the README says.
func TestTheMergeSummaryFailsARunWithAFailedDelivery(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{
		"run.log":    "",
		"phases.txt": "== u60@x merge-1\nphase: user=u60@x n=1 noop_ms=1 rcpt_ms=2 data_ms=70 rcpt=250 data=451 log_before=1 log_after=2\n",
		"fill.txt":   "phase: user=u60@x n=1 noop_ms=1 rcpt_ms=2 data_ms=60 rcpt=250 data=250\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	out, err := exec.CommandContext(t.Context(), "bash", "thread-merge-summary.sh", dir).CombinedOutput()
	if err == nil {
		t.Fatalf("a run with a 451 exited 0:\n%s", out)
	}
	if !strings.Contains(string(out), "deliveries not 250: 1") {
		t.Errorf("the summary does not count it:\n%s", out)
	}
}

// A preparation that did not take fails the aged row with its reason: the
// reason holds colons, so the type is read after the last one, never the first.
func TestAFailedAgedPreparationFailsTheRow(t *testing.T) {
	fn := regexp.MustCompile(`(?ms)^aged_target\(\) \{.*?^\}`).FindString(acceptSource(t))
	if fn == "" {
		t.Fatal("accept.sh lost aged_target")
	}
	for _, tc := range []struct{ spec, want string }{
		{"prep: the fill of u98@d00002.test did not take (/w/acc/fill-aged.txt):maildir", "maildir fail the fill of u98@d00002.test did not take (/w/acc/fill-aged.txt)"},
		{"prep: u149@d00003.test has 3 files older than a day, want 20 or more:sdbox", "sdbox fail u149@d00003.test has 3 files older than a day, want 20 or more"},
		{":sdbox", "sdbox fail "},
		{"u98@d00002.test 30:maildir", "maildir user u98@d00002.test"},
	} {
		out, err := exec.CommandContext(t.Context(), "bash", "-c", fn+"\naged_target \"$1\"", "-", tc.spec).CombinedOutput()
		if err != nil {
			t.Fatalf("aged_target: %v\n%s", err, out)
		}
		if got := strings.TrimRight(string(out), "\n"); got != tc.want {
			t.Errorf("aged_target %q = %q, want %q", tc.spec, got, tc.want)
		}
	}
}

// Every arm keeps the index of every account, base and both journals, before
// the next arm empties the accounts; a header correction names only the
// folder, so a few chosen ones would miss it. No bodies are taken.
func TestEveryArmKeepsTheIndexOfEveryAccount(t *testing.T) {
	arm, err := os.ReadFile("ab-arm.sh")
	if err != nil {
		t.Fatal(err)
	}
	s := string(arm)
	i := strings.Index(s, `step "index"`)
	if i < 0 {
		t.Fatal("ab-arm.sh keeps no index")
	}
	if j := strings.Index(s, `message_inventory > "$OUT/messages-last-arm.txt"`); j < i {
		t.Error("the index is kept after the arm's end, or not at all")
	}
	block := s[i:]
	block = block[:strings.Index(block, "\n\n")]
	if !strings.Contains(block, "first_pod backend") || strings.Contains(block, "| head") {
		t.Error("the index step does not take its pod through first_pod (#1875)")
	}
	for _, name := range []string{"-name yarilo.index ", "-name yarilo.index.log ", "-name yarilo.index.log.2 ", "find /var/mail/vhosts ", `"$OUT/index-$ARM.tgz"`} {
		if !strings.Contains(block, name) {
			t.Errorf("the index step does not name %q", name)
		}
	}
	for _, body := range []string{"Maildir/cur", "dbox-Mails", "storage/m."} {
		if strings.Contains(block, body) {
			t.Errorf("the index step takes bodies: %s", body)
		}
	}
}

// The index step on live backends: a tar that fails, or an archive with
// nothing in it, is said in the arm's output, and the arm goes on under
// set -euo pipefail to its summary.
func TestAFailedIndexStepDoesNotStopTheArm(t *testing.T) {
	arm, err := os.ReadFile("ab-arm.sh")
	if err != nil {
		t.Fatal(err)
	}
	s := string(arm)
	block := s[strings.Index(s, `step "index"`):]
	block = block[:strings.Index(block, "\nfi\n")+4]
	for _, tc := range []struct {
		name, kube, want string
	}{
		{"tar fails", `kube() { echo "tar: vanished" >&2; return 1; }`, "FAILED rc=1, 0 files"},
		{"empty archive", `kube() { tar czf - -T /dev/null; }`, "FAILED rc=0, 0 files"},
		{"one file", `kube() { tar czf - -C "$OUT" -T <(echo f); }`, "-- index of every account: 1 files"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := t.TempDir()
			if err := os.WriteFile(filepath.Join(out, "f"), []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
			script := "set -euo pipefail\nOUT=" + out + "\nARM=B\nstep() { :; }\nfirst_pod() { echo p; }\n" +
				tc.kube + "\n" + block + "\necho arm-went-on\n"
			got, err := exec.CommandContext(t.Context(), "bash", "-c", script).CombinedOutput()
			if err != nil {
				t.Fatalf("the step stopped the arm: %v\n%s", err, got)
			}
			if !strings.Contains(string(got), tc.want) || !strings.Contains(string(got), "arm-went-on") {
				t.Errorf("want %q and the arm going on, got:\n%s", tc.want, got)
			}
		})
	}
}

// Every arm reads the index's counters from every service's telemetry port,
// not only imap's: the header corrections came from fts.
func TestEveryArmReadsTheIndexCountersOfEveryService(t *testing.T) {
	arm, err := os.ReadFile("ab-arm.sh")
	if err != nil {
		t.Fatal(err)
	}
	s := string(arm)
	i := strings.Index(s, `step "index-counters"`)
	if i < 0 {
		t.Fatal("ab-arm.sh reads no index counters")
	}
	if j := strings.Index(s, `message_inventory > "$OUT/messages-last-arm.txt"`); j < i {
		t.Error("the counters are read after the arm's end")
	}
	block := s[i:]
	block = block[:strings.Index(block, "\n\n")]
	for _, want := range []string{"containerPort", "808[0-9]", "fileindex_(stale_pair|header_corrected)_total", "stale_pair_failed=", "|| true"} {
		if !strings.Contains(block, want) {
			t.Errorf("the counters step does not carry %q", want)
		}
	}
}

// The counters step on two pods and two ports, one of which does not answer:
// the summary counts what was asked and what answered and marks the line, and
// a clean read prints both equal with no mark.
func TestTheIndexCountersSayWhatWasRead(t *testing.T) {
	arm, err := os.ReadFile("ab-arm.sh")
	if err != nil {
		t.Fatal(err)
	}
	s := string(arm)
	block := s[strings.Index(s, `step "index-counters"`):]
	block = block[:strings.Index(block, "\n\n# What the next arm")]
	page := `fileindex_header_corrected_total{field=\"seen\"} 2
fileindex_stale_pair_total{path=\"open\",result=\"retried\"} 4
fileindex_stale_pair_total{path=\"reload\",result=\"failed\"} 0
other_total 9`
	for _, tc := range []struct {
		name, down, want string
	}{
		{"every port answered", "none", "-- index counters: ports=4 answered=4 header_corrected=8 stale_pair_retried=16 stale_pair_failed=0"},
		{"a port did not answer", "8085", "-- index counters: INCOMPLETE ports=4 answered=2 header_corrected=4 stale_pair_retried=8 stale_pair_failed=0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := t.TempDir()
			kube := `kube() {
  case "$1 $2" in
    "get pods") printf 'pod/p0\npod/p1\n' ;;
    "get pod") echo "10143 8080 9106 8085 8086x" ;;
    exec*) case "$*" in *":` + tc.down + `/"*) return 1 ;; esac; printf '%s\n' "` + page + `" ;;
  esac
}`
			script := "set -euo pipefail\nOUT=" + out + "\nARM=B\nstep() { :; }\n" + kube + "\n" + block + "\necho arm-went-on\n"
			got, err := exec.CommandContext(t.Context(), "bash", "-c", script).CombinedOutput()
			if err != nil {
				t.Fatalf("the step stopped the arm: %v\n%s", err, got)
			}
			if !strings.Contains(string(got), tc.want) || !strings.Contains(string(got), "arm-went-on") {
				t.Errorf("want %q and the arm going on, got:\n%s", tc.want, got)
			}
		})
	}
}

// store-parallel takes the captures it is asked for inside each type's run,
// from the same helper as the arm, and fails when one is not taken.
func TestStoreParallelProfilesInsideTheRun(t *testing.T) {
	src, err := os.ReadFile("store-parallel.sh")
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	if !strings.Contains(s, `/pprof.sh"`) {
		t.Fatal("store-parallel.sh does not source pprof.sh")
	}
	start := strings.Index(s, `for kind in $PROFILE; do`)
	run := strings.Index(s, `hack/stand/run-job.sh" store-parallel`)
	wait := strings.Index(s, `wait "$pid" || {`)
	if start < 0 || run < 0 || wait < 0 || start >= run || run >= wait {
		t.Fatal("the captures do not start before the run and get waited for after it")
	}
	if !strings.Contains(s, `retry_capture "$kind" "store-$LABEL-$t" "$PROFILE_SECS"`) {
		t.Error("the capture does not go through retry_capture with the type in its label")
	}
	if !strings.Contains(s, "exit 1; }") {
		t.Error("a capture not taken does not fail the run")
	}
}

// Two kinds captured at once take ports of their own: on shared ports the
// second rode the first's forward and lost it when the first closed it.
func TestTwoCaptureKindsTakeTheirOwnPorts(t *testing.T) {
	lib, err := os.ReadFile("pprof.sh")
	if err != nil {
		t.Fatal(err)
	}
	src := string(lib)
	body := src[strings.Index(src, "pprof_capture() {"):]
	if strings.Contains(body, "18080") {
		t.Error("pprof_capture still names a port of its own instead of its kind's")
	}
	fn := src[strings.Index(src, "pprof_port() {"):]
	fn = fn[:strings.Index(fn, "\n}\n")+3]
	ports := map[string]string{}
	for _, kind := range []string{"profile", "block"} {
		out, err := exec.CommandContext(t.Context(), "bash", "-c", fn+"\npprof_port "+kind).Output()
		if err != nil {
			t.Fatal(err)
		}
		ports[kind] = strings.TrimSpace(string(out))
	}
	p, b := ports["profile"], ports["block"]
	pi, _ := strconv.Atoi(p)
	bi, _ := strconv.Atoi(b)
	// Each kind forwards one port a pod: a hundred apart leaves room for every
	// backend pod the stand runs.
	if pi == 0 || bi == 0 || pi == bi || (bi-pi < 100 && pi-bi < 100) {
		t.Errorf("profile on %s and block on %s overlap", p, b)
	}
}

// store-parallel says what the imap process spent across the run, from the
// same two pages as the latencies, per pod and in all.
func TestStoreParallelPrintsThePodsCPU(t *testing.T) {
	src, err := os.ReadFile("store-parallel.sh")
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	i := strings.Index(s, "store_cpu() {")
	if i < 0 {
		t.Fatal("no store_cpu")
	}
	fn := s[i:]
	fn = fn[:strings.Index(fn, "\n}\n")+3]
	if !strings.Contains(s, "process_cpu_seconds_total )") {
		t.Error("the backend page does not keep the process CPU")
	}
	dir := t.TempDir()
	before := filepath.Join(dir, "b")
	after := filepath.Join(dir, "a")
	if err := os.WriteFile(before, []byte("process_cpu_seconds_total{pod=\"p0\"} 100.5\nprocess_cpu_seconds_total{pod=\"p1\"} 50\nimap_command_seconds_bucket{x} 3\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(after, []byte("process_cpu_seconds_total{pod=\"p0\"} 190.5\nprocess_cpu_seconds_total{pod=\"p1\"} 51.25\nimap_command_seconds_bucket{x} 9\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.CommandContext(t.Context(), "bash", "-c", fn+"\nstore_cpu \"$1\" \"$2\"", "-", before, after).CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if want := "p0=90.00s p1=1.25s imap_cpu=91.25s"; !strings.Contains(string(out), want) {
		t.Errorf("store_cpu = %q, want %q", out, want)
	}
}

// CRYPT_ONLY=load keeps the keys, the rows under imaptest and the way back to
// plaintext, and skips every other scenario; the helpers those rows and the
// way back use are defined outside any skipped block.
func TestTheCryptLoadModeSkipsOnlyTheOtherScenarios(t *testing.T) {
	src, err := os.ReadFile("crypt-scenarios.sh")
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	guard := `if [ "$ONLY" != load ]; then`
	inGuard := func(marker string) bool {
		at := strings.Index(s, marker)
		if at < 0 {
			t.Fatalf("no %q in crypt-scenarios.sh", marker)
		}
		open := strings.LastIndex(s[:at], guard)
		if open < 0 {
			return false
		}
		depth := 0
		for _, line := range strings.Split(s[open:at], "\n") {
			l := strings.TrimSpace(line)
			if strings.HasPrefix(l, "if ") {
				depth++
			}
			if l == "fi" {
				depth--
			}
		}
		return depth > 0
	}
	for _, skipped := range []string{`row PASS "mixed-$t"`, `big=$(status_of "$BIG_USER")`, `echo "== pod killed mid-run"`, `echo "== rotation (§13.10)"`, `echo "== speed"`} {
		if !inGuard(skipped) {
			t.Errorf("%s runs in the load mode", skipped)
		}
	}
	for _, kept := range []string{`echo "== under imaptest, each type"`, `echo "== back to plaintext"`, `trap finish EXIT`} {
		if inGuard(kept) {
			t.Errorf("%s is skipped in the load mode", kept)
		}
	}
	first := strings.Index(s, guard)
	for _, helper := range []string{"timed() {", "job_done() {", "load_running() {"} {
		if at := strings.Index(s, helper); at < 0 || at > first {
			t.Errorf("%s is not defined before the first skipped block", helper)
		}
	}
	if !strings.Contains(s, `"load-$name"`) || !strings.Contains(s, `name="$t-r$round"`) {
		t.Error("a round past the first does not carry its number in the row")
	}
	for _, bad := range []string{"CRYPT_ONLY=all", "CRYPT_LOAD_ROUNDS=0", "CRYPT_LOAD_ROUNDS=x"} {
		cmd := exec.CommandContext(t.Context(), "bash", "crypt-scenarios.sh", "tag", t.TempDir()+"/out")
		cmd.Env = append(os.Environ(), bad, "KUBECONFIG=/nonexistent")
		if err := cmd.Run(); err == nil || cmd.ProcessState.ExitCode() != 2 {
			t.Errorf("%s: exit %v, want 2 before anything runs", bad, err)
		}
	}
}

// The thread record line reads an arm's delta: bucket bounds for p50 and p99
// and the saves left outside their conversation.
func TestTheThreadRecordLineReadsADelta(t *testing.T) {
	src := armSource(t)
	i := strings.Index(src, `$1 == "yarilo_threads_save_not_recorded_total"`)
	if i < 0 {
		t.Fatal("no thread record line")
	}
	start := strings.LastIndex(src[:i], "awk '") + len("awk '")
	end := i + strings.Index(src[i:], "\n    '")
	prog := src[start:end]
	delta := filepath.Join(t.TempDir(), "delta.txt")
	page := "yarilo_threads_save_not_recorded_total 3\n" +
		"imap_thread_record_seconds_bucket{le=\"0.001\"} 90\n" +
		"imap_thread_record_seconds_bucket{le=\"0.016\"} 99\n" +
		"imap_thread_record_seconds_bucket{le=\"2\"} 100\n" +
		"imap_thread_record_seconds_bucket{le=\"+Inf\"} 100\n" +
		"maildir_lock_hold_seconds_bucket{site=\"save\",le=\"1\"} 7\n"
	if err := os.WriteFile(delta, []byte(page), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.CommandContext(t.Context(), "awk", prog, delta).CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if want := "thread record: n=100 p50<=0.001s p99<=0.016s not_threaded=3"; string(out) != want {
		t.Errorf("got %q, want %q", out, want)
	}
}

// Every arm keeps the logs of every pod and every metrics page whole, before
// the next arm's deploy replaces them: the lock service's and the LMTP's pages
// held what a window had to explain afterwards.
func TestEveryArmKeepsEveryPodsLogsAndMetrics(t *testing.T) {
	src := armSource(t)
	logs := src[strings.Index(src, `step "logs"`):]
	logs = logs[:strings.Index(logs, `step "metrics"`)]
	if strings.Contains(logs, "component=backend") || !strings.Contains(logs, "status.phase=Running") {
		t.Error("the logs step keeps only the backends")
	}
	i := strings.Index(src, `step "metrics"`)
	if i < 0 {
		t.Fatal("ab-arm.sh keeps no metrics pages")
	}
	if j := strings.Index(src, `message_inventory > "$OUT/messages-last-arm.txt"`); j < i {
		t.Error("the metrics are kept after the arm's end")
	}
	block := src[i:]
	block = block[:strings.Index(block, "\n\n")]
	for _, want := range []string{"containerPort", "/metrics", "wget -T 5", "|| continue", `"$OUT/metrics-$ARM/$po-$port.prom"`} {
		if !strings.Contains(block, want) {
			t.Errorf("the metrics step does not carry %q", want)
		}
	}
	if strings.Contains(block, "grep -E") {
		t.Error("the metrics step filters the pages")
	}
}
