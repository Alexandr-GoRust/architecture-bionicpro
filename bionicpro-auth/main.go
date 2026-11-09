// bionicpro-auth — бэкенд-сервис безопасной работы с токенами Keycloak.
//
// Задание 1, задача 3: токены больше не живут во фронтенде.
//   - /login     — редирект на Keycloak с PKCE (S256);
//   - /callback  — обмен кода (code + code_verifier) на токены;
//   - сессия     — refresh_token в защищённом хранилище (in-memory, шифруется AES-GCM),
//     access_token в памяти; фронтенду — только сессионная cookie (HttpOnly, Secure);
//   - /api/*     — проксирование запросов с подстановкой access_token;
//   - при истечении access_token сервис сам обновляет его по refresh_token;
//   - ротация сессии на каждый запрос (session fixation protection): новый session id,
//     перепривязка токенов, новая cookie.
package main

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

var (
	keycloakBase   = env("KEYCLOAK_URL", "http://localhost:8080")
	keycloakRealm  = env("KEYCLOAK_REALM", "reports-realm")
	clientID       = env("AUTH_CLIENT_ID", "bionicpro-auth")
	clientSecret   = env("AUTH_CLIENT_SECRET", "bionicpro-auth-secret")
	sessionKeyB64  = env("SESSION_ENC_KEY", "") // 32 байта base64; пусто = генерируется при старте
	cookieName     = "bionicpro_session"
	sessionTTL     = 30 * time.Minute            // > access_token (2 мин) — можно успеть обновить
	rotateEvery    = time.Minute                 // ротация сессии не чаще раза в минуту
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// ---------- sessions: защищённое хранилище ----------

type session struct {
	ID           string
	Access       string // access_token (оперативная память)
	RefreshEnc   []byte // refresh_token AES-GCM encrypted at rest
	ExpiresAt    time.Time
	AccessExp    time.Time
	LastRotate   time.Time
	RedirectTgt  string
}

type sessionStore struct {
	mu   sync.RWMutex
	byID map[string]*session
	aead cipher.AEAD
}

func newSessionStore() *sessionStore {
	key := make([]byte, 32)
	if sessionKeyB64 != "" {
		raw, err := base64.StdEncoding.DecodeString(sessionKeyB64)
		if err != nil || len(raw) != 32 {
			log.Fatalf("SESSION_ENC_KEY: expected 32 bytes base64, got %d bytes (err=%v)", len(raw), err)
		}
		key = raw
	} else if _, err := io.ReadFull(rand.Reader, key); err != nil {
		log.Fatal(err)
	}
	blk, err := aes.NewCipher(key)
	if err != nil {
		log.Fatal(err)
	}
	aead, err := cipher.NewGCM(blk)
	if err != nil {
		log.Fatal(err)
	}
	return &sessionStore{byID: map[string]*session{}, aead: aead}
}

func (s *sessionStore) encrypt(pt string) []byte {
	nonce := make([]byte, s.aead.NonceSize())
	io.ReadFull(rand.Reader, nonce)
	return s.aead.Seal(nonce, nonce, []byte(pt), nil)
}

func (s *sessionStore) decrypt(ct []byte) (string, error) {
	if len(ct) < s.aead.NonceSize() {
		return "", errors.New("ciphertext too short")
	}
	ns := s.aead.NonceSize()
	pt, err := s.aead.Open(nil, ct[:ns], ct[ns:], nil)
	if err != nil {
		return "", err
	}
	return string(pt), nil
}

func (s *sessionStore) put(sess *session) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.byID[sess.ID] = sess
	// GC просроченных
	now := time.Now()
	for id, v := range s.byID {
		if now.After(v.ExpiresAt) {
			delete(s.byID, id)
		}
	}
}

func (s *sessionStore) get(id string) (*session, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sess, ok := s.byID[id]
	if !ok || time.Now().After(sess.ExpiresAt) {
		return nil, false
	}
	return sess, true
}

func (s *sessionStore) drop(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.byID, id)
}

// ---------- PKCE ----------

func genVerifier() (verifier string, challenge string, err error) {
	buf := make([]byte, 64)
	if _, err = io.ReadFull(rand.Reader, buf); err != nil {
		return
	}
	verifier = base64.RawURLEncoding.EncodeToString(buf)
	h := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(h[:])
	return
}

// pending логины: state -> {verifier, redirect}
var (
	pendMu sync.Mutex
	pend   = map[string]struct{ verifier, redirect string }{}
)

func rememberLogin(state, verifier, redirect string) {
	pendMu.Lock()
	defer pendMu.Unlock()
	pend[state] = struct{ verifier, redirect string }{verifier, redirect}
	// чистим старше 10 минут
	if len(pend) > 1000 {
		pend = map[string]struct{ verifier, redirect string }{}
	}
}

func takeLogin(state string) (verifier, redirect string, ok bool) {
	pendMu.Lock()
	defer pendMu.Unlock()
	v, exists := pend[state]
	if exists {
		delete(pend, state)
	}
	return v.verifier, v.redirect, exists
}

// ---------- main ----------

func main() {
	// Keycloak может стартовать позже сервиса — ретраи с таймаутом
	var provider *oidc.Provider
	var err error
	hc := &http.Client{Timeout: 10 * time.Second}
	ctx := oidc.ClientContext(context.Background(), hc)
	for i := 0; i < 30; i++ {
		provider, err = oidc.NewProvider(ctx, fmt.Sprintf("%s/realms/%s", keycloakBase, keycloakRealm))
		if err == nil {
			break
		}
		log.Printf("waiting for keycloak: %v", err)
		time.Sleep(2 * time.Second)
	}
	if err != nil {
		log.Fatalf("keycloak unreachable: %v", err)
	}

	oauthCfg := &oauth2.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		Endpoint:     provider.Endpoint(),
		RedirectURL:  env("AUTH_REDIRECT_URL", "http://localhost:8000/callback"),
		Scopes:       []string{"openid", "profile", "email", "offline_access"},
	}

	store := newSessionStore()
	idTokenVerifier := provider.Verifier(&oidc.Config{ClientID: clientID})

	mux := http.NewServeMux()
	mux.HandleFunc("/login", func(w http.ResponseWriter, r *http.Request) {
		verifier, challenge, err := genVerifier()
		if err != nil {
			http.Error(w, "pkce error", http.StatusInternalServerError)
			return
		}
		state := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf("%d", time.Now().UnixNano())))
		// криптостойкий state из 16 байт
		sb := make([]byte, 16)
		io.ReadFull(rand.Reader, sb)
		state = base64.RawURLEncoding.EncodeToString(sb)
		rememberLogin(state, verifier, r.URL.Query().Get("redirect"))
		u := oauthCfg.AuthCodeURL(state,
			oauth2.SetAuthURLParam("code_challenge", challenge),
			oauth2.SetAuthURLParam("code_challenge_method", "S256"),
			oauth2.SetAuthURLParam("prompt", "login"),
		)
		http.Redirect(w, r, u, http.StatusFound)
	})

	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		state := r.URL.Query().Get("state")
		verifier, redirect, ok := takeLogin(state)
		if !ok {
			http.Error(w, "bad state", http.StatusBadRequest)
			return
		}
		code := r.URL.Query().Get("code")
		ctx := r.Context()
		tok, err := oauthCfg.Exchange(ctx, code,
			oauth2.SetAuthURLParam("code_verifier", verifier))
		if err != nil {
			log.Printf("token exchange failed: %v", err)
			http.Error(w, "token exchange failed", http.StatusBadGateway)
			return
		}
		rawID := tok.Extra("id_token")
		if rawID == nil {
			http.Error(w, "no id_token", http.StatusBadGateway)
			return
		}
		idt, err := idTokenVerifier.Verify(ctx, rawID.(string))
		if err != nil {
			http.Error(w, "invalid id_token", http.StatusBadGateway)
			return
		}
		var claims struct {
			Sub   string `json:"sub"`
			Email string `json:"email"`
			Name  string `json:"name"`
			PreferredUsername string `json:"preferred_username"`
		}
		idt.Claims(&claims)

		sess := &session{
			ID:          newSessionID(),
			Access:      tok.AccessToken,
			RefreshEnc:  store.encrypt(tok.RefreshToken),
			AccessExp:   time.Now().Add(time.Duration(getAccessTTL(tok) ) * time.Second),
			ExpiresAt:   time.Now().Add(sessionTTL),
			LastRotate:  time.Now(),
		}
		store.put(sess)
		http.SetCookie(w, &http.Cookie{
			Name:     cookieName,
			Value:    sess.ID,
			Path:     "/",
			HttpOnly: true,
			Secure:   strings.HasPrefix(env("AUTH_REDIRECT_URL", "http://"), "https://"),
			SameSite: http.SameSiteLaxMode,
			MaxAge:   int(sessionTTL.Seconds()),
		})
		if redirect == "" || !strings.HasPrefix(redirect, "/") {
			redirect = "/"
		}
		http.Redirect(w, r, redirect, http.StatusFound)
	})

	mux.HandleFunc("/logout", func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie(cookieName); err == nil {
			store.drop(c.Value)
		}
		http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true})
		http.Redirect(w, r, keycloakBase+"/realms/"+keycloakRealm+"/protocol/openid-connect/logout?redirect_uri="+
			url.QueryEscape(env("FRONTEND_URL", "http://localhost:3000")), http.StatusFound)
	})

	// Кто я (для UI): 200 + данные из валидного access_token, иначе 401.
	mux.HandleFunc("/api/me", withSession(store, oauthCfg, func(w http.ResponseWriter, r *http.Request, sess *session) {
		claims, err := parseUnverifiedClaims(sess.Access)
		if err != nil {
			http.Error(w, "token parse error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(claims)
	}))

	// Проксирование к reports-api: сессия -> access_token -> Bearer.
	mux.HandleFunc("/api/reports", withSession(store, oauthCfg, func(w http.ResponseWriter, r *http.Request, sess *session) {
		claims, _ := parseUnverifiedClaims(sess.Access)
		userSub, _ := claims["sub"].(string)
		apiURL := env("REPORTS_API_URL", "http://localhost:8001") + "/reports?user=" + url.QueryEscape(userSub)
		req, _ := http.NewRequestWithContext(r.Context(), http.MethodGet, apiURL, nil)
		req.Header.Set("Authorization", "Bearer "+sess.Access)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			http.Error(w, "reports api unreachable: "+err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		for k, vv := range resp.Header {
			if k == "Set-Cookie" {
				continue
			}
			for _, v := range vv {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body)
	}))

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "bionicpro-auth: /login /callback /logout /api/me /api/reports")
	})

	addr := env("LISTEN", ":8000")
	log.Printf("bionicpro-auth listening on %s (keycloak=%s realm=%s)", addr, keycloakBase, keycloakRealm)
	log.Fatal(http.ListenAndServe(addr, mux))
}

// withSession — общий мидлвар: находит сессию по cookie, обновляет access_token по refresh,
// выполняет ротацию сессии (session fixation protection) и вызывает handler.
func withSession(store *sessionStore, cfg *oauth2.Config, next func(w http.ResponseWriter, r *http.Request, s *session)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(cookieName)
		if err != nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		sess, ok := store.get(c.Value)
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		// авто-refresh: access истёк (или скоро) — идём за новым по refresh_token
		if time.Now().After(sess.AccessExp.Add(-10 * time.Second)) {
			refresh, err := store.decrypt(sess.RefreshEnc)
			if err != nil {
				http.Error(w, "session corrupted", http.StatusInternalServerError)
				return
			}
			tok, err := refreshExchange(cfg, refresh)
			if err != nil {
				log.Printf("refresh failed: %v", err)
				store.drop(sess.ID)
				http.Error(w, "session expired", http.StatusUnauthorized)
				return
			}
			sess.Access = tok.AccessToken
			if tok.RefreshToken != "" {
				sess.RefreshEnc = store.encrypt(tok.RefreshToken)
			}
			sess.AccessExp = time.Now().Add(time.Duration(getAccessTTL(tok)) * time.Second)
			sess.ExpiresAt = time.Now().Add(sessionTTL)
			log.Printf("access_token refreshed (rotation of refresh=%v)", tok.RefreshToken != "")
		}
		// ротация сессии в рамках действующего access_token (раз в rotateEvery)
		if time.Since(sess.LastRotate) >= rotateEvery {
			store.drop(sess.ID)
			sess.ID = newSessionID()
			sess.LastRotate = time.Now()
			sess.ExpiresAt = time.Now().Add(sessionTTL)
			store.put(sess)
			http.SetCookie(w, &http.Cookie{
				Name: cookieName, Value: sess.ID, Path: "/",
				HttpOnly: true,
				Secure:   strings.HasPrefix(env("AUTH_REDIRECT_URL", "http://"), "https://"),
				SameSite: http.SameSiteLaxMode,
				MaxAge:   int(sessionTTL.Seconds()),
			})
			w.Header().Set("X-Session-Id", sess.ID)
		} else {
			store.put(sess) // продлеваем скользящий TTL
		}
		next(w, r, sess)
	}
}

func refreshExchange(cfg *oauth2.Config, refreshToken string) (*oauth2.Token, error) {
	jar, _ := cookiejar.New(nil)
	hc := &http.Client{Jar: jar, Timeout: 10 * time.Second}
	ctx := oidc.ClientContext(context.Background(), hc)
	return cfg.TokenSource(ctx, &oauth2.Token{RefreshToken: refreshToken}).Token()
}

func getAccessTTL(tok *oauth2.Token) int64 {
	if !tok.Expiry.IsZero() {
		return int64(time.Until(tok.Expiry).Seconds())
	}
	return 120
}

func newSessionID() string {
	b := make([]byte, 24)
	io.ReadFull(rand.Reader, b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func parseUnverifiedClaims(raw string) (map[string]any, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return nil, errors.New("not a jwt")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, err
	}
	m := map[string]any{}
	return m, json.Unmarshal(payload, &m)
}
