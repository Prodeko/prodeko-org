package ghauth

import (
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
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

var testKey = func() *rsa.PrivateKey {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return key
}()

// fakeGitHub answers the two App endpoints and records what it was asked.
type fakeGitHub struct {
	t *testing.T

	mu             sync.Mutex
	installationID int64
	lookups        int
	mints          int
	expiresIn      time.Duration
	now            func() time.Time
	mintStatus     int
	lastJWT        string
	lastMintBody   map[string]any
}

func (f *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastJWT = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/repos/prodeko/prodeko-org/installation":
		f.lookups++
		if f.installationID == 0 {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"message":"Not Found"}`)
			return
		}
		fmt.Fprintf(w, `{"id":%d}`, f.installationID)
	case r.Method == http.MethodPost && r.URL.Path == fmt.Sprintf("/app/installations/%d/access_tokens", f.installationID):
		if f.mintStatus != 0 {
			w.WriteHeader(f.mintStatus)
			fmt.Fprint(w, `{"message":"Not Found"}`)
			return
		}
		f.mints++
		body, _ := io.ReadAll(r.Body)
		f.lastMintBody = nil
		_ = json.Unmarshal(body, &f.lastMintBody)
		expires := f.now().Add(f.expiresIn).UTC().Format(time.RFC3339)
		fmt.Fprintf(w, `{"token":"ghs_token%d","expires_at":%q}`, f.mints, expires)
	default:
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"message":"Not Found"}`)
	}
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func newApp(t *testing.T) (*App, *fakeGitHub, *clock) {
	t.Helper()
	c := &clock{t: time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)}
	fake := &fakeGitHub{t: t, installationID: 42, expiresIn: time.Hour, now: c.now}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	app, err := NewApp(AppConfig{
		AppID:      "5161689",
		PrivateKey: testKey,
		Owner:      "prodeko",
		Repo:       "prodeko-org",
		APIRoot:    srv.URL,
		Now:        c.now,
	})
	if err != nil {
		t.Fatal(err)
	}
	return app, fake, c
}

func TestAppMintsAndCaches(t *testing.T) {
	app, fake, c := newApp(t)
	ctx := context.Background()

	tok, err := app.Token(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if tok != "ghs_token1" {
		t.Fatalf("token = %q", tok)
	}

	c.t = c.t.Add(50 * time.Minute)
	if tok, _ = app.Token(ctx); tok != "ghs_token1" {
		t.Fatalf("token within the hour = %q, want the cached one", tok)
	}
	if fake.mints != 1 || fake.lookups != 1 {
		t.Fatalf("mints=%d lookups=%d, want 1 and 1", fake.mints, fake.lookups)
	}

	// Inside the renewal margin: a new token, and no second installation lookup.
	c.t = c.t.Add(6 * time.Minute)
	if tok, _ = app.Token(ctx); tok != "ghs_token2" {
		t.Fatalf("token near expiry = %q, want a fresh one", tok)
	}
	if fake.mints != 2 || fake.lookups != 1 {
		t.Fatalf("mints=%d lookups=%d, want 2 and 1", fake.mints, fake.lookups)
	}
}

func TestAppNarrowsTokenToTheRepository(t *testing.T) {
	app, fake, _ := newApp(t)
	if _, err := app.Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	repos, _ := fake.lastMintBody["repositories"].([]any)
	if len(repos) != 1 || repos[0] != "prodeko-org" {
		t.Fatalf("mint body repositories = %v, want [prodeko-org]", fake.lastMintBody["repositories"])
	}
}

func TestAppJWT(t *testing.T) {
	app, fake, c := newApp(t)
	if _, err := app.Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(fake.lastJWT, ".")
	if len(parts) != 3 {
		t.Fatalf("JWT has %d parts", len(parts))
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(&testKey.PublicKey, crypto.SHA256, digest[:], sig); err != nil {
		t.Fatalf("JWT signature does not verify: %v", err)
	}
	raw, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var claims struct {
		Iat int64  `json:"iat"`
		Exp int64  `json:"exp"`
		Iss string `json:"iss"`
	}
	if err := json.Unmarshal(raw, &claims); err != nil {
		t.Fatal(err)
	}
	if claims.Iss != "5161689" {
		t.Errorf("iss = %q", claims.Iss)
	}
	if claims.Iat >= c.t.Unix() {
		t.Errorf("iat %d is not in the past", claims.Iat)
	}
	if lifetime := claims.Exp - claims.Iat; lifetime > 600 {
		t.Errorf("JWT lives %ds, over GitHub's ten-minute ceiling", lifetime)
	}
}

func TestAppRelooksUpAfterReinstall(t *testing.T) {
	app, fake, _ := newApp(t)
	ctx := context.Background()

	fake.mintStatus = http.StatusNotFound
	if _, err := app.Token(ctx); err == nil {
		t.Fatal("want an error while the installation answers 404")
	}

	fake.mintStatus = 0
	fake.installationID = 43
	tok, err := app.Token(ctx)
	if err != nil {
		t.Fatalf("after reinstall: %v", err)
	}
	if tok == "" || fake.lookups != 2 {
		t.Fatalf("token=%q lookups=%d, want a token and a second lookup", tok, fake.lookups)
	}
}

func TestAppNotInstalled(t *testing.T) {
	app, fake, _ := newApp(t)
	fake.installationID = 0
	_, err := app.Token(context.Background())
	if err == nil || !strings.Contains(err.Error(), "installed") {
		t.Fatalf("err = %v, want one that asks whether the App is installed", err)
	}
}

func TestNewAppRequiresEverything(t *testing.T) {
	_, err := NewApp(AppConfig{})
	if err == nil {
		t.Fatal("want an error")
	}
	for _, want := range []string{"AppID", "PrivateKey", "Owner", "Repo"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %s", err, want)
		}
	}
}

func TestParsePrivateKey(t *testing.T) {
	pkcs1 := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(testKey)}))
	der8, err := x509.MarshalPKCS8PrivateKey(testKey)
	if err != nil {
		t.Fatal(err)
	}
	pkcs8 := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der8}))

	for name, raw := range map[string]string{
		"pkcs1 pem":              pkcs1,
		"pkcs1 pem with padding": "\n  " + pkcs1 + "\n",
		"pkcs8 pem":              pkcs8,
		"base64 of pkcs1 pem":    base64.StdEncoding.EncodeToString([]byte(pkcs1)),
	} {
		t.Run(name, func(t *testing.T) {
			key, err := ParsePrivateKey(raw)
			if err != nil {
				t.Fatal(err)
			}
			if !key.Equal(testKey) {
				t.Fatal("parsed a different key")
			}
		})
	}

	for name, raw := range map[string]string{
		"empty":       "",
		"garbage":     "not a key",
		"base64 junk": base64.StdEncoding.EncodeToString([]byte("not pem")),
		"wrong block": string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: []byte("junk")})),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParsePrivateKey(raw); err == nil {
				t.Fatal("want an error")
			}
		})
	}
}

func TestStatic(t *testing.T) {
	tok, err := Static("ghp_x").Token(context.Background())
	if err != nil || tok != "ghp_x" {
		t.Fatalf("Static = %q, %v", tok, err)
	}
	if _, err := Static("").Token(context.Background()); err == nil {
		t.Fatal("an empty Static must refuse")
	}
}
