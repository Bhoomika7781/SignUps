# kvdb: a crash-safe key-value store

An embedded key-value database that keeps all its data in one file and stays
correct when it is killed with `kill -9` (or `TerminateProcess` on Windows)
at any instruction. It is built as an append-only log with an in-memory hash
index. It uses no libraries, only the Go standard library.

## Build (one command, any OS)

Requires Go 1.21 or newer. Run this from the repository root:

```
go build -o bin/ ./src/kvdb ./torture
```

This creates `bin/kvdb` and `bin/torture` (`bin\kvdb.exe` and
`bin\torture.exe` on Windows). On Windows, write `bin\kvdb` wherever this
README says `bin/kvdb`.

Run the unit tests with:

```
go test ./tests/ -v
```

## Commands

```
bin/kvdb open    <path> [--max-bytes N]   # open or create, run recovery, print key count and recovery time
bin/kvdb put     <path> <key> <value>     # prints OK
bin/kvdb get     <path> <key>             # prints the value, or NOTFOUND
bin/kvdb del     <path> <key>             # prints OK (deleting a missing key is not an error)
bin/kvdb batch   <path> <file>            # file has one op per line: "put k v" or "del k"
bin/kvdb dump    <path>                   # prints "key<TAB>value" for every key, sorted by key
bin/kvdb check   <path>                   # prints OK, or CORRUPT: <reason> (exit code 1)
bin/kvdb serve   <path> [--max-bytes N]   # long-running mode, see below
bin/kvdb compact <path>                   # rewrite the file with only the live keys
```

`--max-bytes N` works with every command that writes. A write that would make
the file larger than N bytes fails with `FULL` and writes nothing.

`get`, `dump` and `check` open the file read-only and never repair it.
`open`, `put`, `del`, `batch`, `compact` and `serve` run full recovery,
which cuts off any torn tail.

Do not run two *writing* processes on the same file at the same time. There
is no cross-process lock (see BREAKING.md).

### serve mode

`serve` reads one command per line from stdin. Fields are separated by single
spaces. The value is the rest of the line, so it may contain spaces but not
newlines.

| Send | Reply |
|---|---|
| `put <id> <key> <value>` | `ACK <id>` |
| `del <id> <key>` | `ACK <id>` |
| `get <id> <key>` | `VAL <id> <value>` or `NOTFOUND <id>` |
| `batch <id> <n>`, then n lines `put k v` / `del k` | `ACK <id>` |
| `batch <id> put a 1 ; put b 2 ; del c` (one-line batch, for typing by hand) | `ACK <id>` |
| `compact <id>` | `ACK <id>` |
| `quit` | exits |

The first line printed is `READY keys=N recovered_in=Xms`. Errors look like
`ERR <id> FULL <msg>` (limit reached, nothing written), `ERR <id> INVALID <msg>`
or `ERR <id> IO <msg>`.

**`ACK <id>` is printed only after the record has been fsynced.** Every ACK is
a promise that the operation survives a kill.

Concurrency: the part of `<id>` before the first `.` is a *lane*. Each lane runs
in its own goroutine, so `w0.1` and `w1.1` execute at the same time (that is
how the torture tool runs two concurrent writers). Ops in one lane run in
order. Ids without a dot all share one lane.

Example session (typed by hand):

```
bin/kvdb serve my.db
READY keys=0 recovered_in=0.4ms
put 1 name alice
ACK 1
batch 2 put a 1 ; put b 2 ; del name
ACK 2
get 3 a
VAL 3 1
```

## Torture tool

```
bin/torture --iterations 500 --seed 12345
bin/torture --iterations 100 --seed 999 --mode large-values
bin/torture --iterations 100 --seed 7   --mode compaction
bin/torture --iterations 100 --seed 42  --mode recovery-crash
bin/torture --iterations 50  --seed 5   --mode large-batch
bin/torture --iterations 100 --seed 3   --mode disk-full
bin/torture --seed 1 --mode million
```

| Mode | What it targets |
|---|---|
| `mixed` (default) | puts, deletes and batches of 2 to 40 ops from 2 writers on 300 shared keys, plus a concurrent reader that validates every `get` |
| `large-values` | 80% of values are 256 KB to 1 MB (half exactly 1 MB), so kills land inside large writes |
| `large-batch` | batches of 5,000 to 10,000 ops |
| `compaction` | a third client sends `compact` every 0 to 20 ms while 2 writers overwrite and delete 60 keys |
| `recovery-crash` | 30,000 keys preloaded; after each kill, `kvdb open` is started and SIGKILLed 1 to 3 times during recovery, then opened for real |
| `disk-full` | `--max-bytes 262144`; many writes are rejected with FULL, and those must never become visible |
| `million` | writes 1,000,000 keys, kills, and checks that recovery takes under 2 seconds and every key is present |

Other flags: `--writers N` (default 2), `--dir` (default `torture-data`, wiped
at start), `--max-bytes N`, and `--kvdb path` (default: the `kvdb` binary next
to the torture binary).

Each iteration:

1. Start `kvdb serve`.
2. Run the writers, which send random operations.
3. Record every op sent and every ACK received. This record is the oracle,
   kept in memory and in `<dir>/oracle.log`.
4. SIGKILL serve after a random number of milliseconds.
5. Run `kvdb open`, `kvdb check` and `kvdb dump`.
6. Check the dump against the oracle.

The tool stops at the first violation and prints what was expected, what was
found, and the op ids involved. The seed fixes every op stream and every
kill delay. The OS scheduler still decides which exact instruction the kill
lands on.

How the oracle decides (details in `torture/oracle.go`):

- **Self-checking values.** Every value is `<opid>|<key>|<length>|<filler>`,
  where the filler is derived from the first three fields. Any value read back
  can be traced to the op that wrote it. Torn values, garbage, and values
  written for another key are all detected.
- **Real-time order.** If op A's ACK arrived before op B was sent, then A was
  applied before B.
- **What each kind of op is allowed to do:**
  - An ACKed op must be visible unless a later ACKed op replaced it.
  - An op that was in flight at the kill may or may not be visible.
  - An op rejected with FULL must not be visible.
- **All-or-nothing batches.** If any key shows an in-flight batch's value,
  every key of that batch must show the batch's effect, or a value from an op
  that could have come after the batch.

The oracle has been tested against deliberately broken engines. Both ACKing
before the write and applying a batch one op at a time are caught within the
first iteration.

## On-disk format

The database is a single file, `<path>`. During compaction a temporary file
`<path>.compact` may also exist. The data file has no file header. It is a
sequence of records, and every put, delete or batch is exactly one record.
All integers are little-endian.

```
 Record
 +--------+--------+--------+----------------------------------------------+
 | magic  |  plen  |  crc   |                payload (plen bytes)          |
 | "KVR1" | uint32 | uint32 |                                              |
 +--------+--------+--------+----------------------------------------------+
 0        4        8        12                                    12 + plen

 crc = CRC-32C (Castagnoli) over the 4 plen bytes followed by the payload

 Payload
 +--------+-------------------+-------------------+-----+
 | count  |       op 1        |       op 2        | ... |   count = 1..10000
 | uint32 |                   |                   |     |
 +--------+-------------------+-------------------+-----+

 Op
 +------+--------+--------+-------------+---------------+
 | type |  klen  |  vlen  |  key bytes  |  value bytes  |
 | 1 B  | uint32 | uint32 |  klen bytes |  vlen bytes   |
 +------+--------+--------+-------------+---------------+
 type: 1 = put, 2 = delete (a delete always has vlen = 0)
 klen: 1..1024     vlen: 0..1048576
```

Example: `kvdb put db a 1` on an empty file produces this 27-byte file:

```
4b 56 52 31              "KVR1"
0f 00 00 00              plen = 15
49 b5 af ca              crc
01 00 00 00              count = 1
01                       type = put
01 00 00 00              klen = 1
01 00 00 00              vlen = 1
61                       "a"
31                       "1"
```

**Batch ordering (B5):** ops inside a batch are applied in order, so for the
same key the last op wins. `put x; del x` leaves x absent, and `del x; put x`
leaves x present.

The in-memory index maps `key -> (file offset of value, value length)`.
Values are not kept in memory. `get` reads them with one `ReadAt`.

## Write path

For every put, delete and batch, all under one global write lock:

1. Validate and encode the whole batch into one record (before taking the lock).
2. Check `max_bytes`. If the record does not fit, return FULL and write nothing.
   A delete-only batch never fails this way: it is applied by compaction,
   which rewrites the file without those keys.
3. `WriteAt(record, end of last valid record)`
4. `fsync` (`File.Sync`)
5. Update the in-memory index.
6. Release the lock and return success, so `serve` prints `ACK`.

`get` takes the lock in shared mode, so it sees the index either before or
after a batch, never halfway.

## Recovery algorithm (what `open` does)

1. If `<path>.compact` exists, delete it. It is a compaction that was killed
   before its rename, so the main file is still the truth.
2. Open or create `<path>`.
3. Read records from byte 0. Stop at the first record that:
   - has fewer than 12 header bytes,
   - has the wrong magic,
   - has a `plen` larger than the bytes left in the file,
   - fails the CRC check, or
   - has a payload that does not parse.

   A record is applied to the index only after its whole payload has passed
   the CRC check, so a batch is applied completely or not at all.
4. The point where scanning stopped is the valid end. If the file is longer,
   truncate it to the valid end and `fsync`.

Scanning changes nothing, and truncating to the same length twice gives the
same file, so a kill at any point during recovery is harmless: the next `open`
simply does the same work again.

## Compaction

Compaction runs automatically when the file is at least 8 MB and more than
half garbage, when a write hits `max_bytes` and compaction would free enough
room, and on `kvdb compact` or the serve command `compact`.

1. Write every live key into `<path>.compact`, using the same record format.
2. `fsync` the temp file.
3. Close the temp file and the old file.
4. `rename(<path>.compact, <path>)`. This atomic rename is the commit point.
5. `fsync` the directory. This is skipped on Windows (see BREAKING.md).
6. Reopen the file and switch to the new index.

A kill before step 4 leaves the old file complete and authoritative. A kill
after step 4 leaves the new file complete, because it was fsynced in step 2.
