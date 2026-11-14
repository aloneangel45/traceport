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
	// URL basic auth credentials inside authority: scheme://user:pass@host
	{
		re:   regexp.MustCompile("([a-zA-Z][a-zA-Z0-9+.-]*://)([^\\s/:@?#\"'`]+):([^\\s/@?#\"'`]+)@"),
		repl: "${1}" + Mask + ":" + Mask + "@",
	},
	// HTTP / CLI Authorization Bearer tokens
	{
		re:   regexp.MustCompile(`(?i)(\bBearer\s+)([A-Za-z0-9._~+/=:-]{6,})`),
		repl: "${1}" + Mask,
	},
	// Anthropic keys (sk-ant-*)
	{
		re:   regexp.MustCompile(`\bsk-ant-[A-Za-z0-9_-]{8,}\b`),
		repl: Mask,
	},
	// OpenAI project keys (sk-proj-*) and standard keys (sk-*)
	{
		re:   regexp.MustCompile(`\bsk-proj-[A-Za-z0-9_-]{8,}\b`),
		repl: Mask,
	},
	{
		re:   regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{12,}\b`),
		repl: Mask,
	},
	// GitHub classic & fine-grained PATs / OAuth / App tokens
	{
		re:   regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9_]{8,}|github_pat_[A-Za-z0-9_]{8,})\b`),
		repl: Mask,
	},
	// Google API keys (AIza*)
	{
		re:   regexp.MustCompile(`\bAIza[A-Za-z0-9_-]{12,}\b`),
		repl: Mask,
	},
	// AWS long-term & temporary access key IDs (AKIA*, ASIA*)
	{
		re:   regexp.MustCompile(`\b(?:AKIA|ASIA)[A-Z0-9]{12,}\b`),
		repl: Mask,
	},
	// HuggingFace tokens (hf_*)
	{
		re:   regexp.MustCompile(`\bhf_[A-Za-z0-9]{8,}\b`),
		repl: Mask,
	},
	// Slack tokens (xoxb-, xoxa-, xoxp-, xoxr-, xoxs-)
	{
		re:   regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9-]{8,}\b`),
		repl: Mask,
	},
}

var sensitiveFieldNames = map[string]bool{
	"password":      true,
	"passwd":        true,
	"client_secret": true,
	"private_key":   true,
	"access_token":  true,
	"refresh_token": true,
	"api_key":       true,
	"apikey":        true,
	"secret_key":    true,
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
			if isSensitiveKey(k) && strings.TrimSpace(val) != "" {
				cp[k] = Mask
			} else {
				cp[k] = String(val)
			}
		}
		return cp
	case map[string]interface{}:
		cp := make(map[string]interface{}, len(typed))
		for k, val := range typed {
			if strVal, ok := val.(string); ok && isSensitiveKey(k) && strings.TrimSpace(strVal) != "" {
				cp[k] = Mask
			} else {
				cp[k] = Value(val)
			}
		}
		return cp
	default:
		return typed
	}
}

func isSensitiveKey(k string) bool {
	norm := strings.ToLower(strings.TrimSpace(k))
	norm = strings.ReplaceAll(norm, "-", "_")
	return sensitiveFieldNames[norm]
}

// Session returns a deep-cloned copy of sess with sensitive credentials scrubbed across
// command strings, frame contents, and tool invocation arguments and results.
func Session(sess *model.Session) *model.Session {
	if sess == nil {
		return nil
	}
	cp := *sess
	cp.Command = String(sess.Command)
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
