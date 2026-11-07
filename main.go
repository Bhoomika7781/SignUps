package main

import (
	"fmt"
	"os"

	"invx/internal/database"
)

func main() {
	if len(os.Args) < 3 || os.Args[1] != "web" {
		fmt.Println("Usage: go run . web <database-file>")
		return
	}

	db, err := database.Open(os.Args[2])
	if err != nil {
		fmt.Println("Database error:", err)
		return
	}
	defer db.Close()

	if err := runWeb(db, ":8080"); err != nil {
		fmt.Println("Web server error:", err)
	}
}
