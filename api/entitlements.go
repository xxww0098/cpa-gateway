package api

import (
	"errors"
	"time"

	"github.com/xxww0098/cpa-gateway/model"
	"gorm.io/gorm"
)

// UserHoldsEntitlement reports whether userID is currently authorized to bind to
// groupID.
//
// Semantics (aligned with sdk/access.go's accessControlsGroupEntitled helper,
// introduced in Task 7.6). Because package dependency rules forbid importing
// unexported sdk symbols from outside the sdk package, this file keeps an
// independent copy of the predicate. Any change in semantics MUST be mirrored
// in sdk/access.go (and vice versa).
//
// The predicate is:
//   - Unknown or zero IDs → (false, nil). Callers that pass zero-valued IDs
//     intend "no entitlement" rather than "error".
//   - The group row cannot be loaded → (false, nil). We treat missing groups
//     as unbindable rather than raising, because the admin UI can legitimately
//     delete groups while tenants retain stale references.
//   - model.Group.RateMultiplier == 1.0 → (true, nil). This is the baseline
//     group; every Active_User implicitly holds an entitlement for it.
//   - Otherwise → true iff there exists a Subscription row with
//     user_id=userID AND group_id=groupID AND status='active' AND
//     expires_at > NOW() (UTC).
//
// Any persistence error from the subscription existence query is surfaced to
// the caller. A gorm.ErrRecordNotFound on the subscription query is normalized
// to (false, nil).
func UserHoldsEntitlement(db *gorm.DB, userID uint, groupID uint) (bool, error) {
	if db == nil {
		return false, errors.New("database not initialized")
	}
	if userID == 0 || groupID == 0 {
		return false, nil
	}

	// Baseline check: load the group to inspect its RateMultiplier. We treat
	// a missing group as "not entitled" so API key rebinds can never target a
	// vanished group.
	var grp model.Group
	if err := db.First(&grp, groupID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return false, nil
		}
		return false, err
	}
	if grp.RateMultiplier == 1.0 {
		return true, nil
	}

	// Non-baseline group → require an active, unexpired subscription.
	var count int64
	err := db.Model(&model.Subscription{}).
		Where("user_id = ? AND group_id = ? AND status = ? AND expires_at > ?",
			userID, groupID, "active", time.Now().UTC()).
		Limit(1).
		Count(&count).Error
	if err != nil {
		return false, err
	}
	return count > 0, nil
}
