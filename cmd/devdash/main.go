// Command devdash shows what is running on this machine, grouped by git repository.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/dogauzun/devdash/internal/collector"
	"github.com/dogauzun/devdash/internal/engine"
	"github.com/dogauzun/devdash/internal/model"
)

// Set at build time with -ldflags "-X main.version=... -X main.commit=... -X main.date=...".
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

const usage = `usage: devdash [flags]              the dashboard (not implemented yet)
       devdash [flags] --json       print one snapshot as JSON (docs/json-schema.md)
       devdash [flags] port N       who listens on TCP port N: exit 0 found, 1 free
       devdash [flags] kill N       stop the process(es) listening on TCP port N
       devdash version              print version, commit and build date

Flags may come before or after the subcommand and its arguments.

  --roots paths    only count git repositories under these existing directories;
                   comma-separated, repeatable (--roots ~/code,~/work or --roots ~/code
                   --roots ~/work); a leading ~ is $HOME, ~user is not supported
  --tick d         refresh interval of the dashboard (default 2s, minimum 500ms)
  --all            show shells and editors (--json always lists every process)
  --no-docker      do not ask Docker for containers
  --no-color       no colour; also when NO_COLOR is set and not empty
  --json           print one snapshot as JSON on stdout
  -h, --help       print this help

kill prints every pid it will signal, then asks for confirmation on the terminal:
  --tree           also the owner's descendants, parent first, and its process group
                   when it leads one
  --force          SIGKILL instead of SIGTERM
  --yes            do not ask (also skips the second question for a process outside
                   every project); without a terminal, kill needs --yes
  --timeout d      how long to wait for the signalled processes to exit (default 3s)

Exit codes: 0 ok (port: found; kill: every signalled process exited, or nothing
listens on N), 1 port free, 2 usage error (kill: also no terminal to confirm on),
3 kill: permission denied (also an owner devdash cannot see; try sudo),
4 kill: survivors remain, 5 devdash failed (no snapshot could be taken, or output
could not be written; kill: only before anything was signalled), 6 kill: nothing
signalled (devdash refuses the target, or the confirmation was declined).
`

// exitFailed is the exit code of every command when devdash itself fails: the snapshot could
// not be taken or the output could not be written. 1 is "port free", 2 a usage error, and 3,
// 4 and 6 belong to kill.
const exitFailed = 5

// options is the parsed command line. The dashboard (DEV-26 and later) and `kill N` (DEV-25)
// read their settings from here.
type options struct {
	JSON     bool          // --json
	Roots    []string      // --roots: existing directories, absolute, ~ expanded; nil means every repository counts
	Tick     time.Duration // --tick, at least engine.MinTick
	All      bool          // --all: show shells and editors in human views; --json and port ignore it
	NoDocker bool          // --no-docker
	NoColor  bool          // --no-color, or NO_COLOR set and not empty
	Tree     bool          // kill --tree
	Force    bool          // kill --force
	Yes      bool          // kill --yes
	Timeout  time.Duration // kill --timeout, positive
	Cmd      string        // "", "port", "kill" or "version"
	Args     []string      // the subcommand's arguments, flags removed
}

// errUsage marks a usage error whose message was already printed.
var errUsage = errors.New("usage error")

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, collector.New()))
}

// run executes one command line and returns the exit code. c is only used by commands that
// take a snapshot.
func run(args []string, stdout, stderr io.Writer, c collector.Collector) int {
	o, err := parse(args, stderr)
	switch {
	case errors.Is(err, flag.ErrHelp):
		fmt.Fprint(stdout, usage)
		return 0
	case err != nil:
		if !errors.Is(err, errUsage) {
			fmt.Fprintln(stderr, "devdash:", err)
		}
		fmt.Fprint(stderr, usage)
		return 2
	}

	ctx := context.Background()
	switch o.Cmd {
	case "version":
		fmt.Fprintf(stdout, "devdash %s (commit %s, built %s)\n", version, commit, date)
		return 0
	case "port":
		port, _ := parsePort(o.Args[0]) // checked by parse
		return runPort(ctx, o.engine(c), port, stdout, stderr)
	case "kill":
		port, _ := parsePort(o.Args[0]) // checked by parse
		return runKill(ctx, o, o.engine(c), port, stdout, stderr)
	}
	if o.JSON {
		return runJSON(ctx, o.engine(c), stdout, stderr)
	}
	fmt.Fprintln(stderr, "devdash: the dashboard is not implemented yet; try --json")
	return 2
}

// parse reads flags anywhere on the command line, then checks the subcommand and its
// arguments. Flag errors are printed by the flag package and returned as errUsage; other
// usage errors are returned for the caller to print.
func parse(args []string, stderr io.Writer) (options, error) {
	o := options{Tick: engine.DefaultTick, Timeout: engine.DefaultKillTimeout}
	fs := flag.NewFlagSet("devdash", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {} // run prints the usage once, to the right stream
	fs.BoolVar(&o.JSON, "json", false, "")
	fs.Func("roots", "", func(s string) error {
		for _, r := range strings.Split(s, ",") {
			if r == "" {
				continue
			}
			abs, err := rootDir(r)
			if err != nil {
				return err
			}
			o.Roots = append(o.Roots, abs)
		}
		return nil
	})
	fs.DurationVar(&o.Tick, "tick", engine.DefaultTick, "")
	fs.BoolVar(&o.All, "all", false, "")
	fs.BoolVar(&o.NoDocker, "no-docker", false, "")
	fs.BoolVar(&o.NoColor, "no-color", false, "")
	fs.BoolVar(&o.Tree, "tree", false, "")
	fs.BoolVar(&o.Force, "force", false, "")
	fs.BoolVar(&o.Yes, "yes", false, "")
	fs.DurationVar(&o.Timeout, "timeout", engine.DefaultKillTimeout, "")

	// The flag package stops at the first non-flag argument; resume after each one so flags
	// may follow the subcommand. No subcommand takes an argument starting with "-".
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return o, err
			}
			return o, errUsage
		}
		if fs.NArg() == 0 {
			break
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
	o.NoColor = o.NoColor || os.Getenv("NO_COLOR") != ""
	if o.Tick < engine.MinTick {
		return o, fmt.Errorf("--tick %v is below the minimum of %v", o.Tick, engine.MinTick)
	}
	if len(pos) > 0 {
		o.Cmd, o.Args = pos[0], pos[1:]
	}

	switch o.Cmd {
	case "":
	case "version":
		if len(o.Args) > 0 {
			return o, fmt.Errorf("version takes no arguments")
		}
	case "port":
		if len(o.Args) != 1 {
			return o, fmt.Errorf("port takes one port number")
		}
		if _, err := parsePort(o.Args[0]); err != nil {
			return o, err
		}
	case "kill":
		if len(o.Args) != 1 {
			return o, fmt.Errorf("kill takes one port number")
		}
		if _, err := parsePort(o.Args[0]); err != nil {
			return o, err
		}
		if o.Timeout <= 0 {
			return o, fmt.Errorf("--timeout %v is not positive", o.Timeout)
		}
	default:
		return o, fmt.Errorf("unknown command %q", o.Cmd)
	}
	if o.JSON && o.Cmd != "" {
		return o, fmt.Errorf("--json takes no command, got %q", o.Cmd)
	}
	var killOnly error
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "tree", "force", "yes", "timeout":
			if o.Cmd != "kill" && killOnly == nil {
				killOnly = fmt.Errorf("--%s only applies to kill", f.Name)
			}
		}
	})
	return o, killOnly
}

// rootDir turns one --roots entry into an absolute directory. A leading "~" or "~/" is $HOME,
// because a shell expands "~" only at the start of a word (not after a comma or "=").
// "~user" is refused, and so is a path that is not an existing directory.
func rootDir(r string) (string, error) {
	if r == "~" || strings.HasPrefix(r, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		r = home + r[1:]
	} else if strings.HasPrefix(r, "~") {
		return "", fmt.Errorf("%s: ~user is not supported, use the full path", r)
	}
	abs, err := filepath.Abs(r)
	if err != nil {
		return "", err
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return "", err
	}
	if !fi.IsDir() {
		return "", fmt.Errorf("%s is not a directory", abs)
	}
	return abs, nil
}

// parsePort accepts a TCP port, 1 to 65535.
func parsePort(s string) (uint16, error) {
	n, err := strconv.ParseUint(s, 10, 16)
	if err != nil || n == 0 {
		return 0, fmt.Errorf("%q is not a port number (1-65535)", s)
	}
	return uint16(n), nil
}

// engine returns the engine options for these flags. The Docker and colour settings are not
// engine options; their readers take them from o.
func (o options) engine(c collector.Collector) engine.Options {
	home, _ := os.UserHomeDir()
	return engine.Options{Collector: c, Resolver: model.NewResolver(home, o.Roots), Tick: o.Tick}
}
