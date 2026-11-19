package middleware

import (
	"context"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/borg001/ipfs-filestorage/internal/auth/lua"
)

type contextKey string

const (
	contextKeyUserID  contextKey = "user_id"
	contextKeySession contextKey = "session"
)

// mediaTokenCookie is the cookie the web app keeps the session in for the
// storage path.
const mediaTokenCookie = "iamfree_media_token"

// AuthOptions are the token transports beyond the Authorization header,
// X-API-Key and the media cookie.
type AuthOptions struct {
	// MediaSessions opens ?media_session=: the session sealed by the API for
	// media addresses. Nil refuses it.
	MediaSessions MediaSessionOpener
	// QueryTokens accepts a raw session token in ?token= or ?access_token=.
	// The token in an address leaked a full account session through copied
	// picture links, browser history and access logs; it is refused unless
	// a rollout still needs it (MEDIA_QUERY_TOKENS=true).
	QueryTokens bool
}

// MediaSessionOpener opens a sealed media session.
type MediaSessionOpener interface {
	OpenMediaSession(encoded string, now time.Time) (string, error)
}

// AuthMiddleware checks request authentication:
// 1. Static API keys from API_KEYS env (always, default)
// 2. Lua authorize() script if AUTH_LUA_SCRIPT is set (fallback)
func AuthMiddleware(apiKeys []string, luaProvider *lua.Provider) func(http.Handler) http.Handler {
	return AuthMiddlewareWithOptions(apiKeys, luaProvider, AuthOptions{})
}

// AuthMiddlewareWithOptions is AuthMiddleware with the token transports
// beyond the header made explicit.
//
// The request's credential is resolved once, here, and written back as the
// one Authorization header every later reader sees - the Lua script, the
// media policy, grants and the upload budget. Each of them used to read the
// token on its own: the budget hashed the untrimmed header and the first
// ?token=, while authentication also took ?access_token=, a trimmed header and
// (in Lua) the last ?token=, so the same session uploaded without a limit.
func AuthMiddlewareWithOptions(apiKeys []string, luaProvider *lua.Provider, options AuthOptions) func(http.Handler) http.Handler {
	validKeys := make(map[string]struct{}, len(apiKeys))
	for _, k := range apiKeys {
		if trimmed := strings.TrimSpace(k); trimmed != "" {
			validKeys[trimmed] = struct{}{}
		}
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet && r.URL.Path == "/config" {
				next.ServeHTTP(w, r)
				return
			}

			token, status, message := resolveCredential(r, options)
			if status != 0 {
				writeAuthError(w, status, message)
				return
			}

			// 1. Check static API keys (if token present)
			if token != "" {
				if _, ok := validKeys[token]; ok {
					ctx := context.WithValue(r.Context(), contextKeyUserID, "api-key")
					next.ServeHTTP(w, r.WithContext(ctx))
					return
				}
			}

			// 2. Check lua provider (if configured)
			if luaProvider != nil && luaProvider.Enabled() {
				ok, err := luaProvider.Authorize(r.Context(), r)
				if err != nil {
					// Log details server-side only, return generic message to client
					log.Printf("[AUTH] Lua error: %v", err)
					writeAuthError(w, http.StatusUnauthorized, "Authentication failed")
					return
				}
				if ok {
					ctx := context.WithValue(r.Context(), contextKeyUserID, "lua-auth")
					if token != "" {
						ctx = context.WithValue(ctx, contextKeySession, token)
					}
					next.ServeHTTP(w, r.WithContext(ctx))
					return
				}
			}

			if token != "" {
				writeAuthError(w, http.StatusForbidden, "Authentication failed")
				return
			}
			writeAuthError(w, http.StatusUnauthorized, "Authentication required")
		})
	}
}

// credentialParameters are the query parameters that carry a credential. A
// repeated one is refused: Lua read the last value and the budget the first.
var credentialParameters = []string{"token", "access_token", "media_session"}

// resolveCredential finds the request's token in the one order every reader
// shares - Authorization: Bearer, X-API-Key, the media cookie, media_session,
// then (if allowed) token and access_token - trims it and writes a session
// token back as the Authorization header. A non-zero status refuses the
// request.
func resolveCredential(r *http.Request, options AuthOptions) (string, int, string) {
	query := r.URL.Query()
	for _, name := range credentialParameters {
		if len(query[name]) > 1 {
			return "", http.StatusBadRequest, "Repeated credential parameter"
		}
	}
	queryToken := strings.TrimSpace(query.Get("token"))
	if queryToken == "" {
		queryToken = strings.TrimSpace(query.Get("access_token"))
	}
	if queryToken != "" && !options.QueryTokens {
		return "", http.StatusUnauthorized, "Session token in the address is not accepted"
	}
	mediaSession := strings.TrimSpace(query.Get("media_session"))

	session := func(token string) (string, int, string) {
		r.Header.Set("Authorization", "Bearer "+token)
		return token, 0, ""
	}
	if header := strings.TrimSpace(r.Header.Get("Authorization")); header != "" {
		if scheme, value, ok := strings.Cut(header, " "); ok && strings.EqualFold(scheme, "Bearer") {
			if token := strings.TrimSpace(value); token != "" {
				return session(token)
			}
		}
		return "", http.StatusUnauthorized, "Authentication failed"
	}
	if key := strings.TrimSpace(r.Header.Get("X-API-Key")); key != "" {
		return key, 0, ""
	}
	// The web app hands pictures their session through a cookie scoped to the
	// storage path when the storage is served from the app's own origin.
	if cookie, err := r.Cookie(mediaTokenCookie); err == nil && strings.TrimSpace(cookie.Value) != "" {
		return session(strings.TrimSpace(cookie.Value))
	}
	if mediaSession != "" {
		// A media session reads media and nothing else: a leaked address
		// must not upload, delete or refresh face masks.
		if options.MediaSessions == nil || (r.Method != http.MethodGet && r.Method != http.MethodHead) ||
			!(strings.HasPrefix(r.URL.Path, "/file/") || strings.HasPrefix(r.URL.Path, "/stream/")) {
			return "", http.StatusUnauthorized, "Authentication failed"
		}
		token, err := options.MediaSessions.OpenMediaSession(mediaSession, time.Now())
		if err != nil || strings.TrimSpace(token) == "" {
			return "", http.StatusUnauthorized, "Media session expired"
		}
		return session(strings.TrimSpace(token))
	}
	if queryToken != "" {
		return session(queryToken)
	}
	return "", 0, ""
}

// SessionToken is the session token the request was authenticated with, or
// empty for an API key or an unauthenticated request.
func SessionToken(ctx context.Context) string {
	token, _ := ctx.Value(contextKeySession).(string)
	return token
}

// IsAPIKey says the request was authenticated with a static API key.
func IsAPIKey(ctx context.Context) bool {
	return UserIDFromContext(ctx) == "api-key"
}

func writeAuthError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(`{"error":"` + msg + `"}`))
}

// UserIDFromContext returns the user_id value from context (for logging/audit).
func UserIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(contextKeyUserID).(string); ok {
		return v
	}
	return ""
}
