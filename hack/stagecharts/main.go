/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

// stagecharts stages the chart archives and CRD manifests helmfile.yaml installs, each checked
// against its SHA-256 pin in platform-charts.yaml, so helmfile installs only pinned bytes
// (orama-operator#808). Run it from the repository root before every helmfile apply:
//
//	go run ./hack/stagecharts && helmfile apply
//	go run ./hack/stagecharts -offline     # check what is staged; download nothing
//
// It exits non-zero, and helmfile does not run, when any file is not the pinned one.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/oramasearch/orama-operator/internal/platformcharts"
)

func main() {
	pinsFile := flag.String("pins", "platform-charts.yaml", "the pins file")
	dir := flag.String("dir", ".platform-charts", "where the files are staged; helmfile.yaml names this directory")
	offline := flag.Bool("offline", false, "check what is already staged and download nothing")
	flag.Parse()

	pins, err := platformcharts.Load(*pinsFile)
	if err == nil {
		err = platformcharts.Stage(context.Background(), pins, *dir, *offline, func(line string) { fmt.Println(line) })
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "stagecharts: "+err.Error())
		os.Exit(1)
	}
}
