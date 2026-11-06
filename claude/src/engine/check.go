package engine

// check.go: read-only integrity check used by `kvdb check`.

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
)

// CheckReport is the result of Check.
type CheckReport struct {
	OK       bool
	Records  int
	Keys     int
	FileSize int64
	ValidEnd int64
	Problem  string
	TempFile bool // a leftover compaction temp file exists (open will delete it)
}

// Check scans the whole file without changing it. The file is OK when every
// byte belongs to a valid record (no torn tail, no bad checksum).
func Check(path string) (CheckReport, error) {
	var rep CheckReport
	f, err := os.Open(path)
	if err != nil {
		return rep, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return rep, err
	}
	keys := make(map[string]struct{})
	res, err := scanLog(f, st.Size(), func(op Op, _ int64) {
		if op.Type == OpPut {
			keys[string(op.Key)] = struct{}{}
		} else {
			delete(keys, string(op.Key))
		}
	})
	if err != nil {
		return rep, err
	}
	rep.Records = res.Records
	rep.Keys = len(keys)
	rep.FileSize = res.FileSize
	rep.ValidEnd = res.ValidEnd
	rep.OK = res.ValidEnd == res.FileSize
	if !rep.OK {
		rep.Problem = fmt.Sprintf("%s (%d bytes after offset %d would be discarded by open)",
			res.Reason, res.FileSize-res.ValidEnd, res.ValidEnd)
	}
	if _, err := os.Stat(path + TempSuffix); err == nil {
		rep.TempFile = true
	} else if !errors.Is(err, fs.ErrNotExist) {
		return rep, err
	}
	return rep, nil
}
