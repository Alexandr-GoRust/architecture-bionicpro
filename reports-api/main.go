// reports-api — API отчётов о работе протеза.
//
// Задание 2: GET /reports?user=<sub>&from&to — отчёт из ClickHouse (витрина,
// подготовленная Airflow/Debezium заранее — без тяжёлых вычислений на лету).
// Задание 2 задача 4: доступ только к СВОЕМУ отчёту — user в query сверяется
// с sub из валидированного access_token (публичный ключ Keycloak, RS256).
// Задание 3: отчёт кладётся в S3 (Minio) и раздаётся через CDN (Nginx
// reverse proxy с кешем): если отчёт уже в S3 — отдаём CDN-ссылку.
package main

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/big"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/golang-jwt/jwt/v5"
	"github.com/minio/minio-go/v7"
	miniocred "github.com/minio/minio-go/v7/pkg/credentials"
)

var (
	keycloakBase = env("KEYCLOAK_URL", "http://localhost:8080")
	keycloakRealm = env("KEYCLOAK_REALM", "reports-realm")
	chAddr       = env("CLICKHOUSE_ADDR", "clickhouse:9000")
	chDB         = env("CLICKHOUSE_DB", "reports")
	s3Endpoint   = env("S3_ENDPOINT", "minio:9000")
	s3Bucket     = env("S3_BUCKET", "reports")
	cdnBaseURL   = env("CDN_BASE_URL", "http://localhost:8090/reports-cdn")
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// ---------- Keycloak public key (RS256) ----------

type jwks struct {
	Keys []struct {
		Kid string `json:"kid"`
		N   string `json:"n"`
		E   string `json:"e"`
		Kty string `json:"kty"`
	} `json:"keys"`
}

func fetchPublicKey(kid string) (*rsa.PublicKey, error) {
	resp, err := http.Get(fmt.Sprintf("%s/realms/%s/protocol/openid-connect/certs", keycloakBase, keycloakRealm))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var j jwks
	if err := json.NewDecoder(resp.Body).Decode(&j); err != nil {
		return nil, err
	}
	for _, k := range j.Keys {
		if k.Kid != kid {
			continue
		}
		nb, err := base64.RawURLEncoding.DecodeString(k.N)
		if err != nil {
			return nil, err
		}
		eb, err := base64.RawURLEncoding.DecodeString(k.E)
		if err != nil {
			return nil, err
		}
		e := new(big.Int).SetBytes(eb)
		return &rsa.PublicKey{N: new(big.Int).SetBytes(nb), E: int(e.Int64())}, nil
	}
	return nil, fmt.Errorf("kid %s not found", kid)
}

type userClaims struct {
	Sub   string
	Email string
	Roles []string
}

func authMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") {
			http.Error(w, "missing bearer token", http.StatusUnauthorized)
			return
		}
		raw := strings.TrimPrefix(auth, "Bearer ")
		tok, err := jwt.Parse(raw, func(t *jwt.Token) (any, error) {
			if _, ok := t.Method.(*jwt.SigningMethodRSA); !ok {
				return nil, fmt.Errorf("unexpected alg %v", t.Header["alg"])
			}
			kid, _ := t.Header["kid"].(string)
			return fetchPublicKey(kid)
		}, jwt.WithValidMethods([]string{"RS256"}))
		if err != nil || !tok.Valid {
			http.Error(w, "invalid token", http.StatusUnauthorized)
			return
		}
		claims, ok := tok.Claims.(jwt.MapClaims)
		if !ok {
			http.Error(w, "invalid claims", http.StatusUnauthorized)
			return
		}
		uc := &userClaims{}
		if s, ok := claims["sub"].(string); ok {
			uc.Sub = s
		}
		if e, ok := claims["email"].(string); ok {
			uc.Email = e
		}
		if realmAccess, ok := claims["realm_access"].(map[string]any); ok {
			if roles, ok := realmAccess["roles"].([]any); ok {
				for _, role := range roles {
					if rs, ok := role.(string); ok {
						uc.Roles = append(uc.Roles, rs)
					}
				}
			}
		}
		if uc.Sub == "" {
			http.Error(w, "token without sub", http.StatusUnauthorized)
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), userKey{}, uc)))
	}
}

type userKey struct{}

func userFrom(ctx context.Context) *userClaims {
	return ctx.Value(userKey{}).(*userClaims)
}

// ---------- ClickHouse ----------

func chConn() (clickhouse.Conn, error) {
	return clickhouse.Open(&clickhouse.Options{
		Addr: []string{chAddr},
		Auth: clickhouse.Auth{
			Database: chDB,
			Username: os.Getenv("CH_USER"),     // reports_api (только SELECT на reports.*)
			Password: os.Getenv("CH_PASSWORD"),
		},
		Settings: clickhouse.Settings{
			"max_execution_time": 30,
		},
		DialTimeout: 5 * time.Second,
	})
}

type reportRow struct {
	Day        string  `json:"date"`
	ProsthesisID string `json:"prosthesis_id"`
	Events     uint64  `json:"events"`
	AvgResponse float32 `json:"avg_response_ms"`
}

func queryReport(ctx context.Context, user, from, to string) ([]reportRow, error) {
	conn, err := chConn()
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	rows, err := conn.Query(ctx,
		`SELECT toString(day), prosthesis_id, events, avg_response_ms
		 FROM reports_user_daily
		 WHERE user_id = ? AND day BETWEEN ? AND ?
		 ORDER BY day`, user, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []reportRow
	for rows.Next() {
		var rr reportRow
		if err := rows.Scan(&rr.Day, &rr.ProsthesisID, &rr.Events, &rr.AvgResponse); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		out = append(out, rr)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows: %w", err)
	}
	return out, nil
}

// максимум обработанной даты в витрине: за её пределами данных ещё нет (Airflow ещё не прогнал)
func lastProcessedDay(ctx context.Context) (string, error) {
	conn, err := chConn()
	if err != nil {
		return "", err
	}
	defer conn.Close()
	var day time.Time
	if err := conn.QueryRow(ctx, `SELECT max(day) FROM reports_user_daily`).Scan(&day); err != nil {
		return "", err
	}
	return day.Format("2006-01-02"), nil
}

// ---------- S3 (Minio) ----------

var mc *minio.Client

func s3Put(ctx context.Context, key string, body []byte, ctype string) error {
	_, err := mc.PutObject(ctx, s3Bucket, key, bytesReader(body), int64(len(body)),
		minio.PutObjectOptions{ContentType: ctype})
	return err
}

func s3Exists(ctx context.Context, key string) (bool, error) {
	_, err := mc.StatObject(ctx, s3Bucket, key, minio.StatObjectOptions{})
	if err == nil {
		return true, nil
	}
	resp := minio.ToErrorResponse(err)
	if resp.Code == "NoSuchKey" {
		return false, nil
	}
	return false, err
}

// ---------- handler ----------

func reportsHandler(w http.ResponseWriter, r *http.Request) {
	uc := userFrom(r.Context())
	q := r.URL.Query()
	// Задание 2 задача 4: отчёт можно запросить только по себе.
	requested := q.Get("user")
	if requested == "" {
		requested = uc.Sub
	}
	if requested != uc.Sub && !hasRole(uc.Roles, "administrator") {
		http.Error(w, `{"error":"forbidden: you can only request your own report"}`, http.StatusForbidden)
		return
	}
	from := q.Get("from")
	to := q.Get("to")
	if from == "" || to == "" {
		http.Error(w, `{"error":"from and to required (YYYY-MM-DD)"}`, http.StatusBadRequest)
		return
	}

	// отчёт только за обработанный Airflow период
	lastDay, err := lastProcessedDay(r.Context())
	if err != nil {
		log.Printf("lastProcessedDay: %v", err)
	}
	if lastDay != "" && to > lastDay {
		http.Error(w, fmt.Sprintf(`{"error":"data for requested period is not processed yet; last processed day: %s"}`, lastDay), http.StatusAccepted)
		return
	}

	// Задание 3: сначала S3 -> если есть, CDN-ссылка; иначе генерим, кладём, отдаём ссылку.
	objKey := fmt.Sprintf("user=%s/%s_%s.json", uc.Sub, from, to)
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	exists, err := s3Exists(ctx, objKey)
	if err != nil {
		log.Printf("s3 stat: %v", err)
	}
	if !exists {
		rows, err := queryReport(ctx, uc.Sub, from, to)
		if err != nil {
			log.Printf("queryReport: %v", err)
			http.Error(w, `{"error":"clickhouse query failed"}`, http.StatusBadGateway)
			return
		}
		payload, _ := json.MarshalIndent(map[string]any{
			"user_id": uc.Sub,
			"period":  map[string]string{"from": from, "to": to},
			"rows":    rows,
		}, "", "  ")
		if err := s3Put(ctx, objKey, payload, "application/json"); err != nil {
			log.Printf("s3 put: %v", err)
			http.Error(w, `{"error":"s3 upload failed"}`, http.StatusBadGateway)
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"report_url": cdnBaseURL + "/" + objKey,
		"cdn":        "nginx reverse-proxy cache -> minio",
	})
}

func internalObjectHandler(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("X-CDN-Token") != env("CDN_TOKEN", "bionicpro-cdn") {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	key := strings.TrimPrefix(r.URL.Path, "/internal/object/")
	if strings.Contains(key, "..") {
		http.Error(w, "bad key", http.StatusBadRequest)
		return
	}
	obj, err := mc.GetObject(r.Context(), s3Bucket, key, minio.GetObjectOptions{})
	if err != nil {
		http.Error(w, "s3 error", http.StatusBadGateway)
		return
	}
	defer obj.Close()
	st, err := obj.Stat()
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", st.ContentType)
	io.Copy(w, obj)
}

func hasRole(roles []string, role string) bool {
	for _, r := range roles {
		if r == role {
			return true
		}
	}
	return false
}

func main() {
	var err error
	mc, err = minio.New(s3Endpoint, &minio.Options{
		Creds: miniocred.NewStaticV4(
			env("S3_ACCESS_KEY", "minioadmin"),
			env("S3_SECRET_KEY", "minioadmin"), ""),
		Secure: false,
	})
	if err != nil {
		log.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := mc.MakeBucket(ctx, s3Bucket, minio.MakeBucketOptions{}); err != nil && !isExistsErr(err) {
		log.Printf("bucket create: %v (continuing)", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/reports", authMiddleware(reportsHandler))
	// Внутренняя раздача объектов из S3 для CDN (nginx проксирует сюда;
	// авторизация — статический токен docker-сети, объекты сами по себе
	// персонифицированы ключом user=<sub>/<from>_<to>.json).
	mux.HandleFunc("/internal/object/", internalObjectHandler)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	addr := env("LISTEN", ":8001")
	log.Printf("reports-api on %s (clickhouse=%s db=%s s3=%s/%s)", addr, chAddr, chDB, s3Endpoint, s3Bucket)
	log.Fatal(http.ListenAndServe(addr, mux))
}

func isExistsErr(err error) bool {
	e, ok := err.(minio.ErrorResponse)
	return ok && e.Code == "BucketAlreadyOwnedByYou"
}

func bytesReader(b []byte) io.Reader {
	return strings.NewReader(string(b))
}
