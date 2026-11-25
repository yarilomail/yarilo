// Command helper stands in for a component in internal/master's tests. What
// it does is set by its environment:
//
//	HELPER_MODE    serve (default) | crash | ignore-term
//	HELPER_LISTEN  a unix socket to serve after HELPER_DELAY_MS
//	HELPER_STARTS  a file it appends "start <unix-nano>" to
//	HELPER_ENV     a file it writes its environment to
//	HELPER_SAY     a line it prints at start, then every HELPER_SAY_MS
package main

import (
	"fmt"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func main() {
	if f := os.Getenv("HELPER_STARTS"); f != "" {
		fh, err := os.OpenFile(f, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
		if err == nil {
			_, _ = fmt.Fprintf(fh, "start %d\n", time.Now().UnixNano())
			_ = fh.Close()
		}
	}
	if f := os.Getenv("HELPER_ENV"); f != "" {
		_ = os.WriteFile(f, []byte(strings.Join(os.Environ(), "\n")+"\n"), 0o600)
	}
	if say := os.Getenv("HELPER_SAY"); say != "" {
		fmt.Println(say)
		if ms, _ := strconv.Atoi(os.Getenv("HELPER_SAY_MS")); ms > 0 {
			go func() {
				for range time.Tick(time.Duration(ms) * time.Millisecond) {
					fmt.Println(say)
				}
			}()
		}
	}
	switch os.Getenv("HELPER_MODE") {
	case "crash":
		os.Exit(3)
	case "ignore-term":
		signal.Ignore(syscall.SIGTERM)
	}
	if sock := os.Getenv("HELPER_LISTEN"); sock != "" {
		ms, _ := strconv.Atoi(os.Getenv("HELPER_DELAY_MS"))
		time.Sleep(time.Duration(ms) * time.Millisecond)
		_ = os.Remove(sock)
		ln, err := net.Listen("unix", sock)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(4)
		}
		go func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				_ = c.Close()
			}
		}()
	}
	// Wait for SIGTERM, or forever when it is ignored.
	term := make(chan os.Signal, 1)
	if os.Getenv("HELPER_MODE") != "ignore-term" {
		signal.Notify(term, syscall.SIGTERM)
	}
	for {
		select {
		case <-term:
			os.Exit(0)
		case <-time.After(time.Hour):
		}
	}
}
