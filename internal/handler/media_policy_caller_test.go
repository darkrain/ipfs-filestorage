package handler

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/borg001/ipfs-filestorage/internal/config"
	"github.com/borg001/ipfs-filestorage/internal/mediagrant"
)

// The storage proves to the media policy that it is the one asking: the
// policy keeps the file's locators from a reader of a blurred decision and
// gives them only to the storage (theGHub1/docs#152).
func TestPolicyRequestCarriesTheStorageProof(t *testing.T) {
	codec, err := mediagrant.New("k1", "stand", map[string]string{"k1": base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{4}, 32))})
	if err != nil {
		t.Fatal(err)
	}
	cid := testCID("Blurred")
	var proofErr error = mediagrant.ErrInvalid
	policy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proofErr = codec.OpenPolicyCaller(r.Header.Get(mediagrant.PolicyCallerHeader), time.Now())
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"rows": []map[string]interface{}{{"delivery_mode": "blur", "storage_uri": "ipfs://" + cid}}})
	}))
	defer policy.Close()
	resolver := newMediaAccessResolver(config.MediaAccessConfig{URL: policy.URL, TimeoutMs: 1000})
	resolver.callerProof = policyCallerProof(codec)
	request := httptest.NewRequest(http.MethodGet, "/file/"+cid, nil)
	request.Header.Set("Authorization", "Bearer viewer")
	decision, err := resolver.Resolve(request.Context(), request, cid)
	if err != nil || decision.SourceCID != cid {
		t.Fatalf("decision %+v, %v", decision, err)
	}
	if proofErr != nil {
		t.Fatalf("the policy got no valid storage proof: %v", proofErr)
	}
}
