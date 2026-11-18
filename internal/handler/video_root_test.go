package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/borg001/ipfs-filestorage/internal/config"
)

// A text file uploaded and registered as video://<its CID> used to be read
// as a master playlist: the "poster" and playlist lines it carried made any
// CID they named a child of the root, and the storage served that file's
// original - another user's private or face-hidden photo (theGHub1/docs#152).
// A master is trusted only when the API vouches that the encoder wrote it.
func TestForgedVideoMasterOpensNoForeignFile(t *testing.T) {
	forgedRoot := testCID("ForgedRoot")
	victim := testCID("VictimPhoto")
	for _, tc := range []struct {
		name   string
		vouch  bool
		path   string
		want   int
		served string
	}{
		{"forged_poster", false, "/stream/segment/" + victim + ".jpg", http.StatusForbidden, ""},
		{"forged_playlist", false, "/stream/segment/" + victim + ".m3u8", http.StatusForbidden, ""},
		{"encoder_master_poster", true, "/stream/segment/" + testPosterCID + ".jpg", http.StatusOK, "original-poster"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			policy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				root := r.URL.Query().Get("search")
				row := map[string]interface{}{"delivery_mode": "original"}
				switch root {
				case forgedRoot:
					// The attacker's own unplaced upload: original for him.
					row["storage_uri"] = "video://" + forgedRoot
				case testMasterCID:
					row["storage_uri"] = "video://" + testMasterCID
					row["video_root"] = true
				default:
					w.WriteHeader(http.StatusForbidden)
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]interface{}{"rows": []map[string]interface{}{row}})
			}))
			defer policy.Close()
			h := setupVideoTestHandler(t, &config.Config{})
			h.mediaAccess = newMediaAccessResolver(config.MediaAccessConfig{URL: policy.URL, TimeoutMs: 1000})
			cluster := h.cluster.(*mockCluster)
			cluster.files[forgedRoot] = []byte("#EXTM3U\n#EXT-X-IAMFREE-POSTER:SIZE=180x320,URI=\"../segment/" + victim + ".jpg\"\n" +
				"#EXT-X-STREAM-INF:BANDWIDTH=1\n" + victim + ".m3u8\n")
			cluster.files[victim] = []byte("VICTIM-ORIGINAL")
			cluster.files[testMasterCID] = []byte(testVideoMasterPosters(testPosterCID, testCID("BlurPoster")))
			cluster.files[testPosterCID] = []byte("original-poster")

			root := forgedRoot
			if tc.vouch {
				root = testMasterCID
			}
			request := httptest.NewRequest(http.MethodGet, tc.path+"?media_root="+root, nil)
			request.Header.Set("Authorization", "Bearer attacker-token")
			w := httptest.NewRecorder()
			h.HandleStreamSegment(w, request)
			if w.Code != tc.want || strings.Contains(w.Body.String(), "VICTIM-ORIGINAL") {
				t.Fatalf("status %d body %q, want %d", w.Code, w.Body.String(), tc.want)
			}
			if tc.served != "" && w.Body.String() != tc.served {
				t.Fatalf("served %q, want %q", w.Body.String(), tc.served)
			}
		})
	}
}
