package main

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"
)

func Watch(ctx context.Context, cfg Config, runner Runner) error {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer watcher.Close()

	for _, root := range cfg.ClaudeRoots {
		if err := addRecursiveWatches(watcher, root); err != nil {
			log.Printf("watch add failed: path=%s err=%v", root, err)
		}
	}

	scanNow := make(chan struct{}, 1)
	trigger := func() {
		select {
		case scanNow <- struct{}{}:
		default:
		}
	}

	ticker := time.NewTicker(cfg.SweepInterval)
	defer ticker.Stop()

	var debounce <-chan time.Time
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case event := <-watcher.Events:
			if event.Has(fsnotify.Create) {
				if info, err := os.Stat(event.Name); err == nil && info.IsDir() {
					if err := addRecursiveWatches(watcher, event.Name); err != nil {
						log.Printf("watch add failed: path=%s err=%v", event.Name, err)
					}
				}
			}
			if event.Has(fsnotify.Create) || event.Has(fsnotify.Write) || event.Has(fsnotify.Rename) {
				debounce = time.After(cfg.Debounce)
			}
		case err := <-watcher.Errors:
			log.Printf("watch error: %v", err)
		case <-debounce:
			debounce = nil
			trigger()
		case <-ticker.C:
			trigger()
		case <-scanNow:
			stats, err := runner.ScanOnce(ctx)
			if err != nil {
				if IsFatalAuthError(err) {
					return err
				}
				log.Printf("scan failed: %v", err)
				continue
			}
			log.Printf("scan complete: sessions=%d messages=%d posted=%d skipped_noop=%d parse_failures=%d",
				stats.Sessions, stats.Messages, stats.WouldPost, stats.SkippedNoOp, stats.ParseFailures)
		}
	}
}

func addRecursiveWatches(watcher *fsnotify.Watcher, root string) error {
	return filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if err := watcher.Add(path); err != nil {
				log.Printf("watch add skipped: path=%s err=%v", path, err)
			}
		}
		return nil
	})
}
