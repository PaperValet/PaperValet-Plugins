package main

// bridge_src.go — the bridge.mjs script, embedded so a single .so carries
// everything needed to bootstrap the runtime on first use.

import _ "embed"

//go:embed bridge.mjs
var bridgeSource string
