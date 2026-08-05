package index

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"
)

// watchExts limits reindex triggers to source files we actually parse, so
// editor swap files, logs and build artifacts don't cause churn. Keyed by the
// final extension (filepath.Ext).
var watchExts = map[string]bool{
	".go": true, ".ts": true, ".tsx": true, ".js": true, ".jsx": true,
	".py": true, ".rs": true, ".java": true, ".rb": true,
	".c": true, ".h": true, ".cpp": true, ".cc": true, ".hpp": true,
}

// Watch monitors root for source-file changes and calls reindex, debounced by
// `debounce`. reindex is expected to be an incremental pass (hash-skip), so
// re-running it on any change re-embeds only what actually changed. Calls are
// single-flighted: changes arriving while a reindex runs set a dirty flag that
// schedules exactly one more pass afterward. Watch blocks until ctx is done.
func Watch(ctx context.Context, root string, reindex func(context.Context) error, log *slog.Logger, debounce time.Duration) error {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer w.Close()

	addTree := func(base string) {
		_ = filepath.WalkDir(base, func(p string, d os.DirEntry, err error) error {
			if err != nil || !d.IsDir() {
				return nil
			}
			if p != base && SkipDir(d.Name()) {
				return filepath.SkipDir
			}
			if err := w.Add(p); err != nil {
				log.Warn("watch add failed", "dir", p, "err", err)
			}
			return nil
		})
	}
	addTree(root)

	var timerC <-chan time.Time
	var timer *time.Timer
	arm := func() {
		if timer != nil {
			timer.Stop()
		}
		timer = time.NewTimer(debounce)
		timerC = timer.C
	}

	running, dirty := false, false
	done := make(chan error, 1)
	start := func() {
		running, dirty = true, false
		go func() { done <- reindex(ctx) }()
	}

	log.Info("watch started", "root", root, "debounce", debounce.String())
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()

		case ev, ok := <-w.Events:
			if !ok {
				return nil
			}
			// A newly created directory must be added to the watch set.
			if ev.Op&fsnotify.Create != 0 {
				if fi, statErr := os.Stat(ev.Name); statErr == nil && fi.IsDir() {
					if !SkipDir(filepath.Base(ev.Name)) {
						addTree(ev.Name)
					}
					continue
				}
			}
			if watchExts[filepath.Ext(ev.Name)] {
				arm()
			}

		case err, ok := <-w.Errors:
			if !ok {
				return nil
			}
			log.Warn("watch error", "err", err)

		case <-timerC:
			timerC = nil
			if running {
				dirty = true // coalesce into one pass after the current finishes
			} else {
				start()
			}

		case err := <-done:
			running = false
			if err != nil && ctx.Err() == nil {
				log.Warn("reindex failed", "err", err)
			}
			if dirty {
				start()
			}
		}
	}
}
