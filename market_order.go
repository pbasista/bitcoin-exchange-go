package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"

	"gorm.io/gorm"
)

// A market order to buy or sell BTC.
type MarketOrder struct {
	Quantity float64
	Type     string
}

type MarketOrderOutcome struct {
	Quantity     float64
	AveragePrice float64 `json:"average_price"`
}

var NO_MATCHING_STANDING_ORDERS = errors.New("No matching standing orders.")

func getStandingSellOrders(tx *gorm.DB, satoshiUsdCentsLimitPrice float64, offset int64, size int64) (standingOrders []*StandingOrder, err error) {
	var result *gorm.DB
	if satoshiUsdCentsLimitPrice == 0 {
		result = tx.Preload("User").Where(&StandingOrder{Type: "SELL", State: "LIVE"}).Offset(int(offset)).Limit(int(size)).Order("limit_price asc").Find(&standingOrders)
	} else {
		result = tx.Preload("User").Where(&StandingOrder{Type: "SELL", State: "LIVE"}).Where("limit_price <= ?", satoshiUsdCentsLimitPrice).Offset(int(offset)).Limit(int(size)).Order("limit_price asc").Find(&standingOrders)
	}
	log.Printf("DB result: Type: %T, Value: %v", result, result)
	if err := result.Error; err != nil {
		return nil, err
	}
	if result.RowsAffected < size {
		return standingOrders, NO_MATCHING_STANDING_ORDERS
	}
	return standingOrders, nil
}

// Buy the specified amount of Satoshis via the provided standing order
// using the current user's cash balance.
func (user *User) BuyViaStandingOrder(tx *gorm.DB, standingOrder *StandingOrder, satoshiAmount int64) (satisfiedSatoshiAmount int64, transactionUsdCentsAmount int64, fundsExhausted bool) {
	seller := standingOrder.User
	fundsExhausted = false
	// The first estimate of the satisfied Satoshi amount is the requested Satoshi amount.
	satisfiedSatoshiAmount = satoshiAmount
	satoshiAmountBuyLimit := int64(float64(user.USDCentsBalance) / standingOrder.LimitPrice)
	if satisfiedSatoshiAmount >= satoshiAmountBuyLimit {
		satisfiedSatoshiAmount = satoshiAmountBuyLimit
		fundsExhausted = true
	}
	// Assuming that the other party (seller in this case)
	// can always satisfy the remaining order's quantity at its limit price.
	if satisfiedSatoshiAmount > standingOrder.RemainingQuantity {
		satisfiedSatoshiAmount = standingOrder.RemainingQuantity
		fundsExhausted = false
	}
	// No checks are done at this point
	// because the invariant of users having enough funds
	// to satisfy the remaining quantities of all their live orders at limit prices
	// is supposed to always be true.
	transactionUsdCentsAmountFloat := float64(satisfiedSatoshiAmount) * standingOrder.LimitPrice
	transactionUsdCentsAmount = int64(transactionUsdCentsAmountFloat)
	user.USDCentsBalance -= transactionUsdCentsAmount
	seller.USDCentsBalance += transactionUsdCentsAmount
	user.BTCSatoshiBalance += satisfiedSatoshiAmount
	seller.BTCSatoshiBalance -= satisfiedSatoshiAmount
	standingOrder.AveragePrice = (standingOrder.AveragePrice*float64(standingOrder.FulfilledQuantity) + transactionUsdCentsAmountFloat) / float64(standingOrder.FulfilledQuantity+satisfiedSatoshiAmount)
	standingOrder.FulfilledQuantity += satisfiedSatoshiAmount
	standingOrder.RemainingQuantity -= satisfiedSatoshiAmount
	if standingOrder.RemainingQuantity == 0 {
		standingOrder.State = "FULFILLED"
	}
	result := tx.Save(standingOrder)
	if err := result.Error; err != nil {
		panic(err)
	}
	log.Printf("Seller: Type: %T, Value: %v", seller, seller)
	result = tx.Save(&seller)
	if err := result.Error; err != nil {
		panic(err)
	}
	result = tx.Save(user)
	if err := result.Error; err != nil {
		panic(err)
	}
	standingOrder.PerformWebhookRequest()
	return satisfiedSatoshiAmount, transactionUsdCentsAmount, fundsExhausted
}

// Buy the provided amount of Satoshis, if possible,
// by satisfying the existing standing orders
// using the user's available USD cents balance.
func (user *User) BuySatoshis(tx *gorm.DB, remainingSatoshiAmount int64, satoshiUsdCentsLimitPrice float64) (satisfiedSatoshiAmount int64, averageSatoshiUsdCentsPrice float64, err error) {
	var size int64 = 10
	satisfiedSatoshiAmount = 0
	var usdCentsAmount int64 = 0
	fundsExhausted := false
	defer func() {
		if p := recover(); p != nil {
			// modifying the function's return value
			err = fmt.Errorf("The transaction has been rolled back because of the following panic: %v", p)
		}
	}()
outerLoop:
	for offset := int64(0); remainingSatoshiAmount > 0; offset += size {
		standingOrders, err := getStandingSellOrders(tx, satoshiUsdCentsLimitPrice, offset, size)
		log.Printf("Standing orders: Type: %T, Value: %v", standingOrders, standingOrders)
		if err != nil && !errors.Is(err, NO_MATCHING_STANDING_ORDERS) {
			return 0, 0, err
		}
		for _, standingOrder := range standingOrders {
			log.Printf("Standing order: Type: %T, Value: %v", standingOrder, standingOrder)
			satisfiedSatoshiAmountFromOrder, transactionUsdCentsAmount, fundsExhausted := user.BuyViaStandingOrder(tx, standingOrder, remainingSatoshiAmount)
			usdCentsAmount += transactionUsdCentsAmount
			satisfiedSatoshiAmount += satisfiedSatoshiAmountFromOrder
			remainingSatoshiAmount -= satisfiedSatoshiAmountFromOrder
			if remainingSatoshiAmount == 0 {
				break
			}
			if fundsExhausted {
				break outerLoop
			}
		}
		if errors.Is(err, NO_MATCHING_STANDING_ORDERS) {
			// no more matching orders exist
			break
		}
	}
	if satisfiedSatoshiAmount == 0 {
		return 0, 0, NO_MATCHING_STANDING_ORDERS
	}
	if remainingSatoshiAmount > 0 {
		log.Print("Unable to satisfy the order in full quantity.")
		if fundsExhausted {
			log.Print("Reason: Insufficient cash balance.")
		} else {
			log.Print("Reason: No matching orders.")
		}
	}
	averageSatoshiUsdCentsPrice = float64(usdCentsAmount) / float64(satisfiedSatoshiAmount)
	return satisfiedSatoshiAmount, averageSatoshiUsdCentsPrice, nil
}

// Buy the provided amount of Bitcoin, if possible,
// by satisfying the existing standing orders
// using the user's available USD balance.
// If the provided limit price is nonzero,
// limit the matched standing orders to the ones
// whose sell price is at most as high as the provided limit price.
func (user *User) Buy(tx *gorm.DB, amount float64, limitPrice float64) (satisfiedQuantity float64, averagePrice float64, err error) {
	remainingSatoshiAmount := int64(amount * 100000000)
	satoshiUsdCentsLimitPrice := limitPrice / 1000000
	satisfiedSatoshiAmount, averageSatoshiUsdCentsPrice, err := user.BuySatoshis(tx, remainingSatoshiAmount, satoshiUsdCentsLimitPrice)
	satisfiedQuantity = float64(satisfiedSatoshiAmount) / 100000000
	averagePrice = averageSatoshiUsdCentsPrice * 1000000
	return satisfiedQuantity, averagePrice, err
}

func getStandingBuyOrders(tx *gorm.DB, satoshiUsdCentsLimitPrice float64, offset int64, size int64) (standingOrders []*StandingOrder, err error) {
	var result *gorm.DB
	if satoshiUsdCentsLimitPrice == 0 {
		result = tx.Preload("User").Where(&StandingOrder{Type: "BUY", State: "LIVE"}).Offset(int(offset)).Limit(int(size)).Order("limit_price desc").Find(&standingOrders)
	} else {
		result = tx.Preload("User").Where(&StandingOrder{Type: "BUY", State: "LIVE"}).Where("limit_price >= ?", satoshiUsdCentsLimitPrice).Offset(int(offset)).Limit(int(size)).Order("limit_price desc").Find(&standingOrders)
	}
	log.Printf("DB result: Type: %T, Value: %v", result, result)
	if err := result.Error; err != nil {
		return nil, err
	}
	if result.RowsAffected < size {
		return standingOrders, NO_MATCHING_STANDING_ORDERS
	}
	return standingOrders, nil
}

// Sell the specified amount of the current user's Satoshis
// via the provided standing order.
func (user *User) SellViaStandingOrder(tx *gorm.DB, standingOrder *StandingOrder, satoshiAmount int64) (satisfiedSatoshiAmount int64, transactionUsdCentsAmount int64, satoshisExhausted bool) {
	buyer := standingOrder.User
	satoshisExhausted = false
	// The first estimate of the satisfied Satoshi amount is the requested Satoshi amount.
	satisfiedSatoshiAmount = satoshiAmount
	satoshiAmountSellLimit := user.BTCSatoshiBalance
	if satisfiedSatoshiAmount >= satoshiAmountSellLimit {
		satisfiedSatoshiAmount = satoshiAmountSellLimit
		satoshisExhausted = true
	}
	// Assuming that the other party (buyer in this case)
	// can always satisfy the remaining order's quantity at its limit price.
	if satisfiedSatoshiAmount > standingOrder.RemainingQuantity {
		satisfiedSatoshiAmount = standingOrder.RemainingQuantity
		satoshisExhausted = false
	}
	// No checks are done at this point
	// because the invariant of users having enough BTC
	// to satisfy the remaining quantities of all their live orders
	// is supposed to always be true.
	transactionUsdCentsAmountFloat := float64(satisfiedSatoshiAmount) * standingOrder.LimitPrice
	transactionUsdCentsAmount = int64(transactionUsdCentsAmountFloat)
	user.USDCentsBalance += transactionUsdCentsAmount
	buyer.USDCentsBalance -= transactionUsdCentsAmount
	user.BTCSatoshiBalance -= satisfiedSatoshiAmount
	buyer.BTCSatoshiBalance += satisfiedSatoshiAmount
	standingOrder.AveragePrice = (standingOrder.AveragePrice*float64(standingOrder.FulfilledQuantity) + transactionUsdCentsAmountFloat) / float64(standingOrder.FulfilledQuantity+satisfiedSatoshiAmount)
	standingOrder.FulfilledQuantity += satisfiedSatoshiAmount
	standingOrder.RemainingQuantity -= satisfiedSatoshiAmount
	if standingOrder.RemainingQuantity == 0 {
		standingOrder.State = "FULFILLED"
	}
	result := tx.Save(standingOrder)
	if err := result.Error; err != nil {
		panic(err)
	}
	log.Printf("Buyer: Type: %T, Value: %v", buyer, buyer)
	result = tx.Save(&buyer)
	if err := result.Error; err != nil {
		panic(err)
	}
	result = tx.Save(user)
	if err := result.Error; err != nil {
		panic(err)
	}
	standingOrder.PerformWebhookRequest()
	return satisfiedSatoshiAmount, transactionUsdCentsAmount, satoshisExhausted
}

// Sell the provided amount of user's Satoshis, if possible,
// by satisfying the existing standing orders
// using the user's available Satoshi balance.
func (user *User) SellSatoshis(tx *gorm.DB, remainingSatoshiAmount int64, satoshiUsdCentsLimitPrice float64) (satisfiedSatoshiAmount int64, averageSatoshiUsdCentsPrice float64, err error) {
	var size int64 = 10
	satisfiedSatoshiAmount = 0
	var usdCentsAmount int64 = 0
	satoshisExhausted := false
	defer func() {
		if p := recover(); p != nil {
			// modifying the function's return value
			err = fmt.Errorf("The transaction has been rolled back because of the following panic: %v", p)
		}
	}()
outerLoop:
	for offset := int64(0); remainingSatoshiAmount > 0; offset += size {
		standingOrders, err := getStandingBuyOrders(tx, satoshiUsdCentsLimitPrice, offset, size)
		log.Printf("Standing orders: Type: %T, Value: %v", standingOrders, standingOrders)
		if err != nil && !errors.Is(err, NO_MATCHING_STANDING_ORDERS) {
			return 0, 0, err
		}
		for _, standingOrder := range standingOrders {
			log.Printf("Standing order: Type: %T, Value: %v", standingOrder, standingOrder)
			satisfiedSatoshiAmountFromOrder, transactionUsdCentsAmount, satoshisExhausted := user.SellViaStandingOrder(tx, standingOrder, remainingSatoshiAmount)
			usdCentsAmount += transactionUsdCentsAmount
			satisfiedSatoshiAmount += satisfiedSatoshiAmountFromOrder
			remainingSatoshiAmount -= satisfiedSatoshiAmountFromOrder
			if remainingSatoshiAmount == 0 {
				break
			}
			if satoshisExhausted {
				break outerLoop
			}
		}
		if errors.Is(err, NO_MATCHING_STANDING_ORDERS) {
			// no more matching orders exist
			break
		}
	}
	if satisfiedSatoshiAmount == 0 {
		return 0, 0, NO_MATCHING_STANDING_ORDERS
	}
	if remainingSatoshiAmount > 0 {
		log.Print("Unable to satisfy the order in full quantity.")
		if satoshisExhausted {
			log.Print("Reason: Insufficient BTC balance.")
		} else {
			log.Print("Reason: No matching orders.")
		}
	}
	averageSatoshiUsdCentsPrice = float64(usdCentsAmount) / float64(satisfiedSatoshiAmount)
	return satisfiedSatoshiAmount, averageSatoshiUsdCentsPrice, nil
}

// Sell the provided amount of user's Bitcoin, if possible,
// by satisfying the existing standing orders
// using the user's available BTC balance.
// If the provided limit price is nonzero,
// limit the matched standing orders to the ones
// whose buy price is at least as high as the provided limit price.
func (user *User) Sell(tx *gorm.DB, amount float64, limitPrice float64) (satisfiedQuantity float64, averagePrice float64, err error) {
	remainingSatoshiAmount := int64(amount * 100000000)
	satoshiUsdCentsLimitPrice := limitPrice / 1000000
	satisfiedSatoshiAmount, averageSatoshiUsdCentsPrice, err := user.SellSatoshis(tx, remainingSatoshiAmount, satoshiUsdCentsLimitPrice)
	satisfiedQuantity = float64(satisfiedSatoshiAmount) / 100000000
	averagePrice = averageSatoshiUsdCentsPrice * 1000000
	return satisfiedQuantity, averagePrice, err
}

func marketOrderHandler(w http.ResponseWriter, r *http.Request) {
	tx := DB.Begin()
	user := getAuthenticatedUser(tx, r)
	if user == nil {
		tx.Rollback()
		log.Printf("Unable to get authenticated user.")
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	marketOrder := MarketOrder{}
	log.Printf("Request data: Type: %T, Value: %v", marketOrder, marketOrder)
	err := decoder.Decode(&marketOrder)
	if err != nil {
		tx.Rollback()
		log.Printf("Unable to decode request body from JSON. Error: %v", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	log.Printf("Market order request for user %v: %v", user.ID, marketOrder)
	if marketOrder.Type != "BUY" && marketOrder.Type != "SELL" {
		tx.Rollback()
		log.Printf("Unknown type %v of market order has been provided.", marketOrder.Type)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	var satisfiedQuantity, averagePrice float64
	if marketOrder.Type == "BUY" {
		satisfiedQuantity, averagePrice, err = user.Buy(tx, marketOrder.Quantity, 0)
	} else { // marketOrder.Type == "SELL"
		satisfiedQuantity, averagePrice, err = user.Sell(tx, marketOrder.Quantity, 0)
	}
	if errors.Is(err, NO_MATCHING_STANDING_ORDERS) {
		tx.Rollback()
		log.Println("No matching standing order.")
		w.WriteHeader(http.StatusConflict)
		return
	}
	if err != nil {
		tx.Rollback()
		log.Printf("Unable to perform the requested market order %v. Error: %v", marketOrder, err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	result := tx.Commit()
	if err := result.Error; err != nil {
		tx.Rollback()
		log.Printf("Unable to commit the transaction. Error: %v", result.Error)
		return
	}
	// transaction is no longer in progress here
	outcome := MarketOrderOutcome{
		Quantity:     satisfiedQuantity,
		AveragePrice: averagePrice,
	}
	log.Printf("Market order outcome: %v", outcome)
	output, err := json.Marshal(outcome)
	if err != nil {
		log.Printf("Unable to serialize MarketOrderOutcome object to JSON. Error: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Write(output)
}
