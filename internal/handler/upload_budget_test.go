package handler

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/borg001/ipfs-filestorage/internal/auth/lua"
	"github.com/borg001/ipfs-filestorage/internal/middleware"
)

// One session uploads at most its daily budget; another session has its own.
func TestUploadBudgetBoundsASessionPerDay(t *testing.T) {
	budget := newUploadBudget(100)
	first := httptest.NewRequest("POST", "/upload", nil)
	first.Header.Set("Authorization", "Bearer first-session")
	second := httptest.NewRequest("POST", "/upload", nil)
	second.Header.Set("Authorization", "Bearer second-session")
	if !budget.take(uploadSessionKey(first), 60) || budget.take(uploadSessionKey(first), 60) {
		t.Fatal("the first session went past its budget")
	}
	if !budget.take(uploadSessionKey(second), 60) {
		t.Fatal("the second session was charged for the first")
	}
	var none *uploadBudget
	if !none.take("any", 1<<40) {
		t.Fatal("a missing budget refused an upload")
	}
}

// Every spelling of one session shares its budget (theGHub1/docs#170): the
// budget used to hash the untrimmed header and the first ?token=, and to skip
// ?access_token= altogether, while authentication accepted all of them.
func TestUploadBudgetIsChargedToTheAuthenticatedSession(t *testing.T) {
	provider := lua.NewProvider(`
function authorize(req)
  return req.headers["Authorization"] == "Bearer session-a"
end
`, 3000, 0, nil)
	budget := newUploadBudget(100)
	upload := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !budget.allow(r, 60) {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	// Legacy query tokens on, as a rollout may need them: they still meter.
	handler := middleware.AuthMiddlewareWithOptions([]string{"service-key"}, provider, middleware.AuthOptions{QueryTokens: true})(upload)
	send := func(target, authorization string) int {
		request := httptest.NewRequest(http.MethodPost, target, nil)
		if authorization != "" {
			request.Header.Set("Authorization", authorization)
		}
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		return recorder.Code
	}
	if code := send("/upload", "Bearer session-a"); code != http.StatusOK {
		t.Fatalf("first upload: %d", code)
	}
	for _, tc := range []struct{ target, authorization string }{
		{"/upload", "Bearer session-a"},
		{"/upload", "Bearer    session-a"},
		{"/upload?access_token=session-a", ""},
		{"/upload?token=session-a", ""},
	} {
		if code := send(tc.target, tc.authorization); code != http.StatusTooManyRequests {
			t.Fatalf("%s %q went past the spent budget: %d", tc.target, tc.authorization, code)
		}
	}
	if code := send("/upload?token=random&token=session-a", ""); code != http.StatusBadRequest {
		t.Fatalf("a repeated token was not refused: %d", code)
	}
	request := httptest.NewRequest(http.MethodPost, "/upload", nil)
	request.Header.Set("X-API-Key", "service-key")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("a service key is not a session and is not metered: %d", recorder.Code)
	}
}

// A request whose session cannot be named is refused, never let through
// unmetered.
func TestUploadBudgetRefusesAnUnnamedSession(t *testing.T) {
	budget := newUploadBudget(100)
	if budget.allow(httptest.NewRequest(http.MethodPost, "/upload", nil), 1) {
		t.Fatal("an upload without a session passed the budget")
	}
	if budget.take("", 1) {
		t.Fatal("an empty key passed the budget")
	}
}
