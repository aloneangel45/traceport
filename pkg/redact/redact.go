package redact

import (
	"regexp"
	"strings"

	"github.com/aloneangel45/traceport/pkg/model"
)

// Mask is the placeholder substituted for redacted secret material.
const Mask = "[REDACTED]"

type replacerRule struct {
	re   *regexp.Regexp
	repl string
}

var credentialRules = []replacerRule{
	// PEM / OpenSSH / PGP private key blocks (complete and crash-truncated)
	{
		re:   regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 -]*PRIVATE KEY(?: BLOCK)?-----.*?-----END [A-Z0-9 -]*PRIVATE KEY(?: BLOCK)?-----`),
		repl: Mask,
	},
	{
		re:   regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 -]*PRIVATE KEY(?: BLOCK)?-----[\r\n]+[A-Za-z0-9+/=\r\n: -]{16,}`),
		repl: Mask,
	},
	// URL basic auth credentials inside authority: scheme://user:pass@host
	{
		re:   regexp.MustCompile("([a-zA-Z][a-zA-Z0-9+.-]*://)([^\\s/:@?#\"'`]+):([^\\s/@?#\"'`]+)@"),
		repl: "${1}" + Mask + ":" + Mask + "@",
	},
	// URL query-string credential parameters (?api_key=..., &token=..., &access_token=..., etc.)
	{
		re:   regexp.MustCompile("(?i)([?&](?:api_key|apikey|x-api-key|access_token|refresh_token|auth_token|client_secret|webhook_secret|private_key|secret_key|password|passwd|token|secret)=)([^&\\s\"'`<>#]+)"),
		repl: "${1}" + Mask,
	},
	// HTTP / CLI Authorization Bearer and Basic headers
	{
		re:   regexp.MustCompile(`(?i)(\bBearer\s+)([A-Za-z0-9._~+/=:-]{6,})`),
		repl: "${1}" + Mask,
	},
	{
		re:   regexp.MustCompile(`(?i)(\bAuthorization\s*:\s*Basic\s+)([A-Za-z0-9+/=_-]{6,})`),
		repl: "${1}" + Mask,
	},
	// Explicit X-API-Key headers and CLI/env secret assignments
	{
		re:   regexp.MustCompile(`(?i)(\bx-api-key\s*[:=]\s*["']?)([A-Za-z0-9._~+/=-]{8,})`),
		repl: "${1}" + Mask,
	},
	{
		re:   regexp.MustCompile(`(?i)(\b(?:AWS_SECRET_ACCESS_KEY|AWS_SESSION_TOKEN|CLIENT_SECRET|WEBHOOK_SECRET|API_SECRET|DATABASE_PASSWORD|DB_PASSWORD|PGPASSWORD)\s*=\s*["']?)([^\s"';&|]+)`),
		repl: "${1}" + Mask,
	},
	// Anthropic keys (sk-ant-*)
	{
		re:   regexp.MustCompile(`sk-ant-[A-Za-z0-9_-]{8,}`),
		repl: Mask,
	},
	// OpenAI project keys (sk-proj-*) and standard keys (sk-*)
	{
		re:   regexp.MustCompile(`sk-proj-[A-Za-z0-9_-]{8,}`),
		repl: Mask,
	},
	// Stripe live/test secret, restricted, and webhook signing keys
	{
		re:   regexp.MustCompile(`(?:(?:sk|rk)_(?:live|test)_[A-Za-z0-9]{10,}|whsec_[A-Za-z0-9_-]{10,})`),
		repl: Mask,
	},
	{
		re:   regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{12,}\b`),
		repl: Mask,
	},
	// GitHub classic & fine-grained PATs / OAuth / App tokens
	{
		re:   regexp.MustCompile(`(?:gh[pousr]_[A-Za-z0-9_]{8,}|github_pat_[A-Za-z0-9_]{8,})`),
		repl: Mask,
	},
	// GitLab PATs, npm tokens, and SendGrid keys
	{
		re:   regexp.MustCompile(`(?:glpat-[A-Za-z0-9._-]{10,}|(?:\b|_)npm_[A-Za-z0-9]{12,}\b|SG\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,})`),
		repl: Mask,
	},
	// Google API keys (AIza*)
	{
		re:   regexp.MustCompile(`AIza[A-Za-z0-9_-]{12,}`),
		repl: Mask,
	},
	// AWS long-term & temporary access key IDs (AKIA*, ASIA*)
	{
		re:   regexp.MustCompile(`(?:AKIA|ASIA)[A-Z0-9]{12,}`),
		repl: Mask,
	},
	// HuggingFace tokens (hf_*)
	{
		re:   regexp.MustCompile(`\bhf_[A-Za-z0-9]{8,}\b`),
		repl: Mask,
	},
	// Slack tokens (xoxb-, xoxa-, xoxp-, xoxr-, xoxs-)
	{
		re:   regexp.MustCompile(`xox[baprs]-[A-Za-z0-9-]{8,}`),
		repl: Mask,
	},
}

var sensitiveFieldNames = map[string]bool{
	"password":              true,
	"passwd":                true,
	"pwd":                   true,
	"secret":                true,
	"client_secret":         true,
	"webhook_secret":        true,
	"api_secret":            true,
	"private_key":           true,
	"access_token":          true,
	"refresh_token":         true,
	"auth_token":            true,
	"session_token":         true,
	"webhook_token":         true,
	"id_token":              true,
	"token":                 true,
	"api_key":               true,
	"api_keys":              true,
	"apikey":                true,
	"x_api_key":             true,
	"secret_key":            true,
	"aws_secret_access_key": true,
	"aws_session_token":     true,
	"cookie":                true,
	"set_cookie":            true,
}

// String scrubs known API keys, bearer tokens, and basic-auth URL credentials from s.
func String(s string) string {
	if s == "" {
		return ""
	}
	out := s
	for _, rule := range credentialRules {
		out = rule.re.ReplaceAllString(out, rule.repl)
	}
	return out
}

// Value recursively deep-copies and scrubs strings, slices, and maps (such as tool arguments).
func Value(v interface{}) interface{} {
	switch typed := v.(type) {
	case nil:
		return nil
	case string:
		return String(typed)
	case []string:
		cp := make([]string, len(typed))
		for i, elem := range typed {
			cp[i] = String(elem)
		}
		return cp
	case []interface{}:
		cp := make([]interface{}, len(typed))
		for i, elem := range typed {
			cp[i] = Value(elem)
		}
		return cp
	case map[string]string:
		cp := make(map[string]string, len(typed))
		for k, val := range typed {
			cp[k] = scrubKeyedString(k, val)
		}
		return cp
	case map[string]interface{}:
		cp := make(map[string]interface{}, len(typed))
		for k, val := range typed {
			cp[k] = scrubKeyedValue(k, val)
		}
		return cp
	default:
		return typed
	}
}

func normalizeFieldKey(k string) string {
	norm := strings.ToLower(strings.TrimSpace(k))
	return strings.ReplaceAll(norm, "-", "_")
}

func isSensitiveKey(k string) bool {
	return sensitiveFieldNames[normalizeFieldKey(k)]
}

func isAuthHeaderKey(k string) bool {
	norm := normalizeFieldKey(k)
	return norm == "authorization" || norm == "proxy_authorization"
}

func scrubAuthHeaderString(val string) string {
	if strings.TrimSpace(val) == "" {
		return val
	}
	scrubbed := String(val)
	if scrubbed != val {
		return scrubbed
	}
	parts := strings.Fields(val)
	if len(parts) >= 2 {
		return parts[0] + " " + Mask
	}
	return Mask
}

func scrubKeyedString(k, val string) string {
	if isAuthHeaderKey(k) {
		return scrubAuthHeaderString(val)
	}
	if isSensitiveKey(k) && strings.TrimSpace(val) != "" {
		return Mask
	}
	return String(val)
}

func scrubKeyedValue(k string, val interface{}) interface{} {
	if isAuthHeaderKey(k) {
		switch tv := val.(type) {
		case string:
			return scrubAuthHeaderString(tv)
		case []string:
			cp := make([]string, len(tv))
			for i, elem := range tv {
				cp[i] = scrubAuthHeaderString(elem)
			}
			return cp
		case []interface{}:
			cp := make([]interface{}, len(tv))
			for i, elem := range tv {
				if s, ok := elem.(string); ok {
					cp[i] = scrubAuthHeaderString(s)
				} else {
					cp[i] = Value(elem)
				}
			}
			return cp
		}
	}
	if isSensitiveKey(k) {
		switch tv := val.(type) {
		case string:
			if strings.TrimSpace(tv) != "" {
				return Mask
			}
			return tv
		case []string:
			cp := make([]string, len(tv))
			for i, elem := range tv {
				if strings.TrimSpace(elem) != "" {
					cp[i] = Mask
				} else {
					cp[i] = elem
				}
			}
			return cp
		case []interface{}:
			cp := make([]interface{}, len(tv))
			for i, elem := range tv {
				if s, ok := elem.(string); ok {
					if strings.TrimSpace(s) != "" {
						cp[i] = Mask
					} else {
						cp[i] = s
					}
				} else {
					cp[i] = Value(elem)
				}
			}
			return cp
		}
	}
	return Value(val)
}

// Session returns a deep-cloned copy of sess with sensitive credentials scrubbed across
// command strings, workspace paths, frame contents, and tool invocation arguments and results.
func Session(sess *model.Session) *model.Session {
	if sess == nil {
		return nil
	}
	cp := *sess
	cp.Command = String(sess.Command)
	cp.WorkspaceDir = String(sess.WorkspaceDir)
	if sess.Tags != nil {
		cp.Tags = append([]string{}, sess.Tags...)
	}
	cp.Frames = make([]model.Frame, len(sess.Frames))
	for i, f := range sess.Frames {
		frameCopy := f
		frameCopy.Content = String(f.Content)
		if f.Tool != nil {
			toolCopy := *f.Tool
			toolCopy.Name = String(f.Tool.Name)
			toolCopy.Result = String(f.Tool.Result)
			if f.Tool.Arguments != nil {
				if scrubbedMap, ok := Value(f.Tool.Arguments).(map[string]interface{}); ok {
					toolCopy.Arguments = scrubbedMap
				}
			}
			frameCopy.Tool = &toolCopy
		}
		cp.Frames[i] = frameCopy
	}
	return &cp
}
