package helps

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	log "github.com/sirupsen/logrus"
)

// KiroModelLimits are discovered from the credential's ListAvailableModels catalog.
type KiroModelLimits struct {
	MaxInputTokens  int `json:"maxInputTokens"`
	MaxOutputTokens int `json:"maxOutputTokens"`
}

type kiroCatalogKey struct {
	credential, endpoint string
}

type kiroCatalogEntry struct {
	models    map[string]KiroModelLimits
	refreshAt time.Time
	loading   chan struct{}
}

// KiroModelCatalog caches account-scoped limits, retaining the last successful
// catalog when a refresh fails. Its zero value is ready to use.
type KiroModelCatalog struct {
	mu      sync.Mutex
	entries map[kiroCatalogKey]*kiroCatalogEntry
	nowFunc func() time.Time
}

// Lookup uses a prepared REST catalog request and the upstream wire model ID.
// Concurrent readers share a refresh; canceled readers do not cancel its owner.
func (c *KiroModelCatalog) Lookup(ctx context.Context, client *http.Client, credentialID string, request *http.Request, model string) KiroModelLimits {
	if ctx == nil || client == nil || request == nil || request.URL == nil {
		return KiroModelLimits{}
	}
	if credentialID == "" {
		credentialID = fmt.Sprintf("%x", sha256.Sum256([]byte(request.Header.Get("Authorization"))))
	}
	key := kiroCatalogKey{credential: credentialID, endpoint: request.URL.String()}
	for {
		now := time.Now()
		if c.nowFunc != nil {
			now = c.nowFunc()
		}
		c.mu.Lock()
		if c.entries == nil {
			c.entries = make(map[kiroCatalogKey]*kiroCatalogEntry)
		}
		entry := c.entries[key]
		if entry == nil {
			entry = &kiroCatalogEntry{}
			c.entries[key] = entry
		}
		limits := entry.models[model]
		if now.Before(entry.refreshAt) || ctx.Err() != nil {
			c.mu.Unlock()
			return limits
		}
		if loading := entry.loading; loading != nil {
			c.mu.Unlock()
			select {
			case <-loading:
				continue
			case <-ctx.Done():
				return limits
			}
		}
		entry.loading = make(chan struct{})
		c.mu.Unlock()

		models, err := fetchKiroModelCatalog(ctx, client, request)
		c.mu.Lock()
		if err == nil {
			entry.models = models
			entry.refreshAt = now.Add(registry.ModelsRefreshInterval)
		} else if ctx.Err() == nil {
			entry.refreshAt = now.Add(time.Minute)
			log.WithError(err).Debug("kiro: model catalog refresh failed; retaining previous limits")
		}
		close(entry.loading)
		entry.loading = nil
		limits = entry.models[model]
		c.mu.Unlock()
		return limits
	}
}

func fetchKiroModelCatalog(ctx context.Context, client *http.Client, request *http.Request) (map[string]KiroModelLimits, error) {
	models := make(map[string]KiroModelLimits)
	nextToken := ""
	seen := make(map[string]bool)
	for page := 0; page < 100; page++ {
		req := request.Clone(ctx)
		query := req.URL.Query()
		if nextToken != "" {
			query.Set("nextToken", nextToken)
		}
		req.URL.RawQuery = query.Encode()
		resp, errDo := client.Do(req)
		if errDo != nil {
			return nil, fmt.Errorf("fetch Kiro model catalog: %w", errDo)
		}
		body, errRead := io.ReadAll(io.LimitReader(resp.Body, (8<<20)+1))
		errClose := resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("Kiro model catalog returned HTTP %d", resp.StatusCode)
		}
		if errRead != nil {
			return nil, fmt.Errorf("read Kiro model catalog: %w", errRead)
		}
		if errClose != nil {
			return nil, fmt.Errorf("close Kiro model catalog response: %w", errClose)
		}
		if len(body) > 8<<20 {
			return nil, fmt.Errorf("Kiro model catalog exceeded the response size limit")
		}
		var catalog struct {
			Models []struct {
				ModelID     string          `json:"modelId"`
				TokenLimits KiroModelLimits `json:"tokenLimits"`
			} `json:"models"`
			NextToken string `json:"nextToken"`
		}
		if errDecode := json.Unmarshal(body, &catalog); errDecode != nil {
			return nil, fmt.Errorf("decode Kiro model catalog: %w", errDecode)
		}
		if catalog.Models == nil {
			return nil, fmt.Errorf("Kiro model catalog has no models array")
		}
		for _, model := range catalog.Models {
			if id := strings.TrimSpace(model.ModelID); id != "" && model.TokenLimits.MaxInputTokens > 0 {
				models[id] = model.TokenLimits
			}
		}
		nextToken = catalog.NextToken
		if nextToken == "" {
			return models, nil
		}
		if seen[nextToken] {
			return nil, fmt.Errorf("Kiro model catalog repeated a pagination token")
		}
		seen[nextToken] = true
	}
	return nil, fmt.Errorf("Kiro model catalog exceeded the pagination limit")
}
