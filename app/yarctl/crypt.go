package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"filippo.io/age"
)

func dispatchCrypt(args []string) error {
	if len(args) == 0 {
		printCryptUsage()
		return nil
	}
	switch args[0] {
	case "status":
		return cryptStart("status", args[1:])
	case "reencrypt":
		return cryptStart("reencrypt", args[1:])
	case "job":
		return cryptJob(args[1:])
	case "cancel":
		return cryptCancel(args[1:])
	case "keys":
		data, err := backendAPIGet("/api/backend/crypt/keys")
		return printOutput(data, err, printCryptKeys)
	case "keygen":
		return cryptKeygen(args[1:])
	default:
		return fmt.Errorf("unknown crypt command %q - available: status, reencrypt, job, cancel, keys, keygen", args[0])
	}
}

func printCryptUsage() {
	fmt.Println(`yarctl backend crypt <command>

Commands:
  status <user> [folder...] [--namespace NS] [--detach]
        Count the user's messages in each folder by state: plain, current
        (sealed to exactly the keys of public_key), other_key, reference,
        no_key. Every namespace of the user with a store of its own, or
        only NS. A folder that cannot be opened is reported and the command
        exits non-zero. Runs as a job on the user's backend-api; Ctrl-C
        cancels it unless --detach.

  reencrypt <user> [folder...] [--dry-run] [--to age|plain] [--namespace NS] [--detach]
        Rewrite every message that is not under the current key (--to age,
        the default) or that is sealed (--to plain), one at a time: the new
        body is verified by hash before it replaces the old one. A message
        no key opens is left and counted. maildir and sdbox stores; mdbox
        is refused for now. --dry-run reports and writes nothing.

  job <user>
        The user's running or last finished job.

  cancel <user>
        Stop the user's job after the message in hand.

  keys
        The configured keys by their public halves: which one new mail is
        sealed to, public keys no private key opens, reference key
        fingerprints. Never a private key.

  keygen --type x25519|hybrid --out FILE
        Generate a key pair locally. The private key goes only into FILE,
        created with mode 0600 and refused if it exists; the public key is
        printed.`)
}

func cryptStart(kind string, args []string) error {
	fs := flag.NewFlagSet("crypt "+kind, flag.ContinueOnError)
	ns := fs.String("namespace", "", "only this namespace (default: every namespace of the user with a store of its own)")
	detach := fs.Bool("detach", false, "return once the job has started")
	dryRun := fs.Bool("dry-run", false, "report what would be done, write nothing")
	to := fs.String("to", "age", "age or plain")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return fmt.Errorf("usage: yarctl backend crypt %s <user> [folder...]", kind)
	}
	user := fs.Arg(0)
	req := map[string]any{"user": user, "namespace": *ns, "folders": fs.Args()[1:]}
	if kind == "reencrypt" {
		req["dry_run"] = *dryRun
		req["to"] = *to
	}
	if _, err := backendAPIPost("/api/backend/crypt/"+kind, req); err != nil {
		return err
	}
	if *detach {
		fmt.Fprintf(os.Stderr, "crypt %s for %s started; 'yarctl backend crypt job %s' reads it\n", kind, user, user)
		return nil
	}
	return cryptFollow(user)
}

// cryptFollow waits for the user's job, printing progress to stderr; an
// interrupt cancels the job before returning.
func cryptFollow(user string) error {
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(stop)
	path := "/api/backend/crypt/job?user=" + url.QueryEscape(user)
	for {
		select {
		case <-stop:
			_, _ = backendAPIDelete(path)
			return errors.New("interrupted: the job was cancelled")
		default:
		}
		data, err := backendAPIGet(path + "&wait=2s")
		if err != nil {
			return fmt.Errorf("the job is gone (a backend-api restart loses it; rerun the command): %w", err)
		}
		var j cryptJobOut
		if err := json.Unmarshal(data, &j); err != nil {
			return err
		}
		if j.State == "running" {
			fmt.Fprintf(os.Stderr, "%s: %d messages\n", user, j.Seen)
			continue
		}
		if err := printOutput(data, nil, printCryptJob); err != nil {
			return err
		}
		return j.verdict()
	}
}

func cryptJob(args []string) error {
	if len(args) < 1 {
		return errors.New("usage: yarctl backend crypt job <user>")
	}
	data, err := backendAPIGet("/api/backend/crypt/job?user=" + url.QueryEscape(args[0]))
	return printOutput(data, err, printCryptJob)
}

func cryptCancel(args []string) error {
	if len(args) < 1 {
		return errors.New("usage: yarctl backend crypt cancel <user>")
	}
	data, err := backendAPIDelete("/api/backend/crypt/job?user=" + url.QueryEscape(args[0]))
	return printOutput(data, err, printCryptJob)
}

func backendAPIDelete(path string) ([]byte, error) {
	base, err := backendBaseForUser(resolveBackendUser(path, nil))
	if err != nil {
		return nil, err
	}
	return doRequest(base, backendAPIToken, http.MethodDelete, path, nil)
}

type cryptJobOut struct {
	Kind    string `json:"kind"`
	User    string `json:"user"`
	State   string `json:"state"`
	Error   string `json:"error"`
	Seen    int    `json:"seen"`
	Folders []struct {
		Namespace string         `json:"namespace"`
		Folder    string         `json:"folder"`
		States    map[string]int `json:"states"`
		Failed    int            `json:"failed"`
		Error     string         `json:"error"`
	} `json:"folders"`
	Totals        map[string]int `json:"totals"`
	WouldRewrite  int            `json:"would_rewrite"`
	Rewritten     int            `json:"rewritten"`
	Skipped       int            `json:"skipped"`
	DryRun        bool           `json:"dry_run"`
	NoKey         int            `json:"no_key"`
	Failed        int            `json:"failed"`
	FoldersFailed int            `json:"folders_failed"`
}

// verdict is the exit status: a failed or cancelled job, or a message that
// could not be read, is not a success.
func (j cryptJobOut) verdict() error {
	switch {
	case j.State == "failed":
		return fmt.Errorf("crypt %s for %s failed: %s", j.Kind, j.User, j.Error)
	case j.State == "cancelled":
		return fmt.Errorf("crypt %s for %s was cancelled", j.Kind, j.User)
	case j.FoldersFailed > 0:
		return fmt.Errorf("crypt %s for %s: %d folders could not be opened, their messages are not counted", j.Kind, j.User, j.FoldersFailed)
	case j.Failed > 0:
		return fmt.Errorf("crypt %s for %s: %d messages failed and were left as they were (named in the backend-api log)", j.Kind, j.User, j.Failed)
	}
	return nil
}

var cryptStates = []string{"plain", "current", "other_key", "reference", "no_key"}

func printCryptJob(data []byte) error {
	var j cryptJobOut
	if err := json.Unmarshal(data, &j); err != nil {
		return err
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "NAMESPACE\tFOLDER\t%s\tFAILED\n", strings.ToUpper(strings.Join(cryptStates, "\t")))
	row := func(ns, name string, st map[string]int, failed int, ferr string) {
		if ferr != "" {
			fmt.Fprintf(tw, "%s\t%s\tnot opened: %s\n", ns, name, ferr)
			return
		}
		cells := make([]string, len(cryptStates))
		for i, s := range cryptStates {
			cells[i] = fmt.Sprint(st[s])
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\n", ns, name, strings.Join(cells, "\t"), failed)
	}
	for _, f := range j.Folders {
		row(f.Namespace, f.Folder, f.States, f.Failed, f.Error)
	}
	row("", "TOTAL", j.Totals, j.Failed, "")
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Printf("\n%s %s for %s: %s, %d messages", j.Kind, "job", j.User, j.State, j.Seen)
	switch {
	case j.Kind == "reencrypt" && j.DryRun:
		fmt.Printf(", would rewrite %d, no key %d", j.WouldRewrite, j.NoKey)
	case j.Kind == "reencrypt":
		fmt.Printf(", rewritten %d, expunged meanwhile %d, no key %d", j.Rewritten, j.Skipped, j.NoKey)
	}
	if j.Error != "" {
		fmt.Printf(", error: %s", j.Error)
	}
	fmt.Println()
	return nil
}

func printCryptKeys(data []byte) error {
	var r struct {
		Identities []struct {
			Recipient string `json:"recipient"`
			Type      string `json:"type"`
			Current   bool   `json:"current"`
		} `json:"identities"`
		WriteOnly     []string `json:"write_only"`
		ReferenceKeys []string `json:"reference_keys"`
	}
	if err := json.Unmarshal(data, &r); err != nil {
		return err
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "KEY\tTYPE\tNEW MAIL")
	for _, k := range r.Identities {
		mark := ""
		if k.Current {
			mark = "yes"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\n", k.Recipient, k.Type, mark)
	}
	for _, k := range r.WriteOnly {
		fmt.Fprintf(tw, "%s\t-\tyes, but no private key opens it\n", k)
	}
	for _, k := range r.ReferenceKeys {
		fmt.Fprintf(tw, "reference sha256:%s\treference\t\n", k)
	}
	return tw.Flush()
}

// cryptKeygen writes the private key to a new file only; nothing secret
// reaches stdout, stderr or a log.
func cryptKeygen(args []string) error {
	fs := flag.NewFlagSet("crypt keygen", flag.ContinueOnError)
	typ := fs.String("type", "", "x25519 or hybrid")
	out := fs.String("out", "", "file for the private key, created 0600, never overwritten")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if *out == "" {
		return errors.New("usage: yarctl backend crypt keygen --type x25519|hybrid --out FILE")
	}
	var secret, public string
	switch *typ {
	case "x25519":
		id, err := age.GenerateX25519Identity()
		if err != nil {
			return err
		}
		secret, public = id.String(), id.Recipient().String()
	case "hybrid":
		id, err := age.GenerateHybridIdentity()
		if err != nil {
			return err
		}
		secret, public = id.String(), id.Recipient().String()
	default:
		return fmt.Errorf("type: %q is neither x25519 nor hybrid", *typ)
	}
	if err := writeKeyFile(*out, fmt.Sprintf("# created: %s\n# public key: %s\n%s\n",
		time.Now().UTC().Format(time.RFC3339), public, secret)); err != nil {
		return err
	}
	fmt.Println(public)
	return nil
}

func writeKeyFile(path, content string) error {
	old := syscall.Umask(0o077)
	defer syscall.Umask(old)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("keygen: %w", err)
	}
	if _, err := f.WriteString(content); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return fmt.Errorf("keygen: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return fmt.Errorf("keygen: %w", err)
	}
	return f.Close()
}
