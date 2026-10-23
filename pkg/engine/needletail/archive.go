package needletail

import (
	"time"

	"github.com/camfinc/stormo/internal/needletail/memory"
	"github.com/camfinc/stormo/pkg/engine"
)

// ArchivedMemories are the records of the runtime's archive (memories/RECORDS.md, in the nap as
// raw), hot or not, each with its target: rehydrate names the rejected ones in its memory merge
// report, so the runtime deletes them even when the boot that purged them from the hot files never
// ran it (engine.MemoryArchive). Read only: the file is restored verbatim, never rewritten here.
func (n *Needletail) ArchivedMemories(read func(path string) []byte) []engine.ArchivedMemory {
	body := read(memory.RecordsFile)
	if len(body) == 0 {
		return nil
	}
	recs := memory.ParseRecords(string(body), time.Now()).Recs
	out := make([]engine.ArchivedMemory, 0, len(recs))
	for _, r := range recs {
		out = append(out, engine.ArchivedMemory{Target: r.Target(), Text: r.Text})
	}
	return out
}

var _ engine.MemoryArchive = (*Needletail)(nil)
