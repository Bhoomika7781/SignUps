package main

// sql.go: a tiny SQL dialect for the admin UI. It is only a front end:
// every write still goes through engine.Batch, so the crash guarantees are
// exactly the same as for put/del/batch.
//
// The database looks like one table:   kv (key text PRIMARY KEY, value text)
//
//	SELECT * | key, value | count(*) FROM kv
//	       [WHERE <col> <op> '<text>' [AND ...]] [ORDER BY key [ASC|DESC]] [LIMIT n] [OFFSET n]
//	INSERT INTO kv [(key, value)] VALUES ('k','v') [, ('k2','v2') ...]   (upsert)
//	UPDATE kv SET value = 'v' [WHERE ...]
//	DELETE FROM kv [WHERE ...]
//	BEGIN; ...writes...; COMMIT;     -> all writes become ONE atomic batch
//	ROLLBACK;                        -> discard
//	VACUUM;  (or COMPACT;)           -> compaction
//	CHECK;                           -> integrity check
//	CREATE DATABASE name;  DROP DATABASE name;
//	\l  \c name  \conninfo  \d  \d kv  \d+ kv  \dt  \dt+  \di  \dn  \?
//
// <col> is key or value, <op> is = <> != < > <= >= LIKE NOT LIKE ILIKE.

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"kvdb/src/engine"
)

const (
	displayRowCap   = 1000
	displayValueCap = 2000
)

type token struct {
	k byte // 'w' word, 's' string, 'n' number, 'p' punctuation
	s string
}

func tokenize(sql string) ([]token, error) {
	var out []token
	i := 0
	for i < len(sql) {
		c := sql[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		case c == '-' && i+1 < len(sql) && sql[i+1] == '-':
			for i < len(sql) && sql[i] != '\n' {
				i++
			}
		case c == '\'':
			i++
			var b strings.Builder
			closed := false
			for i < len(sql) {
				if sql[i] == '\'' {
					if i+1 < len(sql) && sql[i+1] == '\'' {
						b.WriteByte('\'')
						i += 2
						continue
					}
					i++
					closed = true
					break
				}
				b.WriteByte(sql[i])
				i++
			}
			if !closed {
				return nil, errors.New("unterminated quoted string")
			}
			out = append(out, token{'s', b.String()})
		case c == '"':
			j := strings.IndexByte(sql[i+1:], '"')
			if j < 0 {
				return nil, errors.New("unterminated quoted identifier")
			}
			out = append(out, token{'w', sql[i+1 : i+1+j]})
			i += j + 2
		case c == '\\' && i+1 < len(sql) && sql[i+1] == '?':
			out = append(out, token{'w', `\?`})
			i += 2
		case isWordStart(c):
			j := i + 1
			for j < len(sql) && isWordChar(sql[j]) {
				j++
			}
			out = append(out, token{'w', sql[i:j]})
			i = j
		case c >= '0' && c <= '9':
			j := i
			for j < len(sql) && sql[j] >= '0' && sql[j] <= '9' {
				j++
			}
			out = append(out, token{'n', sql[i:j]})
			i = j
		case (c == '<' || c == '>' || c == '!') && i+1 < len(sql) && (sql[i+1] == '=' || (c == '<' && sql[i+1] == '>')):
			out = append(out, token{'p', sql[i : i+2]})
			i += 2
		default:
			out = append(out, token{'p', string(c)})
			i++
		}
	}
	return out, nil
}

func isWordStart(c byte) bool {
	return c == '_' || c == '\\' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}
func isWordChar(c byte) bool { return isWordStart(c) || c == '.' || (c >= '0' && c <= '9') }

// splitStatements splits tokens at ';'.
func splitStatements(toks []token) [][]token {
	var out [][]token
	var cur []token
	for _, t := range toks {
		if t.k == 'p' && t.s == ";" {
			if len(cur) > 0 {
				out = append(out, cur)
			}
			cur = nil
			continue
		}
		cur = append(cur, t)
	}
	if len(cur) > 0 {
		out = append(out, cur)
	}
	return out
}

type parser struct {
	t []token
	p int
}

func (p *parser) done() bool { return p.p >= len(p.t) }
func (p *parser) near() string {
	if p.done() {
		return "end of input"
	}
	return fmt.Sprintf("%q", p.t[p.p].s)
}
func (p *parser) syntaxErr() error { return fmt.Errorf("syntax error at or near %s", p.near()) }

func (p *parser) peekWord(w string) bool {
	return !p.done() && p.t[p.p].k == 'w' && strings.EqualFold(p.t[p.p].s, w)
}
func (p *parser) word(w string) bool {
	if p.peekWord(w) {
		p.p++
		return true
	}
	return false
}
func (p *parser) expectWord(w string) error {
	if !p.word(w) {
		return p.syntaxErr()
	}
	return nil
}
func (p *parser) punct(s string) bool {
	if !p.done() && p.t[p.p].k == 'p' && p.t[p.p].s == s {
		p.p++
		return true
	}
	return false
}
func (p *parser) expectPunct(s string) error {
	if !p.punct(s) {
		return p.syntaxErr()
	}
	return nil
}
func (p *parser) literal() (string, error) {
	if !p.done() && (p.t[p.p].k == 's' || p.t[p.p].k == 'n') {
		p.p++
		return p.t[p.p-1].s, nil
	}
	return "", p.syntaxErr()
}
func (p *parser) number() (int, error) {
	if !p.done() && p.t[p.p].k == 'n' {
		p.p++
		return strconv.Atoi(p.t[p.p-1].s)
	}
	return 0, p.syntaxErr()
}
func (p *parser) column() (string, error) {
	if !p.done() && p.t[p.p].k == 'w' {
		c := strings.ToLower(p.t[p.p].s)
		if c == "key" || c == "value" {
			p.p++
			return c, nil
		}
		return "", fmt.Errorf("column %q does not exist (columns are key and value)", p.t[p.p].s)
	}
	return "", p.syntaxErr()
}
func (p *parser) table() error {
	if p.done() || p.t[p.p].k != 'w' {
		return p.syntaxErr()
	}
	name := strings.ToLower(p.t[p.p].s)
	if name != "kv" && name != "public.kv" {
		return fmt.Errorf("relation %q does not exist (the only table is kv)", p.t[p.p].s)
	}
	p.p++
	return nil
}

// dbName reads a database name (word or 'quoted'), adding ".db" if it has no extension.
func (p *parser) dbName() (string, error) {
	if p.done() || (p.t[p.p].k != 'w' && p.t[p.p].k != 's') {
		return "", p.syntaxErr()
	}
	name := p.t[p.p].s
	p.p++
	if !strings.Contains(name, ".") {
		name += ".db"
	}
	return name, nil
}

type cond struct {
	col, op, val string
	re           *regexp.Regexp
}

func (p *parser) where() ([]cond, error) {
	if !p.word("WHERE") {
		return nil, nil
	}
	var conds []cond
	for {
		col, err := p.column()
		if err != nil {
			return nil, err
		}
		var op string
		switch {
		case p.word("NOT"):
			if err := p.expectWord("LIKE"); err != nil {
				return nil, err
			}
			op = "NOT LIKE"
		case p.word("LIKE"):
			op = "LIKE"
		case p.word("ILIKE"):
			op = "ILIKE"
		default:
			for _, o := range []string{"=", "<>", "!=", "<=", ">=", "<", ">"} {
				if p.punct(o) {
					op = o
					break
				}
			}
			if op == "" {
				return nil, p.syntaxErr()
			}
		}
		val, err := p.literal()
		if err != nil {
			return nil, err
		}
		c := cond{col: col, op: op, val: val}
		if strings.HasSuffix(op, "LIKE") {
			c.re = likeRegexp(val, op == "ILIKE")
		}
		conds = append(conds, c)
		if !p.word("AND") {
			return conds, nil
		}
	}
}

func likeRegexp(pat string, fold bool) *regexp.Regexp {
	var b strings.Builder
	b.WriteString("^(?s)")
	if fold {
		b.WriteString("(?i)")
	}
	for _, r := range pat {
		switch r {
		case '%':
			b.WriteString(".*")
		case '_':
			b.WriteString(".")
		default:
			b.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	b.WriteString("$")
	return regexp.MustCompile(b.String())
}

func (c cond) match(s string) bool {
	switch c.op {
	case "=":
		return s == c.val
	case "<>", "!=":
		return s != c.val
	case "<":
		return s < c.val
	case ">":
		return s > c.val
	case "<=":
		return s <= c.val
	case ">=":
		return s >= c.val
	case "LIKE", "ILIKE":
		return c.re.MatchString(s)
	case "NOT LIKE":
		return !c.re.MatchString(s)
	}
	return false
}

// ---------------------------------------------------------------------------

type queryResult struct {
	Columns   []string   `json:"columns"`
	Types     []string   `json:"types"`
	Rows      [][]string `json:"rows"`
	HasRows   bool       `json:"hasRows"`
	Messages  []string   `json:"messages"`
	Error     string     `json:"error,omitempty"`
	Ms        float64    `json:"ms"`
	Wrote     bool       `json:"wrote"`
	Truncated bool       `json:"truncated"`
	Database  string     `json:"database"` // database in use after the statements ran (\c can change it)
	Catalog   bool       `json:"catalog"`  // a database was created or dropped
}

// catalog is the set of databases the UI can see (files in one folder).
type catalog interface {
	Open(name string) (*engine.DB, string, error) // database and its file path
	List() ([]dbEntry, error)
	Create(name string) error
	Drop(name string) error
	Addr() string
}

type dbEntry struct {
	Name    string `json:"name"`
	Size    int64  `json:"size"`
	Open    bool   `json:"open"`
	Keys    int    `json:"keys"`
	Torture bool   `json:"torture"`
}

type executor struct {
	cat  catalog
	name string
	db   *engine.DB
	path string
	res  *queryResult
	inTx bool
	tx   []engine.Op
}

func runSQL(cat catalog, name, sql string) *queryResult {
	start := time.Now()
	res := &queryResult{Database: name}
	db, path, err := cat.Open(name)
	if err != nil {
		res.Error = "ERROR:  " + err.Error()
		return res
	}
	ex := &executor{cat: cat, name: name, db: db, path: path, res: res}
	toks, err := tokenize(sql)
	if err == nil {
		stmts := splitStatements(toks)
		if len(stmts) == 0 {
			err = errors.New("no statement to run")
		}
		for _, st := range stmts {
			if err = ex.exec(&parser{t: st}); err != nil {
				break
			}
		}
	}
	if err != nil {
		res.Error = "ERROR:  " + err.Error()
		if ex.inTx {
			res.Messages = append(res.Messages, "ROLLBACK: the transaction was discarded, nothing was written.")
		}
	} else if ex.inTx {
		res.Messages = append(res.Messages, "WARNING:  transaction was not committed, so it was rolled back. Nothing was written.")
	}
	res.Ms = float64(time.Since(start).Microseconds()) / 1000
	return res
}

func (ex *executor) msg(format string, a ...any) {
	ex.res.Messages = append(ex.res.Messages, fmt.Sprintf(format, a...))
}

func (ex *executor) exec(p *parser) error {
	switch {
	case p.word("SELECT"):
		return ex.selectStmt(p)
	case p.word("INSERT"):
		return ex.insertStmt(p)
	case p.word("UPDATE"):
		return ex.updateStmt(p)
	case p.word("DELETE"):
		return ex.deleteStmt(p)
	case p.word("BEGIN") || p.word("START"):
		p.word("TRANSACTION")
		if ex.inTx {
			ex.msg("WARNING:  there is already a transaction in progress")
		}
		ex.inTx, ex.tx = true, nil
		ex.msg("BEGIN")
	case p.word("COMMIT") || p.word("END"):
		if !ex.inTx {
			ex.msg("WARNING:  there is no transaction in progress")
			ex.msg("COMMIT")
			return nil
		}
		ops := ex.tx
		ex.inTx, ex.tx = false, nil
		if len(ops) > engine.MaxBatchOps {
			return fmt.Errorf("transaction has %d writes; one atomic batch holds at most %d", len(ops), engine.MaxBatchOps)
		}
		if len(ops) > 0 {
			if err := ex.db.Batch(ops); err != nil {
				return err
			}
			ex.res.Wrote = true
		}
		ex.msg("COMMIT  (%d writes applied as one atomic batch, fsynced)", len(ops))
	case p.word("ROLLBACK") || p.word("ABORT"):
		n := len(ex.tx)
		ex.inTx, ex.tx = false, nil
		ex.msg("ROLLBACK  (%d pending writes discarded)", n)
	case p.word("VACUUM") || p.word("COMPACT"):
		if ex.inTx {
			return errors.New("VACUUM cannot run inside a transaction block")
		}
		before := ex.db.Size()
		if err := ex.db.Compact(); err != nil {
			return err
		}
		ex.res.Wrote = true
		ex.msg("VACUUM  (file %s -> %s)", humanBytes(before), humanBytes(ex.db.Size()))
	case p.word("CHECK"):
		rep, err := engine.Check(ex.path)
		if err != nil {
			return err
		}
		if rep.OK {
			ex.msg("CHECK OK  (%d records, %d keys, %s)", rep.Records, rep.Keys, humanBytes(rep.FileSize))
		} else {
			ex.msg("CHECK FAILED: %s", rep.Problem)
		}
	case p.word(`\l`) || p.word(`\list`):
		list, err := ex.cat.List()
		if err != nil {
			return err
		}
		rows := [][]string{}
		for _, d := range list {
			status, keys := "closed", "–"
			if d.Open {
				status, keys = "open", strconv.Itoa(d.Keys)
			}
			if d.Name == ex.name {
				status = "connected"
			}
			if d.Torture {
				status += " (torture tool's database)"
			}
			rows = append(rows, []string{d.Name, humanBytes(d.Size), keys, status})
		}
		ex.setGrid([]string{"Name", "Size", "Rows", "Status"}, []string{"name", "text", "bigint", "text"}, rows)
		ex.msg("(%d databases)", len(rows))
	case p.word(`\c`) || p.word(`\connect`):
		name, err := p.dbName()
		if err != nil {
			return err
		}
		if ex.inTx {
			return errors.New("cannot switch database inside a transaction block")
		}
		db, path, err := ex.cat.Open(name)
		if err != nil {
			return err
		}
		ex.db, ex.path, ex.name = db, path, name
		ex.res.Database = name
		ex.msg("You are now connected to database \"%s\".", name)
	case p.word(`\conninfo`):
		ex.msg("You are connected to database \"%s\" (file %s) through %s.", ex.name, ex.path, ex.cat.Addr())
	case p.word(`\dt`):
		plus := p.punct("+")
		p.word("kv")
		if plus {
			live, dead := ex.db.Stats()
			ex.setGrid([]string{"Schema", "Name", "Type", "Owner", "Persistence", "Size", "Rows", "Description"},
				[]string{"name", "name", "text", "name", "text", "text", "bigint", "text"},
				[][]string{{"public", "kv", "table", "kvdb", "permanent", humanBytes(ex.db.Size()), strconv.Itoa(ex.db.Len()),
					fmt.Sprintf("live data %s, reclaimable by VACUUM %s", humanBytes(live), humanBytes(dead))}})
		} else {
			ex.setGrid([]string{"Schema", "Name", "Type", "Owner"}, []string{"name", "name", "text", "name"},
				[][]string{{"public", "kv", "table", "kvdb"}})
		}
		ex.msg("(1 row)")
	case p.word(`\di`):
		p.punct("+")
		ex.setGrid([]string{"Schema", "Name", "Type", "Owner", "Table", "Method", "Lives in"},
			[]string{"name", "name", "text", "name", "name", "text", "text"},
			[][]string{{"public", "kv_pkey", "index", "kvdb", "kv", "hash (key)", "memory, rebuilt from the log at every open"}})
		ex.msg("(1 row)")
	case p.word(`\dn`):
		p.punct("+")
		ex.setGrid([]string{"Name", "Owner"}, []string{"name", "name"}, [][]string{{"public", "kvdb"}})
		ex.msg("(1 row)")
	case p.word(`\d`):
		plus := p.punct("+")
		if p.done() {
			ex.setGrid([]string{"Schema", "Name", "Type", "Owner"}, []string{"name", "name", "text", "name"},
				[][]string{{"public", "kv", "table", "kvdb"}, {"public", "kv_pkey", "index", "kvdb"}})
			ex.msg("(2 rows)")
			break
		}
		if err := p.table(); err != nil {
			return err
		}
		ex.describe(plus)
	case p.word(`\?`) || p.word("HELP"):
		ex.setGrid([]string{"Command", "What it does"}, []string{"text", "text"}, helpRows)
		ex.msg("(%d rows)", len(helpRows))
	case p.word("CREATE"):
		if err := p.expectWord("DATABASE"); err != nil {
			return err
		}
		name, err := p.dbName()
		if err != nil {
			return err
		}
		if err := ex.cat.Create(name); err != nil {
			return err
		}
		ex.res.Catalog = true
		ex.msg("CREATE DATABASE  (%s; connect with \\c %s)", name, name)
	case p.word("DROP"):
		if err := p.expectWord("DATABASE"); err != nil {
			return err
		}
		ifExists := false
		if p.word("IF") {
			if err := p.expectWord("EXISTS"); err != nil {
				return err
			}
			ifExists = true
		}
		name, err := p.dbName()
		if err != nil {
			return err
		}
		if name == ex.name {
			return fmt.Errorf("cannot drop the currently open database (connect to another one with \\c first)")
		}
		if err := ex.cat.Drop(name); err != nil {
			if ifExists && strings.Contains(err.Error(), "does not exist") {
				ex.msg("NOTICE:  database \"%s\" does not exist, skipping", name)
				ex.msg("DROP DATABASE")
				return nil
			}
			return err
		}
		ex.res.Catalog = true
		ex.msg("DROP DATABASE")
	default:
		return p.syntaxErr()
	}
	if !p.done() {
		return p.syntaxErr()
	}
	return nil
}

var helpRows = [][]string{
	{"SELECT * FROM kv [WHERE ...] [ORDER BY key DESC] [LIMIT n] [OFFSET n]", "read rows"},
	{"SELECT count(*) FROM kv [WHERE ...]", "count rows"},
	{"INSERT INTO kv VALUES ('k','v'), ('k2','v2')", "insert or replace (one atomic batch)"},
	{"UPDATE kv SET value = 'v' WHERE key LIKE 'user:%'", "change existing rows"},
	{"DELETE FROM kv WHERE key = 'k'", "delete rows"},
	{"BEGIN; ...; COMMIT;   ROLLBACK;", "all writes in between become ONE atomic batch"},
	{"VACUUM;", "compact the file (crash-safe rename)"},
	{"CHECK;", "verify every record's checksum"},
	{"CREATE DATABASE name;   DROP DATABASE [IF EXISTS] name;", "make or delete a database file (name.db)"},
	{`\l`, "list databases"},
	{`\c name`, "connect to another database"},
	{`\conninfo`, "show the current connection"},
	{`\d`, "list relations"},
	{`\d kv     \d+ kv`, "describe table kv (+ = storage details)"},
	{`\dt   \dt+`, "list tables (+ = size and garbage)"},
	{`\di`, "list indexes"},
	{`\dn`, "list schemas"},
	{"WHERE operators", "= <> != < > <= >= LIKE NOT LIKE ILIKE, joined with AND"},
}

func (ex *executor) describe(plus bool) {
	cols := []string{"Column", "Type", "Collation", "Nullable", "Default"}
	types := []string{"name", "text", "name", "text", "text"}
	rows := [][]string{{"key", "text", "", "not null", ""}, {"value", "text", "", "not null", "''"}}
	if plus {
		cols = append(cols, "Storage", "Description")
		types = append(types, "text", "text")
		rows[0] = append(rows[0], "in memory index + log", "1 to 1024 bytes")
		rows[1] = append(rows[1], "log only (read with one pread)", "0 to 1 MB")
	}
	ex.setGrid(cols, types, rows)
	ex.msg(`Table "public.kv"`)
	ex.msg(`Indexes:`)
	ex.msg(`    "kv_pkey" PRIMARY KEY, hash (key)  -- in memory, rebuilt from the log on open`)
	if plus {
		live, dead := ex.db.Stats()
		info := ex.db.Info()
		max := "none"
		if ex.db.MaxBytes() > 0 {
			max = humanBytes(ex.db.MaxBytes())
		}
		ex.msg(`Storage:`)
		ex.msg(`    file            %s`, ex.path)
		ex.msg(`    file size       %s`, humanBytes(ex.db.Size()))
		ex.msg(`    live data       %s`, humanBytes(live))
		ex.msg(`    reclaimable     %s  (VACUUM removes it)`, humanBytes(dead))
		ex.msg(`    max_bytes       %s`, max)
		ex.msg(`    record format   [magic "KVR1" 4B][length 4B][CRC-32C 4B][payload]`)
		ex.msg(`    last recovery   %.3f ms, %d records replayed, %d torn bytes cut`,
			float64(info.Duration.Microseconds())/1000, info.Records, info.TruncatedBytes)
	}
}

func (ex *executor) setGrid(cols, types []string, rows [][]string) {
	if rows == nil {
		rows = [][]string{} // send [] not null, so the page shows "(0 rows)"
	}
	ex.res.Columns, ex.res.Types, ex.res.Rows, ex.res.HasRows = cols, types, rows, true
	ex.res.Truncated = false
}

// scan walks keys in order and calls fn for every row that matches.
// fn returns false to stop.
func (ex *executor) scan(conds []cond, desc bool, fn func(k string, getV func() (string, error)) (bool, error)) error {
	var keys []string
	for _, c := range conds {
		if c.col == "key" && c.op == "=" { // point lookup, no full scan
			keys = []string{c.val}
			break
		}
	}
	if keys == nil {
		keys = ex.db.Keys()
	}
	for i := range keys {
		k := keys[i]
		if desc {
			k = keys[len(keys)-1-i]
		}
		var val string
		loaded := false
		getV := func() (string, error) {
			if !loaded {
				v, ok, err := ex.db.Get([]byte(k))
				if err != nil {
					return "", err
				}
				if !ok {
					return "", errNoRow
				}
				val, loaded = string(v), true
			}
			return val, nil
		}
		ok := true
		for _, c := range conds {
			s := k
			if c.col == "value" {
				v, err := getV()
				if err != nil {
					if errors.Is(err, errNoRow) {
						ok = false
						break
					}
					return err
				}
				s = v
			}
			if !c.match(s) {
				ok = false
				break
			}
		}
		if !ok {
			continue
		}
		if _, err := getV(); errors.Is(err, errNoRow) {
			continue // key vanished (point lookup of a missing key)
		} else if err != nil {
			return err
		}
		more, err := fn(k, getV)
		if err != nil {
			return err
		}
		if !more {
			return nil
		}
	}
	return nil
}

var errNoRow = errors.New("no such row")

func (ex *executor) selectStmt(p *parser) error {
	var cols []string
	count := false
	switch {
	case p.punct("*"):
		cols = []string{"key", "value"}
	case p.word("count"):
		if err := p.expectPunct("("); err != nil {
			return err
		}
		if !p.punct("*") {
			if _, err := p.column(); err != nil {
				return err
			}
		}
		if err := p.expectPunct(")"); err != nil {
			return err
		}
		count = true
	default:
		for {
			c, err := p.column()
			if err != nil {
				return err
			}
			cols = append(cols, c)
			if !p.punct(",") {
				break
			}
		}
	}
	if err := p.expectWord("FROM"); err != nil {
		return err
	}
	if err := p.table(); err != nil {
		return err
	}
	conds, err := p.where()
	if err != nil {
		return err
	}
	desc := false
	if p.word("ORDER") {
		if err := p.expectWord("BY"); err != nil {
			return err
		}
		c, err := p.column()
		if err != nil {
			return err
		}
		if c != "key" {
			return errors.New("ORDER BY value is not supported (rows are ordered by key)")
		}
		if p.word("DESC") {
			desc = true
		} else {
			p.word("ASC")
		}
	}
	limit, offset := -1, 0
	for {
		if p.word("LIMIT") {
			if limit, err = p.number(); err != nil {
				return err
			}
		} else if p.word("OFFSET") {
			if offset, err = p.number(); err != nil {
				return err
			}
		} else {
			break
		}
	}

	if count {
		n := 0
		err := ex.scan(conds, false, func(string, func() (string, error)) (bool, error) { n++; return true, nil })
		if err != nil {
			return err
		}
		ex.setGrid([]string{"count"}, []string{"bigint"}, [][]string{{strconv.Itoa(n)}})
		ex.msg("SELECT 1")
		return nil
	}

	var rows [][]string
	skipped, truncated := 0, false
	err = ex.scan(conds, desc, func(k string, getV func() (string, error)) (bool, error) {
		if skipped < offset {
			skipped++
			return true, nil
		}
		if limit >= 0 && len(rows) >= limit {
			return false, nil
		}
		if len(rows) >= displayRowCap {
			truncated = true
			return false, nil
		}
		row := make([]string, len(cols))
		for i, c := range cols {
			if c == "key" {
				row[i] = k
			} else {
				v, _ := getV()
				row[i] = clip(v)
			}
		}
		rows = append(rows, row)
		return true, nil
	})
	if err != nil {
		return err
	}
	types := make([]string, len(cols))
	for i := range types {
		types[i] = "text"
	}
	ex.setGrid(cols, types, rows)
	ex.res.Truncated = truncated
	ex.msg("SELECT %d", len(rows))
	if truncated {
		ex.msg("NOTICE:  only the first %d rows are shown; use LIMIT and OFFSET to page", displayRowCap)
	}
	return nil
}

func clip(v string) string {
	if len(v) > displayValueCap {
		return v[:displayValueCap] + fmt.Sprintf("… [%s total]", humanBytes(int64(len(v))))
	}
	return v
}

func (ex *executor) insertStmt(p *parser) error {
	if err := p.expectWord("INTO"); err != nil {
		return err
	}
	if err := p.table(); err != nil {
		return err
	}
	keyFirst := true
	if p.punct("(") {
		a, err := p.column()
		if err != nil {
			return err
		}
		if err := p.expectPunct(","); err != nil {
			return err
		}
		b, err := p.column()
		if err != nil {
			return err
		}
		if err := p.expectPunct(")"); err != nil {
			return err
		}
		if a == b {
			return errors.New("column specified more than once")
		}
		keyFirst = a == "key"
	}
	if err := p.expectWord("VALUES"); err != nil {
		return err
	}
	var ops []engine.Op
	for {
		if err := p.expectPunct("("); err != nil {
			return err
		}
		a, err := p.literal()
		if err != nil {
			return err
		}
		if err := p.expectPunct(","); err != nil {
			return err
		}
		b, err := p.literal()
		if err != nil {
			return err
		}
		if err := p.expectPunct(")"); err != nil {
			return err
		}
		if !keyFirst {
			a, b = b, a
		}
		ops = append(ops, engine.PutOp([]byte(a), []byte(b)))
		if !p.punct(",") {
			break
		}
	}
	return ex.write(ops, fmt.Sprintf("INSERT 0 %d", len(ops)))
}

func (ex *executor) updateStmt(p *parser) error {
	if err := p.table(); err != nil {
		return err
	}
	if err := p.expectWord("SET"); err != nil {
		return err
	}
	c, err := p.column()
	if err != nil {
		return err
	}
	if c != "value" {
		return errors.New("cannot change key with UPDATE; INSERT the new key and DELETE the old one inside BEGIN/COMMIT")
	}
	if err := p.expectPunct("="); err != nil {
		return err
	}
	val, err := p.literal()
	if err != nil {
		return err
	}
	conds, err := p.where()
	if err != nil {
		return err
	}
	var ops []engine.Op
	err = ex.scan(conds, false, func(k string, _ func() (string, error)) (bool, error) {
		ops = append(ops, engine.PutOp([]byte(k), []byte(val)))
		return true, nil
	})
	if err != nil {
		return err
	}
	return ex.write(ops, fmt.Sprintf("UPDATE %d", len(ops)))
}

func (ex *executor) deleteStmt(p *parser) error {
	if err := p.expectWord("FROM"); err != nil {
		return err
	}
	if err := p.table(); err != nil {
		return err
	}
	conds, err := p.where()
	if err != nil {
		return err
	}
	var ops []engine.Op
	err = ex.scan(conds, false, func(k string, _ func() (string, error)) (bool, error) {
		ops = append(ops, engine.DeleteOp([]byte(k)))
		return true, nil
	})
	if err != nil {
		return err
	}
	return ex.write(ops, fmt.Sprintf("DELETE %d", len(ops)))
}

// write applies ops now, or queues them if a transaction is open.
// Outside a transaction one statement is one atomic batch (split only if it
// has more than 10,000 rows, which the message says).
func (ex *executor) write(ops []engine.Op, tag string) error {
	if ex.inTx {
		ex.tx = append(ex.tx, ops...)
		ex.msg("%s  (queued until COMMIT)", tag)
		return nil
	}
	chunks := 0
	for start := 0; start < len(ops); start += engine.MaxBatchOps {
		end := start + engine.MaxBatchOps
		if end > len(ops) {
			end = len(ops)
		}
		if err := ex.db.Batch(ops[start:end]); err != nil {
			if chunks > 0 {
				return fmt.Errorf("%v (the first %d rows were already written)", err, start)
			}
			return err
		}
		chunks++
		ex.res.Wrote = true
	}
	if chunks > 1 {
		ex.msg("%s  (written as %d batches of up to %d rows; each batch is atomic, the statement as a whole is not)", tag, chunks, engine.MaxBatchOps)
	} else {
		ex.msg("%s", tag)
	}
	return nil
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.2f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.2f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f kB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d bytes", n)
}