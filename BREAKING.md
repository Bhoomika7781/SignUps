# How we would break this

## 1. Crash points

"Open" means what the next `open()` does: `Open` → `resolveCompactionTemp` → `OpenLog` → `recover` (`ScanFrames` + `Truncate`).

| # | Crash point (file: function, call) | What is on disk | What the next open() does |
|---|---|---|---|
| 1 | `database/db.go: write`, after `EncodeFrame`, before `log.Append` | Previous frames only; the new frame is in memory | Nothing to cut; key has its old value |
| 2 | `storage/log.go: Append`, inside `file.WriteAt` | Valid frames + a prefix of the new frame (could be 1 byte, the 12-byte header only, or header + part of the payload) | `ScanFrames` reports "partial frame header", "incomplete frame" or "CRC mismatch" at the frame start; `recover` calls `Truncate(ValidEnd)` + fsync |
| 3 | `log.go: Append`, after `WriteAt`, before/inside `file.Sync` | Whole frame in the page cache (survives a process kill) | Frame is valid, so it is applied. The op was never ACKed, which is allowed (rule 2) |
| 4 | `log.go: Append`, after `Sync`, before `l.size` update / index update / `ACK` | Whole frame, durable | Applied. The caller never heard back, but the data is there (A4) |
| 5 | `cmd/kvdb/serve.go: emit`, after printing `ACK` | Whole frame, durable | Applied (A5) |
| 6 | `log.go: Append` error path, inside `file.Truncate(start)` after a failed write/sync | Either still the partial frame or already cut back | Same as #2. If truncate itself failed, `db.broken` is set and every later write returns an error until reopen |
| 7 | `log.go: writeHeader`, creating a new file (`Truncate(0)`, `WriteAt(header)`) | File of 0–16 bytes | `OpenLog`: size < 16 (or exactly 16 with a bad CRC) → writes a fresh header. Safe because no frame can exist before the header was fsynced |
| 8 | `recovery.go: recover`, inside `ScanFrames` | Unchanged (scan is read-only) | Scans again from offset 16; same result (C1) |
| 9 | `recovery.go: recover`, inside `log.Truncate` (`ftruncate` then `fsync`) | Old size with the torn tail, or already truncated | Recomputes the same `ValidEnd`; truncates again or finds nothing to cut (C2) |
| 10 | `db.go: resolveCompactionTemp`, inside `os.Remove(path.compact)` | `path` valid; temp file present or gone | Removes the temp file again, or there is nothing to do |
| 11 | `compact.go: compactLocked`, writing frames to `path.compact` (before `f.Sync`) | `path` untouched; temp file partial | `path` is authoritative; temp file deleted (C3) |
| 12 | `compactLocked`, after `f.Sync`/`f.Close`, before `os.Rename` (also during `db.log.Close`) | `path` untouched; temp file complete | Same as #11: compaction work is thrown away, no data lost |
| 13 | `compactLocked`, inside `os.Rename(tmp, path)` | POSIX: `path` is old or new, both complete | Old: as #11. New: no temp file left, normal scan (C4) |
| 14 | `compactLocked`, after `Rename`, before `SyncDir` / `OpenLog` reopen | `path` = new compacted file, fsynced; no temp file | Normal scan of the new file (C5) |
| 15 | `db.go: Close` → `log.Close` (`Sync` + `Close`) | All ACKed frames already fsynced | Same as a kill at any other moment (C6) |

## 2. The ordering argument

A write calls `pwrite(frame)` (Go `File.WriteAt`) and then `fsync(fd)` (`File.Sync`; `FlushFileBuffers` on Windows, `F_FULLFSYNC` on macOS). Only after `fsync` returns do we update the index and return success, and only then does `serve` print `ACK`. So **ACK ⇒ fsync returned ⇒ the whole frame, including its CRC and `CMIT` marker, is on disk.**

We do not need a separate fsync between "records" and "commit marker", because there is only one write per operation and the commit marker is inside it. Validity is decided by content, not by write order: a frame counts only if every byte is present, the CRC32C over header + payload matches, and the last 4 bytes are `CMIT`. Even if the OS put the frame's blocks on disk in any order (a power-loss concern), a frame with any missing block fails the CRC and is dropped whole.

Appends never overlap. They are serialised by `DB.mu`, and a failed append is truncated back before `mu` is released (or the DB is marked broken). So a bad frame can only be the last thing in the file, and cutting from it to EOF never removes an ACKed frame.

Compaction order: `write(temp)` → `fsync(temp)` → `close(temp)` → `close(old)` → `rename(temp, path)` → `fsync(dir)` → reopen. The temp file is fsynced **before** the rename, so whatever name the rename leaves in place points to a complete file. We fsync the directory after the rename (Linux/macOS, `storage.SyncDir`) so the new name survives power loss. For process kills it is not needed: once `rename(2)` returns, the directory change is visible to the next process. We also fsync the directory after creating a new database file. On Windows `SyncDir` is a no-op (see weakness 2).

## 3. Known weaknesses

1. **The whole index is in RAM and rebuilt by a full scan on every open.** Recovery is O(file size). 1M small keys take about 0.5 s on our machine, but a file with lots of dead data (compaction runs only above 4 MB and 50% garbage), a slow laptop disk, or keys much larger than 10 bytes could push it past 2 s. RAM is about 80–100 bytes per key, plus the key itself.
2. **We rely on `rename` being atomic.** That is true for POSIX (ext4, APFS). On Windows, Go's `os.Rename` uses `MoveFileEx(MOVEFILE_REPLACE_EXISTING)`, and Microsoft does not document it as atomic. We added a fallback (if `path` is missing and `path.compact` scans cleanly, promote it), but we have not tested a power cut on NTFS. We also cannot fsync a directory on Windows with Go's standard library.
3. **Compaction holds the global lock.** Writers and readers block for its duration (about 230 ms for 200,000 keys / 8 MB here). It also reads values with one `pread` per key. A 1 GB database would stall writes for seconds.
4. **Bit rot in the middle of the file is treated as a torn tail.** Recovery stops at the first bad frame and truncates everything after it, so a single flipped bit in an old frame would silently drop every later frame. The fix is in "24 more hours" #2.
5. **A batch must fit in one frame of at most 256 MB.** 10,000 ops of 1 MB each (10 GB) is rejected with an error. The whole frame is also built in memory.
6. **One write = one fsync.** Throughput is limited by fsync latency (no group commit). Slow but correct.
7. **The torture tool's seed fixes the op stream and kill delays, not the exact instruction.** A failure seen once may need a few runs with the same seed to reproduce.

## 4. What we tried that failed

- **Batch as N separate records, each fsynced** (our first engine, `internal/database/batch.go` before the rewrite). A kill after 3 of 10 records left 3 visible: a straight B1 violation. We replaced it with one frame per batch. We later re-introduced this bug on purpose as a mutation test, and the torture tool reported "batch w0.1-1 is PARTIALLY visible" in iteration 1 (seed 777).
- **Recovery that returned an error on a bad record** (first version of `recovery.go`: "invalid record magic" / "checksum mismatch" → `open()` failed). A torn tail made the database unopenable, which breaks rule 5. Now any invalid frame at the end is treated as a torn tail and truncated.
- **Compaction that closed the DB and renamed without fsyncing the temp file or the directory, and without handling a leftover temp file.** The old `maintenance/compact.go` left the DB closed after compacting and never deleted `path.compact` on open.
- **Renaming over the open data file on Windows fails.** Windows will not rename over a file that has an open handle without delete sharing, so we close the old handle before the rename and reopen afterwards (`reopenAfterFailure` if the rename fails).
- **`go.mod` said `go 1.27.3`.** On a machine with an older Go, the toolchain tries to download a newer one, which fails offline. Lowered to `go 1.21`.
- **Torture tool first called `cmd.Wait()` right after `Kill()`.** That can close the stdout pipe before we read ACK lines that were already written, so the oracle would miss promises. Now we drain stdout to EOF first, then `Wait`.
- **Recovery-crash mode with a short kill window kept killing auto-compaction.** A 200,000-key compaction (about 230 ms) was killed before it finished, iteration after iteration, so no writes got through (correct, but not a useful test). We widened the kill window to 600 ms.

## 5. If we had 24 more hours

1. **Persist an index checkpoint (hint file).** Write a sorted key → offset snapshot plus the log offset it covers, with its own CRC, at compaction time. On open, load it and scan only frames after it. This makes recovery time independent of database size.
2. **Detect mid-file corruption instead of truncating it.** When a frame is bad, scan ahead for another `FRM1` with a valid CRC. If one exists, the damage is not a torn tail: refuse to truncate and report "corrupt at offset X" (and add a frame sequence number so frames cannot be confused).
3. **Group commit and background compaction.** Let concurrent writers share one fsync (collect frames for about 1 ms, write them together, ACK all). Build the compacted file from a snapshot without holding `mu`, then hold the lock only to copy the frames written meanwhile and do the rename.
