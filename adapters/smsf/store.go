// SPDX-FileCopyrightText: 2026 Coran Labs Private Limited
// SPDX-License-Identifier: Apache-2.0

package smsf

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/coranlabs/SETU/uilog"
)

type guami struct {
	PLMNID PLMN   `json:"plmnId"`
	AMFID  string `json:"amfId"`
}

// ueCtx is one UE's SMS context, as the AMF's Activate gave it.
type ueCtx struct {
	SUPI       string    `json:"supi"`
	GPSI       string    `json:"gpsi,omitempty"`
	MSISDN     string    `json:"msisdn,omitempty"` // the digits of the gpsi, or of the UDR's am-data
	AMFID      string    `json:"amfId"`            // the serving AMF's NF instance id
	AccessType string    `json:"accessType"`
	GUAMIs     []guami   `json:"guamis,omitempty"`
	Updated    time.Time `json:"updated"`
}

// store holds the SMS contexts by SUPI and finds a SUPI by number.
type store struct {
	file string

	mu       sync.Mutex
	ues      map[string]ueCtx
	byMSISDN map[string]map[string]bool // msisdn -> the SUPIs that have it
	warned   map[string]bool            // shared numbers already logged

	fileMu sync.Mutex // one snapshot write at a time
}

func newStore(file string) *store {
	return &store{file: file, ues: map[string]ueCtx{}, byMSISDN: map[string]map[string]bool{}, warned: map[string]bool{}}
}

// put stores c and reports whether it is new.
func (st *store) put(c ueCtx) (created bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	old, had := st.ues[c.SUPI]
	if had {
		st.unindex(old)
	}
	st.ues[c.SUPI] = c
	st.index(c)
	return !had
}

func (st *store) del(supi string) (ueCtx, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	c, ok := st.ues[supi]
	if ok {
		st.unindex(c)
		delete(st.ues, supi)
	}
	return c, ok
}

func (st *store) get(supi string) (ueCtx, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	c, ok := st.ues[supi]
	return c, ok
}

// lookup finds the one SUPI with this number. A number two SIMs share is nobody's: guessing would hand one
// person's messages to the other.
func (st *store) lookup(msisdn string) (string, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	set := st.byMSISDN[msisdn]
	if len(set) != 1 {
		return "", false
	}
	for supi := range set {
		return supi, true
	}
	return "", false
}

// index adds c to the number index. Called with st.mu held.
func (st *store) index(c ueCtx) {
	if c.MSISDN == "" {
		return
	}
	set := st.byMSISDN[c.MSISDN]
	if set == nil {
		set = map[string]bool{}
		st.byMSISDN[c.MSISDN] = set
	}
	set[c.SUPI] = true
	if len(set) > 1 && !st.warned[c.MSISDN] {
		st.warned[c.MSISDN] = true
		uilog.Event(uilog.Orange, "SMSF", "%d SIMs share number %s: it gets no SMS over NAS until one goes", len(set), uilog.Mask(c.MSISDN))
	}
}

// unindex removes c from the number index. Called with st.mu held.
func (st *store) unindex(c ueCtx) {
	set := st.byMSISDN[c.MSISDN]
	if set == nil {
		return
	}
	delete(set, c.SUPI)
	if len(set) == 0 {
		delete(st.byMSISDN, c.MSISDN)
	}
	if len(set) <= 1 {
		delete(st.warned, c.MSISDN)
	}
}

// save writes every context to the state file: a temporary file renamed over the old one, so a crash mid-write
// leaves the previous snapshot whole. The file names subscribers, so only setu's user reads it.
func (st *store) save() error {
	if st.file == "" {
		return nil
	}
	st.mu.Lock()
	all := make([]ueCtx, 0, len(st.ues))
	for _, c := range st.ues {
		all = append(all, c)
	}
	st.mu.Unlock()
	sort.Slice(all, func(i, j int) bool { return all[i].SUPI < all[j].SUPI })
	b, err := json.MarshalIndent(all, "", "  ")
	if err != nil {
		return err
	}
	st.fileMu.Lock()
	defer st.fileMu.Unlock()
	tmp, err := os.CreateTemp(filepath.Dir(st.file), filepath.Base(st.file)+".tmp*")
	if err != nil {
		return fmt.Errorf("smsf: state file: %w", err)
	}
	defer os.Remove(tmp.Name()) // a no-op once renamed
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return fmt.Errorf("smsf: state file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("smsf: state file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("smsf: state file: %w", err)
	}
	if err := os.Rename(tmp.Name(), st.file); err != nil {
		return fmt.Errorf("smsf: state file: %w", err)
	}
	return nil
}

// load reads the state file, dropping contexts not updated within maxAge: the AMF has long since moved on from
// those, and a stale one would send MT messages to a phone that is not there.
func (st *store) load(maxAge time.Duration, now time.Time) (int, error) {
	if st.file == "" {
		return 0, nil
	}
	b, err := os.ReadFile(st.file)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("smsf: state file: %w", err)
	}
	var all []ueCtx
	if err := json.Unmarshal(b, &all); err != nil {
		return 0, fmt.Errorf("smsf: state file %s: %w", st.file, err)
	}
	n := 0
	for _, c := range all {
		if c.SUPI == "" || now.Sub(c.Updated) > maxAge {
			continue
		}
		st.put(c)
		n++
	}
	return n, nil
}

// saveState writes the contexts after each change. A failed write is logged, not fatal: the contexts in memory are
// still right.
func (s *SMSF) saveState() {
	if err := s.store.save(); err != nil {
		uilog.Event(uilog.Red, "SMSF", "%v", err)
	}
}
