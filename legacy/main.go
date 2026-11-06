package main

import (
	"fmt"
	"os"
)

// Display all available commands.
func printUsage() {
	fmt.Print(`
================================================
             CRASHDB DATABASE
          CRASH-SAFE STORAGE ENGINE
================================================

AVAILABLE COMMANDS:

  PUT     Store a key-value pair
  GET     Retrieve a stored value
  DELETE  Delete a key
  WEB     Start the web dashboard

COMMAND USAGE:

  go run . put database.kv name Prajwal

  go run . get database.kv name

  go run . delete database.kv name

  go run . web database.kv

================================================
`)
}

func main() {

	// Check whether a command was provided.
	if len(os.Args) < 2 {
		printUsage()
		return
	}

	command := os.Args[1]

	// ============================================
	// START WEB DASHBOARD
	// ============================================

	if command == "web" {

		if len(os.Args) < 3 || len(os.Args) > 4 {
			printUsage()
			return
		}

		path := os.Args[2]
		address := ":8080"

		if len(os.Args) == 4 {
			address = os.Args[3]
		}

		db, err := OpenDB(path)

		if err != nil {
			fmt.Println("Database error:", err)
			return
		}

		defer db.Close()

		fmt.Println()
		fmt.Println("========================================")
		fmt.Println("       CRASHDB WEB DASHBOARD")
		fmt.Println("========================================")
		fmt.Println("Database:", path)
		fmt.Println("Starting web server...")

		if err := runWeb(db, address); err != nil {
			fmt.Println("Web server error:", err)
		}

		return
	}

	// ============================================
	// DATABASE CLI COMMANDS
	// ============================================

	if len(os.Args) < 4 {
		printUsage()
		return
	}

	path := os.Args[2]

	db, err := OpenDB(path)

	if err != nil {
		fmt.Println("Database error:", err)
		return
	}

	defer db.Close()

	switch command {

	// ============================================
	// PUT
	// ============================================

	case "put":

		if len(os.Args) != 5 {
			fmt.Println("Usage: go run . put <file> <key> <value>")
			return
		}

		key := os.Args[3]
		value := os.Args[4]

		err := db.Put(key, []byte(value))

		if err != nil {
			fmt.Println("PUT failed:", err)
			return
		}

		fmt.Println()
		fmt.Println("[SUCCESS] Value stored successfully!")
		fmt.Println("Key:", key)
		fmt.Println("Value:", value)

	// ============================================
	// GET
	// ============================================

	case "get":

		if len(os.Args) != 4 {
			fmt.Println("Usage: go run . get <file> <key>")
			return
		}

		key := os.Args[3]

		value, found, err := db.Get(key)

		if err != nil {
			fmt.Println("GET failed:", err)
			return
		}

		if !found {
			fmt.Println("[NOT FOUND] Key does not exist.")
			return
		}

		fmt.Println()
		fmt.Println("[SUCCESS] Key found!")
		fmt.Println("Key:", key)
		fmt.Printf("Value: %s\n", value)

	// ============================================
	// DELETE
	// ============================================

	case "delete":

		if len(os.Args) != 4 {
			fmt.Println("Usage: go run . delete <file> <key>")
			return
		}

		key := os.Args[3]

		err := db.Delete(key)

		if err != nil {
			fmt.Println("DELETE failed:", err)
			return
		}

		fmt.Println()
		fmt.Println("[SUCCESS] Key deleted!")
		fmt.Println("Deleted key:", key)

	// ============================================
	// UNKNOWN COMMAND
	// ============================================

	default:

		fmt.Println("[ERROR] Unknown command:", command)
		printUsage()
	}
}
