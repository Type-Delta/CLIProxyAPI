package helps

import (
	"net/http"
	"strings"

	"github.com/MoeForge/moego/ojson"
	"github.com/MoeForge/moego/regexp"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/antigravitycapture"
)

var userAgentProduct = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*(?:/[a-zA-Z0-9][a-zA-Z0-9._+-]*)?$`)

// AntigravityOAuthClientAllowed matches native clients and configured User-Agent
// products. Text inside User-Agent comments does not identify a client.
func AntigravityOAuthClientAllowed(userAgent string, identities []string) bool {
	return antigravitycapture.IsNativeUserAgent(userAgent) || AntigravityOAuthClientConfigured(userAgent, identities)
}

// AntigravityOAuthClientConfigured requires an explicit incoming identity.
func AntigravityOAuthClientConfigured(userAgent string, identities []string) bool {
	if len(identities) == 0 {
		return false
	}
	var products strings.Builder
	depth := 0
	escaped := false
	for _, ch := range userAgent {
		switch {
		case escaped:
			escaped = false
		case ch == '\\' && depth > 0:
			escaped = true
		case ch == '(':
			if depth == 0 {
				products.WriteByte(' ')
			}
			depth++
		case ch == ')':
			if depth == 0 {
				return false
			}
			depth--
		case depth == 0:
			products.WriteRune(ch)
		}
	}
	if depth != 0 {
		return false
	}
	for _, product := range strings.Fields(products.String()) {
		if !userAgentProduct.MatchString(product) {
			continue
		}
		name, _, _ := strings.Cut(product, "/")
		for _, identity := range identities {
			if strings.EqualFold(name, strings.TrimSpace(identity)) {
				return true
			}
		}
	}
	return false
}

func AntigravityOAuthEnvelopeMismatch(doc *ojson.Doc, countTokens bool, envelopeUserAgent string) string {
	if !doc.Get("request.contents").IsArray() {
		return "request.contents must be an array"
	}
	if countTokens {
		return ""
	}
	for _, key := range []string{"model", "project", "requestId"} {
		value := doc.Get(key)
		if value.Type != ojson.String || strings.TrimSpace(value.Str) == "" {
			return key + " must be a non-empty string"
		}
	}
	if doc.Get("userAgent").Str != envelopeUserAgent {
		return "the envelope userAgent must be antigravity"
	}
	switch doc.Get("requestType").Str {
	case "agent":
		session := doc.Get("request.sessionId")
		if session.Type != ojson.String || strings.TrimSpace(session.Str) == "" {
			return "agent requests require a session ID"
		}
	case "image_gen", "web_search":
	default:
		return "requestType must be a supported Antigravity request type"
	}
	return ""
}

func AntigravityOAuthHeaderMismatch(req *http.Request, token string, profile *antigravitycapture.Profile) string {
	if !antigravitycapture.IsDirectURL(req.URL) || (req.Host != "" && !strings.EqualFold(req.Host, req.URL.Host)) {
		return "the upstream destination is outside the Antigravity Cloud Code endpoints"
	}
	if req.Method != http.MethodPost || len(req.Header.Values("Authorization")) != 1 ||
		token == "" || req.Header.Get("Authorization") != "Bearer "+token {
		return "the outbound request must use the selected OAuth bearer token"
	}
	if !profile.MatchesContentType(req.Header) || !antigravitycapture.IsNativeUserAgent(req.Header.Get("User-Agent")) {
		return "the outbound Content-Type or User-Agent does not match Antigravity"
	}
	return ""
}
