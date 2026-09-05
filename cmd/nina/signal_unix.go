//go:build !windows

package main

import (
	"os"
	"os/signal"
	"syscall"
)

// notifyHUP arranges for SIGHUP to trigger a reload.
func notifyHUP(ch chan<- os.Signal) {
	signal.Notify(ch, syscall.SIGHUP)
}
