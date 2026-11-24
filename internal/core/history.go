package core

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/geyervalmont/olsyn-drive-client/internal/agent"
)

// History is kept across restarts in the account's state folder. It holds
// drive paths and names (never content) and lives beside the journal, which
// holds the same.

func (f *FS) loadHistory(d accountDirs) {
	b, err := os.ReadFile(filepath.Join(d.state, "history.json"))
	if err != nil {
		f.hist.replace(nil)
		return
	}
	var items []agent.Transfer
	if err := json.Unmarshal(b, &items); err != nil {
		f.log.Warn("core: transfer history unreadable; starting empty", "err", err)
		items = nil
	}
	f.hist.replace(items)
}

func (f *FS) saveHistory(d accountDirs) {
	b, err := json.Marshal(f.hist.snapshot())
	if err != nil {
		return
	}
	tmp := filepath.Join(d.state, "history.json.tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		f.log.Warn("core: save transfer history", "err", err)
		return
	}
	if err := os.Rename(tmp, filepath.Join(d.state, "history.json")); err != nil {
		f.log.Warn("core: save transfer history", "err", err)
	}
}
