package audio

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// whisperMu and a lock file let one whisper run at a time, in this tego
// and across all of them: each loads its model (over a GB for mlx), and
// parallel reads in parallel tego -p runs filled an 8 GB Mac.
var whisperMu sync.Mutex

func lockWhisper(ctx context.Context) (unlock func(), err error) {
	locked := make(chan struct{})
	go func() { whisperMu.Lock(); close(locked) }()
	select {
	case <-locked:
	case <-ctx.Done():
		go func() { <-locked; whisperMu.Unlock() }()
		return nil, ctx.Err()
	}
	dir := CacheDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return whisperMu.Unlock, nil // no lock file: still one per process
	}
	f, err := os.OpenFile(filepath.Join(dir, ".whisper.lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return whisperMu.Unlock, nil
	}
	for {
		if syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) == nil {
			return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close(); whisperMu.Unlock() }, nil
		}
		select {
		case <-ctx.Done():
			f.Close()
			whisperMu.Unlock()
			return nil, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}
