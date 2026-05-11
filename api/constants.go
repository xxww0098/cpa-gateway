package api

// User and API response codes shared across panel handlers.
const (
	initialRegisterCredit = 1.0
	userStatusActive      = "active"
	apiErrorBadRequest    = 4000
	apiErrorUnauthorized  = 4001
	apiErrorNotFound      = 4004
	apiErrorConflict      = 4009
	apiErrorInternal      = 5000
)
