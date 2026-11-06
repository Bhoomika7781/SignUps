package main

// shell.go: `kvdb shell <path>` is an interactive prompt in the style of
// PostgreSQL's psql. It uses the same SQL layer as the admin UI (sql.go), so
// every write still goes through engine.Batch with the same crash guarantees.
//
//	my.db=> SELECT * FROM kv LIMIT 5;
//	my.db=> \d+ kv
//	my.db=> \c shop          (switch to shop.db in the same folder)
//	my.db=> \q
//
// SQL statements end with ';' and may span several lines (the prompt changes
// to "->" while a statement is unfinished). Backslash commands run at once.
// Shell-only commands: \q quit, \x expanded display, \timing, \echo.

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"kvdb/src/engine"
)

const shellCellCap = 60 // characters shown per cell in table mode

var rowCountMsg = regexp.MustCompile(`^(SELECT \d+|\(\d+ (rows?|databases)\))$`)

func cmdShell(path string, opts engine.Options) {
	s := &uiServer{opts: opts, dbs: map[string]*engine.DB{}, paths: map[string]string{}}
	if st, err := os.Stat(path); err == nil && st.IsDir() {
		fail("give a database file, e.g. kvdb shell my.db")
	}
	s.dir = filepath.Dir(path)
	if abs, err := filepath.Abs(s.dir); err == nil {
		s.dir = abs
	}
	s.initial = filepath.Base(path)
	if exe, err := os.Executable(); err == nil {
		s.selfPath = exe
	}
	cur := s.initial
	db, _, err := s.Open(cur)
	if err != nil {
		fail("open %s: %v", cur, err)
	}
	info := db.Info()

	out := bufio.NewWriter(os.Stdout)
	defer out.Flush()
	fmt.Fprintf(out, "kvdb shell, connected to %s (%d rows, recovered in %.1f ms)\n", cur, db.Len(),
		float64(info.Duration.Microseconds())/1000)
	if info.TruncatedBytes > 0 {
		fmt.Fprintf(out, "NOTICE:  recovery cut %d torn bytes from the end of the file\n", info.TruncatedBytes)
	}
	fmt.Fprintln(out, `Type \? for help, \q to quit.`)
	fmt.Fprintln(out)

	expanded, timing := false, false
	in := bufio.NewReaderSize(os.Stdin, 1<<20)
	var buf strings.Builder

	for {
		name := cur
		if buf.Len() == 0 {
			fmt.Fprintf(out, "%s=> ", name)
		} else {
			fmt.Fprintf(out, "%s-> ", name)
		}
		out.Flush()

		line, rerr := in.ReadString('\n')
		line = strings.TrimRight(line, "\r\n")
		if rerr != nil && line == "" {
			fmt.Fprintln(out)
			break
		}
		trimmed := strings.TrimSpace(line)

		// Backslash commands run immediately when nothing else is pending.
		if buf.Len() == 0 && strings.HasPrefix(trimmed, `\`) {
			cmd := strings.Fields(trimmed)[0]
			switch cmd {
			case `\q`, `\quit`:
				return
			case `\x`:
				expanded = !expanded
				fmt.Fprintf(out, "Expanded display is %s.\n", onOff(expanded))
				continue
			case `\timing`:
				timing = !timing
				fmt.Fprintf(out, "Timing is %s.\n", onOff(timing))
				continue
			case `\echo`:
				fmt.Fprintln(out, strings.TrimSpace(strings.TrimPrefix(trimmed, `\echo`)))
				continue
			case `\torture`:
				out.Flush()
				runTortureFromShell(s, out, strings.Fields(trimmed)[1:], cur)
				continue
			}
			res := runSQL(s, cur, trimmed)
			if cmd == `\?` && res.Error == "" {
				res.Rows = append(res.Rows,
					[]string{`\x`, "toggle expanded display (one field per line)"},
					[]string{`\timing`, "toggle showing how long each statement took"},
					[]string{`\torture [mode] [iterations] [seed]`, "run the crash tester live (modes: mixed, large-values, large-batch, compaction, recovery-crash, disk-full, million)"},
					[]string{`\c torture_run`, "after \\torture: open the database it just crashed"},
					[]string{`\q`, "quit (safe at any time: every write is already on disk)"})
			}
			cur = printResult(out, res, cur, expanded, timing)
			if rerr != nil {
				break
			}
			continue
		}

		if trimmed == "" && buf.Len() == 0 {
			if rerr != nil {
				break
			}
			continue
		}
		buf.WriteString(line)
		buf.WriteByte('\n')
		if strings.HasSuffix(trimmed, ";") || rerr != nil {
			res := runSQL(s, cur, buf.String())
			buf.Reset()
			cur = printResult(out, res, cur, expanded, timing)
		}
		if rerr != nil {
			break
		}
	}
	for _, d := range s.dbs {
		d.Close()
	}
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

// printResult prints a result the way psql does and returns the database
// that is current after the statement (\c can change it).
func printResult(out *bufio.Writer, res *queryResult, cur string, expanded, timing bool) string {
	if res.HasRows && res.Error == "" {
		if expanded {
			printExpanded(out, res)
		} else {
			printTable(out, res)
		}
		n := len(res.Rows)
		if n == 1 {
			fmt.Fprintln(out, "(1 row)")
		} else {
			fmt.Fprintf(out, "(%d rows)\n", n)
		}
		if res.Truncated {
			fmt.Fprintln(out, "NOTICE:  only the first 1000 rows are shown; use LIMIT and OFFSET to page")
		}
	}
	for _, m := range res.Messages {
		if res.HasRows && (rowCountMsg.MatchString(m) || strings.HasPrefix(m, "NOTICE:  only the first")) {
			continue
		}
		fmt.Fprintln(out, m)
	}
	if res.Error != "" {
		fmt.Fprintln(out, res.Error)
	}
	if timing {
		fmt.Fprintf(out, "Time: %.3f ms\n", res.Ms)
	}
	fmt.Fprintln(out)
	if res.Database != "" {
		return res.Database
	}
	return cur
}

// cell makes a value printable on one line and caps its length.
func cell(v string, max int) string {
	v = strings.NewReplacer("\n", `\n`, "\r", `\r`, "\t", `\t`).Replace(v)
	if max > 0 && utf8.RuneCountInString(v) > max {
		r := []rune(v)
		return string(r[:max-1]) + "…"
	}
	return v
}

func printTable(out *bufio.Writer, res *queryResult) {
	cols := res.Columns
	width := make([]int, len(cols))
	rows := make([][]string, len(res.Rows))
	for i, c := range cols {
		width[i] = utf8.RuneCountInString(c)
	}
	for r, row := range res.Rows {
		rows[r] = make([]string, len(row))
		for i, v := range row {
			rows[r][i] = cell(v, shellCellCap)
			if w := utf8.RuneCountInString(rows[r][i]); i < len(width) && w > width[i] {
				width[i] = w
			}
		}
	}
	pad := func(s string, w int, center bool) string {
		gap := w - utf8.RuneCountInString(s)
		if gap <= 0 {
			return s
		}
		if center {
			left := gap / 2
			return strings.Repeat(" ", left) + s + strings.Repeat(" ", gap-left)
		}
		return s + strings.Repeat(" ", gap)
	}
	var b strings.Builder
	for i, c := range cols {
		if i > 0 {
			b.WriteString("|")
		}
		b.WriteString(" " + pad(c, width[i], true) + " ")
	}
	fmt.Fprintln(out, strings.TrimRight(b.String(), " "))
	b.Reset()
	for i := range cols {
		if i > 0 {
			b.WriteString("+")
		}
		b.WriteString(strings.Repeat("-", width[i]+2))
	}
	fmt.Fprintln(out, b.String())
	for _, row := range rows {
		b.Reset()
		for i := range cols {
			v := ""
			if i < len(row) {
				v = row[i]
			}
			if i > 0 {
				b.WriteString("|")
			}
			b.WriteString(" " + pad(v, width[i], false) + " ")
		}
		fmt.Fprintln(out, strings.TrimRight(b.String(), " "))
	}
}

func printExpanded(out *bufio.Writer, res *queryResult) {
	w := 0
	for _, c := range res.Columns {
		if n := utf8.RuneCountInString(c); n > w {
			w = n
		}
	}
	for r, row := range res.Rows {
		fmt.Fprintf(out, "-[ RECORD %d ]%s\n", r+1, strings.Repeat("-", w+6))
		for i, c := range res.Columns {
			v := ""
			if i < len(row) {
				v = cell(row[i], 0)
			}
			fmt.Fprintf(out, "%-*s | %s\n", w, c, v)
		}
	}
}

// runTortureFromShell runs bin/torture in <folder>/torture-ui-data and streams
// its output into the terminal. Usage: \torture [mode] [iterations] [seed]
func runTortureFromShell(s *uiServer, out *bufio.Writer, args []string, cur string) {
	mode, iterations, seed := "mixed", 20, time.Now().UnixNano()%1_000_000_000
	// positional: mode, iterations, seed (any may be left out)
	pos := 0
	for _, a := range args {
		if n, err := strconv.ParseInt(a, 10, 64); err == nil {
			if pos == 0 || pos == 1 {
				iterations, pos = int(n), 2
			} else {
				seed, pos = n, 3
			}
			continue
		}
		if !tortureModes[a] {
			fmt.Fprintf(out, "ERROR:  unknown mode %q (mixed, large-values, large-batch, compaction, recovery-crash, disk-full, million)\n\n", a)
			return
		}
		mode, pos = a, 1
	}
	if iterations < 1 {
		iterations = 1
	}
	bin := s.tortureBinary()
	if _, err := os.Stat(bin); err != nil {
		fmt.Fprintf(out, "ERROR:  cannot find %s. Build it first: go build -o bin/ ./src/kvdb ./torture\n\n", bin)
		return
	}
	if cur == tortureDBName {
		fmt.Fprintf(out, "ERROR:  you are connected to %s; \\c to another database first (the run wipes it)\n\n", tortureDBName)
		return
	}
	s.mu.Lock()
	s.closeLocked(tortureDBName) // the run wipes its folder; Windows cannot delete open files
	s.mu.Unlock()

	cmdArgs := []string{"--mode", mode, "--iterations", strconv.Itoa(iterations), "--seed", strconv.FormatInt(seed, 10),
		"--dir", s.tortureDir(), "--kvdb", s.selfPath}
	fmt.Fprintf(out, "Running: torture --mode %s --iterations %d --seed %d\n", mode, iterations, seed)
	fmt.Fprintf(out, "(working folder %s; your databases are not touched)\n\n", s.tortureDir())
	out.Flush()
	cmd := exec.Command(bin, cmdArgs...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	err := cmd.Run()
	fmt.Fprintln(out)
	if err != nil {
		fmt.Fprintf(out, "torture exited with: %v\n", err)
	}
	fmt.Fprintf(out, "Look at the crashed database with:  \\c %s   then   SELECT count(*) FROM kv;   \\d+ kv\n\n", strings.TrimSuffix(tortureDBName, ".db"))
}