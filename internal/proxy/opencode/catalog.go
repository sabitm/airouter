package opencode

import "strings"

// Snapshot of https://models.opencode.ai/api.json fetched 2026-11-08.
// Regenerate by filtering providers "opencode" and "opencode-go" to id, SDK
// package, output limit, and reasoning_options. Do not fetch this at runtime.
//
// Go rows retired from the live snapshot stay pinned so stored targets keep a
// known endpoint and reasoning policy instead of falling back to unknown-model
// chat. The provider package is the row SDK; unknown packages map to the
// OpenAI-compatible default used by the inference API.
//
// Official reasoningVariants prefers an effort list. A toggle or budget is used
// only when no effort list exists. Empty options produce no reasoning control.

const (
	zenTier = "zen"
	goTier  = "go"

	sdkCompat    = "openai-compatible"
	sdkOpenAI    = "openai"
	sdkAnthropic = "anthropic"
	sdkGoogle    = "google"

	EndpointChat      = "chat"
	EndpointResponses = "responses"
	EndpointMessages  = "messages"
)

// ModelSpec is one catalog row. OptionsKnown false means reasoning_options was
// absent; both absent and empty produce no reasoning control.
type ModelSpec struct {
	ID           string
	SDK          string
	Output       int
	Efforts      []string
	Toggle       bool
	Budget       bool
	BudgetMin    int
	BudgetMax    int
	OptionsKnown bool
}

var catalog = map[string]map[string]ModelSpec{
	zenTier: {
		"beam-free":                       {SDK: sdkCompat, Output: 131072, Efforts: []string{"low", "medium", "high", "xhigh", "max"}, OptionsKnown: true},
		"big-pickle":                      {SDK: sdkCompat, Output: 32000, OptionsKnown: true},
		"cat-jam-free":                    {SDK: sdkCompat, Output: 131072, Efforts: []string{"low", "medium", "high", "xhigh", "max"}, OptionsKnown: true},
		"claude-3-5-haiku":                {SDK: sdkAnthropic, Output: 8192, OptionsKnown: false},
		"claude-fable-5":                  {SDK: sdkAnthropic, Output: 128000, Efforts: []string{"low", "medium", "high", "xhigh", "max"}, OptionsKnown: true},
		"claude-fable-5-1":                {SDK: sdkAnthropic, Output: 128000, Efforts: []string{"low", "medium", "high", "xhigh", "max"}, OptionsKnown: true},
		"claude-haiku-4-5":                {SDK: sdkAnthropic, Output: 64000, Budget: true, BudgetMin: 1024, OptionsKnown: true},
		"claude-haiku-5-5":                {SDK: sdkAnthropic, Output: 128000, Efforts: []string{"low", "medium", "high", "xhigh", "max"}, Toggle: true, OptionsKnown: true},
		"claude-opus-4-1":                 {SDK: sdkAnthropic, Output: 32000, Budget: true, BudgetMin: 1024, OptionsKnown: true},
		"claude-opus-4-5":                 {SDK: sdkAnthropic, Output: 64000, Efforts: []string{"low", "medium", "high"}, Budget: true, BudgetMin: 1024, OptionsKnown: true},
		"claude-opus-4-6":                 {SDK: sdkAnthropic, Output: 128000, Efforts: []string{"low", "medium", "high", "max"}, Budget: true, BudgetMin: 1024, OptionsKnown: true},
		"claude-opus-4-7":                 {SDK: sdkAnthropic, Output: 128000, Efforts: []string{"low", "medium", "high", "xhigh", "max"}, OptionsKnown: true},
		"claude-opus-4-8":                 {SDK: sdkAnthropic, Output: 128000, Efforts: []string{"low", "medium", "high", "xhigh", "max"}, OptionsKnown: true},
		"claude-opus-5":                   {SDK: sdkAnthropic, Output: 128000, Efforts: []string{"low", "medium", "high", "xhigh", "max"}, OptionsKnown: true},
		"claude-opus-5-5":                 {SDK: sdkAnthropic, Output: 128000, Efforts: []string{"low", "medium", "high", "xhigh", "max"}, OptionsKnown: true},
		"claude-sonnet-4":                 {SDK: sdkAnthropic, Output: 64000, Budget: true, BudgetMin: 1024, OptionsKnown: true},
		"claude-sonnet-4-5":               {SDK: sdkAnthropic, Output: 64000, Budget: true, BudgetMin: 1024, OptionsKnown: true},
		"claude-sonnet-4-6":               {SDK: sdkAnthropic, Output: 64000, Efforts: []string{"low", "medium", "high", "max"}, Budget: true, BudgetMin: 1024, OptionsKnown: true},
		"claude-sonnet-5":                 {SDK: sdkAnthropic, Output: 128000, Efforts: []string{"low", "medium", "high", "xhigh", "max"}, OptionsKnown: true},
		"claude-sonnet-5-5":               {SDK: sdkAnthropic, Output: 128000, Efforts: []string{"low", "medium", "high", "xhigh", "max"}, OptionsKnown: true},
		"crouton-alpha-free":              {SDK: sdkOpenAI, Output: 270000, Efforts: []string{"minimal", "low", "medium", "high", "xhigh", "max"}, OptionsKnown: true},
		"deepseek-v4-flash":               {SDK: sdkCompat, Output: 384000, Efforts: []string{"low", "high", "max"}, Toggle: true, OptionsKnown: true},
		"deepseek-v4-flash-free":          {SDK: sdkCompat, Output: 128000, Efforts: []string{"low", "high", "max"}, OptionsKnown: true},
		"deepseek-v4-flash-vision-exp":    {SDK: sdkCompat, Output: 384000, Efforts: []string{"low", "high", "max"}, Toggle: true, OptionsKnown: true},
		"deepseek-v4-pro":                 {SDK: sdkCompat, Output: 384000, Efforts: []string{"high", "max"}, Toggle: true, OptionsKnown: true},
		"deepseek-v4.1-flash":             {SDK: sdkCompat, Output: 384000, Efforts: []string{"low", "high", "max"}, Toggle: true, OptionsKnown: true},
		"deepseek-v4.1-flash-free":        {SDK: sdkCompat, Output: 32000, Efforts: []string{"low", "high", "max"}, Toggle: true, OptionsKnown: true},
		"exo-free":                        {SDK: sdkCompat, Output: 131072, Efforts: []string{"high"}, OptionsKnown: true},
		"fledge-alpha-free":               {SDK: sdkCompat, Output: 131072, Efforts: []string{"low", "high", "max"}, OptionsKnown: true},
		"flint-40b-free":                  {SDK: sdkCompat, Output: 65536, Efforts: []string{"low", "medium", "high", "xhigh", "max"}, OptionsKnown: true},
		"gemini-3-flash":                  {SDK: sdkGoogle, Output: 65536, Efforts: []string{"minimal", "low", "medium", "high"}, OptionsKnown: true},
		"gemini-3-pro":                    {SDK: sdkGoogle, Output: 65536, Efforts: []string{"low", "high"}, OptionsKnown: true},
		"gemini-3.1-pro":                  {SDK: sdkGoogle, Output: 65536, Efforts: []string{"low", "medium", "high"}, OptionsKnown: true},
		"gemini-3.5-flash":                {SDK: sdkGoogle, Output: 65536, Efforts: []string{"minimal", "low", "medium", "high"}, OptionsKnown: true},
		"gemini-3.5-flash-lite":           {SDK: sdkGoogle, Output: 65536, Efforts: []string{"minimal", "low", "medium", "high"}, OptionsKnown: true},
		"gemini-3.6-flash":                {SDK: sdkGoogle, Output: 65536, Efforts: []string{"minimal", "low", "medium", "high"}, OptionsKnown: true},
		"gemini-3.7-flash":                {SDK: sdkGoogle, Output: 65536, Efforts: []string{"low", "medium", "high"}, OptionsKnown: true},
		"gemini-3.8-flash":                {SDK: sdkGoogle, Output: 65536, Efforts: []string{"low", "medium", "high"}, OptionsKnown: true},
		"gemini-4-argon":                  {SDK: sdkGoogle, Output: 65536, Efforts: []string{"low", "medium", "high"}, OptionsKnown: true},
		"glm-4.6":                         {SDK: sdkCompat, Output: 131072, Toggle: true, OptionsKnown: true},
		"glm-4.7":                         {SDK: sdkCompat, Output: 131072, Toggle: true, OptionsKnown: true},
		"glm-4.7-free":                    {SDK: sdkCompat, Output: 131072, Toggle: true, OptionsKnown: true},
		"glm-5":                           {SDK: sdkCompat, Output: 131072, Toggle: true, OptionsKnown: true},
		"glm-5-free":                      {SDK: sdkCompat, Output: 131072, Toggle: true, OptionsKnown: true},
		"glm-5.1":                         {SDK: sdkCompat, Output: 131072, Toggle: true, OptionsKnown: true},
		"glm-5.2":                         {SDK: sdkCompat, Output: 131072, Efforts: []string{"high", "max"}, OptionsKnown: true},
		"glm-5.3":                         {SDK: sdkCompat, Output: 131072, Efforts: []string{"low", "high", "max"}, OptionsKnown: true},
		"glm-5.3-flash":                   {SDK: sdkCompat, Output: 131072, Efforts: []string{"low", "high", "max"}, OptionsKnown: true},
		"glm-5.3-flash-free":              {SDK: sdkCompat, Output: 32000, Efforts: []string{"low", "high", "max"}, OptionsKnown: true},
		"gpt-5":                           {SDK: sdkOpenAI, Output: 128000, Efforts: []string{"minimal", "low", "medium", "high"}, OptionsKnown: true},
		"gpt-5-codex":                     {SDK: sdkOpenAI, Output: 128000, Efforts: []string{"low", "medium", "high"}, OptionsKnown: true},
		"gpt-5-nano":                      {SDK: sdkOpenAI, Output: 128000, Efforts: []string{"minimal", "low", "medium", "high"}, OptionsKnown: true},
		"gpt-5.1":                         {SDK: sdkOpenAI, Output: 128000, Efforts: []string{"none", "low", "medium", "high"}, OptionsKnown: true},
		"gpt-5.1-codex":                   {SDK: sdkOpenAI, Output: 128000, Efforts: []string{"low", "medium", "high"}, OptionsKnown: true},
		"gpt-5.1-codex-max":               {SDK: sdkOpenAI, Output: 128000, Efforts: []string{"low", "medium", "high", "xhigh"}, OptionsKnown: true},
		"gpt-5.1-codex-mini":              {SDK: sdkOpenAI, Output: 128000, Efforts: []string{"low", "medium", "high"}, OptionsKnown: true},
		"gpt-5.2":                         {SDK: sdkOpenAI, Output: 128000, Efforts: []string{"none", "low", "medium", "high", "xhigh"}, OptionsKnown: true},
		"gpt-5.2-codex":                   {SDK: sdkOpenAI, Output: 128000, Efforts: []string{"low", "medium", "high", "xhigh"}, OptionsKnown: true},
		"gpt-5.3-codex":                   {SDK: sdkOpenAI, Output: 128000, Efforts: []string{"none", "low", "medium", "high", "xhigh"}, OptionsKnown: true},
		"gpt-5.3-codex-spark":             {SDK: sdkOpenAI, Output: 128000, Efforts: []string{"low", "medium", "high", "xhigh"}, OptionsKnown: true},
		"gpt-5.4":                         {SDK: sdkOpenAI, Output: 128000, Efforts: []string{"none", "low", "medium", "high", "xhigh"}, OptionsKnown: true},
		"gpt-5.4-mini":                    {SDK: sdkOpenAI, Output: 128000, Efforts: []string{"none", "low", "medium", "high", "xhigh"}, OptionsKnown: true},
		"gpt-5.4-nano":                    {SDK: sdkOpenAI, Output: 128000, Efforts: []string{"none", "low", "medium", "high", "xhigh"}, OptionsKnown: true},
		"gpt-5.4-pro":                     {SDK: sdkOpenAI, Output: 128000, Efforts: []string{"medium", "high", "xhigh"}, OptionsKnown: true},
		"gpt-5.5":                         {SDK: sdkOpenAI, Output: 128000, Efforts: []string{"none", "low", "medium", "high", "xhigh"}, OptionsKnown: true},
		"gpt-5.5-pro":                     {SDK: sdkOpenAI, Output: 128000, Efforts: []string{"medium", "high", "xhigh"}, OptionsKnown: true},
		"gpt-5.6-luna":                    {SDK: sdkOpenAI, Output: 128000, Efforts: []string{"none", "low", "medium", "high", "xhigh", "max"}, OptionsKnown: true},
		"gpt-5.6-sol":                     {SDK: sdkOpenAI, Output: 128000, Efforts: []string{"none", "low", "medium", "high", "xhigh", "max"}, OptionsKnown: true},
		"gpt-5.6-terra":                   {SDK: sdkOpenAI, Output: 128000, Efforts: []string{"none", "low", "medium", "high", "xhigh", "max"}, OptionsKnown: true},
		"gpt-6-astra":                     {SDK: sdkOpenAI, Output: 128000, Efforts: []string{"low", "medium", "high", "xhigh", "max"}, OptionsKnown: true},
		"gpt-6-luna":                      {SDK: sdkOpenAI, Output: 128000, Efforts: []string{"none", "low", "medium", "high", "xhigh", "max"}, OptionsKnown: true},
		"gpt-6-sol":                       {SDK: sdkOpenAI, Output: 128000, Efforts: []string{"none", "low", "medium", "high", "xhigh", "max"}, OptionsKnown: true},
		"gpt-6.1-sol":                     {SDK: sdkOpenAI, Output: 128000, Efforts: []string{"low", "medium", "high", "xhigh", "max"}, OptionsKnown: true},
		"grok-4.5":                        {SDK: sdkOpenAI, Output: 500000, Efforts: []string{"low", "medium", "high"}, OptionsKnown: true},
		"grok-4.6":                        {SDK: sdkOpenAI, Output: 500000, Efforts: []string{"low", "medium", "high", "xhigh"}, OptionsKnown: true},
		"grok-4.7":                        {SDK: sdkOpenAI, Output: 500000, Efforts: []string{"low", "medium", "high", "xhigh"}, OptionsKnown: true},
		"grok-build-0.1":                  {SDK: sdkOpenAI, Output: 256000, OptionsKnown: true},
		"grok-code":                       {SDK: sdkCompat, Output: 256000, OptionsKnown: true},
		"hy3-free":                        {SDK: sdkCompat, Output: 64000, Efforts: []string{"low", "medium", "high"}, Toggle: true, OptionsKnown: true},
		"hy3-preview-free":                {SDK: sdkCompat, Output: 64000, OptionsKnown: true},
		"iceman-free":                     {SDK: sdkCompat, Output: 131072, OptionsKnown: true},
		"inkling-2-small-free":            {SDK: sdkCompat, Output: 131072, Efforts: []string{"none", "minimal", "low", "medium", "high", "max"}, OptionsKnown: true},
		"kimi-k2":                         {SDK: sdkCompat, Output: 262144, OptionsKnown: false},
		"kimi-k2-thinking":                {SDK: sdkCompat, Output: 262144, OptionsKnown: true},
		"kimi-k2.5":                       {SDK: sdkCompat, Output: 65536, Toggle: true, OptionsKnown: true},
		"kimi-k2.5-free":                  {SDK: sdkCompat, Output: 262144, Toggle: true, OptionsKnown: true},
		"kimi-k2.6":                       {SDK: sdkCompat, Output: 65536, Toggle: true, OptionsKnown: true},
		"kimi-k2.7-code":                  {SDK: sdkCompat, Output: 262144, OptionsKnown: true},
		"kimi-k3":                         {SDK: sdkCompat, Output: 131072, Efforts: []string{"low", "high", "max"}, Toggle: true, OptionsKnown: true},
		"laguna-s-2.1-free":               {SDK: sdkCompat, Output: 32000, Efforts: []string{"low", "medium", "high"}, OptionsKnown: true},
		"ling-2.6-flash-free":             {SDK: sdkCompat, Output: 32800, OptionsKnown: false},
		"ling-3.0-flash-fin-free":         {SDK: sdkCompat, Output: 32768, Toggle: true, OptionsKnown: true},
		"ling-3.0-flash-free":             {SDK: sdkCompat, Output: 32768, Efforts: []string{"low", "medium", "high"}, OptionsKnown: true},
		"ling-3.0-tiny-free":              {SDK: sdkCompat, Output: 32768, OptionsKnown: true},
		"ling-3.1-flash-free":             {SDK: sdkCompat, Output: 32768, Toggle: true, OptionsKnown: true},
		"ll-alpha-free":                   {SDK: sdkCompat, Output: 65536, Efforts: []string{"low", "medium", "high", "xhigh", "max"}, OptionsKnown: true},
		"longcat-2.0-free":                {SDK: sdkCompat, Output: 131072, Toggle: true, OptionsKnown: true},
		"longcat-2.5-preview-free":        {SDK: sdkCompat, Output: 131072, Toggle: true, OptionsKnown: true},
		"mimo-v2-flash-free":              {SDK: sdkCompat, Output: 65536, OptionsKnown: true},
		"mimo-v2-omni-free":               {SDK: sdkCompat, Output: 64000, OptionsKnown: true},
		"mimo-v2-pro-free":                {SDK: sdkCompat, Output: 64000, OptionsKnown: true},
		"mimo-v2.5-free":                  {SDK: sdkCompat, Output: 32000, OptionsKnown: true},
		"mimo-v2.6-flash-free":            {SDK: sdkCompat, Output: 32000, OptionsKnown: true},
		"minimax-m2.1":                    {SDK: sdkCompat, Output: 131072, OptionsKnown: true},
		"minimax-m2.1-free":               {SDK: sdkAnthropic, Output: 131072, OptionsKnown: true},
		"minimax-m2.5":                    {SDK: sdkCompat, Output: 131072, OptionsKnown: true},
		"minimax-m2.5-free":               {SDK: sdkAnthropic, Output: 131072, OptionsKnown: true},
		"minimax-m2.7":                    {SDK: sdkCompat, Output: 131072, OptionsKnown: true},
		"minimax-m3":                      {SDK: sdkCompat, Output: 128000, Toggle: true, OptionsKnown: true},
		"minimax-m3-free":                 {SDK: sdkAnthropic, Output: 32000, Toggle: true, OptionsKnown: true},
		"minimax-m3.1-flash-free":         {SDK: sdkCompat, Output: 512000, Efforts: []string{"low", "medium", "high", "xhigh", "max"}, OptionsKnown: true},
		"mistral-large-4":                 {SDK: sdkCompat, Output: 262144, Efforts: []string{"none", "high"}, OptionsKnown: true},
		"muse-spark-1.2":                  {SDK: sdkOpenAI, Output: 131072, Efforts: []string{"minimal", "low", "medium", "high", "xhigh"}, OptionsKnown: true},
		"muse-spark-1.2-contributor-free": {SDK: sdkOpenAI, Output: 131072, Efforts: []string{"minimal", "low", "medium", "high", "xhigh"}, OptionsKnown: true},
		"muse-spark-1.3":                  {SDK: sdkOpenAI, Output: 131072, Efforts: []string{"minimal", "low", "medium", "high", "xhigh"}, OptionsKnown: true},
		"muse-spark-1.3-contributor-free": {SDK: sdkOpenAI, Output: 131072, Efforts: []string{"minimal", "low", "medium", "high", "xhigh"}, OptionsKnown: true},
		"muse-wonder-1.0":                 {SDK: sdkOpenAI, Output: 131072, Efforts: []string{"minimal", "low", "medium", "high", "xhigh", "max"}, OptionsKnown: true},
		"nemotron-3-super-free":           {SDK: sdkCompat, Output: 128000, OptionsKnown: true},
		"nemotron-3-ultra-free":           {SDK: sdkCompat, Output: 128000, OptionsKnown: true},
		"nemotron-3.5-lightning-free":     {SDK: sdkCompat, Output: 262144, OptionsKnown: true},
		"nemotron-3.5-super-vl-free":      {SDK: sdkCompat, Output: 131072, Efforts: []string{"none", "minimal", "low", "medium", "high", "xhigh"}, OptionsKnown: true},
		"north-mini-code-free":            {SDK: sdkCompat, Output: 64000, Efforts: []string{"none", "high"}, OptionsKnown: true},
		"pulse-alpha-free":                {SDK: sdkCompat, Output: 131072, Efforts: []string{"low", "high", "max"}, OptionsKnown: true},
		"qwen3-coder":                     {SDK: sdkCompat, Output: 65536, OptionsKnown: false},
		"qwen3.5-plus":                    {SDK: sdkAnthropic, Output: 65536, Toggle: true, Budget: true, OptionsKnown: true},
		"qwen3.6-plus":                    {SDK: sdkAnthropic, Output: 65536, Toggle: true, Budget: true, OptionsKnown: true},
		"qwen3.6-plus-free":               {SDK: sdkAnthropic, Output: 65536, Toggle: true, Budget: true, OptionsKnown: true},
		"qwen3.8-flash":                   {SDK: sdkAnthropic, Output: 131072, Efforts: []string{"low", "medium", "xhigh"}, Toggle: true, Budget: true, OptionsKnown: true},
		"qwen3.8-max":                     {SDK: sdkCompat, Output: 131072, Efforts: []string{"low", "medium", "xhigh"}, Toggle: true, Budget: true, BudgetMin: 1024, OptionsKnown: true},
		"ring-2.6-1t-free":                {SDK: sdkCompat, Output: 66000, OptionsKnown: true},
		"space-bunny-free":                {SDK: sdkCompat, Output: 524288, Efforts: []string{"low", "medium", "high", "xhigh", "max"}, OptionsKnown: true},
		"step-5-preview-free":             {SDK: sdkCompat, Output: 65536, Efforts: []string{"low", "medium", "high"}, OptionsKnown: true},
		"trinity-large-preview-free":      {SDK: sdkCompat, Output: 131072, OptionsKnown: false},
		"x-preview-f-free":                {SDK: sdkCompat, Output: 131072, Efforts: []string{"low", "high", "max"}, OptionsKnown: true},
	},
	goTier: {
		"beam-free":                  {SDK: sdkCompat, Output: 131072, Efforts: []string{"low", "medium", "high", "xhigh", "max"}, OptionsKnown: true},
		"cat-jam-free":               {SDK: sdkCompat, Output: 131072, Efforts: []string{"low", "medium", "high", "xhigh", "max"}, OptionsKnown: true},
		"claude-haiku-5-5":           {SDK: sdkAnthropic, Output: 128000, Efforts: []string{"low", "medium", "high", "xhigh", "max"}, Toggle: true, OptionsKnown: true},
		"crouton-alpha-free":         {SDK: sdkOpenAI, Output: 270000, Efforts: []string{"minimal", "low", "medium", "high", "xhigh", "max"}, OptionsKnown: true},
		"deepseek-v4-pro":            {SDK: sdkCompat, Output: 384000, Efforts: []string{"high", "max"}, OptionsKnown: true},
		"deepseek-v4.1-flash":        {SDK: sdkCompat, Output: 384000, Efforts: []string{"low", "high", "max"}, OptionsKnown: true},
		"gemini-4-argon":             {SDK: sdkGoogle, Output: 65536, Efforts: []string{"low", "medium", "high"}, OptionsKnown: true},
		"gemini-4-argon-contributor": {SDK: sdkGoogle, Output: 65536, Efforts: []string{"low", "medium", "high"}, OptionsKnown: true},
		"glm-5.2":                    {SDK: sdkCompat, Output: 131072, Efforts: []string{"high", "max"}, OptionsKnown: true},
		"glm-5.3":                    {SDK: sdkCompat, Output: 131072, Efforts: []string{"low", "high", "max"}, OptionsKnown: true},
		"glm-5.3-flash":              {SDK: sdkCompat, Output: 131072, Efforts: []string{"low", "high", "max"}, OptionsKnown: true},
		"gpt-5.6-luna":               {SDK: sdkOpenAI, Output: 128000, Efforts: []string{"none", "low", "medium", "high", "xhigh", "max"}, OptionsKnown: true},
		"gpt-6-luna":                 {SDK: sdkOpenAI, Output: 128000, Efforts: []string{"none", "low", "medium", "high", "xhigh", "max"}, OptionsKnown: true},
		"grok-4.7":                   {SDK: sdkOpenAI, Output: 500000, Efforts: []string{"low", "medium", "high", "xhigh"}, OptionsKnown: true},
		"hy3":                        {SDK: sdkCompat, Output: 128000, Efforts: []string{"none", "low", "high"}, OptionsKnown: true},
		"hy4":                        {SDK: sdkCompat, Output: 65536, Efforts: []string{"none", "high"}, OptionsKnown: true},
		"hy4-preview":                {SDK: sdkCompat, Output: 64000, Efforts: []string{"none", "high"}, OptionsKnown: true},
		"iceman-contributor-free":    {SDK: sdkCompat, Output: 131072, OptionsKnown: true},
		"kimi-k2.7-code":             {SDK: sdkCompat, Output: 262144, OptionsKnown: true},
		"kimi-k3":                    {SDK: sdkCompat, Output: 131072, Efforts: []string{"max"}, OptionsKnown: true},
		"kimi-k3.1-pro":              {SDK: sdkCompat, Output: 131072, Efforts: []string{"low", "high", "max"}, OptionsKnown: true},
		"longcat-2.0":                {SDK: sdkCompat, Output: 131072, Toggle: true, OptionsKnown: true},
		"longcat-2.5-preview-free":   {SDK: sdkCompat, Output: 131072, Toggle: true, OptionsKnown: true},
		"mimo-v2.5":                  {SDK: sdkCompat, Output: 128000, OptionsKnown: true},
		"mimo-v2.5-pro":              {SDK: sdkCompat, Output: 128000, OptionsKnown: true},
		"mimo-v2.6-flash":            {SDK: sdkCompat, Output: 131072, OptionsKnown: true},
		"mimo-v2.6-pro":              {SDK: sdkCompat, Output: 131072, OptionsKnown: true},
		"minimax-m2.7":               {SDK: sdkAnthropic, Output: 131072, OptionsKnown: true},
		"minimax-m3":                 {SDK: sdkAnthropic, Output: 131072, Toggle: true, OptionsKnown: true},
		"minimax-m3.1-flash":         {SDK: sdkCompat, Output: 512000, Efforts: []string{"low", "medium", "high", "xhigh", "max"}, OptionsKnown: true},
		"minimax-m3.1-flash-free":    {SDK: sdkCompat, Output: 512000, Efforts: []string{"low", "medium", "high", "xhigh", "max"}, OptionsKnown: true},
		"muse-spark-1.2-contributor": {SDK: sdkOpenAI, Output: 131072, Efforts: []string{"minimal", "low", "medium", "high", "xhigh"}, OptionsKnown: true},
		"muse-spark-1.3-contributor": {SDK: sdkOpenAI, Output: 131072, Efforts: []string{"minimal", "low", "medium", "high", "xhigh"}, OptionsKnown: true},
		"qwen3.7-plus":               {SDK: sdkAnthropic, Output: 65536, Toggle: true, Budget: true, BudgetMax: 262144, OptionsKnown: true},
		"qwen3.8-flash":              {SDK: sdkAnthropic, Output: 131072, Efforts: []string{"low", "medium", "xhigh"}, Toggle: true, Budget: true, OptionsKnown: true},
		"qwen3.8-max":                {SDK: sdkAnthropic, Output: 131072, Efforts: []string{"low", "medium", "xhigh"}, Toggle: true, Budget: true, BudgetMax: 262144, OptionsKnown: true},
		"space-bunny":                {SDK: sdkCompat, Output: 524288, Efforts: []string{"low", "medium", "high", "xhigh", "max"}, OptionsKnown: true},
		"step-5-preview":             {SDK: sdkCompat, Output: 65536, Efforts: []string{"low", "medium", "high"}, OptionsKnown: true},
		"step-5-preview-free":        {SDK: sdkCompat, Output: 65536, Efforts: []string{"low", "medium", "high"}, OptionsKnown: true},
		// Rows the live Go snapshot no longer lists. They stay pinned so stored
		// targets keep a known endpoint family and reasoning policy.
		"deepseek-v4-flash":            {SDK: sdkCompat, Output: 384000, Efforts: []string{"low", "high", "max"}, OptionsKnown: true},
		"deepseek-v4-flash-vision-exp": {SDK: sdkCompat, Output: 384000, Efforts: []string{"low", "high", "max"}, Toggle: true, OptionsKnown: true},
		"glm-5":                        {SDK: sdkCompat, Output: 32768, OptionsKnown: true},
		"glm-5.1":                      {SDK: sdkCompat, Output: 32768, OptionsKnown: true},
		"grok-4.5":                     {SDK: sdkOpenAI, Output: 500000, Efforts: []string{"low", "medium", "high"}, OptionsKnown: true},
		"grok-4.6":                     {SDK: sdkOpenAI, Output: 500000, Efforts: []string{"low", "medium", "high", "xhigh"}, OptionsKnown: true},
		"kimi-k2.5":                    {SDK: sdkCompat, Output: 65536, OptionsKnown: true},
		"kimi-k2.6":                    {SDK: sdkCompat, Output: 65536, OptionsKnown: true},
		"mimo-v2-omni":                 {SDK: sdkCompat, Output: 128000, OptionsKnown: true},
		"mimo-v2-pro":                  {SDK: sdkCompat, Output: 128000, OptionsKnown: true},
		"minimax-m2.5":                 {SDK: sdkAnthropic, Output: 65536, OptionsKnown: true},
		"omen-alpha":                   {SDK: sdkCompat, Output: 128000, Efforts: []string{"low", "high"}, OptionsKnown: true},
		"ox-alpha-free":                {SDK: sdkCompat, Output: 131072, Efforts: []string{"low", "high", "max"}, OptionsKnown: true},
		"qwen3.5-plus":                 {SDK: sdkCompat, Output: 65536, Toggle: true, Budget: true, BudgetMax: 81920, OptionsKnown: true},
		"qwen3.6-plus":                 {SDK: sdkCompat, Output: 65536, Toggle: true, Budget: true, BudgetMax: 81920, OptionsKnown: true},
		"qwen3.7-max":                  {SDK: sdkCompat, Output: 65536, Toggle: true, Budget: true, BudgetMax: 262144, OptionsKnown: true},
		"space-bunny-free":             {SDK: sdkCompat, Output: 524288, Efforts: []string{"low", "medium", "high", "xhigh", "max"}, OptionsKnown: true},
	},
}

// Lookup returns the catalog row for a tier and model id. The id may carry a
// vendor prefix or a trailing (level) suffix. ok is false when the row is absent.
func Lookup(tier, model string) (ModelSpec, bool) {
	rows := catalog[tier]
	if rows == nil {
		rows = catalog[zenTier]
	}
	id := NormalizeModelID(model)
	spec, ok := rows[id]
	if ok {
		spec.ID = id
	}
	return spec, ok
}

// NormalizeModelID lowercases model, drops a vendor prefix, and drops a
// trailing (level) suffix.
func NormalizeModelID(model string) string {
	m := strings.ToLower(strings.TrimSpace(model))
	if i := strings.LastIndex(m, "/"); i >= 0 {
		m = m[i+1:]
	}
	if i := strings.LastIndex(m, "("); i >= 0 && strings.HasSuffix(m, ")") {
		m = strings.TrimSpace(m[:i])
	}
	return m
}

// EndpointFor reports the upstream family for a catalog row.
// Google native thinkingConfig is deferred: Zen serves those models at
// /v1/models/{id}, which is not a shared codec path, so they stay on chat and
// the writer strips reasoning controls.
func EndpointFor(spec ModelSpec, ok bool) string {
	if !ok {
		return EndpointChat
	}
	switch spec.SDK {
	case sdkOpenAI:
		return EndpointResponses
	case sdkAnthropic:
		return EndpointMessages
	default:
		return EndpointChat
	}
}
