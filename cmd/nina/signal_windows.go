//go:build windows

package main

import "os"

// notifyHUP is a no-op on Windows, which has no SIGHUP. Reloads there come from
// the file watcher.
func notifyHUP(_ chan<- os.Signal) {}
