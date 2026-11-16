// SPDX-FileCopyrightText: 2026 Coran Labs Private Limited
// SPDX-License-Identifier: Apache-2.0

package sdcore

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	api "github.com/coranlabs/SETU/api/v1"
	"github.com/coranlabs/SETU/connectors/sdcore/rx2n5"
)

// TestPolicyCreateDelete characterizes the N5 client against a fake PCF:
// create must POST the translated app-session and return the Location; delete must
// REBASE the k8s-internal Location onto the reachable base (the verified fix
// without which PCC rules leaked and calls 580'd after the first) and treat 404 as
// success (idempotency).
func TestPolicyCreateDelete(t *testing.T) {
	var deletePaths []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/npcf-policyauthorization/v1/app-sessions":
			body, _ := io.ReadAll(r.Body)
			var asc rx2n5.AppSessionContext
			if err := json.Unmarshal(body, &asc); err != nil {
				t.Errorf("PCF received invalid JSON: %v", err)
			}
			if asc.AscReqData.UeIPv4 != "10.45.0.2" {
				t.Errorf("ueIpv4 = %q, want 10.45.0.2", asc.AscReqData.UeIPv4)
			}
			if asc.AscReqData.EvSubsc.NotifUri == "" {
				t.Error("notifUri missing — this PCF panics without it (verified)")
			}
			// Return the k8s-internal hostname exactly like the real PCF does.
			w.Header().Set("Location", "https://pcf:29507/npcf-policyauthorization/v1/app-sessions/test-1")
			w.WriteHeader(http.StatusCreated)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/delete"):
			deletePaths = append(deletePaths, r.URL.Path)
			if strings.Contains(r.URL.Path, "gone-") {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer ts.Close()

	c, err := New(Config{PCFBase: ts.URL, UDRBase: ts.URL, PLMNID: "00101",
		NotifURI: "http://192.0.2.1:7777/notif"})
	if err != nil {
		t.Fatal(err)
	}
	aar := rx2n5.BuildAudioAAR("sess-conn-1", "10.45.0.2", "9000000001", 5000, 5001)
	ref, err := c.Policy().Create(context.Background(), &api.MediaGrant{ID: "sess-conn-1", Raw: aar})
	if err != nil {
		t.Fatal(err)
	}
	if ref != "https://pcf:29507/npcf-policyauthorization/v1/app-sessions/test-1" {
		t.Fatalf("ref = %q", ref)
	}

	if err := c.Policy().Delete(context.Background(), ref); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if len(deletePaths) != 1 || deletePaths[0] != "/npcf-policyauthorization/v1/app-sessions/test-1/delete" {
		t.Fatalf("delete hit %v — the k8s-internal Location was not rebased onto the reachable base", deletePaths)
	}
	// 404 = already gone = success.
	if err := c.Policy().Delete(context.Background(),
		"https://pcf:29507/npcf-policyauthorization/v1/app-sessions/gone-2"); err != nil {
		t.Fatalf("delete of missing session must be idempotent success, got %v", err)
	}
}

// TestAuthVectorsAndSQNWriteback characterizes LocalMilenage auth against a fake
// UDR serving the real testdata shape: vector geometry must be AKAv1 (RAND/AUTN 16,
// XRES 8, CK/IK 16) and the write-back must be (SQN+32) mod 2^48 as JSON-Patch —
// skipping it collapses every re-auth on SQN replay (verified).
func TestAuthVectorsAndSQNWriteback(t *testing.T) {
	authSub, err := os.ReadFile("testdata/authsub_standard.json")
	if err != nil {
		t.Fatal(err)
	}
	var patched string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			w.Write(authSub)
		case http.MethodPatch:
			if ct := r.Header.Get("Content-Type"); ct != "application/json-patch+json" {
				t.Errorf("PATCH content-type = %q", ct)
			}
			b, _ := io.ReadAll(r.Body)
			patched = string(b)
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer ts.Close()

	c, err := New(Config{PCFBase: ts.URL, UDRBase: ts.URL, PLMNID: "00101",
		NotifURI: "http://192.0.2.1:7777/notif"})
	if err != nil {
		t.Fatal(err)
	}
	vecs, err := c.Auth().Vectors(context.Background(), api.AuthChallenge{IMSI: "001010000000001"})
	if err != nil {
		t.Fatal(err)
	}
	if len(vecs) != 1 {
		t.Fatalf("got %d vectors, want 1", len(vecs))
	}
	v := vecs[0]
	if len(v.RAND) != 16 || len(v.AUTN) != 16 || len(v.XRES) != 8 || len(v.CK) != 16 || len(v.IK) != 16 {
		t.Fatalf("vector geometry RAND=%d AUTN=%d XRES=%d CK=%d IK=%d, want 16/16/8/16/16",
			len(v.RAND), len(v.AUTN), len(v.XRES), len(v.CK), len(v.IK))
	}
	// testdata SQN 16f3b3f70fc2 + 0x20 = 16f3b3f70fe2
	want := `[{"op":"replace","path":"/sequenceNumber/sqn","value":"16f3b3f70fe2"}]`
	if patched != want {
		t.Fatalf("SQN write-back = %s, want %s", patched, want)
	}
}

func TestNoCompiledInEndpoints(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("New must refuse empty PCF/UDR endpoints — compiled-in defaults are the verified reinstall trap")
	}
}

// TestPolicyUpdate: a second AAR for one Rx session is a PATCH of its app-session (TS 29.514 4.2.3), rebased onto the
// reachable PCF and carrying the new media; a PCF without PATCH is told apart from one that refuses the media.
func TestPolicyUpdate(t *testing.T) {
	var got struct {
		method, path, ctype string
		media               int
	}
	status := http.StatusOK
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.method, got.path, got.ctype = r.Method, r.URL.Path, r.Header.Get("Content-Type")
		var body struct {
			MedComponents map[string]json.RawMessage `json:"medComponents"`
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		got.media = len(body.MedComponents)
		w.WriteHeader(status)
	}))
	defer ts.Close()
	c, err := New(Config{PCFBase: ts.URL, UDRBase: ts.URL, PLMNID: "00101", NotifURI: "http://192.0.2.1:7777/notif"})
	if err != nil {
		t.Fatal(err)
	}
	ref := api.CoreRef("https://pcf:29507/npcf-policyauthorization/v1/app-sessions/test-1")
	g := &api.MediaGrant{ID: "sess-1", Raw: rx2n5.BuildAudioAAR("sess-1", "10.45.0.2", "9000000001", 5000, 5001)}

	if err := c.Policy().Update(context.Background(), ref, g); err != nil {
		t.Fatalf("update: %v", err)
	}
	if got.method != http.MethodPatch || got.path != "/npcf-policyauthorization/v1/app-sessions/test-1" ||
		got.ctype != "application/merge-patch+json" || got.media == 0 {
		t.Fatalf("PCF saw %+v", got)
	}
	for code, want := range map[int]error{
		http.StatusNoContent:           nil,
		http.StatusNotFound:            api.ErrUnsupported, // no such route (or session): re-create
		http.StatusMethodNotAllowed:    api.ErrUnsupported,
		http.StatusNotImplemented:      api.ErrUnsupported,
		http.StatusInternalServerError: api.ErrRejected,
	} {
		status = code
		err := c.Policy().Update(context.Background(), ref, g)
		if (want == nil && err != nil) || (want != nil && !errors.Is(err, want)) {
			t.Errorf("HTTP %d: %v, want %v", code, err, want)
		}
	}
}
