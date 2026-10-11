package classify

import (
	"os/exec"
	"sync"
	"time"
)

// studioAppPattern matches the main executables of the DAWs on this machine.
// While one of them runs, local model inference is skipped so it never competes
// with real-time audio for memory or GPU.
const studioAppPattern = `^/Applications/(Ableton Live[^/]*|Logic Pro[^/]*|MainStage[^/]*|Pro Tools)\.app/Contents/MacOS/`

const studioCheckTTL = 30 * time.Second

var studio struct {
	sync.Mutex
	busy      bool
	checkedAt time.Time
}

// StudioBusy reports whether a music app is running. The result is cached briefly
// so a batch of emails doesn't spawn pgrep for every message.
func StudioBusy() bool {
	studio.Lock()
	defer studio.Unlock()

	if time.Since(studio.checkedAt) < studioCheckTTL {
		return studio.busy
	}
	// pgrep exits 0 when at least one process matches
	studio.busy = exec.Command("pgrep", "-f", studioAppPattern).Run() == nil
	studio.checkedAt = time.Now()
	return studio.busy
}
