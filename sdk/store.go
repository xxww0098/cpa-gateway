package sdk

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/xxww0098/cpa-gateway/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// postgresAuthStore implements cliproxyauth.Store using GORM/PostgreSQL.
// It is an internal type; callers obtain a cliproxyauth.Store via NewStore.
type postgresAuthStore struct {
	db *gorm.DB
}

// NewStore returns a cliproxyauth.Store backed by PostgreSQL through GORM.
// It persists SDK auth records into the `auth_records` table (see model.AuthRecord).
func NewStore(db *gorm.DB) cliproxyauth.Store {
	return &postgresAuthStore{db: db}
}

// List returns all persisted SDK auth records ordered by creation time.
func (s *postgresAuthStore) List(ctx context.Context) ([]*cliproxyauth.Auth, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("postgres auth store is not initialized")
	}

	var records []model.AuthRecord
	if err := s.db.WithContext(ctx).Order("created_at ASC, id ASC").Find(&records).Error; err != nil {
		return nil, fmt.Errorf("listing auth records: %w", err)
	}

	auths := make([]*cliproxyauth.Auth, 0, len(records))
	for i := range records {
		auth, err := authFromRecord(records[i])
		if err != nil {
			return nil, fmt.Errorf("decoding auth record %q: %w", records[i].ID, err)
		}
		auths = append(auths, auth)
	}
	return auths, nil
}

// Save upserts a SDK auth record by ID. Runtime-only auths are skipped.
func (s *postgresAuthStore) Save(ctx context.Context, auth *cliproxyauth.Auth) (string, error) {
	if s == nil || s.db == nil {
		return "", fmt.Errorf("postgres auth store is not initialized")
	}
	if auth == nil {
		return "", fmt.Errorf("auth is required")
	}
	if strings.TrimSpace(auth.ID) == "" {
		return "", fmt.Errorf("auth id is required")
	}
	if isRuntimeOnlyAuth(auth) {
		return auth.ID, nil
	}

	record, err := authRecordFromAuth(auth)
	if err != nil {
		return "", err
	}

	if err := s.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "id"}},
		UpdateAll: true,
	}).Create(record).Error; err != nil {
		return "", fmt.Errorf("saving auth record %q: %w", auth.ID, err)
	}
	return auth.ID, nil
}

// Delete removes a SDK auth record by ID. Empty IDs are no-ops.
func (s *postgresAuthStore) Delete(ctx context.Context, id string) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("postgres auth store is not initialized")
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return nil
	}
	if err := s.db.WithContext(ctx).Delete(&model.AuthRecord{ID: id}).Error; err != nil {
		return fmt.Errorf("deleting auth record %q: %w", id, err)
	}
	return nil
}

func authRecordFromAuth(auth *cliproxyauth.Auth) (*model.AuthRecord, error) {
	attributes, err := marshalJSONB(auth.Attributes)
	if err != nil {
		return nil, fmt.Errorf("encoding auth attributes: %w", err)
	}
	metadata, err := marshalJSONB(auth.Metadata)
	if err != nil {
		return nil, fmt.Errorf("encoding auth metadata: %w", err)
	}
	quota, err := marshalJSONB(auth.Quota)
	if err != nil {
		return nil, fmt.Errorf("encoding auth quota: %w", err)
	}
	modelStates, err := marshalJSONB(auth.ModelStates)
	if err != nil {
		return nil, fmt.Errorf("encoding auth model states: %w", err)
	}
	lastError, err := marshalOptionalJSONB(auth.LastError)
	if err != nil {
		return nil, fmt.Errorf("encoding auth last error: %w", err)
	}

	return &model.AuthRecord{
		ID:               auth.ID,
		Provider:         auth.Provider,
		Prefix:           auth.Prefix,
		Label:            auth.Label,
		Status:           string(auth.Status),
		StatusMessage:    auth.StatusMessage,
		Disabled:         auth.Disabled,
		Unavailable:      auth.Unavailable,
		ProxyURL:         auth.ProxyURL,
		Attributes:       attributes,
		Metadata:         metadata,
		Quota:            quota,
		ModelStates:      modelStates,
		LastError:        lastError,
		CreatedAt:        auth.CreatedAt,
		UpdatedAt:        auth.UpdatedAt,
		LastRefreshedAt:  auth.LastRefreshedAt,
		NextRefreshAfter: auth.NextRefreshAfter,
		NextRetryAfter:   auth.NextRetryAfter,
	}, nil
}

func authFromRecord(r model.AuthRecord) (*cliproxyauth.Auth, error) {
	auth := &cliproxyauth.Auth{
		ID:               r.ID,
		Provider:         r.Provider,
		Prefix:           r.Prefix,
		Label:            r.Label,
		Status:           cliproxyauth.Status(r.Status),
		StatusMessage:    r.StatusMessage,
		Disabled:         r.Disabled,
		Unavailable:      r.Unavailable,
		ProxyURL:         r.ProxyURL,
		CreatedAt:        r.CreatedAt,
		UpdatedAt:        r.UpdatedAt,
		LastRefreshedAt:  r.LastRefreshedAt,
		NextRefreshAfter: r.NextRefreshAfter,
		NextRetryAfter:   r.NextRetryAfter,
	}
	if auth.Status == "" {
		auth.Status = cliproxyauth.StatusActive
	}

	if err := unmarshalJSONB(r.Attributes, &auth.Attributes); err != nil {
		return nil, fmt.Errorf("attributes: %w", err)
	}
	if err := unmarshalJSONB(r.Metadata, &auth.Metadata); err != nil {
		return nil, fmt.Errorf("metadata: %w", err)
	}
	if err := unmarshalJSONB(r.Quota, &auth.Quota); err != nil {
		return nil, fmt.Errorf("quota: %w", err)
	}
	if err := unmarshalJSONB(r.ModelStates, &auth.ModelStates); err != nil {
		return nil, fmt.Errorf("model states: %w", err)
	}
	if len(r.LastError) > 0 && string(r.LastError) != "null" {
		if err := unmarshalJSONB(r.LastError, &auth.LastError); err != nil {
			return nil, fmt.Errorf("last error: %w", err)
		}
	}

	return auth, nil
}

func marshalJSONB(value any) (json.RawMessage, error) {
	if value == nil {
		return json.RawMessage("{}"), nil
	}
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	if string(data) == "null" {
		return json.RawMessage("{}"), nil
	}
	return json.RawMessage(data), nil
}

func marshalOptionalJSONB(value any) (json.RawMessage, error) {
	if value == nil {
		return nil, nil
	}
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	if string(data) == "null" {
		return nil, nil
	}
	return json.RawMessage(data), nil
}

func unmarshalJSONB(data json.RawMessage, dest any) error {
	if len(data) == 0 || string(data) == "null" {
		return nil
	}
	return json.Unmarshal(data, dest)
}

func isRuntimeOnlyAuth(auth *cliproxyauth.Auth) bool {
	if auth == nil || auth.Attributes == nil {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(auth.Attributes["runtime_only"]), "true")
}
