package main

import (
	"fmt"
	"os"
	"sync"
	"time"
)

var (
	debugMu   sync.Mutex
	debugFile *os.File
	debugInit sync.Once
	debugOn   = os.Getenv("ASK_DEBUG") != ""
)

func debugLog(format string, args ...any) {
	if !debugOn {
		return
	}
	debugInit.Do(func() {
		f, err := os.OpenFile("/tmp/ask.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err == nil {
			debugFile = f
		}
	})
	if debugFile == nil {
		return
	}
	debugMu.Lock()
	defer debugMu.Unlock()
	fmt.Fprintf(debugFile, "%s "+format+"\n", append([]any{time.Now().Format("15:04:05.000")}, args...)...)
}

func debugTrace(label string, start time.Time) {
	if !debugOn {
		return
	}
	debugLog("%s %dµs", label, time.Since(start).Microseconds())
}
