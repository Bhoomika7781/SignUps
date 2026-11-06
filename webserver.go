
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"sync"
	"time"
)

// ========================================
// ACTIVITY STRUCT
// ========================================

type Activity struct {
	Operation string    `json:"operation"`
	Key       string    `json:"key"`
	Value     string    `json:"value"`
	Status    string    `json:"status"`
	Time      time.Time `json:"time"`
}

// ========================================
// WEB SERVER STRUCT
// ========================================

type WebServer struct {
	db      *DB
	started time.Time

	mu         sync.Mutex
	operations uint64
	recent     []Activity
}

// ========================================
// RECORD DATABASE ACTIVITY
// ========================================

func (s *WebServer) record(op, key, value, status string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.operations++

	s.recent = append(s.recent, Activity{
		Operation: op,
		Key:       key,
		Value:     value,
		Status:    status,
		Time:      time.Now(),
	})

	if len(s.recent) > 30 {
		s.recent = s.recent[len(s.recent)-30:]
	}
}

// ========================================
// SEND JSON RESPONSE
// ========================================

func sendJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)

	_ = json.NewEncoder(w).Encode(data)
}

// ========================================
// DATABASE STATISTICS
// GET /api/stats
// ========================================

func (s *WebServer) stats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	keys, bytes, err := s.db.Stats()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	s.mu.Lock()
	ops := s.operations
	s.mu.Unlock()

	sendJSON(w, http.StatusOK, map[string]any{
		"status":     "online",
		"totalKeys":  keys,
		"fileBytes":  bytes,
		"operations": ops,
		"uptime":     int64(time.Since(s.started).Seconds()),
	})
}

// ========================================
// LIST ALL RECORDS
// GET /api/records
// ========================================

func (s *WebServer) records(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	items, err := s.db.List()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	type Record struct {
		Key   string `json:"key"`
		Value string `json:"value"`
	}

	result := make([]Record, 0, len(items))

	for key, value := range items {
		result = append(result, Record{
			Key:   key,
			Value: string(value),
		})
	}

	sort.Slice(result, func(i, j int) bool {
		return result[i].Key < result[j].Key
	})

	sendJSON(w, http.StatusOK, result)
}

// ========================================
// PUT OPERATION
// POST /api/put
// ========================================

func (s *WebServer) put(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var input struct {
		Key   string `json:"key"`
		Value string `json:"value"`
	}

	r.Body = http.MaxBytesReader(w, r.Body, 2<<20)

	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		sendJSON(w, http.StatusBadRequest, map[string]string{
			"error": "Invalid request",
		})
		return
	}

	if err := s.db.Put(input.Key, []byte(input.Value)); err != nil {
		sendJSON(w, http.StatusBadRequest, map[string]string{
			"error": err.Error(),
		})
		return
	}

	s.record("PUT", input.Key, input.Value, "SUCCESS")

	sendJSON(w, http.StatusOK, map[string]string{
		"message": "Value stored successfully",
	})
}

// ========================================
// GET OPERATION
// GET /api/get?key=name
// ========================================

func (s *WebServer) get(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	key := r.URL.Query().Get("key")

	if key == "" {
		sendJSON(w, http.StatusBadRequest, map[string]string{
			"error": "Key is required",
		})
		return
	}

	value, found, err := s.db.Get(key)

	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if !found {
		sendJSON(w, http.StatusNotFound, map[string]string{
			"error": "Key not found",
		})
		return
	}

	s.record("GET", key, string(value), "SUCCESS")

	sendJSON(w, http.StatusOK, map[string]string{
		"key":   key,
		"value": string(value),
	})
}

// ========================================
// DELETE OPERATION
// POST /api/delete
// ========================================

func (s *WebServer) delete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var input struct {
		Key string `json:"key"`
	}

	r.Body = http.MaxBytesReader(w, r.Body, 64*1024)

	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		sendJSON(w, http.StatusBadRequest, map[string]string{
			"error": "Invalid request",
		})
		return
	}

	if err := s.db.Delete(input.Key); err != nil {
		sendJSON(w, http.StatusBadRequest, map[string]string{
			"error": err.Error(),
		})
		return
	}

	s.record("DELETE", input.Key, "", "SUCCESS")

	sendJSON(w, http.StatusOK, map[string]string{
		"message": "Key deleted successfully",
	})
}

// ========================================
// NUMERIC CALCULATION
// POST /api/calculate
// ========================================

func (s *WebServer) calculate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var input struct {
		Key       string `json:"key"`
		Type      string `json:"type"`
		Operation string `json:"operation"`
		Operand   string `json:"operand"`
	}

	r.Body = http.MaxBytesReader(w, r.Body, 64*1024)

	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		sendJSON(w, http.StatusBadRequest, map[string]string{
			"error": "Invalid request",
		})
		return
	}

	result, err := s.db.Calculate(
		input.Key,
		input.Type,
		input.Operation,
		input.Operand,
	)

	if err != nil {
		s.record("CALCULATE", input.Key, "", "FAILED")

		sendJSON(w, http.StatusBadRequest, map[string]string{
			"error": err.Error(),
		})
		return
	}

	operationNames := map[string]string{
		"+": "ADD",
		"-": "SUBTRACT",
		"*": "MULTIPLY",
		"/": "DIVIDE",
	}

	opName := operationNames[input.Operation]
	if opName == "" {
		opName = "CALCULATE"
	}

	s.record(opName, input.Key, result, "SUCCESS")

	sendJSON(w, http.StatusOK, map[string]string{
		"message":   "Calculation completed successfully",
		"key":       input.Key,
		"type":      input.Type,
		"operation": input.Operation,
		"operand":   input.Operand,
		"value":     result,
	})
}

// ========================================
// RECENT DATABASE ACTIVITY
// GET /api/activity
// ========================================

func (s *WebServer) activity(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	s.mu.Lock()
	result := append([]Activity(nil), s.recent...)
	s.mu.Unlock()

	sendJSON(w, http.StatusOK, result)
}

// ========================================
// START WEB SERVER
// ========================================

func runWeb(db *DB, address string) error {
	server := &WebServer{
		db:      db,
		started: time.Now(),
	}

	mux := http.NewServeMux()

	mux.HandleFunc("/api/stats", server.stats)
	mux.HandleFunc("/api/records", server.records)
	mux.HandleFunc("/api/put", server.put)
	mux.HandleFunc("/api/get", server.get)
	mux.HandleFunc("/api/delete", server.delete)
	mux.HandleFunc("/api/activity", server.activity)
	mux.HandleFunc("/api/calculate", server.calculate)

	mux.Handle("/", http.FileServer(http.Dir("web")))

	fmt.Println()
	fmt.Println("========================================")
	fmt.Println("       CRASHDB WEB DASHBOARD")
	fmt.Println("========================================")
	fmt.Println("Database server is ready!")
	fmt.Println("Open http://localhost" + address)
	fmt.Println("API: /api/calculate")
	fmt.Println("========================================")

	return http.ListenAndServe(address, mux)
}
