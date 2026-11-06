package main

import (
	"bytes"
	"testing"
)

func TestDatabasePersistence(t *testing.T) {
	path := t.TempDir() + "/database.kv"

	// Open database.
	db, err := OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}

	// Store a value.
	if err := db.Put("name", []byte("Alice")); err != nil {
		t.Fatal(err)
	}

	// Retrieve it.
	value, found, err := db.Get("name")
	if err != nil || !found || !bytes.Equal(value, []byte("Alice")) {
		t.Fatal("PUT or GET failed")
	}

	// Close and reopen the database.
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db, err = OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Check that the value survived reopening.
	value, found, err = db.Get("name")
	if err != nil || !found || !bytes.Equal(value, []byte("Alice")) {
		t.Fatal("data did not survive reopening")
	}

	// Delete and verify.
	if err := db.Delete("name"); err != nil {
		t.Fatal(err)
	}

	_, found, err = db.Get("name")
	if err != nil || found {
		t.Fatal("DELETE failed")
	}
}
