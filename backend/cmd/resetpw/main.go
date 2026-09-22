package main

import (
	"database/sql"
	"flag"
	"fmt"
	"log"
	"os"

	_ "github.com/jackc/pgx/v5/stdlib"
	"golang.org/x/crypto/bcrypt"
)

func main() {
	defaultDSN := os.Getenv("NOVA_DB_DSN")
	if defaultDSN == "" {
		defaultDSN = "postgres://postgres:nova@127.0.0.1:5434/novaworkbench?sslmode=disable"
	}
	dsn := flag.String("dsn", defaultDSN, "postgres dsn (falls back to $NOVA_DB_DSN)")
	pw := flag.String("password", "", "new admin password")
	flag.Parse()
	if *pw == "" {
		log.Fatal("--password required")
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(*pw), bcrypt.DefaultCost)
	if err != nil {
		log.Fatalf("bcrypt: %v", err)
	}

	db, err := sql.Open("pgx", *dsn)
	if err != nil {
		log.Fatalf("open: %v", err)
	}
	defer db.Close()

	res, err := db.Exec(
		`UPDATE users SET password_hash = $1, updated_at = now() WHERE username = 'admin'`,
		string(hash),
	)
	if err != nil {
		log.Fatalf("update: %v", err)
	}
	n, _ := res.RowsAffected()
	fmt.Printf("admin password reset — rows affected: %d\n", n)
}
