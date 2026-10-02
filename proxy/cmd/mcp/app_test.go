package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"strings"
	"testing"

	"github.com/prodeko/prodeko-org/proxy/internal/ghauth"
)

var appKeyPEM = func() string {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
}()

func TestLoadEnvGitHubApp(t *testing.T) {
	b64 := base64.StdEncoding.EncodeToString([]byte(appKeyPEM))
	for _, tc := range []struct {
		name   string
		edit   func(map[string]string)
		badVar string // "" means accepted as an App
		dryRun bool
	}{
		{"pem", func(m map[string]string) {
			m["GITHUB_REPO"], m["GITHUB_APP_ID"], m["GITHUB_APP_PRIVATE_KEY"] = "prodeko/prodeko-org", "5161706", appKeyPEM
		}, "", false},
		{"base64 pem", func(m map[string]string) {
			m["GITHUB_REPO"], m["GITHUB_APP_ID"], m["GITHUB_APP_PRIVATE_KEY"] = "prodeko/prodeko-org", "5161706", b64
		}, "", false},
		{"token and app", func(m map[string]string) {
			m["GITHUB_REPO"], m["GITHUB_TOKEN"], m["GITHUB_APP_ID"], m["GITHUB_APP_PRIVATE_KEY"] = "prodeko/prodeko-org", "ghp_x", "5161706", appKeyPEM
		}, "GITHUB_TOKEN", false},
		{"app without repo", func(m map[string]string) {
			m["GITHUB_APP_ID"], m["GITHUB_APP_PRIVATE_KEY"] = "5161706", appKeyPEM
		}, "GITHUB_REPO", false},
		{"id without key", func(m map[string]string) {
			m["GITHUB_REPO"], m["GITHUB_APP_ID"] = "prodeko/prodeko-org", "5161706"
		}, "GITHUB_APP_PRIVATE_KEY", false},
		{"client id instead of app id", func(m map[string]string) {
			m["GITHUB_REPO"], m["GITHUB_APP_ID"], m["GITHUB_APP_PRIVATE_KEY"] = "prodeko/prodeko-org", "Iv23liWuIK3MzLrPVfPi", appKeyPEM
		}, "GITHUB_APP_ID", false},
		{"not a key", func(m map[string]string) {
			m["GITHUB_REPO"], m["GITHUB_APP_ID"], m["GITHUB_APP_PRIVATE_KEY"] = "prodeko/prodeko-org", "5161706", "hunter2"
		}, "GITHUB_APP_PRIVATE_KEY", false},
		{"neither is a dry run", func(map[string]string) {}, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			vars := completeEnv()
			tc.edit(vars)
			cfg, err := loadEnv(lookupFrom(vars))
			if tc.badVar != "" {
				if err == nil || !strings.Contains(err.Error(), tc.badVar) {
					t.Fatalf("err = %v, want one naming %s", err, tc.badVar)
				}
				if strings.Contains(err.Error(), "PRIVATE KEY-----") || strings.Contains(err.Error(), "hunter2") {
					t.Errorf("error leaks the key: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("loadEnv: %v", err)
			}
			cred, err := githubCredential(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if tc.dryRun {
				if cred != nil {
					t.Fatalf("credential = %T, want none", cred)
				}
				return
			}
			if _, ok := cred.(*ghauth.App); !ok {
				t.Fatalf("credential = %T, want *ghauth.App", cred)
			}
			if banner := cfg.String(); strings.Contains(banner, "PRIVATE KEY") || strings.Contains(banner, b64[:40]) {
				t.Errorf("banner leaks the key:\n%s", banner)
			}
		})
	}
}
