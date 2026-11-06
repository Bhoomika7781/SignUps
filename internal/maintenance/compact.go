package maintenance

import (
	"errors"
	"os"
	"sort"

	"invx/internal/database"
	"invx/internal/storage"
)

// Compact creates a new database file containing
// only the latest values.
func Compact(db *database.DB, path string) error {
	if db == nil {
		return errors.New("database is nil")
	}

	tempPath := path + ".compact"

	tempLog, err := storage.OpenLog(tempPath)
	if err != nil {
		return err
	}

	keys := db.Keys()
	sort.Strings(keys)

	for _, key := range keys {
		value, found, err := db.Get(key)
		if err != nil {
			tempLog.Close()
			os.Remove(tempPath)
			return err
		}

		if !found {
			continue
		}

		record, err := storage.EncodeRecord(storage.RecordPut, key, value)
		if err != nil {
			tempLog.Close()
			os.Remove(tempPath)
			return err
		}

		if _, err := tempLog.Append(record); err != nil {
			tempLog.Close()
			os.Remove(tempPath)
			return err
		}
	}

	if err := tempLog.Close(); err != nil {
		os.Remove(tempPath)
		return err
	}

	// Replace the original database file.
	if err := db.Close(); err != nil {
		os.Remove(tempPath)
		return err
	}

	if err := os.Rename(tempPath, path); err != nil {
		return err
	}

	return nil
}
