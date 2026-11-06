package main

// ui.go: `kvdb ui <file-or-folder> [--port N]` serves a local admin page
// (styled like a SQL admin tool) on http://127.0.0.1:N.
//
//   - Every *.db file in the folder is a "database" you can switch to,
//     create (CREATE DATABASE) or drop (DROP DATABASE).
//   - The Torture tab runs the real torture tool (bin/torture) in its own
//     folder, <folder>/torture-ui-data, and streams its output to the page.
//
// ui.html is embedded in the binary, so everything works offline.
// Do not point the UI at a database another kvdb process is writing at the
// same time. Ctrl+C is safe: it is just a kill, and every ACKed write is
// already fsynced.

import (
	"bufio"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"kvdb/src/engine"
)

//go:embed ui.html
var uiPage []byte

// tortureDBName is how the torture tool's database shows up in the list.
const tortureDBName = "torture_run.db"

type uiServer struct {
	dir      string
	initial  string
	opts     engine.Options
	addr     string
	mu       sync.Mutex
	dbs      map[string]*engine.DB
	paths    map[string]string
	torture  tortureRun
	selfPath string
}

func cmdUI(path string, opts engine.Options, rest []string) {
	port := 8080
	for i := 0; i < len(rest); i++ {
		if (rest[i] == "--port" || rest[i] == "-port") && i+1 < len(rest) {
			p, err := strconv.Atoi(rest[i+1])
			if err != nil {
				fail("bad --port %q", rest[i+1])
			}
			port = p
			i++
		}
	}

	s := &uiServer{opts: opts, dbs: map[string]*engine.DB{}, paths: map[string]string{}}
	if st, err := os.Stat(path); err == nil && st.IsDir() {
		s.dir = path
	} else {
		s.dir = filepath.Dir(path)
		s.initial = filepath.Base(path)
	}
	if abs, err := filepath.Abs(s.dir); err == nil {
		s.dir = abs
	}
	if s.initial == "" {
		list, _ := s.List()
		for _, d := range list {
			if !d.Torture {
				s.initial = d.Name
				break
			}
		}
		if s.initial == "" {
			s.initial = "main.db"
		}
	}
	if _, _, err := s.Open(s.initial); err != nil {
		fail("open %s: %v", s.initial, err)
	}
	if exe, err := os.Executable(); err == nil {
		s.selfPath = exe
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(uiPage)
	})
	mux.HandleFunc("/api/dbs", s.handleDBs)
	mux.HandleFunc("/api/info", s.handleInfo)
	mux.HandleFunc("/api/query", s.handleQuery)
	mux.HandleFunc("/api/get", s.handleGet)
	mux.HandleFunc("/api/torture/start", s.handleTortureStart)
	mux.HandleFunc("/api/torture/stop", s.handleTortureStop)
	mux.HandleFunc("/api/torture/status", s.handleTortureStatus)
	mux.HandleFunc("/api/torture/oracle", s.handleTortureOracle)

	s.addr = "127.0.0.1:" + strconv.Itoa(port)
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		fail("cannot listen on %s: %v (try --port 8081)", s.addr, err)
	}
	fmt.Printf("kvdb admin UI\n  databases folder: %s\n  open http://%s in your browser\n  press Ctrl+C to stop\n", s.dir, s.addr)
	if err := http.Serve(ln, mux); err != nil {
		fail("%v", err)
	}
}

// ---------------------------------------------------------------------------
// catalog (implements the interface used by sql.go)

func (s *uiServer) tortureDir() string { return filepath.Join(s.dir, "torture-ui-data") }

func (s *uiServer) resolve(name string) (string, error) {
	if name == tortureDBName {
		return filepath.Join(s.tortureDir(), "test.db"), nil
	}
	if name == "" || name == "." || name == ".." || name != filepath.Base(name) || strings.ContainsAny(name, `/\:*?"<>|`) {
		return "", fmt.Errorf("invalid database name %q", name)
	}
	if strings.HasSuffix(name, engine.TempSuffix) {
		return "", fmt.Errorf("%q is a compaction temp file, not a database", name)
	}
	return filepath.Join(s.dir, name), nil
}

// Addr describes how the user is connected (shown by \conninfo).
func (s *uiServer) Addr() string {
	if s.addr == "" {
		return "kvdb shell"
	}
	return "kvdb ui at http://" + s.addr
}

// Open returns the open database, opening it (with recovery) on first use.
func (s *uiServer) Open(name string) (*engine.DB, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.openLocked(name, false)
}

func (s *uiServer) openLocked(name string, create bool) (*engine.DB, string, error) {
	if db, ok := s.dbs[name]; ok {
		return db, s.paths[name], nil
	}
	path, err := s.resolve(name)
	if err != nil {
		return nil, "", err
	}
	if name == tortureDBName && s.torture.isRunning() {
		return nil, "", errors.New("the torture tool is using this database right now; wait until the run finishes")
	}
	_, statErr := os.Stat(path)
	exists := statErr == nil
	if !exists && !create && name != s.initial {
		return nil, "", fmt.Errorf("database %q does not exist", name)
	}
	if exists && create {
		return nil, "", fmt.Errorf("database %q already exists", name)
	}
	db, err := engine.Open(path, s.opts)
	if err != nil {
		return nil, "", err
	}
	s.dbs[name] = db
	s.paths[name] = path
	return db, path, nil
}

func (s *uiServer) closeLocked(name string) {
	if db, ok := s.dbs[name]; ok {
		db.Close()
		delete(s.dbs, name)
		delete(s.paths, name)
	}
}

func (s *uiServer) Create(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if name == tortureDBName {
		return errors.New("that name is reserved for the torture tool")
	}
	_, _, err := s.openLocked(name, true)
	return err
}

func (s *uiServer) Drop(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if name == tortureDBName {
		return errors.New("the torture database is managed by the torture tool (it is wiped at the start of every run)")
	}
	path, err := s.resolve(name)
	if err != nil {
		return err
	}
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("database %q does not exist", name)
	}
	s.closeLocked(name) // Windows cannot delete an open file
	if err := os.Remove(path); err != nil {
		return err
	}
	os.Remove(path + engine.TempSuffix)
	return nil
}

func (s *uiServer) List() ([]dbEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := map[string]bool{}
	var out []dbEntry
	add := func(name, path string, torture bool) {
		if seen[name] {
			return
		}
		seen[name] = true
		e := dbEntry{Name: name, Torture: torture}
		if st, err := os.Stat(path); err == nil {
			e.Size = st.Size()
		}
		if db, ok := s.dbs[name]; ok {
			e.Open, e.Keys, e.Size = true, db.Len(), db.Size()
		}
		out = append(out, e)
	}
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	for _, en := range entries {
		if en.Type().IsRegular() && strings.HasSuffix(en.Name(), ".db") {
			add(en.Name(), filepath.Join(s.dir, en.Name()), false)
		}
	}
	for name, path := range s.paths {
		if name != tortureDBName {
			add(name, path, false)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	tp := filepath.Join(s.tortureDir(), "test.db")
	if _, err := os.Stat(tp); err == nil && !s.torture.isRunning() {
		add(tortureDBName, tp, true)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// HTTP handlers

func dbParam(r *http.Request, s *uiServer) string {
	if n := r.URL.Query().Get("db"); n != "" {
		return n
	}
	return s.initial
}

func (s *uiServer) handleDBs(w http.ResponseWriter, r *http.Request) {
	list, err := s.List()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, map[string]any{"dir": s.dir, "initial": s.initial, "databases": list, "torture": tortureDBName})
}

func (s *uiServer) handleInfo(w http.ResponseWriter, r *http.Request) {
	name := dbParam(r, s)
	db, path, err := s.Open(name)
	if err != nil {
		writeJSON(w, map[string]any{"error": err.Error()})
		return
	}
	info := db.Info()
	live, dead := db.Stats()
	_, tempErr := os.Stat(path + engine.TempSuffix)
	writeJSON(w, map[string]any{
		"name":           name,
		"path":           path,
		"dir":            s.dir,
		"keys":           db.Len(),
		"size":           db.Size(),
		"live":           live,
		"dead":           dead,
		"maxBytes":       db.MaxBytes(),
		"recoveryMs":     float64(info.Duration.Microseconds()) / 1000,
		"records":        info.Records,
		"truncatedBytes": info.TruncatedBytes,
		"removedTemp":    info.RemovedTemp,
		"tempFile":       tempErr == nil,
		"torture":        name == tortureDBName,
		"os":             runtime.GOOS + "/" + runtime.GOARCH,
		"pid":            os.Getpid(),
		"addr":           s.addr,
	})
}

func (s *uiServer) handleQuery(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SQL string `json:"sql"`
		DB  string `json:"db"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.DB == "" {
		req.DB = s.initial
	}
	writeJSON(w, runSQL(s, req.DB, req.SQL))
}

func (s *uiServer) handleGet(w http.ResponseWriter, r *http.Request) {
	db, _, err := s.Open(dbParam(r, s))
	if err != nil {
		writeJSON(w, map[string]any{"error": err.Error()})
		return
	}
	key := r.URL.Query().Get("key")
	v, ok, err := db.Get([]byte(key))
	resp := map[string]any{"key": key, "found": ok, "value": string(v), "length": len(v)}
	if err != nil {
		resp["error"] = err.Error()
	}
	writeJSON(w, resp)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(v)
}

// ---------------------------------------------------------------------------
// torture runner: starts bin/torture as a child process and keeps its output.

type tortureRun struct {
	mu       sync.Mutex
	cmd      *exec.Cmd
	running  bool
	lines    []string
	base     int // number of lines dropped from the front
	exitCode int
	exitMsg  string
	started  time.Time
	finished time.Time
	command  string
	runID    int
}

func (t *tortureRun) isRunning() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.running
}

func (t *tortureRun) add(line string) {
	t.mu.Lock()
	t.lines = append(t.lines, line)
	if len(t.lines) > 40000 {
		drop := len(t.lines) - 30000
		t.lines = append([]string(nil), t.lines[drop:]...)
		t.base += drop
	}
	t.mu.Unlock()
}

func (s *uiServer) tortureBinary() string {
	name := "torture"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return filepath.Join(filepath.Dir(s.selfPath), name)
}

var tortureModes = map[string]bool{"mixed": true, "large-values": true, "large-batch": true,
	"compaction": true, "recovery-crash": true, "disk-full": true, "million": true}

func (s *uiServer) handleTortureStart(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Mode       string `json:"mode"`
		Iterations int    `json:"iterations"`
		Seed       int64  `json:"seed"`
		Writers    int    `json:"writers"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, map[string]any{"error": err.Error()})
		return
	}
	if !tortureModes[req.Mode] {
		writeJSON(w, map[string]any{"error": "unknown mode " + req.Mode})
		return
	}
	if req.Iterations < 1 || req.Iterations > 100000 {
		req.Iterations = 20
	}
	if req.Writers < 2 || req.Writers > 16 {
		req.Writers = 2
	}
	if req.Seed <= 0 {
		req.Seed = time.Now().UnixNano() % 1_000_000_000
	}
	bin := s.tortureBinary()
	if _, err := os.Stat(bin); err != nil {
		writeJSON(w, map[string]any{"error": "cannot find " + bin + ". Build it first: go build -o bin/ ./src/kvdb ./torture"})
		return
	}

	t := &s.torture
	t.mu.Lock()
	if t.running {
		t.mu.Unlock()
		writeJSON(w, map[string]any{"error": "a torture run is already in progress"})
		return
	}
	t.running = true
	t.mu.Unlock()

	// The torture tool wipes its folder; close our handle first (Windows
	// cannot delete open files).
	s.mu.Lock()
	s.closeLocked(tortureDBName)
	s.mu.Unlock()

	args := []string{"--mode", req.Mode, "--iterations", strconv.Itoa(req.Iterations),
		"--seed", strconv.FormatInt(req.Seed, 10), "--writers", strconv.Itoa(req.Writers),
		"--dir", s.tortureDir(), "--kvdb", s.selfPath}
	cmd := exec.Command(bin, args...)
	pr, pw, err := os.Pipe()
	if err != nil {
		t.mu.Lock()
		t.running = false
		t.mu.Unlock()
		writeJSON(w, map[string]any{"error": err.Error()})
		return
	}
	cmd.Stdout, cmd.Stderr = pw, pw
	if err := cmd.Start(); err != nil {
		pr.Close()
		pw.Close()
		t.mu.Lock()
		t.running = false
		t.mu.Unlock()
		writeJSON(w, map[string]any{"error": err.Error()})
		return
	}
	pw.Close()

	short := fmt.Sprintf("torture --iterations %d --seed %d --mode %s --writers %d", req.Iterations, req.Seed, req.Mode, req.Writers)
	t.mu.Lock()
	t.cmd = cmd
	t.lines, t.base = nil, 0
	t.exitCode, t.exitMsg = 0, ""
	t.started, t.finished = time.Now(), time.Time{}
	t.command = short
	t.runID++
	t.mu.Unlock()

	go func() {
		br := bufio.NewReader(pr)
		for {
			line, err := br.ReadString('\n')
			if line != "" {
				t.add(strings.TrimRight(line, "\r\n"))
			}
			if err != nil {
				break
			}
		}
		pr.Close()
		werr := cmd.Wait()
		t.mu.Lock()
		t.running = false
		t.finished = time.Now()
		t.exitCode = 0
		if werr != nil {
			t.exitCode = -1
			var ee *exec.ExitError
			if errors.As(werr, &ee) {
				t.exitCode = ee.ExitCode()
			}
			t.exitMsg = werr.Error()
		}
		t.mu.Unlock()
	}()
	writeJSON(w, map[string]any{"ok": true, "command": short, "seed": req.Seed})
}

func (s *uiServer) handleTortureStop(w http.ResponseWriter, r *http.Request) {
	t := &s.torture
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.running && t.cmd != nil && t.cmd.Process != nil {
		t.cmd.Process.Kill()
		t.exitMsg = "stopped by user"
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *uiServer) handleTortureStatus(w http.ResponseWriter, r *http.Request) {
	since, _ := strconv.Atoi(r.URL.Query().Get("since"))
	t := &s.torture
	t.mu.Lock()
	defer t.mu.Unlock()
	if since < t.base {
		since = t.base
	}
	idx := since - t.base
	if idx > len(t.lines) {
		idx = len(t.lines)
	}
	lines := append([]string{}, t.lines[idx:]...)
	elapsed := time.Duration(0)
	if !t.started.IsZero() {
		end := t.finished
		if t.running || end.IsZero() {
			end = time.Now()
		}
		elapsed = end.Sub(t.started)
	}
	writeJSON(w, map[string]any{
		"running": t.running, "lines": lines, "next": t.base + len(t.lines),
		"exitCode": t.exitCode, "exitMsg": t.exitMsg, "command": t.command,
		"elapsedMs": elapsed.Milliseconds(), "started": !t.started.IsZero(), "runId": t.runID,
	})
}

// handleTortureOracle returns the end of the torture tool's oracle log.
func (s *uiServer) handleTortureOracle(w http.ResponseWriter, r *http.Request) {
	f, err := os.Open(filepath.Join(s.tortureDir(), "oracle.log"))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			writeJSON(w, map[string]any{"text": "(no oracle log yet: start a torture run)"})
			return
		}
		writeJSON(w, map[string]any{"error": err.Error()})
		return
	}
	defer f.Close()
	st, _ := f.Stat()
	const tail = 96 << 10
	off := int64(0)
	if st.Size() > tail {
		off = st.Size() - tail
	}
	f.Seek(off, io.SeekStart)
	b, _ := io.ReadAll(f)
	text := string(b)
	if off > 0 {
		if i := strings.IndexByte(text, '\n'); i >= 0 {
			text = text[i+1:]
		}
	}
	writeJSON(w, map[string]any{"text": text, "size": st.Size()})
}