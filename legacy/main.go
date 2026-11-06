package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 4 {
		fmt.Println("Usage:")
		fmt.Println("  go run . put <database-file> <key> <value>")
		fmt.Println("  go run . get <database-file> <key>")
		fmt.Println("  go run . delete <database-file> <key>")
		return
	}

	command := os.Args[1]
	path := os.Args[2]

	db, err := OpenDB(path)
	if err != nil {
		fmt.Println("Database error:", err)
		return
	}
	defer db.Close()

	switch command {
	case "put":
		if len(os.Args) != 5 {
			fmt.Println("Usage: go run . put <database-file> <key> <value>")
			return
		}

		err := db.Put(os.Args[3], []byte(os.Args[4]))
		if err != nil {
			fmt.Println("PUT failed:", err)
			return
		}
		fmt.Println("ACK: value stored successfully")

	case "get":
		if len(os.Args) != 4 {
			fmt.Println("Usage: go run . get <database-file> <key>")
			return
		}

		value, found, err := db.Get(os.Args[3])
		if err != nil {
			fmt.Println("GET failed:", err)
			return
		}

		if !found {
			fmt.Println("Key not found")
		} else {
			fmt.Printf("Value: %s\n", value)
		}

	case "delete":
		if len(os.Args) != 4 {
			fmt.Println("Usage: go run . delete <database-file> <key>")
			return
		}

		if err := db.Delete(os.Args[3]); err != nil {
			fmt.Println("DELETE failed:", err)
			return
		}
		fmt.Println("ACK: key deleted")

	default:
		fmt.Println("Unknown command:", command)
	}
}
