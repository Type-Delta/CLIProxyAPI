package model

import "time"

type rangeFixture struct {
	WeekStartsOn string             `json:"week_starts_on"`
	Semantics    string             `json:"semantics"`
	Cases        []rangeFixtureCase `json:"cases"`
}

type rangeFixtureCase struct {
	Name         string    `json:"name"`
	Kind         string    `json:"kind"`
	Now          time.Time `json:"now"`
	TimeZone     string    `json:"time_zone"`
	Start        time.Time `json:"start"`
	End          time.Time `json:"end"`
	ElapsedHours *int64    `json:"elapsed_hours,omitempty"`
}

type fixtureTokens struct {
	Input         int64 `json:"input"`
	Output        int64 `json:"output"`
	Reasoning     int64 `json:"reasoning"`
	Cached        int64 `json:"cached"`
	CacheRead     int64 `json:"cache_read"`
	CacheCreation int64 `json:"cache_creation"`
	Total         int64 `json:"total"`
}

type tokenCategoriesFixture struct {
	AccountingSchema string `json:"accounting_schema"`
	Cases            []struct {
		Name string `json:"name"`
		fixtureTokens
		Quality TokenQuality `json:"quality"`
	} `json:"cases"`
}

type pricingFixture struct {
	CurrencyUnit string `json:"currency_unit"`
	Rounding     string `json:"rounding"`
	CacheRule    string `json:"cache_rule"`
	RetryRule    string `json:"retry_rule"`
	Precedence   string `json:"precedence"`
	Rules        []struct {
		RuleID string `json:"rule_id"`
		Match  struct {
			Model string `json:"model,omitempty"`
			Alias string `json:"alias,omitempty"`
		} `json:"match"`
		InputPrice            *NanoUSD `json:"input_per_million_usd"`
		OutputPrice           *NanoUSD `json:"output_per_million_usd"`
		CacheReadMultiplier   string   `json:"cache_read_multiplier,omitempty"`
		CacheCreateMultiplier string   `json:"cache_creation_multiplier,omitempty"`
		Source                string   `json:"source"`
	} `json:"rules"`
	Vectors []struct {
		Name            string    `json:"name"`
		Tokens          *int64    `json:"tokens,omitempty"`
		PricePerMillion *NanoUSD  `json:"price_per_million_usd,omitempty"`
		Multiplier      string    `json:"multiplier,omitempty"`
		Cost            *NanoUSD  `json:"cost_usd,omitempty"`
		Input           *int64    `json:"input,omitempty"`
		CacheRead       *int64    `json:"cache_read,omitempty"`
		CacheCreation   *int64    `json:"cache_creation,omitempty"`
		ChargedInput    *int64    `json:"charged_input,omitempty"`
		Output          *int64    `json:"output,omitempty"`
		Model           string    `json:"model,omitempty"`
		Alias           string    `json:"alias,omitempty"`
		SelectedRuleID  string    `json:"selected_rule_id,omitempty"`
		AttemptCosts    []NanoUSD `json:"attempt_costs_usd,omitempty"`
		AggregateCost   *NanoUSD  `json:"aggregate_cost_usd,omitempty"`
		KnownCost       *NanoUSD  `json:"known_cost_usd,omitempty"`
		UnpricedTokens  *int64    `json:"unpriced_tokens,omitempty"`
		ErrorCode       string    `json:"error_code,omitempty"`
	} `json:"vectors"`
}

type latencyFixture struct {
	FormatVersion    string `json:"format_version"`
	RelativeError    string `json:"relative_error"`
	SamplingPriority string `json:"sampling_priority"`
	Vectors          []struct {
		Name     string  `json:"name"`
		ValuesMS []int64 `json:"values_ms"`
		Bins     []struct {
			Index int64 `json:"index"`
			Count int64 `json:"count"`
		} `json:"bins"`
		P50           *int64 `json:"p50_ms"`
		P90           *int64 `json:"p90_ms"`
		P95           *int64 `json:"p95_ms"`
		P99           *int64 `json:"p99_ms"`
		SerializedHex string `json:"serialized_hex"`
	} `json:"vectors"`
	Sampling struct {
		Capacity           int64    `json:"capacity"`
		AttemptIDs         []string `json:"attempt_ids"`
		SelectedAttemptIDs []string `json:"selected_attempt_ids"`
	} `json:"sampling"`
}

type fixtureAggregate struct {
	ProxyRequests    int64   `json:"proxy_requests"`
	UpstreamAttempts int64   `json:"upstream_attempts"`
	TotalTokens      int64   `json:"total_tokens"`
	KnownCost        NanoUSD `json:"known_cost_usd"`
	UnpricedTokens   int64   `json:"unpriced_tokens"`
}

type reconciliationFixture struct {
	Range   Range            `json:"range"`
	Overall fixtureAggregate `json:"overall"`
	PerKey  []struct {
		KeyID  string    `json:"key_id"`
		Status KeyStatus `json:"status"`
		fixtureAggregate
	} `json:"per_key"`
	MultiKey struct {
		KeyIDs []string `json:"key_ids"`
		fixtureAggregate
	} `json:"multi_key"`
	Views []reconciliationView `json:"view_reconciliation"`
}

type reconciliationView struct {
	Name    string   `json:"name"`
	KeyIDs  []string `json:"key_ids"`
	Summary struct {
		ProxyRequests    int64         `json:"proxy_requests"`
		UpstreamAttempts int64         `json:"upstream_attempts"`
		Tokens           fixtureTokens `json:"tokens"`
		KnownCost        NanoUSD       `json:"known_cost_usd"`
		UnpricedTokens   int64         `json:"unpriced_tokens"`
	} `json:"summary"`
	Timeseries []struct {
		Start            time.Time `json:"start"`
		End              time.Time `json:"end"`
		ProxyRequests    int64     `json:"proxy_requests"`
		UpstreamAttempts int64     `json:"upstream_attempts"`
		TotalTokens      int64     `json:"total_tokens"`
		KnownCost        NanoUSD   `json:"known_cost_usd"`
		UnpricedTokens   int64     `json:"unpriced_tokens"`
	} `json:"timeseries"`
	Dimensions []struct {
		Dimension      string  `json:"dimension"`
		Value          string  `json:"value"`
		TotalTokens    int64   `json:"total_tokens"`
		KnownCost      NanoUSD `json:"known_cost_usd"`
		UnpricedTokens int64   `json:"unpriced_tokens"`
	} `json:"dimensions"`
	Events []string `json:"events"`
}

type leaderboardFixtureRow struct {
	Rank           int     `json:"rank"`
	KeyID          string  `json:"key_id"`
	ShortKeyID     string  `json:"short_key_id"`
	TotalTokens    int64   `json:"total_tokens"`
	KnownCost      NanoUSD `json:"known_cost_usd"`
	UnpricedTokens int64   `json:"unpriced_tokens"`
	Percent        string  `json:"percent_of_total"`
}

type leaderboardFixture struct {
	Range      Range                   `json:"range"`
	Tokens     []leaderboardFixtureRow `json:"tokens"`
	Cost       []leaderboardFixtureRow `json:"cost"`
	TieRule    string                  `json:"tie_rule"`
	Pagination struct {
		PageSize    int `json:"page_size"`
		TokensPage1 []struct {
			Rank  int    `json:"rank"`
			KeyID string `json:"key_id"`
		} `json:"tokens_page_1"`
		Cursor      Cursor `json:"tokens_cursor_input"`
		TokensPage2 []struct {
			Rank  int    `json:"rank"`
			KeyID string `json:"key_id"`
		} `json:"tokens_page_2"`
	} `json:"pagination"`
}

type credentialIdentityFixture struct {
	Algorithm              string                     `json:"algorithm"`
	IdentityEpoch          int64                      `json:"identity_epoch"`
	IdentityKeyFingerprint string                     `json:"identity_key_fingerprint"`
	Vectors                []credentialIdentityVector `json:"vectors"`
	KeyState               []identityKeyStateFixture  `json:"key_state"`
}

type credentialIdentityVector struct {
	Name            string  `json:"name"`
	ProviderInput   string  `json:"provider_input"`
	SourceSelector  *string `json:"source_selector"`
	InputValue      *string `json:"input_value"`
	FallbackPresent bool    `json:"fallback_present"`
	InputRedacted   bool    `json:"input_redacted,omitempty"`
	InputSHA256     string  `json:"input_sha256,omitempty"`
	CredentialID    *string `json:"credential_id"`
}

type identityKeyStateFixture struct {
	Name               string         `json:"name"`
	DatabaseExists     bool           `json:"database_exists"`
	KeyReadable        bool           `json:"key_readable"`
	FingerprintMatches bool           `json:"fingerprint_matches"`
	State              AnalyticsState `json:"state"`
	Available          bool           `json:"available"`
	IdentityEpoch      int64          `json:"identity_epoch,omitempty"`
	Recovery           string         `json:"recovery,omitempty"`
	ArchivesPrevious   bool           `json:"archives_previous,omitempty"`
}

type keyIdentityFixture struct {
	MinimumPrefixLength int `json:"minimum_prefix_length"`
	PrefixStep          int `json:"prefix_step"`
	Vectors             []struct {
		Name                       string    `json:"name"`
		FullIDs                    []string  `json:"full_ids"`
		DisplayIDs                 []string  `json:"display_ids"`
		OneIdentity                bool      `json:"one_identity,omitempty"`
		DistinctSourceFingerprints []string  `json:"distinct_source_fingerprints,omitempty"`
		Status                     KeyStatus `json:"status,omitempty"`
		RequiredAction             string    `json:"required_action,omitempty"`
	} `json:"vectors"`
}

type queryContractsFixture struct {
	Bounds struct {
		MaxBodyBytes    int `json:"max_body_bytes"`
		MaxRangeDays    int `json:"max_range_days"`
		MaxBuckets      int `json:"max_buckets"`
		MaxPageSize     int `json:"max_page_size"`
		DefaultPageSize int `json:"default_page_size"`
		MaxKeyFilters   int `json:"max_key_filters"`
		MaxFilterValues int `json:"max_filter_values"`
		MaxCursorBytes  int `json:"max_cursor_bytes"`
		MaxExportRows   int `json:"max_export_rows"`
	} `json:"bounds"`
	Operations       []Operation       `json:"operations"`
	LeaderboardSorts []LeaderboardSort `json:"leaderboard_sorts"`
	SingleKeyQuery   Query             `json:"single_key_query"`
	MultiKeyQuery    Query             `json:"multi_key_query"`
	EventCursor      Cursor            `json:"event_cursor_input"`
}
