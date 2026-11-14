package redact

import (
	"strings"
	"testing"
	"time"

	"github.com/aloneangel45/traceport/pkg/model"
)

func TestString_AllCredentialFamilies(t *testing.T) {
	openAIClassic := "sk-" + "1234567890abcdef12345678"
	openAIProj := "sk-proj-" + "AbCdEf1234567890XyZ_test"
	anthropicKey := "sk-ant-" + "api03-9876543210abcdefABCDEF"
	ghPAT := "ghp_" + "1234567890abcdefghijklmnop"
	ghOAuth := "gho_" + "abcdef1234567890"
	ghApp := "ghs_" + "fedcba0987654321"
	ghFine := "github_pat_" + "11ABCDEF0123456789_xyzXYZ987654321"
	googleKey := "AIza" + "SyDaGmWKa4JsXZ-0123456789abcdef"
	awsAKIA := "AKIA" + "IOSFODNN7EXAMPLE"
	awsASIA := "ASIA" + "Q987654321ABCDEF"
	hfToken := "hf_" + "AbCdEfGhIjKlMnOpQrStUvWxYz123456"
	slackBot := "xox" + "b-1234567890-0987654321-abcdefGHIJKL"
	slackApp := "xox" + "a-2-1234567890-abcdef"
	slackUser := "xox" + "p-111222333-444555666"
	slackRef := "xox" + "r-999888777666"
	slackSess := "xox" + "s-555444333222"
	jwtTok := "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9" + ".payload.sig"
	basicPass := "s3cr3t" + "P4ss!"

	cases := []struct {
		name   string
		input  string
		secret string
		keep   string
	}{
		{
			name:   "OpenAI classic key",
			input:  "OPENAI_API_KEY=" + openAIClassic + " python run.py",
			secret: openAIClassic,
			keep:   "OPENAI_API_KEY=[REDACTED] python run.py",
		},
		{
			name:   "OpenAI project key",
			input:  `{"key":"` + openAIProj + `"}`,
			secret: openAIProj,
			keep:   `{"key":"[REDACTED]"}`,
		},
		{
			name:   "Anthropic key",
			input:  "ANTHROPIC_API_KEY=" + anthropicKey,
			secret: anthropicKey,
			keep:   "ANTHROPIC_API_KEY=[REDACTED]",
		},
		{
			name:   "GitHub PAT classic ghp",
			input:  "token " + ghPAT,
			secret: ghPAT,
			keep:   "token [REDACTED]",
		},
		{
			name:   "GitHub OAuth gho and App ghs",
			input:  "oauth=" + ghOAuth + " app=" + ghApp,
			secret: ghOAuth,
			keep:   "oauth=[REDACTED] app=[REDACTED]",
		},
		{
			name:   "GitHub fine-grained PAT",
			input:  "export GH_TOKEN=" + ghFine,
			secret: ghFine,
			keep:   "export GH_TOKEN=[REDACTED]",
		},
		{
			name:   "Google API key",
			input:  "https://generativelanguage.googleapis.com/v1?key=" + googleKey,
			secret: googleKey,
			keep:   "https://generativelanguage.googleapis.com/v1?key=[REDACTED]",
		},
		{
			name:   "AWS AKIA and ASIA access keys",
			input:  "aws_key=" + awsAKIA + " temp_key=" + awsASIA,
			secret: awsAKIA,
			keep:   "aws_key=[REDACTED] temp_key=[REDACTED]",
		},
		{
			name:   "HuggingFace token",
			input:  "HF_TOKEN=" + hfToken,
			secret: hfToken,
			keep:   "HF_TOKEN=[REDACTED]",
		},
		{
			name:   "Slack bot and app tokens",
			input:  "bot=" + slackBot + " app=" + slackApp + " user=" + slackUser + " refresh=" + slackRef + " session=" + slackSess,
			secret: slackBot,
			keep:   "bot=[REDACTED] app=[REDACTED] user=[REDACTED] refresh=[REDACTED] session=[REDACTED]",
		},
		{
			name:   "Bearer authorization header",
			input:  `curl -H "Authorization: Bearer ` + jwtTok + `" https://api.internal`,
			secret: jwtTok,
			keep:   `curl -H "Authorization: Bearer [REDACTED]" https://api.internal`,
		},
		{
			name:   "URL basic auth credentials",
			input:  "git clone https://deploy_user:" + basicPass + "@github.com/org/private.git",
			secret: basicPass,
			keep:   "git clone https://[REDACTED]:[REDACTED]@github.com/org/private.git",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := String(tc.input)
			if strings.Contains(got, tc.secret) {
				t.Fatalf("secret %q leaked in output: %q", tc.secret, got)
			}
			if got != tc.keep {
				t.Fatalf("got %q, want %q", got, tc.keep)
			}
		})
	}
}

func TestString_PreservesSafeURLsAndIdentifiers(t *testing.T) {
	safeInputs := []string{
		"https://example.com:8080/api/v1/users?email=alice@example.com",
		"hf_model_cache_dir=/tmp/models",
		"package main // normal source code without credentials",
	}
	for _, in := range safeInputs {
		if got := String(in); got != in {
			t.Fatalf("unexpected redaction on safe string:\n in:  %q\n got: %q", in, got)
		}
	}
}

func TestSession_DeepCopyAndScrubbing(t *testing.T) {
	projKey := "sk-proj-" + "secretProjectKey999999"
	ghTok := "ghp_" + "secretTokenValue123456"
	bearerTok := "superSecret" + "JwtToken123456"
	plainPW := "hunter2" + "_plaintext_pw"
	gKey := "AIza" + "SySecretGoogleKey0123456789abcd"

	orig := &model.Session{
		ID:        "trace_sec_1",
		Command:   "OPENAI_API_KEY=" + projKey + " python agent.py",
		Tags:      []string{"security", "audit"},
		StartTime: time.Now().UTC(),
		Status:    "completed",
		Frames: []model.Frame{
			{
				Index:   1,
				Type:    model.EventOutput,
				Content: "Cloning https://bot:" + ghTok + "@github.com/org/repo.git",
			},
			{
				Index:   2,
				Type:    model.EventToolCall,
				Content: "calling http_request",
				Tool: &model.ToolInvocation{
					Name: "http_request",
					Arguments: map[string]interface{}{
						"url":        "https://api.service.io/v1",
						"headers":    map[string]interface{}{"Authorization": "Bearer " + bearerTok},
						"password":   plainPW,
						"max_tokens": 512,
					},
					Result: `{"google_key":"` + gKey + `"}`,
				},
			},
		},
	}

	scrubbed := Session(orig)

	// Verify original session was NOT mutated in place.
	if !strings.Contains(orig.Command, projKey) {
		t.Fatalf("original session command was mutated")
	}
	origHeaders := orig.Frames[1].Tool.Arguments["headers"].(map[string]interface{})
	if !strings.Contains(origHeaders["Authorization"].(string), bearerTok) {
		t.Fatalf("original tool arguments map was mutated")
	}

	// Verify scrubbed copy removed every secret.
	for _, forbidden := range []string{projKey, ghTok, bearerTok, plainPW, gKey} {
		if strings.Contains(scrubbed.Command, forbidden) ||
			strings.Contains(scrubbed.Frames[0].Content, forbidden) ||
			strings.Contains(scrubbed.Frames[1].Tool.Result, forbidden) {
			t.Fatalf("forbidden secret %q survived Session redaction", forbidden)
		}
	}

	scrubbedHeaders := scrubbed.Frames[1].Tool.Arguments["headers"].(map[string]interface{})
	if scrubbedHeaders["Authorization"] != "Bearer [REDACTED]" {
		t.Fatalf("unexpected scrubbed Authorization header: %v", scrubbedHeaders["Authorization"])
	}
	if scrubbed.Frames[1].Tool.Arguments["password"] != Mask {
		t.Fatalf("expected sensitive password key to be masked, got %v", scrubbed.Frames[1].Tool.Arguments["password"])
	}
	if scrubbed.Frames[1].Tool.Arguments["max_tokens"] != 512 {
		t.Fatalf("expected non-secret numeric argument max_tokens=512, got %v", scrubbed.Frames[1].Tool.Arguments["max_tokens"])
	}
}

func TestRedact_PEMPrivateKeysQueryParamsHeadersAndNestedSlices(t *testing.T) {
	pemKey := "-----BEGIN OPENSSH PRIVATE KEY-----\n" +
		"b3BlbnNzaC1rZXktdjEAAAAABG5vbmUAAAAEbm9uZQAAAAAAAAABAAAAMwAAAAtzc2gtZW\n" +
		"-----END OPENSSH PRIVATE KEY-----"
	urlSecret := "myQuerySecretValue987654"
	xApiVal := "customXApiKeySecret123456"
	awsSecret := "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
	stripeKey := "sk_live_" + "51AbCdEfGhIjKlMnOpQrStUv"
	gitlabPat := "glpat-" + "abcdef1234567890XYZ"
	npmTok := "npm_" + "1234567890abcdefABCDEF"

	rawText := strings.Join([]string{
		"PEM:\n" + pemKey,
		"URL: https://api.example.com/v1/hook?api_key=" + urlSecret + "&mode=fast",
		"HDR: X-API-Key: " + xApiVal,
		"ENV: AWS_SECRET_ACCESS_KEY=" + awsSecret,
		"STRIPE: " + stripeKey,
		"GITLAB: " + gitlabPat,
		"NPM: " + npmTok,
	}, "\n")

	scrubbedText := String(rawText)
	for _, secret := range []string{"b3BlbnNzaC1rZXktdjE", urlSecret, xApiVal, awsSecret, stripeKey, gitlabPat, npmTok} {
		if strings.Contains(scrubbedText, secret) {
			t.Fatalf("secret %q leaked in String() output:\n%s", secret, scrubbedText)
		}
	}
	if !strings.Contains(scrubbedText, "&mode=fast") {
		t.Fatalf("expected non-secret query parameter &mode=fast preserved, got:\n%s", scrubbedText)
	}

	sess := &model.Session{
		ID:           "trace_sec_deep",
		Command:      "curl https://svc.local?access_token=" + urlSecret,
		WorkspaceDir: "/home/ci_" + gitlabPat + "/repo",
		Frames: []model.Frame{
			{
				Index:   1,
				Type:    model.EventToolCall,
				Content: "invoke deploy",
				Tool: &model.ToolInvocation{
					Name: "deploy",
					Arguments: map[string]interface{}{
						"headers": map[string]interface{}{
							"x-api-key":     "opaqueHeaderSecret999",
							"Authorization": "Basic dXNlcjpzZWNyZXRfcGFzc3dvcmQ=",
						},
						"api_keys":       []interface{}{"rawKeyOne", "rawKeyTwo"},
						"webhook_secret": "opaqueWebhookSecret",
					},
				},
			},
		},
	}

	cleanSess := Session(sess)
	if strings.Contains(cleanSess.WorkspaceDir, gitlabPat) {
		t.Fatalf("WorkspaceDir leaked GitLab token: %q", cleanSess.WorkspaceDir)
	}
	hdrs := cleanSess.Frames[0].Tool.Arguments["headers"].(map[string]interface{})
	if hdrs["x-api-key"] != Mask {
		t.Fatalf("expected x-api-key masked, got %v", hdrs["x-api-key"])
	}
	if hdrs["Authorization"] != "Basic [REDACTED]" {
		t.Fatalf("expected Basic auth masked, got %v", hdrs["Authorization"])
	}
	keysSlice := cleanSess.Frames[0].Tool.Arguments["api_keys"].([]interface{})
	if keysSlice[0] != Mask || keysSlice[1] != Mask {
		t.Fatalf("expected slice under sensitive key api_keys masked, got %v", keysSlice)
	}
	if cleanSess.Frames[0].Tool.Arguments["webhook_secret"] != Mask {
		t.Fatalf("expected webhook_secret masked, got %v", cleanSess.Frames[0].Tool.Arguments["webhook_secret"])
	}
}
