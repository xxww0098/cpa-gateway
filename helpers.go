package main

import (
	"context"
	"errors"
	"sort"
	"strings"

	"github.com/xxww0098/cpa-gateway/model"
)

// visibleCatalogModelIDsSorted returns distinct visible catalog model IDs
// (excluding the models_url sentinel row), sorted. Used by handler_proxy.go
// (GET /v1/models) which still lives at the root until Wave 4 removes it.
func visibleCatalogModelIDsSorted(ctx context.Context) ([]string, error) {
	if GlobalDB == nil {
		return nil, errors.New("database not initialized")
	}
	var entries []model.ModelCatalogEntry
	if err := GlobalDB.WithContext(ctx).
		Where("visible = ? AND model_id <> ?", true, "__models_url__").
		Find(&entries).Error; err != nil {
		return nil, err
	}
	seen := make(map[string]struct{})
	for _, e := range entries {
		id := strings.TrimSpace(e.ModelID)
		if id == "" {
			continue
		}
		seen[id] = struct{}{}
	}
	ids := make([]string, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids, nil
}
