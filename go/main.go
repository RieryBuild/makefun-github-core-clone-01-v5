// forge: a standalone GitHub-compatible git forge in one process.
//
// Smart-HTTP clone/push (git protocol v0/v2), REST v3, GraphQL v4 and MCP
// tools, all over an in-memory store. Zero dependencies, statically linked.
//
// The launcher (main.ts) spawns the binary with no port argument; the
// forge binds an ephemeral port (:0) and prints "[forge] listening on port
// N" once the socket is live, which the launcher reads as the port and the
// readiness signal. A port may still be passed explicitly (first argument,
// `--port N`, or FORGE_PORT) for local runs. MAKEFUN_SHOP_TOKEN (the Bearer
// token) and MAKEFUN_TRIAL_NOW (the fixed trial clock) come from the
// environment.

package main

import (
	"fmt"
	"net"
	"os"
	"strconv"
)

func main() {
	port := 0
	args := os.Args[1:]
	for i := 0; i < len(args); i++ {
		if args[i] == "--port" && i+1 < len(args) {
			i++
			if n, err := strconv.Atoi(args[i]); err == nil {
				port = n
			}
			continue
		}
		if n, err := strconv.Atoi(args[i]); err == nil {
			port = n
		}
	}
	if port == 0 {
		if raw := os.Getenv("FORGE_PORT"); raw != "" {
			if n, err := strconv.Atoi(raw); err == nil {
				port = n
			}
		}
	}

	forge := NewForge()
	dispatch := NewDispatcher(forge)

	ln, err := net.Listen("tcp", ":"+strconv.Itoa(port))
	if err != nil {
		fmt.Fprintf(os.Stderr, "[forge] listen: %v\n", err)
		os.Exit(1)
	}
	actual := ln.Addr().(*net.TCPAddr).Port
	fmt.Printf("[forge] listening on port %d\n", actual)
	Serve(ln, dispatch.Dispatch)
}
