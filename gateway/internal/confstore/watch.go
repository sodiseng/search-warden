package confstore

import (
	"context"
	"path/filepath"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
)

// Watch reloads after hand edits until ctx is done. fsnotify gives ~instant
// pickup; periodic hash polling covers missed events (e.g. bind mounts from
// Docker Desktop on Windows/macOS, which often do not deliver inotify events).
func (m *Manager) Watch(ctx context.Context) {
	var events <-chan fsnotify.Event
	var errs <-chan error
	if w, err := fsnotify.NewWatcher(); err != nil {
		m.log.Warn("file watcher unavailable; using polling only", "err", err)
	} else {
		defer w.Close()
		for _, dir := range []string{m.dir, filepath.Join(m.dir, "keys")} {
			if err := w.Add(dir); err != nil {
				m.log.Warn("cannot watch directory; using polling for it", "dir", dir, "err", err)
			}
		}
		events, errs = w.Events, w.Errors
	}

	poll := time.NewTicker(m.opts.PollInterval)
	defer poll.Stop()
	debounce := time.NewTimer(time.Hour)
	debounce.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-events:
			if !ok {
				events = nil
				continue
			}
			if m.relevant(ev.Name) {
				debounce.Reset(m.opts.Debounce) // editors often write a temp file and rename it
			}
		case err, ok := <-errs:
			if !ok {
				errs = nil
				continue
			}
			m.log.Warn("file watcher error", "err", err)
		case <-debounce.C:
			m.Reload()
		case <-poll.C:
			m.Reload()
		}
	}
}

func (m *Manager) relevant(path string) bool {
	rel, err := filepath.Rel(m.dir, path)
	if err != nil {
		return false
	}
	rel = filepath.ToSlash(rel)
	if strings.HasPrefix(filepath.Base(rel), ".") {
		return false // our own temp files
	}
	return isManaged(rel)
}
