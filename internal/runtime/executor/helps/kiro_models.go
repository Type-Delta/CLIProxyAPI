package helps

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/MoeForge/moego/ojson"
	kiroauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/kiro"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// KiroModelLimits are discovered from the credential's ListAvailableModels catalog.
type KiroModelLimits struct {
	MaxInputTokens  int `json:"maxInputTokens"`
	MaxOutputTokens int `json:"maxOutputTokens"`
}

// KiroModel is an account's advertised wire model, including optional limits.
type KiroModel struct {
	ID          string          `json:"modelId"`
	Name        string          `json:"modelName"`
	TokenLimits KiroModelLimits `json:"tokenLimits"`
}

type kiroCatalogKey struct {
	credential, endpoint string
}

type kiroCatalogEntry struct {
	models    map[string]KiroModel
	refreshAt time.Time
	loading   chan struct{}
	err       error
}

// KiroModelCatalog caches account-scoped models, retaining the last successful
// catalog when a refresh fails. Its zero value is ready to use.
type KiroModelCatalog struct {
	mu      sync.Mutex
	entries map[kiroCatalogKey]*kiroCatalogEntry
	nowFunc func() time.Time
}

// Lookup uses a prepared REST catalog request and the upstream wire model ID.
// Concurrent readers share a refresh; canceled readers do not cancel its owner.
func (c *KiroModelCatalog) Lookup(ctx context.Context, client *http.Client, credentialID string, request *http.Request, model string) KiroModelLimits {
	models, _ := c.Models(ctx, client, credentialID, request)
	return models[model].TokenLimits
}

// Models returns an isolated catalog snapshot. A failed refresh returns the last
// successful snapshot with its error; nil means discovery has never succeeded.
func (c *KiroModelCatalog) Models(ctx context.Context, client *http.Client, credentialID string, request *http.Request) (map[string]KiroModel, error) {
	if ctx == nil || client == nil || request == nil || request.URL == nil {
		return nil, fmt.Errorf("Kiro model catalog requires a context, client and request")
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
		models := entry.models
		if now.Before(entry.refreshAt) || ctx.Err() != nil {
			err := entry.err
			if ctx.Err() != nil {
				err = ctx.Err()
			}
			c.mu.Unlock()
			return maps.Clone(models), err
		}
		if loading := entry.loading; loading != nil {
			c.mu.Unlock()
			select {
			case <-loading:
				continue
			case <-ctx.Done():
				return maps.Clone(models), ctx.Err()
			}
		}
		entry.loading = make(chan struct{})
		c.mu.Unlock()

		models, err := fetchKiroModelCatalog(ctx, client, request)
		c.mu.Lock()
		entry.err = err
		if err == nil {
			entry.models = models
			entry.refreshAt = now.Add(registry.ModelsRefreshInterval)
		} else if ctx.Err() == nil {
			entry.refreshAt = now.Add(time.Minute)
			log.WithError(err).Debug("kiro: model catalog refresh failed; retaining previous models")
		}
		close(entry.loading)
		entry.loading = nil
		models = entry.models
		c.mu.Unlock()
		return maps.Clone(models), err
	}
}

func fetchKiroModelCatalog(ctx context.Context, client *http.Client, request *http.Request) (map[string]KiroModel, error) {
	models := make(map[string]KiroModel)
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
		if !ojson.ValidBytes(body) {
			return nil, fmt.Errorf("decode Kiro model catalog: invalid JSON")
		}
		catalog := ojson.ParseBytes(body)
		items := catalog.Get("models")
		if !items.IsArray() {
			return nil, fmt.Errorf("Kiro model catalog has no models array")
		}
		for _, item := range items.Array() {
			id := item.Get("modelId")
			if id.Type != ojson.String || strings.TrimSpace(id.Str) == "" {
				return nil, fmt.Errorf("Kiro model catalog contains an invalid model ID")
			}
			var limits [2]int
			for i, field := range []string{"maxInputTokens", "maxOutputTokens"} {
				value := item.Get("tokenLimits." + field)
				if !value.Exists() || value.Type == ojson.Null {
					continue
				}
				limit, errLimit := strconv.Atoi(value.Raw)
				if errLimit != nil || limit < 0 {
					return nil, fmt.Errorf("Kiro model catalog contains invalid token limits")
				}
				limits[i] = limit
			}
			model := KiroModel{ID: strings.TrimSpace(id.Str), Name: item.Get("modelName").String(), TokenLimits: KiroModelLimits{MaxInputTokens: limits[0], MaxOutputTokens: limits[1]}}
			models[model.ID] = model
		}
		next := catalog.Get("nextToken")
		if next.Exists() && next.Type != ojson.Null && next.Type != ojson.String {
			return nil, fmt.Errorf("Kiro model catalog contains an invalid pagination token")
		}
		nextToken = next.String()
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

var kiroPublicModelVersion = regexp.MustCompile(`^(claude-(?:opus|sonnet|haiku|fable)-\d+)\.(\d{1,2})$`)

// ModelsForAuth uses the live catalog for membership and static metadata only
// to enrich known models. All Kiro models use Claude-compatible translation.
func (c *KiroModelCatalog) ModelsForAuth(ctx context.Context, cfg *config.Config, auth *coreauth.Auth) ([]*registry.ModelInfo, error) {
	request, err := kiroCatalogRequest(ctx, auth)
	if err != nil {
		return nil, err
	}
	catalog, err := c.Models(ctx, NewProxyAwareHTTPClient(ctx, cfg, auth, 0), kiroCatalogCredentialID(cfg, auth), request)
	models := make([]*registry.ModelInfo, 0, len(catalog))
	for _, model := range catalog {
		id := kiroPublicModelVersion.ReplaceAllString(model.ID, "${1}-${2}")
		info := registry.LookupStaticModelInfo(id)
		if info == nil {
			info = &registry.ModelInfo{ID: id, Object: "model", OwnedBy: "kiro"}
		}
		info.ID = id
		info.Type = "claude"
		if model.Name != "" {
			info.DisplayName = model.Name
		}
		if model.TokenLimits.MaxInputTokens > 0 {
			info.ContextLength = model.TokenLimits.MaxInputTokens
			info.InputTokenLimit = model.TokenLimits.MaxInputTokens
		}
		if model.TokenLimits.MaxOutputTokens > 0 {
			info.MaxCompletionTokens = model.TokenLimits.MaxOutputTokens
			info.OutputTokenLimit = model.TokenLimits.MaxOutputTokens
		}
		models = append(models, info)
	}
	sort.Slice(models, func(i, j int) bool { return models[i].ID < models[j].ID })
	return models, err
}

// RefreshDueForAuth lets the service retry failed discovery without repeatedly
// republishing fresh registrations or disturbing their cooldown projections.
func (c *KiroModelCatalog) RefreshDueForAuth(cfg *config.Config, auth *coreauth.Auth) bool {
	request, err := kiroCatalogRequest(context.Background(), auth)
	if err != nil {
		return false
	}
	key := kiroCatalogKey{credential: kiroCatalogCredentialID(cfg, auth), endpoint: request.URL.String()}
	now := time.Now()
	if c.nowFunc != nil {
		now = c.nowFunc()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	entry := c.entries[key]
	return entry == nil || (entry.loading == nil && !now.Before(entry.refreshAt))
}

func kiroCatalogCredentialID(cfg *config.Config, auth *coreauth.Auth) string {
	proxy := ""
	if cfg != nil {
		proxy = cfg.ProxyURL
	}
	return fmt.Sprintf("%q/%d/%q/%q", auth.ID, auth.RegistrationEpoch, auth.ProxyURL, proxy)
}

func kiroCatalogRequest(ctx context.Context, auth *coreauth.Auth) (*http.Request, error) {
	if auth == nil {
		return nil, fmt.Errorf("Kiro model catalog requires a credential")
	}
	region := authMetadataString(auth.Metadata, "region")
	if region == "" {
		region = kiroauth.DefaultRegion
	}
	query := url.Values{"origin": {"AI_EDITOR"}}
	profile := authMetadataString(auth.Metadata, "profile_arn")
	if profile == "" {
		profile = authMetadataString(auth.Metadata, "profileArn")
	}
	if profile != "" {
		query.Set("profileArn", profile)
	}
	token := authMetadataString(auth.Metadata, "access_token")
	if token == "" {
		token = authMetadataString(auth.Metadata, "accessToken")
	}
	if token == "" {
		return nil, fmt.Errorf("Kiro model catalog requires an access token")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("https://codewhisperer.%s.amazonaws.com/ListAvailableModels?%s", region, query.Encode()), nil)
	if err != nil {
		return nil, fmt.Errorf("create Kiro model catalog request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "aws-sdk-js/1.0.27 KiroIDE-0.7.45-cpa")
	return request, nil
}
