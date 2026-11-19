package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// The proxy checked /config through the rate limit and authentication; while
// the storage was busy the check timed out, the proxy marked it down and no
// image on the platform loaded (theGHub1/docs#201). /healthz is answered
// before every other layer and touches neither IPFS nor the configuration.
func TestHealthIsAnsweredBeforeEveryOtherLayer(t *testing.T) {
	blocked := make(chan struct{})
	busy := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Stands for a rate limit, an auth check or IPFS that does not answer.
		<-blocked
	})
	server := httptest.NewServer(WithHealth(busy))
	defer server.Close()
	// Runs before Close, which waits for the blocked request.
	defer close(blocked)

	client := &http.Client{Timeout: time.Second}
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		req, _ := http.NewRequest(method, server.URL+HealthPath, nil)
		started := time.Now()
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, HealthPath, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || resp.Header.Get("Cache-Control") != "no-store" {
			t.Fatalf("%s: status %d, cache %q", method, resp.StatusCode, resp.Header.Get("Cache-Control"))
		}
		if elapsed := time.Since(started); elapsed > 200*time.Millisecond {
			t.Fatalf("%s took %v", method, elapsed)
		}
	}

	req, _ := http.NewRequest(http.MethodPost, server.URL+HealthPath, nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST status %d", resp.StatusCode)
	}

	// Every other path still goes through the stack.
	if _, err := (&http.Client{Timeout: 100 * time.Millisecond}).Get(server.URL + "/config"); err == nil {
		t.Fatal("/config bypassed the wrapped handler")
	}
}

// The two refusals of a video the browser did not compress tell the person
// what to do, in Russian too.
func TestVideoCompressionRefusalsAreLocalized(t *testing.T) {
	for code, want := range map[string]string{
		"video_encoding_required":    "Видео нужно сжать перед загрузкой: подходит MP4 с H.264 и AAC. Обновите страницу и попробуйте снова.",
		"video_resolution_too_large": "Разрешение видео больше 720p: не больше 1280 точек по длинной стороне и 60 кадров в секунду. Обновите страницу и попробуйте снова.",
	} {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/upload-video?lang=ru", nil)
		writeUploadError(w, req, http.StatusBadRequest, code, map[string]any{"max_long_side": 1280})
		var response uploadErrorResponse
		if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if response.Code != code || response.Message != want {
			t.Fatalf("%s: %+v", code, response)
		}
	}
}
