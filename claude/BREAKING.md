# BREAKING.md: how we would break kvdb

Design: one append-only file of records `[magic "KVR1" 4B][plen 4B][crc 4B][payload]`, one record per put, delete or
batch, with the CRC-32C over `plen` and the payload. Write path: `WriteAt` → `File.Sync` → index update → return (then
`serve` prints `ACK`). Recovery scans from byte 0 and truncates at the first short or bad-CRC record.

## 1. Crash points

| # | Process dies at… | On disk | Next `open()` |
|---|---|---|---|
| 1 | `db.go Batch`, before `f.WriteAt` | Nothing new | Clean scan. The op never happened; no ACK. |
| 2 | inside `f.WriteAt` (torn write) | A prefix of the record: short header, or `plen` > bytes left | `scanLog` stops there; `Open` truncates to the record start and fsyncs. |
| 3 | after `WriteAt`, before or inside `f.Sync` | The full record (the kernel cache survives a process kill) | CRC passes, so it is replayed. Legal: no ACK was sent. |
| 4 | after `f.Sync`, before the index update or return | The full record, durable | Replayed (A4). |
| 5 | `main.go serve`, after `Batch` returns, before `ACK` is flushed | Durable record | Replayed. The oracle counts it as in flight. |
| 6 | `undoWriteLocked`, after a failed write, before `Truncate` | A partial record at the tail | As #2. |
| 7 | `Open`, inside `scanLog` | Unchanged (the scan only reads) | The same scan again (C1). |
| 8 | `Open`, between `f.Truncate` and `f.Sync` | Tail cut or not yet cut | Same `validEnd`, so the truncate is idempotent (C2). |
| 9 | `Open`, at `os.Remove(path+".compact")` | Temp file gone or still there | Removed again if present. |
| 10 | `compactLocked`, writing the temp file (before `tf.Sync`) | Old file intact; temp file partial | Temp file deleted; old file authoritative (C3). |
| 11 | after `tf.Sync`, before `os.Rename` | Two complete files | Temp file deleted. Work wasted, nothing lost. |
| 12 | inside `os.Rename` | `path` = old file or the new, already-fsynced file (atomic) | Opens whichever one; both hold every ACKed write (C4). |
| 13 | after `os.Rename`, before the reopen | `path` = new file; no temp file | Recovers from the new file (C5). |
| 14 | a delete-only batch at `max_bytes` (runs `compactLocked(drop)`) | As 10–13 | Keys present if killed before the rename, gone after. Never half. |
| 15 | `Close` | Nothing pending (every write was already fsynced) | Same as a kill at any time (C6). |

## 2. Ordering argument

**Writes:** `write(record at size)` → `fsync(file)` → index update → `ACK`. ACK follows the fsync, so ACKed bytes are
on disk. Records are appended at the end of the last valid record, so garbage can only ever sit *after* valid data. One
CRC covers the whole batch, and the index changes only after the fsync, so a batch is all or nothing.

**Compaction:** `write(tmp)` → `fsync(tmp)` → `close` both files → `rename(tmp, path)` → `fsync(dir)`. The temp file is
complete before the rename, and the rename is the single commit point.

**Directory fsync:** done after the rename and after creating a new file, on Linux and macOS, so the directory entry
survives power loss. A process kill does not need it: once `rename()` returns, the kernel's directory is updated. Go
cannot fsync a directory on Windows, so there we rely on NTFS metadata journaling (weakness 2).

## 3. Known weaknesses

1. **Recovery is O(file size).** `scanLog` reads every byte to check the CRCs. 1M small keys take 1.39 s on Linux and
   **<<___ s>>** on our laptop; a log of 1 MB values, or one never compacted, can pass 2 s.
2. **No directory fsync on Windows.** NTFS rename atomicity is assumed, not verified. Power loss on Windows is not
   argued.
3. **One bad record mid-file loses everything after it.** We cannot tell a torn tail from bit rot, so a flipped bit near
   the start would truncate later ACKed data.
4. **No cross-process lock.** Two writer processes on one file would interleave appends.
5. **Compaction holds the global lock** and needs space for a second copy; `--max-bytes` limits only the main file.
6. **One fsync per write, no group commit:** about 25–30 writes per second on our laptop (fsync ≈ 35 ms).

## 4. What failed, and what the torture tool found

- **Proving the oracle can fail:** we planted two bugs: ACK printed before `Batch`, and a batch written as single-op
  records. Both were caught in iteration 1 ("MISSING", "PARTIAL BATCH").
- **recovery-crash starved itself:** with a 46 MB preload, the first write after each restart started auto-compaction,
  which outlasted the 5–300 ms kill window. It was safe every time, but almost nothing else got tested, and it shows a
  real livelock: under constant kills the file never shrinks. We widened the window to 20–1500 ms and cut the preload.
- **Torn writes are rare on Windows:** `WriteAt` takes µs and `fsync` ~35 ms, so kills land in crash point 3 and mixed
  mode shows `torn=0`. We added `TestTornTailAtEveryByte` (cuts the file at every byte of the last record) and the
  large-values mode.
- **Found while writing the tool:** `cmd.StdoutPipe` is closed by `Wait()` and can drop the last ACKs. We use our own
  `os.Pipe` and read to EOF.
- Laptop results: **<<paste RESULT lines>>**

## 5. With 24 more hours

1. **Hint file** (`key → offset`, checksummed) written at compaction, so recovery replays only the log tail (fixes 1).
2. **Power-loss simulation and bit-rot handling:** drop unsynced writes in a test layer; on a CRC failure, look for a
   later valid record and refuse to silently truncate (fixes 3).
3. **Lock file and group commit:** `LockFileEx`/`flock` for a single writer process, and one fsync per group of
   concurrent writes (fixes 4 and 6).