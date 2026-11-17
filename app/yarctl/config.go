package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"reflect"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/yarilomail/yarilo/v2/pkg/build"
	"github.com/yarilomail/yarilo/v2/pkg/config"
)

// errConfigRefused is a check that failed; the reason is already printed.
var errConfigRefused = errors.New("configuration refused")

// configFile is where a server reads its configuration.
func configFile() string {
	if p := os.Getenv("CONFIG"); p != "" {
		return p
	}
	return "/etc/yarilo/yarilo.yaml"
}

// dispatchConfig is yarctl config: the configuration as a process receives
// it, read with the servers' own loader, without calling any service.
func dispatchConfig(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("config", flag.ContinueOnError)
	fs.SetOutput(stderr)
	path := fs.String("c", configFile(), "configuration file")
	all := fs.Bool("a", false, "every key with its effective value")
	defaults := fs.Bool("d", false, "the defaults; the file is not read")
	valuesOnly := fs.Bool("h", false, "values only, without key names")
	showSecrets := fs.Bool("P", false, "show secrets")
	var roles []string
	fs.Func("r", "check only this role's start checks (repeatable): "+strings.Join(config.Roles(), ", "), func(v string) error {
		roles = append(roles, v)
		return nil
	})
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: yarctl config [-c file] [-a|-d] [-h] [-P] [key...]\n       yarctl config [-c file] [-r role]... check")
		fs.PrintDefaults()
	}
	// Flags anywhere, before or after the keys, as elsewhere in yarctl.
	var rest []string
	for {
		if err := fs.Parse(args); err != nil {
			return err
		}
		args = fs.Args()
		if len(args) == 0 {
			break
		}
		rest = append(rest, args[0])
		args = args[1:]
	}

	// Before parsing: the version and the file are on screen even when the
	// file does not load.
	source := *path
	if *defaults {
		source = "defaults"
	}
	fmt.Fprintf(stdout, "# yarilo %s %s\n", build.Version, source)

	if len(rest) == 1 && rest[0] == "check" {
		if _, err := config.LoadChecked(*path, roles...); err != nil {
			fmt.Fprintln(stderr, err)
			return errConfigRefused
		}
		return nil
	}

	var cfg *config.Config
	if *defaults {
		cfg = config.Defaults()
	} else {
		var err error
		if cfg, err = config.Load(*path); err != nil {
			fmt.Fprintln(stderr, err)
			return errConfigRefused
		}
	}
	settings := visible(config.Walk(cfg, false))
	switch {
	case len(rest) > 0:
		var missing []string
		settings, missing = named(settings, rest)
		if len(missing) > 0 {
			fmt.Fprintf(stderr, "no such key: %s\n", strings.Join(missing, ", "))
			return errConfigRefused
		}
	case !*all && !*defaults:
		settings = differing(settings, visible(config.Walk(config.Defaults(), false)))
	}

	if *valuesOnly {
		for _, s := range settings {
			fmt.Fprintln(stdout, valueText(s, *showSecrets))
		}
		return nil
	}
	if len(settings) == 0 {
		return nil
	}
	enc := yaml.NewEncoder(stdout)
	enc.SetIndent(2)
	if err := enc.Encode(config.Tree(settings, *showSecrets)); err != nil {
		return err
	}
	return enc.Close()
}

// visible drops the deprecated spellings: the loader has copied them into
// their canonical keys, and the output must load back.
func visible(in []config.Setting) []config.Setting {
	out := in[:0:0]
	for _, s := range in {
		if !s.Alias {
			out = append(out, s)
		}
	}
	return out
}

// differing is what the file changes against the defaults.
func differing(in, defaults []config.Setting) []config.Setting {
	def := make(map[string]any, len(defaults))
	for _, s := range defaults {
		def[s.Key] = s.Value
	}
	var out []config.Setting
	for _, s := range in {
		if d, ok := def[s.Key]; !ok || !reflect.DeepEqual(d, s.Value) {
			out = append(out, s)
		}
	}
	return out
}

// named keeps the keys asked for, in the order asked: a key itself, or every
// key of a section; missing are the names that matched nothing.
func named(in []config.Setting, keys []string) (out []config.Setting, missing []string) {
	for _, k := range keys {
		found := false
		for _, s := range in {
			if s.Key == k || strings.HasPrefix(s.Key, k+".") || strings.HasPrefix(s.Key, k+"[") {
				out = append(out, s)
				found = true
			}
		}
		if !found {
			missing = append(missing, k)
		}
	}
	return out, missing
}

func valueText(s config.Setting, showSecrets bool) string {
	if s.Secret && !showSecrets && !reflect.ValueOf(s.Value).IsZero() {
		return config.Hidden
	}
	switch v := s.Value.(type) {
	case string:
		return v
	case []string:
		return strings.Join(v, ",")
	}
	b, err := yaml.Marshal(s.Value)
	if err != nil {
		return fmt.Sprint(s.Value)
	}
	return strings.TrimSpace(string(b))
}
