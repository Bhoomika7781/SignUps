# invx: a crash-safe key-value database

invx is a single-file, embedded key-value store written in Go using only the
standard library. Every write is a checksummed **frame** that is appended to the
end of the file and fsynced before the call returns. Recovery replays the frames
and cuts off any torn tail. A batch is one frame, so it is all-or-nothing.

| Path | What it is |
|---|---|
| `internal/storage/` | On-disk format (`format.go`), frame scanner (`scan.go`), append/fsync/truncate (`log.go`), lock file and directory fsync (`os_unix.go`, `os_windows.go`) |
| `internal/database/` | Engine: `db.go` (open, put, get, delete, write path), `batch.go`, `recovery.go`, `compact.go`, `check.go`, `index.go` |
| `cmd/kvdb/` | Command-line wrapper, including `serve` mode (`serve.go`) |
| `cmd/torture/` | Torture tool: child process manager, oracle, kill loop |
| `tests/` | Unit tests (torn tail at every byte, disk full, compaction, concurrency, 1M-key recovery) |
| `ARCHITECTURE.md` | Diagram, 32-row scenario proof table, three guarantees |
| `BREAKING.md` | How we would break it |
| `PROMPTS.md` | Every AI prompt we used |
| `diagrams/` | Architecture diagram source (Mermaid) and PNG |

## Build

You need Go 1.21 or newer. There are no other dependencies and no network
access is needed.

```
go build -o . ./cmd/...
```

This puts `kvdb` and `torture` in the repository root (`kvdb.exe` and
`torture.exe` on Windows). On Windows PowerShell, type `.\kvdb` instead of
`./kvdb`.

Run the unit tests with `go test ./tests/` (add `-short` to skip the
1,000,000-key tests).

## Commands

Every command accepts `--max-bytes N` anywhere on the line. `N` can be a plain
number (`50000000`), use underscores (`50_000_000`) or a suffix (`50MB`).

```
./kvdb open    <path> [--max-bytes N]  # open/create + recover; prints key count and recovery time
./kvdb put     <path> <key> <value>    # prints OK once the value is on disk
./kvdb get     <path> <key>            # prints the value, or NOTFOUND
./kvdb del     <path> <key>            # deleting a missing key is not an error
./kvdb batch   <path> <file>           # one op per line: "put k v" or "del k"; atomic
./kvdb dump    <path>                  # every key<TAB>value, sorted by key
./kvdb check   <path>                  # verifies the file; prints OK ... or CORRUPT: <reason>
./kvdb serve   <path>                  # long-running mode, see below
./kvdb compact <path>                  # rewrite the file with live keys only
./kvdb fill    <path> <count>          # helper: write <count> keys in 10,000-op batches
```

Example:

```
./kvdb open demo.db
./kvdb put demo.db name Prajwal
./kvdb get demo.db name            # Prajwal
./kvdb batch demo.db ops.txt       # ops.txt: "put a 1" / "put b 2" / "del name"
./kvdb dump demo.db
./kvdb check demo.db               # OK keys=2 frames=3 bytes=...
```

Notes:
- In `put`, everything after the key is the value, so `put db k hello world`
  stores `hello world`. In a batch file, everything after `put <key> ` is the
  value. Lines that are empty or start with `#` are ignored.
- Every command runs recovery when it opens the file, so `check` and `dump`
  right after a kill are safe.
- Only one process can have a database open at a time (`<path>.lock`). A
  second process waits up to 3 seconds and then fails with "database is
  locked".
- Values are byte strings. The CLI passes them as typed. Limits: keys are 1 to
  1,024 bytes, values are 0 to 1,048,576 bytes, and a batch holds at most
  10,000 ops and 256 MB.

### `serve` protocol

`serve` reads one command per line from stdin. It prints `ACK <id>` **only
after the operation is fsynced**.

```
[id] put <key> <value...>   -> ACK <id>
[id] del <key>              -> ACK <id>
[id] get <key>              -> VALUE <id> <value>  |  NOTFOUND <id>
[id] batch <n>              -> next n lines are "put k v" / "del k", then ACK <id>
[id] batch                  -> lines until a line "end", then ACK <id>
[id] compact                -> ACK <id>
[id] count                  -> COUNT <id> <n>
quit                        -> close and exit (EOF does the same)
any failure                 -> ERR <id> <message>   (the op had no effect)
```

The id is optional. If you leave it out, ids count 1, 2, 3, and so on. An id
like `w1.17` names a **channel** (`w1`). Commands on one channel run in order,
and different channels run concurrently. The torture tool uses this to drive
two writers at the same time. It prints `READY keys=N recovery_ms=X` once open
has finished.

Typing by hand:

```
./kvdb serve demo.db
put a 1
batch 3
put x 1
put y 2
del a
get x
```

### Batch ordering (scenario B5)

Ops in a batch are applied **in the order written**. `put X` followed by `del X`
in the same batch leaves X deleted. `del X` followed by `put X` leaves X set.
Recovery replays a frame's ops in the same order, so the rule holds after a
crash too.

## On-disk format

There is one data file. All integers are little-endian. The checksum is
CRC32C (Castagnoli).

```
offset 0                                                     end of file
+-------------+---------+---------+---------+-----+---------+
| file header | frame 1 | frame 2 | frame 3 | ... | frame N |
|  16 bytes   |         |         |         |     |         |
+-------------+---------+---------+---------+-----+---------+

FILE HEADER (16 bytes)
  0   8  magic        "INVXDB01"            49 4e 56 58 44 42 30 31
  8   4  version      1                     01 00 00 00
 12   4  CRC32C of bytes 0..11

FRAME (one per put, per delete, or per whole batch)
  0   4  frame magic  "FRM1"                46 52 4d 31
  4   4  payload length P (bytes)
  8   4  op count N (1..10000)
 12   P  payload: N ops back to back (below)
12+P  4  CRC32C over frame bytes [0, 12+P)  <- covers magic, P, N and payload
16+P  4  commit marker "CMIT"               43 4d 49 54

OP (inside the payload)
  0   1  kind         1 = put, 2 = delete
  1   2  key length K (1..1024)
  3   4  value length V (0..1048576; always 0 for delete)
  7   K  key bytes
 7+K  V  value bytes
```

A frame is **valid** only when all of these hold:
- the frame magic is `FRM1`,
- `P` is at most 256 MB and `N` is between 1 and 10,000,
- all `20+P` bytes are present,
- the CRC matches,
- the commit marker is `CMIT`,
- the ops exactly fill the payload.

Example: `put a 1` on an empty database makes a 45-byte file (16 header + 12 frame header + 9 payload + 8 trailer).
`[16-byte header] [FRM1] [09 00 00 00] [01 00 00 00] [01 01 00 01 00 00 00 'a' '1'] [crc] [CMIT]`

Auxiliary files (not data):
- `<path>.lock` is an empty lock file. The OS releases the lock when the
  process dies.
- `<path>.compact` exists only while a compaction is running, or after a
  compaction that crashed before its rename. `open()` deletes it, or promotes
  it if the main file is missing; see recovery step 2.

In memory, a hash map (`key -> offset, length of value`) is rebuilt on every
open. The index is never persisted.

## Recovery algorithm (plain English)

`open(path)` does the following:

1. Takes the lock file, so no other process is using the database.
2. Handles any leftover compaction temp file `<path>.compact`. If `<path>`
   exists, the temp file is garbage from a compaction that never finished its
   rename, so it is deleted. If `<path>` is missing and the temp file is
   complete, the rename was half done (only possible where rename is not
   atomic), so the temp file is renamed into place.
3. Opens `<path>`, creating it if needed. If the file is shorter than 16 bytes,
   the crash happened while the header was being created. No write was ever
   acknowledged in that file, so a fresh header is written and fsynced. If the
   header is present but wrong, it is not our file and open refuses.
4. Reads frames from offset 16 in order. Each valid frame is applied to the
   index as a whole, in op order.
5. Stops at the first invalid frame. Appends are serialised and each one is
   fsynced before the next starts, so an invalid frame can only be the last
   write, which never returned success. Everything from that offset to the end
   is a torn write.
6. Truncates the file to the end of the last valid frame and fsyncs. Doing this
   twice is harmless, so if recovery itself is killed, the next open simply
   repeats it and reaches the same state.

The only change recovery ever makes to the file is that truncate (plus deleting
or renaming the temp file), and every such step can be repeated safely.

## Compaction and disk full

- **Compaction** writes every live key into `<path>.compact`, fsyncs it, closes
  the old file, renames the temp file over `<path>`, fsyncs the directory
  (Linux/macOS) and reopens. It runs automatically after a write when the file
  is over 4 MB and more than half of it is dead data. You can also run it with
  `kvdb compact` or the serve command `compact`.
- **Disk full**: with `--max-bytes N`, a write whose frame would push the file
  past N first triggers a compaction (if that would free enough space). If it
  still does not fit, the write fails with
  `disk full: write would exceed max-bytes limit` and nothing is written.
  Compaction also refuses to produce a file larger than N.

## Torture tool

```
./torture --iterations 500 --seed 12345
./torture --iterations 100 --seed 999 --mode large-values
./torture --iterations 100 --seed 7   --mode compaction
./torture --iterations 100 --seed 42  --mode recovery-crash
./torture --iterations 100 --seed 5   --mode large-batch
./torture --iterations 100 --seed 3   --mode disk-full
```

Each iteration does the following:
1. Starts `kvdb serve` as a child process.
2. Runs two concurrent writers. Each one sends random puts, deletes and
   batches, on its own private keys and on shared keys.
3. Records every ACK and ERR in its own oracle (`torture-data/oracle.log`).
4. Kills the child with SIGKILL (TerminateProcess on Windows) after a random
   number of milliseconds drawn from the seed.
5. Runs `kvdb open`, `kvdb check` and `kvdb dump`.
6. Checks the dump against the oracle:
   - every ACKed op is present,
   - each writer's keys equal "all ACKed ops" or "all ACKed ops plus the whole
     in-flight op" (this is the batch atomicity check),
   - shared keys hold a value some op really wrote and that no later ACKed op
     overwrote,
   - an op that got ERR is absent,
   - no unknown keys or values appear.

The tool stops at the first failure and prints expected versus found with the
op ids. The database is kept between iterations, so damage accumulates.

| Mode | Targets |
|---|---|
| `default` | random puts, deletes and batches of 2 to 40 ops; 2 writers plus shared keys |
| `large-values` | 256 KB to 1 MB values, batches of up to 4 of them |
| `large-batch` | writer 0 sends only 10,000-op batches while writer 1 does single ops |
| `recovery-crash` | 200,000 preloaded keys; after each kill, `kvdb open` is started and killed 1 to 3 times mid-recovery |
| `compaction` | 30,000 preloaded keys; a third client sends `compact` continuously, so kills land inside compaction |
| `disk-full` | `--max-bytes 1MiB`; ERR must say "disk full", the failed op must be absent, and writes must succeed again after deletes and compaction |

Other flags:
- `--writers N` (default 2)
- `--dir DIR` (default `torture-data`)
- `--kvdb PATH` (default: next to the torture binary)
- `--max-bytes N`

The seed fixes every random choice: the ops, the values, the kill delay and the
recovery-kill delays. The exact instruction the kill lands on still depends on
OS timing.
