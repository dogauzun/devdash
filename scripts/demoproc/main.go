// Command demoproc plays a dev server, watcher, test runner or agent in the README demo
// (scripts/demo.sh, demo.tape; DEV-65). scripts/demo.sh links it under each name it plays
// (vite, node, nodemon, ...), so the process name and argv read like the real tool's.
//
// With DEMO_CHILD set ("node server.js") it starts that command as its child, without
// DEMO_CHILD, and waits for it, as a watcher does. Otherwise, with --port N or PORT=N, it
// serves HTTP on every interface at N. Otherwise it sleeps. SIGTERM and SIGINT end it at once,
// leaving a child running, as a watcher killed without --tree does.
package main

import (
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
)

func main() {
	signal.Ignore(syscall.SIGHUP)
	name := filepath.Base(os.Args[0])

	if child := strings.Fields(os.Getenv("DEMO_CHILD")); len(child) > 0 {
		if err := os.Unsetenv("DEMO_CHILD"); err != nil {
			fail(err)
		}
		cmd := exec.Command(child[0], child[1:]...)
		if err := cmd.Start(); err != nil {
			fail(err)
		}
		_ = cmd.Wait()
		sleep()
	}

	if port := portArg(); port != "" {
		h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprintf(w, "%s says hello\n", name)
		})
		fail(http.ListenAndServe(":"+port, h))
	}
	sleep()
}

// portArg is the value after --port, else $PORT.
func portArg() string {
	for i, a := range os.Args[1:] {
		if v, ok := strings.CutPrefix(a, "--port="); ok {
			return v
		}
		if a == "--port" && i+2 < len(os.Args) {
			return os.Args[i+2]
		}
	}
	return os.Getenv("PORT")
}

// sleep waits for SIGTERM or SIGINT and exits.
func sleep() {
	c := make(chan os.Signal, 1)
	signal.Notify(c, syscall.SIGTERM, syscall.SIGINT)
	<-c
	os.Exit(0)
}

func fail(err error) {
	fmt.Fprintf(os.Stderr, "%s: %v\n", filepath.Base(os.Args[0]), err)
	os.Exit(1)
}
