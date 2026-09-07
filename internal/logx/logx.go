// Package logx is the harness's own logger. vdk writes its debug output
// through Go's global logger, which the probe captures; everything the
// harness says goes through this one instead so the two never meet.
package logx

import (
	"log"
	"os"
)

// L writes to stderr with microsecond timestamps.
var L = log.New(os.Stderr, "", log.Ltime|log.Lmicroseconds)

// Printf logs one line.
func Printf(format string, a ...any) { L.Printf(format, a...) }

// Fatalf logs and exits.
func Fatalf(format string, a ...any) { L.Fatalf(format, a...) }
