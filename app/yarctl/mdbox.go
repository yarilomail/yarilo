package main

import (
	"flag"
	"fmt"
)

func dispatchMdbox(args []string) error {
	if len(args) == 0 {
		printMdboxUsage()
		return nil
	}
	switch args[0] {
	case "purge":
		return mdboxPurge(args[1:])
	case "altmove":
		return mdboxAltMove(args[1:])
	default:
		return fmt.Errorf("unknown mdbox command %q - available: purge, altmove", args[0])
	}
}

func printMdboxUsage() {
	fmt.Println(`yarctl backend mdbox <command>

Commands:
  purge <user> | --all | --user-file PATH [--jobs N] [--namespace NS]
        Compact the user's mdbox storage tree: every m.<N> with
        at least one zero-ref record is rewritten without those
        records (or unlinked entirely if all records are dead).
        A store other than mdbox is skipped.
        --all:       every user user/iterate lists, each on its backend.
        --user-file: the users a file lists, one per line, for a
                     userdb that does not iterate.
        --jobs:      purges at once (default 1).
        With --all or --user-file, a summary; the exit is non-zero
        and names the users whose purge failed.

  altmove <user> [--namespace NS] [--before RFC3339] [--reverse]
        Move messages to alt (cold) storage, then purge.
        --before:  only move messages with InternalDate before this
                   RFC3339 timestamp (e.g. 2025-01-01T00:00:00Z).
                   Omit to move all messages.
        --reverse: move FROM alt storage back to primary.
        Requires storage.mdbox_alt_storage_path to be configured.`)
}

func mdboxPurge(args []string) error {
	fs := flag.NewFlagSet("mdbox purge", flag.ContinueOnError)
	ns := fs.String("namespace", "personal", "namespace slug")
	all := fs.Bool("all", false, "every user user/iterate lists")
	userFile := fs.String("user-file", "", "the users a file lists, one per line")
	jobs := fs.Int("jobs", 1, "purges in flight at once with --all or --user-file")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	many := *all || *userFile != ""
	switch {
	case *all && *userFile != "":
		return fmt.Errorf("mdbox purge: --all and --user-file exclude each other")
	case many && fs.NArg() > 0:
		return fmt.Errorf("mdbox purge: a user name does not go with --all or --user-file")
	case !many && fs.NArg() < 1:
		return fmt.Errorf("usage: yarctl backend mdbox purge <user> | --all | --user-file PATH [--jobs N] [--namespace NS]")
	case *jobs < 1:
		return fmt.Errorf("mdbox purge: --jobs must be at least 1")
	}
	if !many {
		return printJSON(backendAPIPost("/api/backend/mdbox/purge", map[string]any{
			"user":      fs.Arg(0),
			"namespace": *ns,
		}))
	}
	var users []string
	var err error
	if *all {
		users, err = iterateUsers()
	} else {
		users, err = readUserFile(*userFile)
	}
	if err != nil {
		return err
	}
	return purgeMany(users, *ns, *jobs)
}

func mdboxAltMove(args []string) error {
	fs := flag.NewFlagSet("mdbox altmove", flag.ContinueOnError)
	ns := fs.String("namespace", "personal", "namespace slug")
	before := fs.String("before", "", "RFC3339 cutoff - move messages with InternalDate before this timestamp")
	reverse := fs.Bool("reverse", false, "move FROM alt storage back to primary")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return fmt.Errorf("usage: yarctl backend mdbox altmove <user> [--namespace NS] [--before RFC3339] [--reverse]")
	}
	return printJSON(backendAPIPost("/api/backend/mdbox/altmove", map[string]any{
		"user":      fs.Arg(0),
		"namespace": *ns,
		"before":    *before,
		"reverse":   *reverse,
	}))
}
