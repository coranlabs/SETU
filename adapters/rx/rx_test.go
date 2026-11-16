// SPDX-FileCopyrightText: 2026 Coran Labs Private Limited
// SPDX-License-Identifier: Apache-2.0

package rx

import (
	"context"
	"fmt"
	"strings"
	"testing"

	api "github.com/coranlabs/SETU/api/v1"
	"github.com/coranlabs/SETU/connectors/sdcore/rx2n5"
	"github.com/coranlabs/SETU/fabric"
	"github.com/coranlabs/SETU/wire/diameter"
)

// fakeCore scripts the connector behaviour so the adapter's wire answers can be
// characterized without a real core.
type fakeCore struct {
	createErr error
	created   []string // grant IDs seen
	deleted   []api.CoreRef
	canUpdate bool          // the core updates a grant in place (N5 PATCH)
	updated   []api.CoreRef // grants updated
	order     []string      // "create", "update", "delete" as they happened
}

func (f *fakeCore) Name() string           { return "fake" }
func (f *fakeCore) Capabilities() api.Caps { return api.Caps{} }
func (f *fakeCore) Policy() api.PolicyBackend {
	return f
}
func (f *fakeCore) Subscriber() api.SubscriberBackend { return nil }
func (f *fakeCore) Auth() api.AuthBackend             { return nil }
func (f *fakeCore) Events() api.EventSource           { return nil }

func (f *fakeCore) Create(_ context.Context, g *api.MediaGrant) (api.CoreRef, error) {
	if f.createErr != nil {
		return "", f.createErr
	}
	f.order = append(f.order, "create")
	f.created = append(f.created, g.ID)
	return api.CoreRef("https://pcf:29507/npcf-policyauthorization/v1/app-sessions/" + g.ID), nil
}
func (f *fakeCore) Update(_ context.Context, ref api.CoreRef, _ *api.MediaGrant) error {
	if !f.canUpdate {
		return api.ErrUnsupported
	}
	f.order = append(f.order, "update")
	f.updated = append(f.updated, ref)
	return nil
}
func (f *fakeCore) Delete(_ context.Context, ref api.CoreRef) error {
	f.order = append(f.order, "delete")
	f.deleted = append(f.deleted, ref)
	return nil
}

func newAdapter(t *testing.T, core api.CoreConnector, strict bool) *Adapter {
	t.Helper()
	grants, _, err := fabric.Open("")
	if err != nil {
		t.Fatal(err)
	}
	return &Adapter{
		OriginHost:  "rxgw.ims.mnc001.mcc001.3gppnetwork.org",
		OriginRealm: "ims.mnc001.mcc001.3gppnetwork.org",
		Core:        core, Grants: grants, Strict: strict,
	}
}

func resultCode(t *testing.T, ans diameter.Message) uint32 {
	t.Helper()
	a, err := diameter.Find(ans.AVPs, 268, 0) // Result-Code
	if err != nil {
		return 0
	}
	v, _ := a.U32Val()
	return v
}

func experimentalCode(t *testing.T, ans diameter.Message) uint32 {
	t.Helper()
	g, err := diameter.Find(ans.AVPs, 297, 0) // Experimental-Result
	if err != nil {
		return 0
	}
	kids, err := g.SubAVPs()
	if err != nil {
		return 0
	}
	c, err := diameter.Find(kids, 298, 0)
	if err != nil {
		return 0
	}
	v, _ := c.U32Val()
	return v
}

// TestAARLegacyHappyPath: AAR -> 2001, grant bound; STR -> core delete of that ref.
// This is the proven wire behaviour the migration must preserve bit-for-bit.
func TestAARLegacyHappyPath(t *testing.T) {
	core := &fakeCore{}
	a := newAdapter(t, core, false)
	aar := rx2n5.BuildAudioAAR("sess-legacy-1", "10.45.0.2", "9000000001", 5000, 5001)

	ans, err := a.Handle(nil, aar)
	if err != nil {
		t.Fatal(err)
	}
	if rc := resultCode(t, ans); rc != 2001 {
		t.Fatalf("AAR answer Result-Code = %d, want 2001", rc)
	}
	if len(core.created) != 1 || core.created[0] != "sess-legacy-1" {
		t.Fatalf("core saw creates %v, want [sess-legacy-1]", core.created)
	}
	if a.Grants.Len() != 1 {
		t.Fatalf("grants = %d, want 1", a.Grants.Len())
	}

	str := rx2n5.BuildSTR("sess-legacy-1")
	ans, err = a.Handle(nil, str)
	if err != nil {
		t.Fatal(err)
	}
	if rc := resultCode(t, ans); rc != 2001 {
		t.Fatalf("STR answer Result-Code = %d, want 2001", rc)
	}
	if len(core.deleted) != 1 || core.deleted[0] != "https://pcf:29507/npcf-policyauthorization/v1/app-sessions/sess-legacy-1" {
		t.Fatalf("core saw deletes %v", core.deleted)
	}
	if a.Grants.Len() != 0 {
		t.Fatalf("grants = %d after STR, want 0", a.Grants.Len())
	}
}

// TestAARLegacyBestEffort: create fails but legacy mode still answers 2001 —
// exactly the deployed system's (verified) behaviour, kept for bit-compatibility.
func TestAARLegacyBestEffort(t *testing.T) {
	core := &fakeCore{createErr: fmt.Errorf("%w: PCF down", api.ErrUnavailable)}
	a := newAdapter(t, core, false)
	ans, err := a.Handle(nil, rx2n5.BuildAudioAAR("sess-be", "10.45.0.2", "9000000001", 5000, 5001))
	if err != nil {
		t.Fatal(err)
	}
	if rc := resultCode(t, ans); rc != 2001 {
		t.Fatalf("legacy best-effort Result-Code = %d, want 2001", rc)
	}
	if a.Grants.Len() != 0 {
		t.Fatal("failed create must not bind a grant")
	}
}

// TestAARStrictTaxonomy: strict mode maps waist errors onto TS 29.214 answers.
func TestAARStrictTaxonomy(t *testing.T) {
	cases := []struct {
		err     error
		wantRC  uint32 // Result-Code (0 = absent)
		wantExp uint32 // Experimental-Result-Code (0 = absent)
	}{
		{fmt.Errorf("%w: no SM policy", api.ErrNoSession), 0, 5065},
		{fmt.Errorf("%w: refused", api.ErrRejected), 0, 5063},
		{fmt.Errorf("%w: timeout", api.ErrUnavailable), 3004, 0},
	}
	for _, tc := range cases {
		core := &fakeCore{createErr: tc.err}
		a := newAdapter(t, core, true)
		ans, err := a.Handle(nil, rx2n5.BuildAudioAAR("sess-strict", "10.45.0.2", "9000000001", 5000, 5001))
		if err != nil {
			t.Fatal(err)
		}
		if rc := resultCode(t, ans); rc != tc.wantRC {
			t.Errorf("%v: Result-Code = %d, want %d", tc.err, rc, tc.wantRC)
		}
		if ec := experimentalCode(t, ans); ec != tc.wantExp {
			t.Errorf("%v: Experimental-Result-Code = %d, want %d", tc.err, ec, tc.wantExp)
		}
	}
}

// Live 2026-11-11: the P-CSCF sends a second AAR for the same Rx session when the call is answered. setu created a
// second app-session for it: the voice bearer was torn down 0.5 s after it came up, the caller hung up 3 s after the
// answer, and the first session's rules leaked into the next call. The second AAR updates the first grant.
func TestASecondAARForTheSameSessionUpdatesItsGrant(t *testing.T) {
	core := &fakeCore{canUpdate: true}
	a := newAdapter(t, core, true)
	for i := 0; i < 2; i++ { // the call offered, then answered
		if ans, _ := a.Handle(nil, rx2n5.BuildAudioAAR("sess-call", "10.45.0.2", "9000000001", 5000, 5001)); resultCode(t, ans) != 2001 {
			t.Fatalf("AAR %d answered %d", i+1, resultCode(t, ans))
		}
	}
	if len(core.created) != 1 || len(core.updated) != 1 || a.Grants.Len() != 1 {
		t.Fatalf("created %v, updated %v, grants %d: want one grant, created once and updated once",
			core.created, core.updated, a.Grants.Len())
	}
	a.Handle(nil, rx2n5.BuildSTR("sess-call"))
	if len(core.deleted) != 1 || a.Grants.Len() != 0 {
		t.Fatalf("after STR: deleted %v, grants %d", core.deleted, a.Grants.Len())
	}
}

// A core that cannot update has the old grant released before the new one is made: nothing is left behind.
func TestACoreThatCannotUpdateReleasesTheOldGrantFirst(t *testing.T) {
	core := &fakeCore{}
	a := newAdapter(t, core, true)
	a.Handle(nil, rx2n5.BuildAudioAAR("sess-call", "10.45.0.2", "9000000001", 5000, 5001))
	a.Handle(nil, rx2n5.BuildAudioAAR("sess-call", "10.45.0.2", "9000000001", 5002, 5003))
	if got := strings.Join(core.order, ","); got != "create,delete,create" {
		t.Fatalf("core saw %s, want create,delete,create", got)
	}
	if a.Grants.Len() != 1 {
		t.Fatalf("grants = %d, want 1", a.Grants.Len())
	}
}
