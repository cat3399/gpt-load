package channel

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	app_errors "gpt-load/internal/errors"
	"gpt-load/internal/models"
	"gpt-load/internal/utils"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/sirupsen/logrus"
)

const (
	vertexDefaultTokenURI = "https://oauth2.googleapis.com/token"
	vertexOAuthScope      = "https://www.googleapis.com/auth/cloud-platform"
)

func init() {
	Register("vertex_gemini", newVertexGeminiChannel)
}

type VertexGeminiChannel struct {
	*BaseChannel

	tokenCacheMu sync.Mutex
	tokenCache   map[uint]vertexAccessToken
}

type vertexAccessToken struct {
	AccessToken string
	Expiry      time.Time
}

type gcpServiceAccount struct {
	ProjectID    string `json:"project_id"`
	PrivateKeyID string `json:"private_key_id"`
	PrivateKey   string `json:"private_key"`
	ClientEmail  string `json:"client_email"`
	TokenURI     string `json:"token_uri"`
}

func newVertexGeminiChannel(f *Factory, group *models.Group) (ChannelProxy, error) {
	base, err := f.newBaseChannel("vertex_gemini", group)
	if err != nil {
		return nil, err
	}

	return &VertexGeminiChannel{
		BaseChannel: base,
		tokenCache:  make(map[uint]vertexAccessToken),
	}, nil
}

func (ch *VertexGeminiChannel) ModifyRequest(req *http.Request, apiKey *models.APIKey, group *models.Group) error {
	sa, err := parseGCPServiceAccount(apiKey.KeyValue)
	if err != nil {
		return err
	}

	// Compatibility rewrite: allow proxy-side Gemini native paths, but call Vertex upstream.
	ch.rewriteGeminiNativePathToVertex(req, sa)

	accessToken, err := ch.getOrMintAccessToken(req.Context(), apiKey.ID, sa)
	if err != nil {
		return err
	}

	req.Header.Set("Authorization", "Bearer "+accessToken)
	return nil
}

func (ch *VertexGeminiChannel) IsStreamRequest(c *gin.Context, bodyBytes []byte) bool {
	path := c.Request.URL.Path
	if strings.HasSuffix(path, ":streamGenerateContent") {
		return true
	}

	// Also check for standard streaming indicators as a fallback.
	if strings.Contains(c.GetHeader("Accept"), "text/event-stream") {
		return true
	}
	if c.Query("stream") == "true" {
		return true
	}

	type streamPayload struct {
		Stream bool `json:"stream"`
	}
	var p streamPayload
	if err := json.Unmarshal(bodyBytes, &p); err == nil {
		return p.Stream
	}

	return false
}

func (ch *VertexGeminiChannel) ExtractModel(c *gin.Context, bodyBytes []byte) string {
	// gemini/vertex native: model in path segment after "models/"
	path := c.Request.URL.Path
	parts := strings.Split(path, "/")
	for i, part := range parts {
		if part == "models" && i+1 < len(parts) {
			modelPart := parts[i+1]
			return strings.Split(modelPart, ":")[0]
		}
	}

	// openai compatible fallback: model in body
	type modelPayload struct {
		Model string `json:"model"`
	}
	var p modelPayload
	if err := json.Unmarshal(bodyBytes, &p); err == nil && p.Model != "" {
		return p.Model
	}

	return ""
}

func (ch *VertexGeminiChannel) ValidateKey(ctx context.Context, apiKey *models.APIKey, group *models.Group) (bool, error) {
	upstreamURL := ch.getUpstreamURL()
	if upstreamURL == nil {
		return false, fmt.Errorf("no upstream URL configured for channel %s", ch.Name)
	}

	sa, err := parseGCPServiceAccount(apiKey.KeyValue)
	if err != nil {
		return false, err
	}

	projectID := extractVertexProjectID(upstreamURL)
	if projectID == "" {
		projectID = sa.ProjectID
	}
	if projectID == "" {
		return false, fmt.Errorf("missing project_id (not found in upstream url path or service account json)")
	}

	location := extractVertexLocation(upstreamURL)
	if location == "" {
		return false, fmt.Errorf("unable to infer vertex location from upstream host/path")
	}

	accessToken, err := ch.getOrMintAccessToken(ctx, apiKey.ID, sa)
	if err != nil {
		return false, err
	}

	reqURL, err := buildVertexModelMethodURL(upstreamURL, projectID, location, ch.TestModel, "generateContent")
	if err != nil {
		return false, err
	}

	payload := gin.H{
		"contents": []gin.H{
			{
				"role": "user",
				"parts": []gin.H{
					{"text": "hi"},
				},
			},
		},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return false, fmt.Errorf("failed to marshal validation payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", reqURL, bytes.NewBuffer(body))
	if err != nil {
		return false, fmt.Errorf("failed to create validation request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")

	// Apply custom header rules if available
	if len(group.HeaderRuleList) > 0 {
		headerCtx := utils.NewHeaderVariableContext(group, apiKey)
		utils.ApplyHeaderRules(req, group.HeaderRuleList, headerCtx)
	}

	resp, err := ch.HTTPClient.Do(req)
	if err != nil {
		return false, fmt.Errorf("failed to send validation request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return true, nil
	}

	errorBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return false, fmt.Errorf("key is invalid (status %d), but failed to read error body: %w", resp.StatusCode, err)
	}

	parsedError := app_errors.ParseUpstreamError(errorBody)
	return false, fmt.Errorf("[status %d] %s", resp.StatusCode, parsedError)
}

func (ch *VertexGeminiChannel) ApplyModelRedirect(req *http.Request, bodyBytes []byte, group *models.Group) ([]byte, error) {
	if len(group.ModelRedirectMap) == 0 {
		return bodyBytes, nil
	}

	// Allow OpenAI-compatible payloads when upstream supports it.
	if strings.Contains(req.URL.Path, "/openai/") {
		return ch.BaseChannel.ApplyModelRedirect(req, bodyBytes, group)
	}

	return ch.applyNativeFormatRedirect(req, bodyBytes, group)
}

func (ch *VertexGeminiChannel) applyNativeFormatRedirect(req *http.Request, bodyBytes []byte, group *models.Group) ([]byte, error) {
	path := req.URL.Path
	parts := strings.Split(path, "/")

	for i, part := range parts {
		if part == "models" && i+1 < len(parts) {
			modelPart := parts[i+1]
			originalModel := strings.Split(modelPart, ":")[0]

			if targetModel, found := group.ModelRedirectMap[originalModel]; found {
				suffix := ""
				if colonIndex := strings.Index(modelPart, ":"); colonIndex != -1 {
					suffix = modelPart[colonIndex:]
				}
				parts[i+1] = targetModel + suffix
				req.URL.Path = strings.Join(parts, "/")

				logrus.WithFields(logrus.Fields{
					"group":          group.Name,
					"original_model": originalModel,
					"target_model":   targetModel,
					"channel":        "vertex_gemini",
					"original_path":  path,
					"new_path":       req.URL.Path,
				}).Debug("Model redirected")

				return bodyBytes, nil
			}

			if group.ModelRedirectStrict {
				return nil, fmt.Errorf("model '%s' is not configured in redirect rules", originalModel)
			}
			return bodyBytes, nil
		}
	}

	return bodyBytes, nil
}

func (ch *VertexGeminiChannel) TransformModelList(req *http.Request, bodyBytes []byte, group *models.Group) (map[string]any, error) {
	var response map[string]any
	if err := json.Unmarshal(bodyBytes, &response); err != nil {
		logrus.WithError(err).Debug("Failed to parse model list response, returning empty")
		return nil, err
	}

	if modelsInterface, hasModels := response["models"]; hasModels {
		return ch.transformGeminiNativeFormat(req, response, modelsInterface, group), nil
	}

	if _, hasData := response["data"]; hasData {
		return ch.BaseChannel.TransformModelList(req, bodyBytes, group)
	}

	return response, nil
}

func (ch *VertexGeminiChannel) transformGeminiNativeFormat(req *http.Request, response map[string]any, modelsInterface any, group *models.Group) map[string]any {
	upstreamModels, ok := modelsInterface.([]any)
	if !ok {
		return response
	}

	configuredModels := buildConfiguredGeminiModels(group.ModelRedirectMap)

	if group.ModelRedirectStrict {
		response["models"] = configuredModels
		delete(response, "nextPageToken")
		return response
	}

	if isFirstPage(req) {
		response["models"] = mergeGeminiModelLists(upstreamModels, configuredModels)
		return response
	}

	response["models"] = upstreamModels
	return response
}

func (ch *VertexGeminiChannel) rewriteGeminiNativePathToVertex(req *http.Request, sa gcpServiceAccount) {
	const geminiModelsPrefixV1Beta = "/v1beta/models"
	const geminiModelsPrefixV1 = "/v1/models"

	if req == nil || req.URL == nil {
		return
	}

	idx := strings.Index(req.URL.Path, geminiModelsPrefixV1Beta)
	matchedPrefix := geminiModelsPrefixV1Beta
	if idx == -1 {
		idx = strings.Index(req.URL.Path, geminiModelsPrefixV1)
		matchedPrefix = geminiModelsPrefixV1
	}
	if idx == -1 {
		return
	}

	prefixBefore := req.URL.Path[:idx]
	suffixAfter := req.URL.Path[idx+len(matchedPrefix):]

	replacement, ok := ch.vertexModelsReplacement(prefixBefore, req.URL, sa)
	if !ok {
		return
	}

	req.URL.Path = prefixBefore + replacement + suffixAfter
}

func (ch *VertexGeminiChannel) vertexModelsReplacement(prefixBefore string, u *url.URL, sa gcpServiceAccount) (string, bool) {
	// If upstream base path already includes a Vertex prefix, only append the missing parts.
	switch {
	case strings.Contains(prefixBefore, "/publishers/google/models"):
		// Already at ".../publishers/google/models", just strip "/v1beta/models".
		return "", true
	case strings.Contains(prefixBefore, "/publishers/google"):
		// Already at ".../publishers/google", append "/models".
		if strings.HasSuffix(strings.TrimRight(prefixBefore, "/"), "/models") {
			return "", true
		}
		return "/models", true
	case strings.Contains(prefixBefore, "/projects/") && strings.Contains(prefixBefore, "/locations/"):
		// Already at ".../projects/{p}/locations/{l}", append "/publishers/google/models".
		return "/publishers/google/models", true
	}

	// Otherwise build a full Vertex models prefix under any upstream prefix path.
	if sa.ProjectID == "" {
		return "", false
	}
	location := extractVertexLocation(u)
	if location == "" {
		return "", false
	}

	return fmt.Sprintf("/v1/projects/%s/locations/%s/publishers/google/models", sa.ProjectID, location), true
}

func (ch *VertexGeminiChannel) getOrMintAccessToken(ctx context.Context, apiKeyID uint, sa gcpServiceAccount) (string, error) {
	// Key IDs should always exist for stored keys, but be defensive for ad-hoc tests.
	cacheKey := apiKeyID

	ch.tokenCacheMu.Lock()
	cached, ok := ch.tokenCache[cacheKey]
	if ok && cached.AccessToken != "" && time.Until(cached.Expiry) > 2*time.Minute {
		token := cached.AccessToken
		ch.tokenCacheMu.Unlock()
		return token, nil
	}
	ch.tokenCacheMu.Unlock()

	token, expiry, err := ch.mintAccessTokenFromServiceAccount(ctx, sa)
	if err != nil {
		return "", err
	}

	ch.tokenCacheMu.Lock()
	ch.tokenCache[cacheKey] = vertexAccessToken{AccessToken: token, Expiry: expiry}
	ch.tokenCacheMu.Unlock()

	return token, nil
}

func (ch *VertexGeminiChannel) mintAccessTokenFromServiceAccount(ctx context.Context, sa gcpServiceAccount) (string, time.Time, error) {
	if sa.ClientEmail == "" || sa.PrivateKey == "" {
		return "", time.Time{}, fmt.Errorf("invalid service account json: missing client_email/private_key")
	}

	tokenCtx := ctx
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		tokenCtx, cancel = context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
	}

	tokenURI := sa.TokenURI
	if tokenURI == "" {
		tokenURI = vertexDefaultTokenURI
	}

	now := time.Now().Unix()
	exp := now + 3600

	type jwtHeader struct {
		Alg string `json:"alg"`
		Typ string `json:"typ"`
		Kid string `json:"kid,omitempty"`
	}
	type jwtClaims struct {
		Iss   string `json:"iss"`
		Scope string `json:"scope"`
		Aud   string `json:"aud"`
		Iat   int64  `json:"iat"`
		Exp   int64  `json:"exp"`
	}

	headerJSON, err := json.Marshal(jwtHeader{Alg: "RS256", Typ: "JWT", Kid: sa.PrivateKeyID})
	if err != nil {
		return "", time.Time{}, fmt.Errorf("failed to marshal jwt header: %w", err)
	}
	claimsJSON, err := json.Marshal(jwtClaims{
		Iss:   sa.ClientEmail,
		Scope: vertexOAuthScope,
		Aud:   tokenURI,
		Iat:   now,
		Exp:   exp,
	})
	if err != nil {
		return "", time.Time{}, fmt.Errorf("failed to marshal jwt claims: %w", err)
	}

	headerB64 := base64.RawURLEncoding.EncodeToString(headerJSON)
	claimsB64 := base64.RawURLEncoding.EncodeToString(claimsJSON)
	unsigned := headerB64 + "." + claimsB64

	sigB64, err := rs256Sign(unsigned, sa.PrivateKey)
	if err != nil {
		return "", time.Time{}, err
	}

	assertion := unsigned + "." + sigB64

	form := url.Values{}
	form.Set("grant_type", "urn:ietf:params:oauth:grant-type:jwt-bearer")
	form.Set("assertion", assertion)

	req, err := http.NewRequestWithContext(tokenCtx, "POST", tokenURI, strings.NewReader(form.Encode()))
	if err != nil {
		return "", time.Time{}, fmt.Errorf("failed to create token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := ch.HTTPClient.Do(req)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("failed to exchange access token: %w", err)
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("failed to read token response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		parsed := app_errors.ParseUpstreamError(bodyBytes)
		return "", time.Time{}, fmt.Errorf("[status %d] %s", resp.StatusCode, parsed)
	}

	var tr struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(bodyBytes, &tr); err != nil {
		return "", time.Time{}, fmt.Errorf("failed to parse token response: %w", err)
	}
	if tr.AccessToken == "" {
		return "", time.Time{}, fmt.Errorf("token response missing access_token")
	}

	expiresIn := tr.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = 3600
	}
	expiry := time.Now().Add(time.Duration(expiresIn) * time.Second)
	return tr.AccessToken, expiry, nil
}

func rs256Sign(unsigned string, privateKeyPEM string) (string, error) {
	priv, err := parseRSAPrivateKeyFromPEM(privateKeyPEM)
	if err != nil {
		return "", err
	}

	sum := sha256.Sum256([]byte(unsigned))
	sig, err := rsa.SignPKCS1v15(rand.Reader, priv, crypto.SHA256, sum[:])
	if err != nil {
		return "", fmt.Errorf("failed to sign jwt: %w", err)
	}

	return base64.RawURLEncoding.EncodeToString(sig), nil
}

func parseRSAPrivateKeyFromPEM(pemStr string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(pemStr))
	if block == nil {
		return nil, fmt.Errorf("invalid private key pem")
	}

	// Service account keys are usually PKCS8 ("BEGIN PRIVATE KEY").
	if key, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		if rsaKey, ok := key.(*rsa.PrivateKey); ok {
			return rsaKey, nil
		}
		return nil, fmt.Errorf("private key is not rsa")
	}

	// Fallback to PKCS1 ("BEGIN RSA PRIVATE KEY") if needed.
	if rsaKey, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return rsaKey, nil
	}

	return nil, fmt.Errorf("failed to parse rsa private key")
}

func parseGCPServiceAccount(keyValue string) (gcpServiceAccount, error) {
	trimmed := strings.TrimSpace(keyValue)
	if trimmed == "" {
		return gcpServiceAccount{}, fmt.Errorf("empty key value")
	}

	var sa gcpServiceAccount
	if err := json.Unmarshal([]byte(trimmed), &sa); err != nil {
		return gcpServiceAccount{}, fmt.Errorf("vertex_gemini expects a GCP service account JSON as key: %w", err)
	}

	// ProjectID can be supplied via upstream path, but keep a helpful validation here.
	if sa.ClientEmail == "" || sa.PrivateKey == "" {
		return gcpServiceAccount{}, fmt.Errorf("invalid service account json: missing client_email/private_key")
	}

	return sa, nil
}

func extractVertexLocation(u *url.URL) string {
	if u == nil {
		return ""
	}

	// Prefer extracting from path: .../locations/{location}/...
	parts := strings.Split(u.Path, "/")
	for i, part := range parts {
		if part == "locations" && i+1 < len(parts) {
			location := parts[i+1]
			if location != "" {
				return location
			}
		}
	}

	// Fallback to hostname convention: {location}-aiplatform.googleapis.com / aiplatform.googleapis.com (global)
	host := u.Hostname()
	if host == "" {
		return ""
	}
	if host == "aiplatform.googleapis.com" {
		return "global"
	}
	const suffix = "-aiplatform.googleapis.com"
	if strings.HasSuffix(host, suffix) {
		location := strings.TrimSuffix(host, suffix)
		if location != "" {
			return location
		}
	}

	return ""
}

func extractVertexProjectID(u *url.URL) string {
	if u == nil {
		return ""
	}

	parts := strings.Split(u.Path, "/")
	for i, part := range parts {
		if part == "projects" && i+1 < len(parts) {
			projectID := parts[i+1]
			if projectID != "" {
				return projectID
			}
		}
	}
	return ""
}

func buildVertexModelMethodURL(upstreamURL *url.URL, projectID string, location string, model string, method string) (string, error) {
	if upstreamURL == nil {
		return "", fmt.Errorf("nil upstream url")
	}
	if projectID == "" || location == "" || model == "" || method == "" {
		return "", fmt.Errorf("missing required vertex url parts")
	}

	vertexPath := fmt.Sprintf(
		"/v1/projects/%s/locations/%s/publishers/google/models/%s:%s",
		projectID,
		location,
		model,
		method,
	)

	finalURL := *upstreamURL

	// Preserve any upstream prefix path (e.g. reverse proxy base path), but avoid double /v1/projects.
	basePath := strings.TrimRight(finalURL.Path, "/")
	if idx := strings.Index(basePath, "/v1/projects/"); idx != -1 {
		basePath = basePath[:idx]
	}
	finalURL.Path = strings.TrimRight(basePath, "/") + vertexPath
	finalURL.RawQuery = ""

	return finalURL.String(), nil
}
