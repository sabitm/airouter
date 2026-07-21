package qoder

// Endpoint and client fingerprint constants ported from 9router's
// open-sse/shared/qoder/constants.js. Signature validation matches these values.

const (
	OpenAPIBase = "https://openapi.qoder.sh"
	CenterBase  = "https://center.qoder.sh"
	ChatBase    = "https://api3.qoder.sh"

	LoginURL       = "https://qoder.com/device/selectAccounts"
	DeviceTokenURL = OpenAPIBase + "/api/v1/deviceToken/poll"
	UserInfoURL    = OpenAPIBase + "/api/v1/userinfo"
	ModelListURL   = ChatBase + "/algo/api/v2/model/list"

	// DefaultBaseURL is the inference host root; the codec appends UpstreamPath.
	DefaultBaseURL = ChatBase + "/algo"

	// UpstreamPath includes query flags required by the chat endpoint. Encode=1
	// tells the server to reverse the WAF-bypass body encoding.
	UpstreamPath = "/api/v2/service/pro/sse/agent_chat_generation?FetchKeys=llm_model_result&AgentId=agent_common&Encode=1"

	// ChatURL is the absolute chat URL used when signing (sigPath strips /algo).
	ChatURL = ChatBase + "/algo" + "/api/v2/service/pro/sse/agent_chat_generation?FetchKeys=llm_model_result&AgentId=agent_common&Encode=1"

	IDEVersion   = "1.0.0"
	ClientType   = "5"
	DataPolicy   = "disagree"
	LoginVersion = "v2"
	MachineOS    = "x86_64_windows"
	MachineType  = "5"

	DefaultMaxTokens = 32768
)

// RSA public key for COSY encryption (Qoder IDE / qodercli).
const rsaPublicKeyPEM = `-----BEGIN PUBLIC KEY-----
MIGfMA0GCSqGSIb3DQEBAQUAA4GNADCBiQKBgQDA8iMH5c02LilrsERw9t6Pv5Nc
4k6Pz1EaDicBMpdpxKduSZu5OANqUq8er4GM95omAGIOPOh+Nx0spthYA2BqGz+l
6HRkPJ7S236FZz73In/KVuLnwI8JJ2CbuJap8kvheCCZpmAWpb/cPx/3Vr/J6I17
XcW+ML9FoCI6AOvOzwIDAQAB
-----END PUBLIC KEY-----`

// StaticModels is the fallback catalog when live /model/list is unavailable.
// Keys match 9router's registry; chat still requires a live model_config block.
var StaticModels = []string{
	"auto",
	"ultimate",
	"performance",
	"efficient",
	"lite",
	"qmodel",
	"qmodel_latest",
	"dmodel",
	"dfmodel",
	"gm51model",
	"kmodel",
	"mmodel",
}
