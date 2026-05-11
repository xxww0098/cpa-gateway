package main

import (
	"errors"

	"github.com/xxww0098/cpa-gateway/ledger"
)

var (
	// ErrInsufficientBalance aliases the canonical ledger.ErrInsufficientBalance.
	ErrInsufficientBalance = ledger.ErrInsufficientBalance

	// ErrInvalidAPIKey indicates an API key is missing, malformed, inactive, or unknown.
	ErrInvalidAPIKey = errors.New("invalid API key")

	// ErrUserNotFound aliases the canonical ledger.ErrUserNotFound.
	ErrUserNotFound = ledger.ErrUserNotFound
)
