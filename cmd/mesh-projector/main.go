/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

// mesh-projector is the operator's node-local half of zetta-tunnel's mesh. It runs beside the
// node agent in this node's mesh DaemonSet, reads this node's template from this node's own mounted
// Secret (written by the operator; no other node's is mounted), finds each enrolled pod's network namespace under the host's /proc,
// and publishes the agent's projection into the directory the agent reads. It has no credential
// and makes no API call.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/oramasearch/orama-operator/internal/meshprojection"
)

func main() {
	node := flag.String("node", os.Getenv("NODE_NAME"), "this node's name")
	templates := flag.String("templates", "/etc/zetta-mesh-templates", "where this node's template Secret is mounted, in the directory <templates>/<shard of the node name>")
	proc := flag.String("proc", "/host/proc", "the host's /proc")
	output := flag.String("output", "/run/zetta-mesh", "the directory the node agent reads")
	interval := flag.Duration("interval", time.Second, "how often the template and /proc are read")
	flag.Parse()
	if *node == "" {
		fail("--node (or NODE_NAME) is required")
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	shard := filepath.Join(*templates, strconv.Itoa(meshprojection.Shard(*node)))
	ticker := time.NewTicker(*interval)
	defer ticker.Stop()
	for {
		if err := once(shard, *node, *proc, *output); err != nil {
			// The last projection stays: the agent keeps enforcing it and capture stays installed.
			fmt.Fprintln(os.Stderr, "mesh-projector: "+err.Error())
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func once(shard, node, proc, output string) error {
	template, files, err := meshprojection.ReadShard(shard, node)
	if err != nil {
		return fmt.Errorf("read the template: %w", err)
	}
	resolve, err := meshprojection.ProcResolver(proc)
	if err != nil {
		return fmt.Errorf("read %s: %w", proc, err)
	}
	_, err = meshprojection.Write(output, meshprojection.Render(template, resolve), files)
	return err
}

func fail(message string) {
	fmt.Fprintln(os.Stderr, "mesh-projector: "+message)
	os.Exit(2)
}
