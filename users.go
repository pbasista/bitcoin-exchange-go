package main

import (
	"errors"
	"html"
	"io"
	"log"
	"net/http"

	"gorm.io/gorm"
)

type User struct {
	ID    string `gorm:"primaryKey"`
	Token string `gorm:"default:gen_random_uuid(); not null; uniqueIndex"`
	// Using signed integers in the models because the underlying database (PostgreSQL) only supports signed integers.
	USDCentsBalance   int64 `gorm:"default:0; not null"`
	BTCSatoshiBalance int64 `gorm:"default:0; not null"`
}

func getUserFromDb(tx *gorm.DB, user *User, query_parameters ...interface{}) (bool, error) {
	var result *gorm.DB
	if query_parameters == nil {
		// assuming that the user struct has its primary key set
		result = tx.Take(user)
	} else {
		result = tx.Where(user, query_parameters).Take(user)
	}
	log.Printf("Query result: Type: %T, Value: %v", result, result)
	if errors.Is(result.Error, gorm.ErrRecordNotFound) {
		return false, nil
	}
	if err := result.Error; err != nil {
		log.Printf("Unable to get user with query %v and parameters %v. Error: %v", user, query_parameters, err)
		return false, err
	}
	log.Printf("User found: Type: %T, Value: %v", user, user)
	return true, nil
}

func registerUser(userId string) (user *User, err error) {
	log.Printf("Registering user with ID %v.", userId)
	user = &User{
		ID: userId,
	}
	tx := DB.Begin()
	found, err := getUserFromDb(tx, user)
	if err != nil {
		tx.Rollback()
		log.Printf("Unable to get user from DB: Error: %v", err)
		return nil, err
	}
	if found {
		tx.Rollback()
		log.Printf("User with ID %v is already registered.", userId)
		return user, nil
	}
	result := tx.Create(user)
	if err := result.Error; err != nil {
		tx.Rollback()
		log.Printf("Unable to create user %v. Error: %v", user, err)
		return nil, err
	}
	result = tx.Commit()
	if err := result.Error; err != nil {
		tx.Rollback()
		log.Printf("Unable to commit the transaction. Error: %v", result.Error)
		return nil, err
	}
	log.Printf("Registered user with ID %v.", userId)
	return user, nil
}

func registerUserHandler(w http.ResponseWriter, r *http.Request) {
	escapedUrlPath := html.EscapeString(r.URL.Path)
	match := URL_PATH_PARTS.FindStringSubmatch(escapedUrlPath)
	if match == nil || match[2] == "" {
		log.Println("No user ID to register. Ignoring.")
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	userId := match[2]
	user, err := registerUser(userId)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	io.WriteString(w, user.Token)
}

func getAuthenticatedUser(tx *gorm.DB, r *http.Request) (user *User) {
	token := r.Header.Get("Token")
	log.Printf("Authentication token: Type: %T, Value: %v", token, token)
	if token == "" {
		log.Println("No authentication token provided.")
		// GORM cannot query the database with struct conditions
		// if all the fields have nil values.
		// In such case, it would attempt to use the provided query parameter
		// as the value for the primary key and return the matching instance.
		// However, in case of struct queries, such an attempt would fail
		// on conversion from query struct to the primary key's type.
		// Therefore, the case of empty or missing token
		// needs to be handled separately.
		return nil
	}
	user = &User{
		Token: token,
	}
	found, err := getUserFromDb(tx, user, "Token")
	if err != nil {
		log.Printf("Unable to get user from the DB: Error: %v", err)
		return nil
	}
	log.Printf("User from DB: Type: %T, Value: %v", user, user)
	if !found {
		log.Printf("User with token %v not found in the DB.", token)
		return nil
	}
	return user
}
