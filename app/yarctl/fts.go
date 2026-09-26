package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/url"
	"strings"
)

func dispatchFTS(args []string) error {
	if len(args) == 0 {
		printFTSUsage()
		return nil
	}
	switch args[0] {
	case "status":
		return ftsStatus(args[1:])
	case "rescan":
		return ftsRescan(args[1:])
	case "optimize":
		return ftsOptimize(args[1:])
	case "lookup":
		return ftsLookup(args[1:])
	default:
		return fmt.Errorf("unknown fts command %q — available: status, rescan, optimize, lookup", args[0])
	}
}

// parseUserArg accepts the positional <user> before or after the flags: the
// flag package stops parsing at the first positional argument, so
// "fts status u1 --folder X" would otherwise silently ignore the flags.
func parseUserArg(fs *flag.FlagSet, args []string) (string, error) {
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		if err := parseFlags(fs, args[1:]); err != nil {
			return "", err
		}
		if fs.NArg() != 0 {
			return "", nil
		}
		return args[0], nil
	}
	if err := parseFlags(fs, args); err != nil {
		return "", err
	}
	if fs.NArg() != 1 {
		return "", nil
	}
	return fs.Arg(0), nil
}

func printFTSUsage() {
	fmt.Println(`yarctl fts <command>

Commands:
  status   <user> --folder NAME     — per-mailbox indexing checkpoint
  rescan   <user> [--folder NAME]   — reconcile the index against the mailbox;
                                       without --folder every folder is rescanned
  optimize <user>                   — compact every index owned by the user
                                       (mailboxes past fts_flatcurve_optimize_limit
                                       are also auto-compacted in the background;
                                       this forces it immediately, whole-user)
  lookup   <user> [--folder NAME] [--header NAME:VALUE] [--body TEXT] [--text TEXT]
                                    — ask the index what SEARCH would, with the
                                       query built as SEARCH builds it; flags repeat
                                       and are ANDed

All commands reach the yarilo-fts service via yarilo-backend-api. They return
HTTP 501 when the backend-api has no fts_addr configured.`)
}

// ftsStatus prints the indexing checkpoint for one folder.
// GET /api/backend/fts/status?user=&folder=
func ftsStatus(args []string) error {
	fs := flag.NewFlagSet("fts status", flag.ContinueOnError)
	folder := fs.String("folder", "INBOX", "folder name")
	user, err := parseUserArg(fs, args)
	if err != nil {
		return err
	}
	if user == "" {
		return fmt.Errorf("usage: yarctl fts status <user> [--folder NAME]")
	}
	data, err := backendAPIGet("/api/backend/fts/status?user=" +
		url.QueryEscape(user) + "&folder=" + url.QueryEscape(*folder))
	return printOutput(data, err, func(data []byte) error {
		var r struct {
			User             string `json:"user"`
			Folder           string `json:"folder"`
			LastIndexedUID   uint32 `json:"last_indexed_uid"`
			SettingsChecksum uint32 `json:"settings_checksum"`
			Documents        uint64 `json:"documents"`
			Copies           uint64 `json:"copies"`
			Messages         uint64 `json:"messages"`
			UnrecordedCopies uint64 `json:"unrecorded_copies"`
		}
		if err := json.Unmarshal(data, &r); err != nil {
			return err
		}
		fmt.Printf("%s [%s]: last indexed UID %d (settings checksum %08x)\n",
			r.User, r.Folder, r.LastIndexedUID, r.SettingsChecksum)
		fmt.Printf("%s: %d document(s), %d live copies, %d live messages, %d copies with no store row\n",
			r.User, r.Documents, r.Copies, r.Messages, r.UnrecordedCopies)
		return nil
	})
}

// ftsRescan reconciles one or every folder.
// POST /api/backend/fts/rescan?user=&folder=
func ftsRescan(args []string) error {
	fs := flag.NewFlagSet("fts rescan", flag.ContinueOnError)
	folder := fs.String("folder", "", "folder name; empty = all folders")
	user, err := parseUserArg(fs, args)
	if err != nil {
		return err
	}
	if user == "" {
		return fmt.Errorf("usage: yarctl fts rescan <user> [--folder NAME]")
	}
	path := "/api/backend/fts/rescan?user=" + url.QueryEscape(user)
	if *folder != "" {
		path += "&folder=" + url.QueryEscape(*folder)
	}
	data, err := backendAPIPost(path, nil)
	return printOutput(data, err, func(data []byte) error {
		var r struct {
			User    string   `json:"user"`
			Folders []string `json:"folders"`
		}
		if err := json.Unmarshal(data, &r); err != nil {
			return err
		}
		fmt.Printf("Rescanned %s: %d folder(s) [%s]\n",
			r.User, len(r.Folders), strings.Join(r.Folders, ", "))
		return nil
	})
}

// ftsOptimize compacts every index owned by the user.
// POST /api/backend/fts/optimize?user=
func ftsOptimize(args []string) error {
	fs := flag.NewFlagSet("fts optimize", flag.ContinueOnError)
	user, err := parseUserArg(fs, args)
	if err != nil {
		return err
	}
	if user == "" {
		return fmt.Errorf("usage: yarctl fts optimize <user>")
	}
	data, err := backendAPIPost("/api/backend/fts/optimize?user="+url.QueryEscape(user), nil)
	return printOutput(data, err, func(data []byte) error {
		fmt.Printf("Optimized indexes for %s\n", user)
		return nil
	})
}

// repeated collects a flag given more than once.
type repeated []string

func (r *repeated) String() string     { return strings.Join(*r, ",") }
func (r *repeated) Set(v string) error { *r = append(*r, v); return nil }

// ftsLookup asks the index with the query SEARCH would build.
// GET /api/backend/fts/lookup?user=&folder=&header=&body=&text=
func ftsLookup(args []string) error {
	fs := flag.NewFlagSet("fts lookup", flag.ContinueOnError)
	folder := fs.String("folder", "INBOX", "folder name")
	var headers, bodies, texts repeated
	fs.Var(&headers, "header", "NAME:VALUE, as SEARCH HEADER; repeatable")
	fs.Var(&bodies, "body", "text, as SEARCH BODY; repeatable")
	fs.Var(&texts, "text", "text, as SEARCH TEXT; repeatable")
	user, err := parseUserArg(fs, args)
	if err != nil {
		return err
	}
	if user == "" || len(headers)+len(bodies)+len(texts) == 0 {
		return fmt.Errorf("usage: yarctl fts lookup <user> [--folder NAME] [--header NAME:VALUE] [--body TEXT] [--text TEXT]")
	}
	v := url.Values{"user": {user}, "folder": {*folder}}
	for _, h := range headers {
		v.Add("header", h)
	}
	for _, b := range bodies {
		v.Add("body", b)
	}
	for _, t := range texts {
		v.Add("text", t)
	}
	data, err := backendAPIGet("/api/backend/fts/lookup?" + v.Encode())
	return printOutput(data, err, func(data []byte) error {
		var r struct {
			User   string `json:"user"`
			Folder string `json:"folder"`
			Terms  []struct {
				Field  string     `json:"field"`
				Header string     `json:"header"`
				Words  [][]string `json:"words"`
			} `json:"terms"`
			Impossible bool     `json:"impossible"`
			Definite   []uint32 `json:"definite"`
			Maybe      []uint32 `json:"maybe"`
		}
		if err := json.Unmarshal(data, &r); err != nil {
			return err
		}
		for _, t := range r.Terms {
			where := t.Field
			if t.Header != "" {
				where += " " + t.Header
			}
			fmt.Printf("term %s: %v\n", where, t.Words)
		}
		if r.Impossible {
			fmt.Printf("%s [%s]: a criterion is stopwords only; nothing can match, the index was not asked\n", r.User, r.Folder)
			return nil
		}
		fmt.Printf("%s [%s]: %d definite %v, %d to verify %v\n",
			r.User, r.Folder, len(r.Definite), r.Definite, len(r.Maybe), r.Maybe)
		return nil
	})
}
