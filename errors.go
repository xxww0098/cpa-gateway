package main

import "errors"

var (
	// ErrInsufficientBalance indicates the user does not have enough available balance.
	ErrInsufficientBalance = errors.New("insufficient balance")

	// ErrInvalidAPIKey indicates an API key is missing, malformed, inactive, or unknown.
	ErrInvalidAPIKey = errors.New("invalid API key")

	// ErrUserNotFound indicates the requested user does not exist.
	ErrUserNotFound = errors.New("user not found")
)
