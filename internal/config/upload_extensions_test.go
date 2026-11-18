package config

import (
	"os"
	"testing"
)

// Storage takes only the files the app's upload fields accept: a field never
// offered json, doc, docx or zip, and a text file uploaded as anything else
// was kept and pinned for anyone (theGHub1/docs#152). Plain text and pdf stay
// for support and chat attachments.
func TestDefaultUploadExtensionsAreWhatTheAppUploads(t *testing.T) {
	os.Unsetenv("UPLOAD_ALLOWED_EXTENSIONS")
	cfg := Load()
	allowed := map[string]bool{}
	for _, ext := range cfg.Upload.AllowedExtensions {
		allowed[ext] = true
	}
	for _, ext := range []string{"json", "doc", "docx", "zip", "svg", "html", "htm", "js"} {
		if allowed[ext] {
			t.Errorf("%s is accepted by default", ext)
		}
	}
	for _, ext := range []string{"jpg", "jpeg", "png", "webp", "heic", "heif", "mp4", "mov", "webm", "pdf", "txt"} {
		if !allowed[ext] {
			t.Errorf("%s is not accepted by default", ext)
		}
	}
	for _, mime := range []string{"application/json", "application/zip", "application/msword"} {
		if cfg.Upload.AllowedMimeTypes[mime] {
			t.Errorf("%s is accepted by default", mime)
		}
	}
}
