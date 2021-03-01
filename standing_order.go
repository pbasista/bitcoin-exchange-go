package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"log"
	"net/http"
	"strconv"

	"gorm.io/gorm"
)

// An existing standing order to buy or sell BTC.
type StandingOrder struct {
	// Using signed integers in the models because the underlying database (PostgreSQL) only supports signed integers.
	ID     int64  `gorm:"primaryKey"`
	UserId string `gorm:"not null; index:idx_user"`
	Type   string `gorm:"not null; index:idx_limit; index:idx_user"`
	State  string `gorm:"not null; index:idx_limit; index:idx_user"`
	// limit USD cents price for one Satoshi
	LimitPrice float64 `json:"limit_price" gorm:"not null; index:idx_limit"`
	// Average USD cents price for one Satoshi
	// of the already fulfilled part of the order.
	AveragePrice float64 `json:"average_price"`
	// fulfilled quantity is represented in Satoshis
	FulfilledQuantity int64 `json:"fulfilled_quantity" gorm:"default:0; not null"`
	// remaining quantity is represented in Satoshis
	RemainingQuantity int64  `json:"remaining_quantity" gorm:"not null"`
	WebhookURL        string `json:"webhook_url"`
	User              User   `json:"-"`
}

// A new standing order to buy or sell BTC.
type NewStandingOrder struct {
	Type string
	// quantity in BTC
	Quantity float64
	// limit USD price for one BTC
	LimitPrice float64 `json:"limit_price"`
	WebhookURL string  `json:"webhook_url"`
}

type StandingOrderId struct {
	ID int64
}

var PERMISSION_DENIED = errors.New("Permission denied.")
var INSUFFICIENT_BALANCE = errors.New("Insufficient balance.")

func getStandingOrderFromDb(tx *gorm.DB, id int64) (*StandingOrder, error) {
	standingOrder := &StandingOrder{}
	result := tx.Where(&StandingOrder{ID: id}, "ID").Take(standingOrder)
	if err := result.Error; err != nil {
		log.Printf("Unable to find standing order with ID %v. Error: %v", id, err)
		return nil, err
	}
	log.Printf("Standing order found: Type: %T, Value: %v", standingOrder, standingOrder)
	return standingOrder, nil
}

func (standingOrder *StandingOrder) PerformWebhookRequest() {
	if standingOrder.WebhookURL == "" {
		return
	}
	log.Printf("Performing a webhook request for standing order %v to URL %v.", standingOrder.ID, standingOrder.WebhookURL)
	buffer := bytes.NewBufferString(fmt.Sprint(standingOrder.ID))
	response, err := http.Post(standingOrder.WebhookURL, "text/plain", buffer)
	if err != nil {
		log.Printf("Unable to perform a webhook of standing order with ID %v", standingOrder.ID)
	}
	response.Body.Close()
}

// Set status of the user's standing order with the provided ID to cancelled.
func (user *User) DeleteStandingOrder(id int64) error {
	tx := DB.Begin()
	standingOrder, err := getStandingOrderFromDb(tx, id)
	if err != nil {
		// nothing to commit
		tx.Rollback()
		return err
	}
	if standingOrder.UserId != user.ID {
		tx.Rollback()
		return PERMISSION_DENIED
	}
	standingOrder.State = "CANCELLED"
	result := tx.Save(standingOrder)
	if err := result.Error; err != nil {
		tx.Rollback()
		log.Printf("Unable to save the standing order %v. Error: %v", standingOrder, err)
		return err
	}
	result = tx.Commit()
	if err := result.Error; err != nil {
		tx.Rollback()
		log.Printf("Unable to commit the transaction. Error: %v", result.Error)
		return err
	}
	standingOrder.PerformWebhookRequest()
	return nil
}

// Get the user's standing order with the provided ID.
func (user *User) GetStandingOrder(id int64) (*StandingOrder, error) {
	return getStandingOrderFromDb(DB, id)
}

// Get the amount of USD cents that are blocked by the remaining parts of the user's live standing orders.
func (user *User) GetBlockedUsdCents(tx *gorm.DB) (int64, error) {
	var standingOrders []*StandingOrder
	result := tx.Where(&StandingOrder{Type: "BUY", State: "LIVE", UserId: user.ID}).Select("remaining_quantity", "limit_price").Find(&standingOrders)
	if err := result.Error; err != nil {
		log.Printf("Unable to get standing orders of user with ID %v. Error: %v", user.ID, err)
		return 0, err
	}
	var blockedUsdCentsAmount int64 = 0
	for _, standingOrder := range standingOrders {
		log.Printf("Standing order: Type: %T, Value: %v", standingOrder, standingOrder)
		blockedUsdCentsAmount += int64(float64(standingOrder.RemainingQuantity) * standingOrder.LimitPrice)
	}
	return blockedUsdCentsAmount, nil
}

// Get the amount of Satoshis that are blocked by the remaining parts of the user's live standing orders.
func (user *User) GetBlockedSatoshis(tx *gorm.DB) (int64, error) {
	var standingOrders []*StandingOrder
	result := tx.Where(&StandingOrder{Type: "SELL", State: "LIVE", UserId: user.ID}).Select("remaining_quantity").Find(&standingOrders)
	if err := result.Error; err != nil {
		log.Printf("Unable to get standing orders of user with ID %v. Error: %v", user.ID, err)
		return 0, err
	}
	var blockedSatoshiAmount int64 = 0
	for _, standingOrder := range standingOrders {
		log.Printf("Standing order: Type: %T, Value: %v", standingOrder, standingOrder)
		blockedSatoshiAmount += standingOrder.RemainingQuantity
	}
	return blockedSatoshiAmount, nil
}

func (user *User) ExecuteStandingOrder(standingOrder *StandingOrder) error {
	log.Printf("Executing standing order %v.", standingOrder)
	var satisfiedSatoshiAmount int64
	var averageSatoshiUsdCentsPrice float64
	var err error
	tx := DB.Begin()
	if standingOrder.Type == "BUY" {
		satisfiedSatoshiAmount, averageSatoshiUsdCentsPrice, err = user.BuySatoshis(tx, standingOrder.RemainingQuantity, standingOrder.LimitPrice)
	} else { // standingOrder.Type == "SELL"
		satisfiedSatoshiAmount, averageSatoshiUsdCentsPrice, err = user.SellSatoshis(tx, standingOrder.RemainingQuantity, standingOrder.LimitPrice)
	}
	if errors.Is(err, NO_MATCHING_STANDING_ORDERS) {
		// It is also possible to commit in this case
		// because no changes have been made to the database yet.
		// The rollback operation is used for consistency.
		tx.Rollback()
		log.Println("No matching standing order.")
		return nil
	}
	if err != nil {
		tx.Rollback()
		log.Printf("Unable to execute standing order %v. Error: %v", standingOrder, err)
		return err
	}
	standingOrder.AveragePrice = (standingOrder.AveragePrice*float64(standingOrder.FulfilledQuantity) + averageSatoshiUsdCentsPrice*float64(satisfiedSatoshiAmount)) / float64(standingOrder.FulfilledQuantity+satisfiedSatoshiAmount)
	standingOrder.FulfilledQuantity += satisfiedSatoshiAmount
	standingOrder.RemainingQuantity -= satisfiedSatoshiAmount
	if standingOrder.RemainingQuantity == 0 {
		standingOrder.State = "FULFILLED"
	}
	result := tx.Save(standingOrder)
	if err := result.Error; err != nil {
		tx.Rollback()
		log.Printf("Unable to save the standing order %v. Error: %v", standingOrder, err)
		return err
	}
	result = tx.Commit()
	if err := result.Error; err != nil {
		tx.Rollback()
		log.Printf("Unable to commit the transaction. Error: %v", result.Error)
		return err
	}
	standingOrder.PerformWebhookRequest()
	return nil
}

// Create the user's standing order from the provided HTTP request.
// If the order has been successfully created,
// try to execute it against the other standing orders.
func (user *User) CreateStandingOrder(tx *gorm.DB, newStandingOrder *NewStandingOrder) (*StandingOrder, error) {
	satoshiAmount := int64(newStandingOrder.Quantity * 100000000)
	limitUsdCentsSatoshiPrice := newStandingOrder.LimitPrice / 1000000
	state := "LIVE"
	if newStandingOrder.Type == "BUY" {
		blockedUsdCentsAmount, err := user.GetBlockedUsdCents(tx)
		log.Printf("User with ID %v has %v USD cents blocked by the live standing orders.", user.ID, blockedUsdCentsAmount)
		if err != nil {
			tx.Rollback()
			log.Printf("Unable to determine the blocked USD cents amount of user with ID %v. Error: %v", user.ID, err)
			return nil, err
		}
		availableUsdCentsAmount := user.USDCentsBalance - blockedUsdCentsAmount
		satoshiAmountBuyLimit := int64(float64(availableUsdCentsAmount) / limitUsdCentsSatoshiPrice)
		if satoshiAmount > satoshiAmountBuyLimit {
			log.Printf("User with ID %v only has %v USD available out of their %v USD balance, which is sufficient to buy %v BTC at the limit price of this new standing order %v. However, its desired quantity is %v BTC, for whose purchase the user needs to have the available balance of at least %v USD. Marking it as cancelled.", user.ID, float64(availableUsdCentsAmount)/100, float64(user.USDCentsBalance)/100, float64(satoshiAmountBuyLimit)/100000000, newStandingOrder, float64(satoshiAmount)/100000000, float64(satoshiAmount)*limitUsdCentsSatoshiPrice/100)
			state = "CANCELLED"
		}
	} else { // newStandingOrder.Type == "SELL"
		blockedSatoshiAmount, err := user.GetBlockedSatoshis(tx)
		log.Printf("User with ID %v has %v Satoshis blocked by the live standing orders.", user.ID, blockedSatoshiAmount)
		if err != nil {
			tx.Rollback()
			log.Printf("Unable to determine the blocked Satoshi amount of user with ID %v. Error: %v", user.ID, err)
			return nil, err
		}
		satoshiAmountSellLimit := user.BTCSatoshiBalance - blockedSatoshiAmount
		if satoshiAmount > satoshiAmountSellLimit {
			log.Printf("User with ID %v only has %v BTC available out of their %v BTC balance but it is necessary to have %v BTC available in order to fully satisfy the new standing order %v. Marking it as cancelled.", user.ID, float64(satoshiAmountSellLimit)/100000000, float64(user.BTCSatoshiBalance)/100000000, float64(satoshiAmount)/100000000, newStandingOrder)
			state = "CANCELLED"
		}
	}
	standingOrder := &StandingOrder{
		Type:              newStandingOrder.Type,
		State:             state,
		RemainingQuantity: satoshiAmount,
		LimitPrice:        limitUsdCentsSatoshiPrice,
		WebhookURL:        newStandingOrder.WebhookURL,
		UserId:            user.ID,
	}
	result := tx.Create(standingOrder)
	if err := result.Error; err != nil {
		tx.Rollback()
		log.Printf("Unable to create standing order %v. Error: %v", standingOrder, err)
		return nil, err
	}
	result = tx.Commit()
	if err := result.Error; err != nil {
		tx.Rollback()
		log.Printf("Unable to commit the transaction. Error: %v", result.Error)
		return nil, err
	}
	// transaction is no longer in progress here
	var err error = nil
	if state == "CANCELLED" {
		err = INSUFFICIENT_BALANCE
	} else {
		go user.ExecuteStandingOrder(standingOrder)
	}
	return standingOrder, err
}

func getStandingOrderId(r *http.Request) (int64, error) {
	escapedUrlPath := html.EscapeString(r.URL.Path)
	match := URL_PATH_PARTS.FindStringSubmatch(escapedUrlPath)
	if match == nil || match[2] == "" {
		return 0, errors.New("No standing order ID to show. Ignoring.")
	}
	value, err := strconv.ParseInt(match[2], 10, 64)
	if err != nil {
		log.Printf("Unable to convert provided string to int64. Error: %v", err)
		return 0, err
	}
	return value, nil
}

func getNewStandingOrderFromRequest(r *http.Request) (*NewStandingOrder, error) {
	var newStandingOrder NewStandingOrder
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	err := decoder.Decode(&newStandingOrder)
	if err != nil {
		log.Printf("Unable to decode NewStandingOrder from JSON. Error: %v", err)
		return nil, err
	}
	if newStandingOrder.Type != "BUY" && newStandingOrder.Type != "SELL" {
		err = fmt.Errorf("Unknown type %v of standing order has been provided.", newStandingOrder.Type)
		return nil, err
	}
	return &newStandingOrder, nil
}

func deleteStandingOrderHandler(user *User, w http.ResponseWriter, r *http.Request) {
	standingOrderId, err := getStandingOrderId(r)
	if err != nil {
		log.Printf("Unable to get standing order ID. Error: %v", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	err = user.DeleteStandingOrder(standingOrderId)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		log.Printf("Standing order with ID %v not found.", standingOrderId)
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if errors.Is(err, PERMISSION_DENIED) {
		log.Printf("No permission to delete standing order with ID %v.", standingOrderId)
		w.WriteHeader(http.StatusForbidden)
		return
	}
	if err != nil {
		log.Printf("Unable to delete standing order %v. Error: %v", standingOrderId, err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	log.Printf("Deleted standing order with ID %v", standingOrderId)
}

func getStandingOrderHandler(user *User, w http.ResponseWriter, r *http.Request) {
	standingOrderId, err := getStandingOrderId(r)
	if err != nil {
		log.Printf("Unable to get standing order ID. Error: %v", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	standingOrder, err := user.GetStandingOrder(standingOrderId)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		log.Printf("Standing order with ID %v not found.", standingOrderId)
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if err != nil {
		log.Printf("Unable to get standing order %v. Error: %v", standingOrderId, err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	output, err := json.Marshal(standingOrder)
	if err != nil {
		log.Printf("Unable to serialize StandingOrder object to JSON. Error: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Write(output)
}

func postStandingOrderHandler(tx *gorm.DB, user *User, w http.ResponseWriter, r *http.Request) {
	newStandingOrder, err := getNewStandingOrderFromRequest(r)
	if err != nil {
		tx.Rollback()
		log.Printf("Unable to get standing order from request. Error: %v", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	// the CreateStandingOrder method commits or rolls back the transaction as necessary
	standingOrder, err := user.CreateStandingOrder(tx, newStandingOrder)
	// transaction is no longer in progress here
	if err != nil && !errors.Is(err, INSUFFICIENT_BALANCE) {
		log.Printf("Unable to create standing order %v. Error: %v", newStandingOrder, err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	if errors.Is(err, INSUFFICIENT_BALANCE) {
		log.Printf("Insufficient balance to create standing order %v. Created as cancelled.", newStandingOrder)
		w.WriteHeader(http.StatusConflict)
		// the output will still contain the created standing order's ID
	}
	output, err := json.Marshal(StandingOrderId{ID: standingOrder.ID})
	if err != nil {
		log.Printf("Unable to serialize StandingOrderId object to JSON. Error: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Write(output)
}

func standingOrderHandler(w http.ResponseWriter, r *http.Request) {
	tx := DB.Begin()
	user := getAuthenticatedUser(tx, r)
	if user == nil {
		tx.Rollback()
		log.Printf("Unable to get authenticated user.")
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	switch r.Method {
	case "DELETE":
		// DB transaction with the read operation on user is unnecessary in this case
		tx.Rollback()
		deleteStandingOrderHandler(user, w, r)
	case "GET":
		// DB transaction with the read operation on user is unnecessary in this case
		tx.Rollback()
		getStandingOrderHandler(user, w, r)
	case "POST":
		// the POST handler commits or rolls back the transaction as necessary
		postStandingOrderHandler(tx, user, w, r)
	default:
		tx.Rollback()
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}
