package kiro

import (
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"airouter/internal/domain"
)

const (
	HeaderTokenType        = "TokenType"
	HeaderKiroIDP          = "X-Kiro-Idp"
	HeaderKiroProfile      = "X-Kiro-Profile-Arn"
	HeaderAgentMode        = "x-amzn-kiro-agent-mode"
	HeaderOptOut           = "x-amzn-codewhisperer-optout"
	TokenTypeAPIKey        = "API_KEY"
	TokenTypeExternal      = "EXTERNAL_IDP"
	TokenTypeSSOOIDC       = "SSO_OIDC"
	DefaultRegion          = "us-east-1"
	TransportCodeWhisperer = "codewhisperer"
	TransportRuntime       = "runtime"
	DiscoveryLegacy        = "legacy"
	DiscoveryManagement    = "management"
	RuntimeServiceTarget   = "KiroRuntimeService.GenerateAssistantResponse"
	JSONContentType        = "application/x-amz-json-1.0"
)

// Identity is the Kiro credential and profile evidence available for one
// request. Empty fields stay omitted. Protocol alone is not identity.
type Identity struct {
	Method     domain.AuthMethod
	Auth       domain.AuthScheme
	Token      string
	KiroAuth   string
	ProfileArn string
	Region     string
	// IDP is set only when the connection records a known social or enterprise
	// identity. It is not inferred from KiroAuth text alone.
	IDP string
	// ContentOptOut is true only when the connection explicitly disables
	// content collection. The header is otherwise omitted.
	ContentOptOut bool
	// AgentMode is sent only when explicitly configured. v1/spec are the
	// observed IDE values; other values are not invented.
	AgentMode string
	Transport string
	Discovery string
}

// IdentityFromProvider reads Kiro evidence from a provider without guessing a
// shared profile or a modern endpoint.
func IdentityFromProvider(p *domain.Provider) Identity {
	id := Identity{}
	if p == nil {
		return id
	}
	id.Method = p.Method()
	id.Auth = p.Auth()
	id.Token = p.APIKey
	if p.OAuthCreds == nil {
		return id
	}
	c := p.OAuthCreds
	id.KiroAuth = strings.TrimSpace(c.KiroAuth)
	id.ProfileArn = strings.TrimSpace(c.ProfileArn)
	id.Region = strings.TrimSpace(c.Region)
	id.IDP = strings.TrimSpace(c.KiroIDP)
	id.ContentOptOut = c.KiroContentOptOut
	id.AgentMode = strings.TrimSpace(c.KiroAgentMode)
	id.Transport = strings.TrimSpace(c.KiroTransport)
	id.Discovery = strings.TrimSpace(c.KiroDiscovery)
	return id
}

// ApplyAuthHeaders sets credential and Kiro identity headers that are backed
// by this identity. It does not log token or profile values. Header names are
// canonical; the wire carries one value for each name.
func ApplyAuthHeaders(h http.Header, id Identity) {
	if h == nil {
		return
	}
	switch id.Auth {
	case domain.AuthXAPIKey:
		h.Set("x-api-key", id.Token)
	default:
		h.Set("Authorization", "Bearer "+id.Token)
	}
	if tokenType := TokenType(id); tokenType != "" {
		h.Set(HeaderTokenType, tokenType)
	}
	if idp := idpHeader(id); idp != "" {
		h.Set(HeaderKiroIDP, idp)
	}
	// The BFF sign-in middleware sets X-Kiro-Profile-Arn whenever a profile is
	// present. A known identity provider is the evidence that this request is
	// that sign-in path. Builder ID is a known provider, so an auth-returned
	// ARN is kept. Absence of an IDP is not proof that the ARN is shared.
	if arn := ProfileArnForHeader(id); idpHeader(id) != "" && arn != "" {
		h.Set(HeaderKiroProfile, arn)
	}
	if mode := agentMode(id.AgentMode); mode != "" {
		h.Set(HeaderAgentMode, mode)
	}
	if id.ContentOptOut {
		h.Set(HeaderOptOut, "true")
	}
}

// TokenType returns the verified token marker. API_KEY, EXTERNAL_IDP, and
// SSO_OIDC all use the canonical TokenType header. SSO_OIDC is returned only
// for Builder ID or IAM Identity Center. Social and unknown OAuth stay unmarked.
func TokenType(id Identity) string {
	if id.Method == domain.AuthAPIKey {
		return TokenTypeAPIKey
	}
	switch normalizeAuth(id.KiroAuth) {
	case "external_idp", "externalidp", "external-idp":
		return TokenTypeExternal
	case "builder-id", "builder_id", "builderid", "builder id", "idc", "iam-identity-center", "enterprise", "internal":
		return TokenTypeSSOOIDC
	default:
		return ""
	}
}

func idpHeader(id Identity) string {
	switch strings.ToLower(strings.TrimSpace(id.IDP)) {
	case "google":
		return "Google"
	case "github":
		return "Github"
	case "builderid", "builder-id", "builder_id":
		return "BuilderId"
	case "awsidc", "aws-idc", "enterprise", "internal":
		return "AWSIdC"
	case "externaloidc", "external-oidc", "externalidp", "external_idp":
		return "ExternalOIDC"
	default:
		return ""
	}
}

func agentMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "vibe", "spec", "autopilot":
		return strings.ToLower(strings.TrimSpace(mode))
	default:
		return ""
	}
}

// ProfileArnForBody returns a configured or auth-returned profile ARN for a
// JSON body. It does not inject an official shared ARN and does not delete an
// account-derived ARN because its auth flavor is Builder ID. An absent value
// stays absent. The proxy cannot prove that a stored ARN belongs to the live
// account when provenance was not recorded.
func ProfileArnForBody(id Identity) string {
	return strings.TrimSpace(id.ProfileArn)
}

// ProfileArnForHeader is the same configured value used by profile headers.
func ProfileArnForHeader(id Identity) string {
	return ProfileArnForBody(id)
}

// Region prefers a valid profile ARN region, then a valid configured region,
// then us-east-1. Invalid labels are not used as host components.
func Region(id Identity) string {
	if region := regionFromProfileARN(id.ProfileArn); region != "" {
		return region
	}
	if region := regionLabel(id.Region); region != "" {
		return region
	}
	return DefaultRegion
}

func regionFromProfileARN(arn string) string {
	parts := strings.Split(strings.TrimSpace(arn), ":")
	if len(parts) < 4 || parts[0] != "arn" {
		return ""
	}
	return regionLabel(parts[3])
}

// regionPattern matches the official region check:
// ^[a-z]{2,4}(-[a-z]+)+-\d{1,2}$.
var regionPattern = regexp.MustCompile(`^[a-z]{2,4}(-[a-z]+)+-\d{1,2}$`)

func regionLabel(region string) string {
	region = strings.ToLower(strings.TrimSpace(region))
	if !regionPattern.MatchString(region) {
		return ""
	}
	return region
}

// RuntimeURL is the official Runtime host root for a region. An invalid
// region falls back to us-east-1. The active RPC serializer posts to "/"
// rather than the unused /generateAssistantResponse HTTP trait.
func RuntimeURL(region string) string {
	return "https://runtime." + fallbackRegion(region) + ".kiro.dev/"
}

// ManagementURL is the official control-plane host root for a region. The
// active RPC serializer posts to this root, not to a named operation path.
func ManagementURL(region string) string {
	return "https://management." + fallbackRegion(region) + ".kiro.dev/"
}

// ChatURL selects the chat endpoint for the identity transport. Legacy appends
// the CodeWhisperer operation path. Runtime uses the regional host root unless
// BaseURL is an explicit non-public override. A blank or official legacy host
// does not redirect Runtime to that host. A custom override keeps its path and
// gains the serializer's trailing slash.
func ChatURL(baseURL string, id Identity) string {
	if !UseRuntime(id) {
		return strings.TrimRight(strings.TrimSpace(baseURL), "/") + UpstreamPath
	}
	if override := explicitEndpointOverride(baseURL); override != "" {
		return rpcRoot(override)
	}
	return RuntimeURL(Region(id))
}

func explicitEndpointOverride(baseURL string) string {
	baseURL = strings.TrimSpace(baseURL)
	if baseURL == "" {
		return ""
	}
	u, err := url.Parse(baseURL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return ""
	}
	host := strings.ToLower(u.Hostname())
	if host == "codewhisperer.us-east-1.amazonaws.com" || strings.HasSuffix(host, ".kiro.dev") {
		return ""
	}
	return baseURL
}

// rpcRoot is the URL the AWS JSON 1.0 serializer produces: the endpoint path,
// with a trailing slash, and no query. A bare host becomes "/".
func rpcRoot(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return strings.TrimRight(strings.TrimSpace(raw), "/") + "/"
	}
	if u.Path == "" || u.Path == "/" {
		u.Path = "/"
	} else {
		u.Path = strings.TrimRight(u.Path, "/") + "/"
	}
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}

func fallbackRegion(region string) string {
	if region = regionLabel(region); region != "" {
		return region
	}
	return DefaultRegion
}

func normalizeAuth(auth string) string {
	return strings.ToLower(strings.TrimSpace(auth))
}

// UseRuntime reports whether this identity explicitly selected Kiro Runtime.
// Blank and unknown values stay on legacy CodeWhisperer.
func UseRuntime(id Identity) bool {
	return strings.EqualFold(strings.TrimSpace(id.Transport), TransportRuntime)
}

// UseManagementDiscovery reports whether model discovery should try the
// management control plane before the legacy catalog. Blank stays legacy-only.
func UseManagementDiscovery(id Identity) bool {
	return strings.EqualFold(strings.TrimSpace(id.Discovery), DiscoveryManagement)
}
