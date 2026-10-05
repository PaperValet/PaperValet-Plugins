package main

import "time"

// timeNow and timeAfter are shims over the time package so tests can
// shorten the relay's waits by swapping them.
var (
	timeNow   = time.Now
	timeAfter = time.After
)
