package middleware

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/borg001/ipfs-filestorage/internal/auth/lua"
)

// A session token in a media address was a whole account session for anyone
// who saw the address (theGHub1/docs#154). It is refused unless a rollout
// turns it back on.
func TestAuthMiddlewareRefusesSessionTokenInAddress(t *testing.T) {
	handler := AuthMiddleware([]string{"mytoken"}, nil)(testOkHandler())
	for _, target := range []string{"/file/Qm123?token=mytoken", "/file/Qm123?access_token=mytoken"} {
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, target, nil))
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("%s: expected 401, got %d", target, rr.Code)
		}
	}
}

// One session is one credential, whichever way it is written (#170): the
// header is trimmed, and a repeated parameter is refused because Lua read its
// last value and the upload budget its first.
func TestAuthMiddlewareCanonicalSession(t *testing.T) {
	luaProvider := lua.NewProvider(`
function authorize(req)
  return req.headers["Authorization"] == "Bearer session-a"
end
`, 3000, 0, nil)
	var seen string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = SessionToken(r.Context())
		w.WriteHeader(http.StatusOK)
	})
	handler := AuthMiddlewareWithOptions(nil, luaProvider, AuthOptions{QueryTokens: true})(next)
	for _, tc := range []struct {
		name, target, header string
		want                 int
	}{
		{"bearer", "/upload", "Bearer session-a", http.StatusOK},
		{"padded_bearer", "/upload", "Bearer     session-a  ", http.StatusOK},
		{"lowercase_bearer", "/upload", "bearer session-a", http.StatusOK},
		{"access_token", "/upload?access_token=session-a", "", http.StatusOK},
		{"token", "/upload?token=session-a", "", http.StatusOK},
		{"repeated_token", "/upload?token=random&token=session-a", "", http.StatusBadRequest},
		{"repeated_access_token", "/upload?access_token=random&access_token=session-a", "", http.StatusBadRequest},
		{"empty_bearer", "/upload", "Bearer    ", http.StatusUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			seen = ""
			req := httptest.NewRequest(http.MethodPost, tc.target, nil)
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, req)
			if rr.Code != tc.want {
				t.Fatalf("expected %d, got %d", tc.want, rr.Code)
			}
			if tc.want == http.StatusOK && seen != "session-a" {
				t.Fatalf("expected the canonical session, got %q", seen)
			}
		})
	}
}

type stubSessions map[string]string

func (s stubSessions) OpenMediaSession(encoded string, _ time.Time) (string, error) {
	if token, ok := s[encoded]; ok {
		return token, nil
	}
	return "", errors.New("invalid")
}

// A media session opens media reads, and nothing else.
func TestAuthMiddlewareMediaSessionReadsMediaOnly(t *testing.T) {
	luaProvider := lua.NewProvider(`
function authorize(req)
  return req.headers["Authorization"] == "Bearer session-a"
end
`, 3000, 0, nil)
	handler := AuthMiddlewareWithOptions(nil, luaProvider, AuthOptions{MediaSessions: stubSessions{"sealed": "session-a"}})(testOkHandler())
	for _, tc := range []struct {
		method, target string
		want           int
	}{
		{http.MethodGet, "/file/Qm123?media_session=sealed", http.StatusOK},
		{http.MethodGet, "/stream/link/7/master.m3u8?media_session=sealed", http.StatusOK},
		{http.MethodGet, "/file/Qm123?media_session=forged", http.StatusUnauthorized},
		{http.MethodPost, "/upload?media_session=sealed", http.StatusUnauthorized},
		{http.MethodDelete, "/file/Qm123?media_session=sealed", http.StatusUnauthorized},
		{http.MethodPost, "/admin/face-masks/refresh?media_session=sealed", http.StatusUnauthorized},
	} {
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, httptest.NewRequest(tc.method, tc.target, nil))
		if rr.Code != tc.want {
			t.Fatalf("%s %s: expected %d, got %d", tc.method, tc.target, tc.want, rr.Code)
		}
	}
}
