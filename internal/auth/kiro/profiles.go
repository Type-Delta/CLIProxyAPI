package kiro

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// ResolveProfileARN asks the Kiro control plane for the first profile
// associated with an Identity Center token. Device-flow token responses do not
// always include profileArn, although the runtime endpoint requires it.
func (a *Auth) ResolveProfileARN(ctx context.Context, accessToken, region string) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if strings.TrimSpace(accessToken) == "" {
		return "", fmt.Errorf("Kiro profile lookup requires an access token")
	}
	region = normalizeRegion(region)
	body := strings.NewReader(`{"nextToken":null}`)
	endpoint := fmt.Sprintf("https://q.%s.amazonaws.com/", region)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, body)
	if err != nil {
		return "", fmt.Errorf("create Kiro profile lookup request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(accessToken))
	req.Header.Set("Content-Type", "application/x-amz-json-1.0")
	req.Header.Set("X-Amz-Target", "AmazonCodeWhispererService.ListAvailableProfiles")
	req.Header.Set("User-Agent", "aws-sdk-rust/1.3.9 ua/2.1 api/codewhispererruntime/1.0")
	req.Header.Set("x-amz-user-agent", "aws-sdk-rust/1.3.9 ua/2.1 api/codewhispererruntime/1.0")
	resp, err := a.httpClient().Do(req)
	if err != nil {
		return "", fmt.Errorf("Kiro profile lookup request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("Kiro profile lookup returned %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	var result struct {
		Profiles []struct {
			ARN        string `json:"arn"`
			ProfileARN string `json:"profileArn"`
		} `json:"profiles"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decode Kiro profile lookup response: %w", err)
	}
	if len(result.Profiles) == 0 {
		return "", nil
	}
	profile := strings.TrimSpace(result.Profiles[0].ARN)
	if profile == "" {
		profile = strings.TrimSpace(result.Profiles[0].ProfileARN)
	}
	return profile, nil
}
