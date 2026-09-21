package runtime

import (
	"context"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/rivencove/nina/internal/specsrc"
)

// Watch triggers reloads from configuration file changes and upstream spec
// refreshes until ctx is cancelled.
func (s *Server) Watch(ctx context.Context) {
	go s.watchFiles(ctx)
	go s.pollSpecs(ctx)
}

// watchFiles reloads when the config file or a local spec file changes.
//
// Directories are watched rather than files: editors and deployment tooling
// almost always write to a temporary file and rename it over the target, which
// destroys the inode a file watch is attached to.
func (s *Server) watchFiles(ctx context.Context) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		s.log.Error("cannot watch for configuration changes; reload will need SIGHUP", "error", err)
		return
	}
	defer w.Close()

	watched := map[string]bool{}
	interesting := map[string]bool{}
	for _, f := range s.Current().Config.WatchFiles() {
		abs, err := filepath.Abs(f)
		if err != nil {
			continue
		}
		interesting[abs] = true
		dir := filepath.Dir(abs)
		if watched[dir] {
			continue
		}
		if err := w.Add(dir); err != nil {
			s.log.Warn("cannot watch directory", "dir", dir, "error", err)
			continue
		}
		watched[dir] = true
	}
	s.log.Info("watching for configuration changes", "directories", len(watched), "files", len(interesting))

	// Debounce: a single save often produces create, write and rename events in
	// quick succession, and each one must not cause its own rebuild.
	const debounce = 200 * time.Millisecond
	var timer *time.Timer
	var timerC <-chan time.Time

	for {
		select {
		case <-ctx.Done():
			return

		case ev, ok := <-w.Events:
			if !ok {
				return
			}
			if ev.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Rename|fsnotify.Remove) == 0 {
				continue
			}
			abs, err := filepath.Abs(ev.Name)
			if err != nil || !interesting[abs] {
				continue
			}
			if timer == nil {
				timer = time.NewTimer(debounce)
			} else {
				timer.Reset(debounce)
			}
			timerC = timer.C

		case <-timerC:
			timerC = nil
			if err := s.Reload(ctx, "file change"); err != nil {
				// Already logged with context by Reload; the old runtime stands.
				continue
			}

		case err, ok := <-w.Errors:
			if !ok {
				return
			}
			s.log.Warn("file watch error", "error", err)
		}
	}
}

// pollSpecs re-fetches upstream documents on their configured interval and
// reloads only when the content has actually changed.
//
// The hash comparison matters: without it, a five minute poll across twenty
// backends would mean a full runtime rebuild every five minutes forever.
func (s *Server) pollSpecs(ctx context.Context) {
	cfg := s.Current().Config
	for i := range cfg.Backends {
		b := &cfg.Backends[i]
		if b.Spec.Refresh <= 0 || b.Spec.URL == "" {
			continue
		}
		src := specsrc.Source{
			Backend: b.Name,
			URL:     b.Spec.URL,
			OnError: b.Spec.OnError,
		}
		go s.pollOne(ctx, src, b.Spec.Refresh.Std())
	}
}

func (s *Server) pollOne(ctx context.Context, src specsrc.Source, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			rt := s.Current()
			if rt == nil {
				continue
			}
			fctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			res, err := s.deps.Fetcher.Fetch(fctx, src)
			cancel()
			if err != nil {
				s.log.Warn("spec refresh failed", "backend", src.Backend, "error", err)
				continue
			}
			if res.Hash == rt.SpecHash(src.Backend) {
				continue // unchanged; no rebuild
			}
			s.log.Info("upstream spec changed; reloading", "backend", src.Backend)
			_ = s.Reload(ctx, "spec change: "+src.Backend)
		}
	}
}
