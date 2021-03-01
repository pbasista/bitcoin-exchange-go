// Bitcoin exchange.
//
// An exercise implementation of a bitcoin exchange with limited functionality.
//
// The order matching uses the following principle:
// When a new order of any type arrives,
// it fulfills the suitable existing standing orders
// at their limit prices.
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"regexp"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

var URL_PATH_PARTS = regexp.MustCompile("^/([a-zA-Z0-9_]+)/(.*)$")

// The transaction isolation level "repeatable read":
// https://www.postgresql.org/docs/current/transaction-iso.html#XACT-REPEATABLE-READ
// is required for topping up the balance and executing orders
// in order to avoid inconsistent outcome of concurrent transactions
// that use the read-modify-write sequence of operations.
var DSN string = "host=localhost dbname=bitcoin_exchange default_transaction_isolation='repeatable read'"

var DB *gorm.DB

func parseFlags() (init bool, port uint) {
	flag.BoolVar(&init, "init", false, "Initialize the database.")
	flag.UintVar(&port, "port", 8000, "Port on which to start the HTTP server.")
	flag.Parse()
	return init, port
}

func initDatabase() {
	log.Printf("Initializing the database.")
	DB.AutoMigrate(&User{}, &StandingOrder{})
	log.Printf("The database has been initialized.")
}

func registerHandlers() {
	log.Printf("Registering HTTP handlers.")
	http.HandleFunc("/register/", registerUserHandler)
	http.HandleFunc("/balance", balanceHandler)
	http.HandleFunc("/market_order", marketOrderHandler)
	http.HandleFunc("/standing_order", standingOrderHandler)
	http.HandleFunc("/standing_order/", standingOrderHandler)
	log.Printf("The HTTP handlers have been registered.")
}

func main() {
	init, port := parseFlags()
	var err error
	DB, err = gorm.Open(postgres.Open(DSN), &gorm.Config{})
	if err != nil {
		log.Fatal("Unable to connect to the database.")
	}
	if init {
		initDatabase()
		return
	}
	registerHandlers()
	log.Fatal(http.ListenAndServe(fmt.Sprintf(":%v", port), nil))
}
