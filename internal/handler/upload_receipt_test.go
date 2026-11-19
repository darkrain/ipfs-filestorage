package handler

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/borg001/ipfs-filestorage/internal/config"
	"github.com/borg001/ipfs-filestorage/internal/mediagrant"
)

// An upload answers with a receipt sealed to the uploading session: the API
// records the uploader from it, and only the uploader may then attach the
// file (theGHub1/docs#163) or register it as a video root (#152).
func TestUploadAnswersWithASessionBoundReceipt(t *testing.T) {
	codec, err := mediagrant.New("k1", "stand", map[string]string{"k1": base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{3}, 32))})
	if err != nil {
		t.Fatal(err)
	}
	h := setupTestHandler(&config.Config{Upload: config.UploadConfig{
		MaxFileSize:       1 << 20,
		AllowedExtensions: []string{"txt"},
		AllowedMimeTypes:  map[string]bool{"text/plain; charset=utf-8": true},
	}})
	h.mediaGrants = &mediaGrantVerifier{codec: codec}

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, _ := writer.CreateFormFile("file", "note.txt")
	_, _ = part.Write([]byte("plain text"))
	_ = writer.Close()
	request := httptest.NewRequest(http.MethodPost, "/upload", body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	request.Header.Set("Authorization", "Bearer session-one")
	w := httptest.NewRecorder()
	h.HandleUpload(w, request)
	if w.Code != http.StatusOK {
		t.Fatalf("upload: %d %s", w.Code, w.Body.String())
	}
	var response Response
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	receipt, err := codec.OpenReceipt(response.Receipt, "session-one", time.Now())
	if err != nil || receipt.Kind != "file" || receipt.CID != response.CID {
		t.Fatalf("receipt %+v (%v) for %s", receipt, err, response.CID)
	}
	if _, err := codec.OpenReceipt(response.Receipt, "session-two", time.Now()); err == nil {
		t.Fatal("another session opened the receipt")
	}
}
