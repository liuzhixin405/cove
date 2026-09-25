package memory

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/liuzhixin405/cove/internal/fsatomic"
	"github.com/liuzhixin405/cove/internal/log"
)

// extractionRecordName holds the last automatic extraction's time and count.
// Hidden, so All() does not load it as a memory; on disk rather than in memory
// so a fresh process (cove doctor) can report it too.
const extractionRecordName = ".last-extraction.json"

type extractionRecord struct {
	At    time.Time `json:"at"`
	Count int       `json:"count"`
}

// RecordExtraction notes that an automatic extraction just finished and saved
// n memories (0 is a run that found nothing worth keeping). The extract runner
// writes memory files directly, so the entry cache is dropped as well.
func (s *Store) RecordExtraction(n int) {
	if len(s.dirs) > 0 {
		RecordExtractionIn(s.dirs[0], n)
	}
	s.invalidateCache()
}

// RecordExtractionIn is RecordExtraction for a caller that has only the
// memory directory. Failures are logged: the record is informational.
func RecordExtractionIn(dir string, n int) {
	data, err := json.Marshal(extractionRecord{At: time.Now(), Count: n})
	if err != nil {
		return
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		log.Debugf("[memory] record extraction: %v", err)
		return
	}
	if err := fsatomic.WriteFile(filepath.Join(dir, extractionRecordName), data, 0o600); err != nil {
		log.Debugf("[memory] record extraction: %v", err)
	}
}

// lastExtraction reads the newest extraction record across the store's dirs.
func (s *Store) lastExtraction() extractionRecord {
	var best extractionRecord
	for _, d := range s.dirs {
		data, err := os.ReadFile(filepath.Join(d, extractionRecordName))
		if err != nil {
			continue
		}
		var rec extractionRecord
		if json.Unmarshal(data, &rec) == nil && rec.At.After(best.At) {
			best = rec
		}
	}
	return best
}
