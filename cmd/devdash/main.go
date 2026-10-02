// Command devdash shows what is running on this machine, grouped by git repository.
package main

import (
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"

	"github.com/dogauzun/devdash/internal/collector"
	"github.com/dogauzun/devdash/internal/docker"
	"github.com/dogauzun/devdash/internal/engine"
	"github.com/dogauzun/devdash/internal/model"
	"github.com/dogauzun/devdash/internal/tui"
)

// Set at build time with -ldflags "-X main.version=... -X main.commit=... -X main.date=...";
// any left at its default falls back to the embedded build info (version.go).
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

const usage = `usage: devdash [flags]              the dashboard (? inside it lists the keys)
       devdash [flags] --json       print one snapshot as JSON (docs/json-schema.md)
       devdash [flags] port N       who listens on TCP port N: exit 0 found, 1 free
       devdash [flags] kill N       stop the process(es) listening on TCP port N
       devdash version              print version, commit and commit date

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

// options is the parsed command line. The dashboard and `kill N`
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

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, collector.New()))
}

// run executes one command line and returns the exit code. c is only used by commands that
// take a snapshot.
func run(args []string, stdout, stderr io.Writer, c collector.Collector) int {
	o, err := parse(args)
	switch {
	case errors.Is(err, flag.ErrHelp):
		fmt.Fprint(stdout, usage)
		return 0
	case err != nil:
		fmt.Fprintln(stderr, "devdash:", err)
		fmt.Fprint(stderr, usage)
		return 2
	}

	ctx := context.Background()
	switch o.Cmd {
	case "version":
		ver, rev, built := versionInfo(version, commit, date, buildInfo())
		fmt.Fprintf(stdout, "devdash %s (commit %s, built %s)\n", ver, rev, built)
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
	return runTUI(ctx, o, c, stderr)
}

// runTUI runs the dashboard until the user quits. The engine's refresh loop runs alongside it
// and stops with it.
func runTUI(ctx context.Context, o options, c collector.Collector, stderr io.Writer) int {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	eo := o.dashboard(c)
	e := engine.New(eo)
	done := make(chan struct{})
	go func() { defer close(done); e.Run(ctx) }()
	defer func() { cancel(); <-done }()

	var opts []tea.ProgramOption
	if o.NoColor {
		opts = append(opts, tea.WithColorProfile(colorprofile.Ascii)) // keeps reverse and faint, drops colour
	}
	if err := tui.Run(ctx, tuiOptions(o, e, eo.Docker), opts...); err != nil {
		fmt.Fprintln(stderr, "devdash:", err)
		return exitFailed
	}
	return 0
}

// tuiOptions is the dashboard's options for these flags on engine e, whose container source is
// src (the engine options' Docker). The detail pane names the endpoint src asks at the time it
// is drawn, so it follows a rediscovering Source; nothing without one (--no-docker, none found
// yet, or endpointError, whose warning already says why).
func tuiOptions(o options, e *engine.Engine, src engine.ContainerSource) tui.Options {
	to := tui.Options{Source: e, Kill: e.Kill, ShowAll: o.All}
	if s, ok := src.(*docker.Source); ok {
		to.DockerSocket = func() string { return dockerLabel(s.Endpoint()) }
	}
	return to
}

// dockerLabel names endpoint ep as a user would recognise it: the socket path for unix, the
// tcp://host:port URL for tcp, then how it was found in parentheses unless it is a default
// path; "" for the zero Endpoint (none found yet).
func dockerLabel(ep docker.Endpoint) string {
	if ep == (docker.Endpoint{}) {
		return ""
	}
	s := ep.Address
	if ep.Network != "unix" {
		s = ep.String()
	}
	if ep.Source != "" && ep.Source != "default" {
		s += " (" + ep.Source + ")"
	}
	return s
}

// parse reads flags anywhere on the command line, then checks the subcommand and its
// arguments. Every usage error is returned for the caller to print after "devdash: ", the flag
// package's own included: it prints nothing, and its errors name the flag with two dashes, as
// the usage does (checkedValue, flagError).
func parse(args []string) (options, error) {
	o := options{Tick: engine.DefaultTick, Timeout: engine.DefaultKillTimeout}
	fs := flag.NewFlagSet("devdash", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {} // run prints the usage once, to the right stream
	boolVar := func(p *bool, name string) {
		fs.BoolFunc(name, "", func(s string) error {
			b, err := strconv.ParseBool(s)
			if err != nil {
				return errors.New("not true or false")
			}
			*p = b
			return nil
		})
	}
	durationVar := func(p *time.Duration, name string) {
		fs.Func(name, "", func(s string) error {
			d, err := time.ParseDuration(s)
			if err != nil {
				return errors.New("not a duration")
			}
			*p = d
			return nil
		})
	}
	boolVar(&o.JSON, "json")
	fs.Func("roots", "", func(s string) error {
		for _, r := range strings.Split(s, ",") {
			if r == "" {
				continue
			}
			abs, err := rootDir(r)
			if err != nil {
				return badEntry{r, err}
			}
			o.Roots = append(o.Roots, abs)
		}
		return nil
	})
	durationVar(&o.Tick, "tick")
	boolVar(&o.All, "all")
	boolVar(&o.NoDocker, "no-docker")
	boolVar(&o.NoColor, "no-color")
	boolVar(&o.Tree, "tree")
	boolVar(&o.Force, "force")
	boolVar(&o.Yes, "yes")
	durationVar(&o.Timeout, "timeout")
	var bad error // the value a flag refused, as "--name value: reason"
	fs.VisitAll(func(f *flag.Flag) { f.Value = checkedValue{f.Value, f.Name, &bad} })

	// The flag package stops at the first non-flag argument; resume after each one so flags
	// may follow the subcommand. No subcommand takes an argument starting with "-".
	var pos []string
	for rest := args; ; {
		if err := fs.Parse(rest); err != nil {
			return o, flagError(err, bad, slices.Contains(args, "kill"))
		}
		if fs.NArg() == 0 {
			break
		}
		pos = append(pos, fs.Arg(0))
		rest = fs.Args()[1:]
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
// "~user" is refused, and so is a path that is not an existing directory. The error says why
// without naming r; the caller names it.
func rootDir(r string) (string, error) {
	if r == "~" || strings.HasPrefix(r, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		r = home + r[1:]
	} else if strings.HasPrefix(r, "~") {
		return "", errors.New("~user is not supported, use the full path")
	}
	abs, err := filepath.Abs(r)
	if err != nil {
		return "", err
	}
	fi, err := os.Stat(abs)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		if pe, ok := errors.AsType[*fs.PathError](err); ok {
			return "", pe.Err // "permission denied", without "stat" and the path
		}
		return "", err
	}
	if err != nil || !fi.IsDir() {
		return "", errors.New("not an existing directory")
	}
	return abs, nil
}

// badEntry is a --roots error naming the one comma-separated entry refused.
type badEntry struct {
	entry string
	err   error
}

func (e badEntry) Error() string { return e.entry + ": " + e.err.Error() }

// checkedValue wraps a flag's Value so that a value it refuses is recorded in *bad as
// "--name value: reason" ("--name=value" for a boolean flag, which takes a value only after
// "="; for --roots the refused entry, not the whole list), in place of the flag package's
// `invalid value "x" for flag -name: reason`.
type checkedValue struct {
	flag.Value
	name string
	bad  *error
}

// IsBoolFlag keeps a boolean flag one that takes no separate argument.
func (v checkedValue) IsBoolFlag() bool {
	b, ok := v.Value.(interface{ IsBoolFlag() bool })
	return ok && b.IsBoolFlag()
}

func (v checkedValue) Set(s string) error {
	err := v.Value.Set(s)
	if err == nil {
		return nil
	}
	sep := " "
	if v.IsBoolFlag() {
		sep = "="
	}
	if e, ok := errors.AsType[badEntry](err); ok {
		s, err = e.entry, e.err
	}
	if s == "" || strings.ContainsAny(s, " \t\n") {
		s = strconv.Quote(s)
	}
	*v.bad = fmt.Errorf("--%s%s%s: %w", v.name, sep, s, err)
	return err
}

// flagError is the usage error for err from FlagSet.Parse: bad, the value a flag refused, when
// set; an unknown flag or one missing its value, named with two dashes as the usage names
// them; flag.ErrHelp and anything else ("bad flag syntax: ---x") as it is. An unknown flag
// that is all digits keeps one dash, as typed (`port -1`, `kill -9`), and with kill on the
// command line, -9 adds a hint at --force.
func flagError(err, bad error, kill bool) error {
	if bad != nil {
		return bad
	}
	msg := err.Error()
	if name, ok := strings.CutPrefix(msg, "flag provided but not defined: -"); ok {
		if strings.Trim(name, "0123456789") != "" {
			return fmt.Errorf("unknown flag --%s", name)
		}
		if kill && name == "9" {
			return errors.New("unknown flag -9 (use --force to send SIGKILL)")
		}
		return fmt.Errorf("unknown flag -%s", name)
	}
	if name, ok := strings.CutPrefix(msg, "flag needs an argument: -"); ok {
		return fmt.Errorf("--%s needs a value", name)
	}
	return err
}

// parsePort accepts a TCP port, 1 to 65535.
func parsePort(s string) (uint16, error) {
	n, err := strconv.ParseUint(s, 10, 16)
	if err != nil || n == 0 {
		return 0, fmt.Errorf("%q is not a port number (1-65535)", s)
	}
	return uint16(n), nil
}

// engine returns the engine options for these flags, for the one-shot commands (port, kill,
// --json). Unless --no-docker, it looks for a Docker endpoint once, retried after a failure
// on the first 5 s Docker beat once 10 ticks of --tick have passed (a zero Tick, as in tests,
// is the default tick); the colour setting is not an engine option, its readers take it from o.
func (o options) engine(c collector.Collector) engine.Options { return o.engineOptions(c, false) }

// dashboard is engine for the dashboard, which runs until the user quits: when no endpoint is
// found at start, its source looks again on the same cadence, so Docker started later shows up.
func (o options) dashboard(c collector.Collector) engine.Options { return o.engineOptions(c, true) }

func (o options) engineOptions(c collector.Collector, rediscover bool) engine.Options {
	home, _ := os.UserHomeDir()
	eo := engine.Options{Collector: c, Resolver: model.NewResolver(home, o.Roots), Tick: o.Tick}
	if !o.NoDocker {
		eo.Docker = dockerSource(home, cmp.Or(o.Tick, engine.DefaultTick), rediscover)
	}
	return eo
}

// discover finds the Docker endpoint; tests replace it.
var discover = docker.Discover

// dockerSource is the container source for this machine: a docker.Source when an endpoint is
// found, and endpointError when DOCKER_HOST or the docker context names one devdash cannot
// use. When none is found (no containers, no warning) it is nil, or with rediscover a
// docker.NewDiscoverySource that runs discovery again until it finds one. tick is the refresh
// interval, which with engine.DockerTick sets the Source's retry cadence.
func dockerSource(home string, tick time.Duration, rediscover bool) engine.ContainerSource {
	env := docker.Env{Getenv: os.Getenv, Home: home}
	ep, ok, err := discover(env)
	switch {
	case err != nil:
		return endpointError{err}
	case !ok && rediscover:
		return docker.NewDiscoverySource(func() (docker.Endpoint, bool, error) { return discover(env) }, tick, engine.DockerTick)
	case !ok:
		return nil // not a typed nil: Options.Docker must compare equal to nil
	}
	return docker.NewSource(ep, tick, engine.DockerTick)
}

// endpointError is the source for an endpoint devdash cannot use: no containers, and one
// docker_endpoint_invalid warning in every snapshot, so the footer and --json say why.
type endpointError struct{ err error }

func (e endpointError) Fetch(context.Context) ([]model.Container, *model.Warning) {
	return nil, docker.InvalidEndpoint(e.err)
}
