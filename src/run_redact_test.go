package bus

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestRedactRunArgs(t *testing.T) {
	t.Setenv("EXAMPLE_API_KEY", "known-environment-value")
	for _, tc := range []struct {
		name    string
		args    []string
		secrets []string
	}{
		{"flags", []string{"tool", "--api-key", "short-secret", "--password=another-secret"}, []string{"short-secret", "another-secret"}},
		{"assignments", []string{"env", "DATABASE_PASSWORD='short secret'", "TOKEN=tiny", "Authorization: Bearer abcdef"}, []string{"short secret", "tiny", "abcdef"}},
		{"url", []string{"curl", "https://user:shortpass@example.org/?token=tiny&view=public"}, []string{"user", "shortpass", "tiny"}},
		{"shell", []string{"bash", "-lc", "echo secret", "positional-secret"}, []string{"echo secret", "positional-secret"}},
		{"eval", []string{"python", "-c", "print('secret')"}, []string{"print('secret')"}},
		{"environment", []string{"tool", "prefix-known-environment-value-suffix"}, []string{"known-environment-value"}},
		{"patterns", []string{"tool", "ghp_abcdef", "sk-live-abc", "AIzaabcdef", "AKIAIOSFODNN7EXAMPLE", "npm_abcdef", "xoxb-abc", "sk_live_abc"}, []string{"ghp_abcdef", "sk-live-abc", "AIzaabcdef", "AKIAIOSFODNN7EXAMPLE", "npm_abcdef", "xoxb-abc", "sk_live_abc"}},
		{"entropy", []string{"tool", "h7Vk2Pw9Xq4Lm8Ns5Rt1", "d8b1326fe9ca7045ab83c219", "prefix=h7Vk2Pw9Xq4Lm8Ns5Rt1"}, []string{"h7Vk2Pw9Xq4Lm8Ns5Rt1", "d8b1326fe9ca7045ab83c219"}},
		{"private key", []string{"tool", "-----BEGIN PRIVATE KEY-----\nsecret\n-----END PRIVATE KEY-----"}, []string{"secret"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			original := append([]string{}, tc.args...)
			got := strings.Join(redactRunArgs(tc.args), " ")
			for _, secret := range tc.secrets {
				if strings.Contains(got, secret) {
					t.Fatalf("secret %q leaked in %q", secret, got)
				}
			}
			if !reflect.DeepEqual(original, tc.args) {
				t.Fatal("execution argv mutated")
			}
		})
	}
	args := []string{"go", "test", "./src", "--count=1", "--output", "report.txt"}
	if got := redactRunArgs(args); !reflect.DeepEqual(got, args) {
		t.Fatalf("ordinary arguments changed: %q", got)
	}
}

func TestWebRunsRedactsSavedHistory(t *testing.T) {
	resourceTestRoot(t)
	if err := os.MkdirAll(filepath.Join(root, "runs"), 0700); err != nil {
		t.Fatal(err)
	}
	legacy, _ := json.Marshal([]runRecord{{ID: "test", Status: "completed", Command: []string{"tool", "--token=legacy-secret", "h7Vk2Pw9Xq4Lm8Ns5Rt1"}}})
	if err := os.WriteFile(filepath.Join(root, "runs", "state.json"), legacy, 0600); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRecorder()
	(&webServer{}).runs(r, httptest.NewRequest("GET", "/api/runs", nil))
	if r.Code != 200 || r.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("response %d %v", r.Code, r.Header())
	}
	if strings.Contains(r.Body.String(), "legacy-secret") || strings.Contains(r.Body.String(), "h7Vk2Pw9Xq4Lm8Ns5Rt1") {
		t.Fatal("raw secret in API")
	}
	var data struct {
		Runs []runRecord `json:"runs"`
	}
	if err := json.Unmarshal(r.Body.Bytes(), &data); err != nil || len(data.Runs) != 1 {
		t.Fatalf("invalid response: %s", r.Body.String())
	}
	// The common stored representation is sanitized as well.
	record := newRunRecord(runOptions{argv: []string{"tool", "--token=short-secret"}})
	if err := saveRun(record); err != nil {
		t.Fatal(err)
	}
	stored, err := os.ReadFile(filepath.Join(root, "runs", "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(stored), `--token=short-secret`) {
		t.Fatal("new run persisted raw secret")
	}
}
