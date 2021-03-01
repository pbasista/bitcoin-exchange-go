package main

import (
	"encoding/json"
	"log"
	"net/http"
	"strconv"

	"gorm.io/gorm"
)

type CoinbasePrice struct {
	Base     string
	Currency string
	Amount   string
}

type CoinbaseResponse struct {
	Data CoinbasePrice
}

type Balance struct {
	BTC                   float64
	BTC_current_USD_value float64
	USD                   float64
}

type BalanceUpdate struct {
	TopupAmount float64 `json:"topup_amount"`
	Currency    string
}

func getBitcoinUSDPrice() (float64, error) {
	response, err := http.Get("https://api.coinbase.com/v2/prices/spot?currency=USD")
	if err != nil {
		log.Printf("Unable to get Bitcoin price in USD. Error: %v", err)
		return 0, err
	}
	defer response.Body.Close()
	var responseData CoinbaseResponse
	decoder := json.NewDecoder(response.Body)
	decoder.DisallowUnknownFields()
	err = decoder.Decode(&responseData)
	if err != nil {
		log.Printf("Unable to decode CoinbaseResponse from JSON. Error: %v", err)
		return 0, err
	}
	value, err := strconv.ParseFloat(responseData.Data.Amount, 64)
	if err != nil {
		log.Printf("Unable to convert provided string to float64. Error: %v", err)
		return 0, err
	}
	return value, nil
}

func getBalanceHandler(user *User, w http.ResponseWriter, r *http.Request) {
	bitcoinUsdPrice, err := getBitcoinUSDPrice()
	if err != nil {
		log.Printf("Unable to get Bitcoin USD price. Error: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	btcBalance := float64(user.BTCSatoshiBalance) / 100000000
	usdBalance := float64(user.USDCentsBalance) / 100
	balance := Balance{
		BTC:                   btcBalance,
		BTC_current_USD_value: btcBalance * bitcoinUsdPrice,
		USD:                   usdBalance,
	}
	log.Printf("Balance of user %v: %v", user.ID, balance)
	output, err := json.Marshal(balance)
	if err != nil {
		log.Printf("Unable to serialize Balance object to JSON. Error: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Write(output)
}

func postBalanceHandler(tx *gorm.DB, user *User, w http.ResponseWriter, r *http.Request) {
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	balanceUpdate := BalanceUpdate{}
	log.Printf("Request data: Type: %T, Value: %v", balanceUpdate, balanceUpdate)
	err := decoder.Decode(&balanceUpdate)
	if err != nil {
		tx.Rollback()
		log.Printf("Unable to decode request body from JSON. Error: %v", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	log.Printf("Balance update request for user %v: %v", user.ID, balanceUpdate)
	if balanceUpdate.Currency != "BTC" && balanceUpdate.Currency != "USD" {
		tx.Rollback()
		log.Printf("Unknown currency %v has been provided.", balanceUpdate.Currency)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if balanceUpdate.Currency == "USD" {
		user.USDCentsBalance += int64(balanceUpdate.TopupAmount * 100)
	}
	if balanceUpdate.Currency == "BTC" {
		user.BTCSatoshiBalance += int64(balanceUpdate.TopupAmount * 100000000)
	}
	result := tx.Save(user)
	if result.Error != nil {
		tx.Rollback()
		log.Printf("Unable to save user %v. Error: %v", user, result.Error)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	result = tx.Commit()
	if result.Error != nil {
		tx.Rollback()
		log.Printf("Unable to commit the transaction. Error: %v", result.Error)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	log.Printf("Balance of user with ID %v has been updated.", user.ID)
}

func balanceHandler(w http.ResponseWriter, r *http.Request) {
	tx := DB.Begin()
	user := getAuthenticatedUser(tx, r)
	if user == nil {
		tx.Rollback()
		log.Printf("Unable to get authenticated user.")
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	switch r.Method {
	case "GET":
		tx.Rollback() // DB transaction is unnecessary in this case
		getBalanceHandler(user, w, r)
	case "POST":
		// the POST handler commits or rolls back the transaction as necessary
		postBalanceHandler(tx, user, w, r)
	default:
		tx.Rollback()
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}
