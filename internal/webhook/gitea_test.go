package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"testing"
)

// giteaSig computes the X-Gitea-Signature header value for body under secret
// exactly like Gitea does (services/webhook/deliver.go): the bare hex
// HMAC-SHA256 of the raw body, without the "sha256=" prefix GitHub uses.
func giteaSig(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// TestWebhookProviderSignatures is the authentication matrix of POST /webhook:
// a valid Gitea signature is accepted in the bare-hex form Gitea sends (and
// with a "sha256=" prefix), a wrong digest, a missing header and any foreign
// algorithm prefix are rejected, and the GitHub and GitLab paths
// keep working exactly as before.
func TestWebhookProviderSignatures(t *testing.T) {
	const secret = "s3cr3t"
	body := []byte(`{"ref":"refs/heads/main","repository":{"name":"host"}}`)
	digest := giteaSig(secret, body)

	tests := []struct {
		name    string
		secret  string
		headers map[string]string
		want    int
	}{
		{
			// The form Gitea actually sends, and the only header that
			// counts here: X-Hub-Signature-256 is deliberately absent.
			name:    "gitea valid signature (bare hex)",
			secret:  secret,
			headers: map[string]string{giteaSignatureHeader: digest},
			want:    http.StatusOK,
		},
		{
			name:    "gitea signature with sha256= prefix",
			secret:  secret,
			headers: map[string]string{giteaSignatureHeader: signaturePrefix + digest},
			want:    http.StatusOK,
		},
		{
			// A real Gitea delivery carries both headers.
			name:   "gitea delivery with both headers",
			secret: secret,
			headers: map[string]string{
				giteaSignatureHeader:  digest,
				githubSignatureHeader: signaturePrefix + digest,
			},
			want: http.StatusOK,
		},
		{
			name:    "gitea wrong secret",
			secret:  secret,
			headers: map[string]string{giteaSignatureHeader: giteaSig("other", body)},
			want:    http.StatusUnauthorized,
		},
		{
			name:    "gitea signature over other body",
			secret:  secret,
			headers: map[string]string{giteaSignatureHeader: giteaSig(secret, []byte(`{"ref":"other"}`))},
			want:    http.StatusUnauthorized,
		},
		{
			name:    "gitea header missing",
			secret:  secret,
			headers: nil,
			want:    http.StatusUnauthorized,
		},
		{
			name:    "gitea foreign algorithm prefix",
			secret:  secret,
			headers: map[string]string{giteaSignatureHeader: "sha1=" + digest},
			want:    http.StatusUnauthorized,
		},
		{
			name:    "gitea digest not hex",
			secret:  secret,
			headers: map[string]string{giteaSignatureHeader: "not-hex"},
			want:    http.StatusUnauthorized,
		},
		{
			name:    "github valid signature",
			secret:  secret,
			headers: map[string]string{githubSignatureHeader: githubSig(secret, body)},
			want:    http.StatusOK,
		},
		{
			name:    "github wrong signature",
			secret:  secret,
			headers: map[string]string{githubSignatureHeader: githubSig("other", body)},
			want:    http.StatusUnauthorized,
		},
		{
			name:    "gitlab valid token",
			secret:  secret,
			headers: map[string]string{gitlabTokenHeader: secret},
			want:    http.StatusOK,
		},
		{
			name:    "gitlab wrong token",
			secret:  secret,
			headers: map[string]string{gitlabTokenHeader: "wrong"},
			want:    http.StatusUnauthorized,
		},
		{
			// No secret, no endpoint: an unauthenticated trigger is never
			// served.
			name:    "no secret configured",
			secret:  "",
			headers: nil,
			want:    http.StatusServiceUnavailable,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			triggers := 0
			h := New(Deps{Secret: tc.secret, Trigger: countingTrigger(&triggers), Logger: quietLogger()})
			rec := do(h, http.MethodPost, "/webhook", body, tc.headers)
			if rec.Code != tc.want {
				t.Errorf("POST /webhook status = %d, want %d", rec.Code, tc.want)
			}
			wantTriggers := 0
			if tc.want == http.StatusOK {
				wantTriggers = 1
			}
			if triggers != wantTriggers {
				t.Errorf("trigger calls = %d, want %d", triggers, wantTriggers)
			}
		})
	}
}
