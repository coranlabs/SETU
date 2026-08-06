// SPDX-FileCopyrightText: 2026 Coran Labs Private Limited
// SPDX-License-Identifier: Apache-2.0

package sdcore

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"

	api "github.com/coranlabs/SETU/api/v1"
	"github.com/coranlabs/SETU/connectors/sdcore/udr"
	"github.com/coranlabs/SETU/crypto/milenage"
)

// auth generates AKA vectors locally: K/OPc/AMF/SQN come from the UDR, MILENAGE
// does the rest.
type auth Connector

func (a *auth) Vectors(ctx context.Context, ch api.AuthChallenge) ([]api.AuthVector, error) {
	if ch.Count <= 0 {
		ch.Count = 1
	}
	av, err := a.fetchAuthSub(ctx, ch.IMSI)
	if err != nil {
		return nil, err
	}
	out := make([]api.AuthVector, 0, ch.Count)
	sqn := av.SQN
	for i := 0; i < ch.Count; i++ {
		rnd := make([]byte, 16)
		if _, err := rand.Read(rnd); err != nil {
			return nil, err
		}
		q := milenage.Generate(av.K, av.OPc, av.AMF, sqn, rnd)
		out = append(out, api.AuthVector{RAND: q.RAND, AUTN: q.AUTN, XRES: q.XRES, CK: q.CK, IK: q.IK})
	}
	// IMS-AKA and 5G-AKA share this counter, so it has to move or the USIM
	// rejects the next authentication as a replay.
	a.advanceSQN(ctx, ch.IMSI, av.SQN)
	return out, nil
}

func (a *auth) fetchAuthSub(ctx context.Context, imsi string) (udr.AuthVector, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, authSubURL(a.cfg.UDRBase, imsi), nil)
	resp, err := a.hc.Do(req)
	if err != nil {
		return udr.AuthVector{}, fmt.Errorf("%w: %v", api.ErrUnavailable, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return udr.AuthVector{}, fmt.Errorf("%w: UDR auth-subscription status %d", api.ErrRejected, resp.StatusCode)
	}
	return udr.ParseAuthSubscription(body)
}

// advanceSQN writes back (sqn+step) mod 2^48. Best effort; on failure the UE
// resyncs on its next attempt.
func (a *auth) advanceSQN(ctx context.Context, imsi string, sqn []byte) {
	n := new(big.Int).SetBytes(sqn)
	n.Add(n, big.NewInt(a.Capabilities().SQNStep))
	mask := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 48), big.NewInt(1))
	n.And(n, mask)
	b := n.Bytes()
	out := make([]byte, 6)
	copy(out[6-len(b):], b)
	body := fmt.Sprintf(`[{"op":"replace","path":"/sequenceNumber/sqn","value":"%s"}]`, hex.EncodeToString(out))
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, authSubURL(a.cfg.UDRBase, imsi), strings.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json-patch+json")
	if resp, err := a.hc.Do(req); err == nil {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
}

func (a *auth) Capabilities() api.Caps { return (*Connector)(a).Capabilities() }
