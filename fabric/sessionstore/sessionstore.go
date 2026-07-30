// SPDX-FileCopyrightText: 2026 Coran Labs Private Limited
// SPDX-License-Identifier: Apache-2.0

// Package sessionstore persists the Rx↔N5 gateway's session state so a gateway
// restart does not orphan live VoNR app-sessions on the PCF. It stores only the
// serializable subset of a session — the N5 app-session Location, the notify
// token, and the last-seen time. The live Diameter peer connection is deliberately
// NOT persisted: after a restart the P-CSCF's cdp re-establishes the Rx
// association (a fresh CER), so the peer pointer is naturally rebuilt on the next
// inbound request; until then the persisted Location still lets the gateway PATCH
// or DELETE the PCF app-session over N5 (HTTP, peer-independent).
//
// FileStore is a crash-safe, append-only write-ahead log on local disk (stdlib
// only). It is the single-node durability tier; the Store interface is what a
// distributed store (e.g. Redis) plugs into for active/active HA.
package sessionstore

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"syscall"
	"time"
)

// Record is the persisted, serializable subset of a gateway session.
type Record struct {
	Location string    `json:"loc"`  // N5 app-session URL (for PATCH/DELETE)
	Token    string    `json:"tok"`  // per-session notify token
	LastSeen time.Time `json:"seen"` // for idle GC after restore
}

// Store persists session records keyed by Rx Session-Id.
type Store interface {
	Put(sid string, r Record) error
	Delete(sid string) error
	Load() (map[string]Record, error)
	Close() error
}

// walEntry is one line of the write-ahead log.
type walEntry struct {
	Op  string  `json:"op"` // "put" | "del"
	SID string  `json:"sid"`
	Rec *Record `json:"rec,omitempty"`
}

// FileStore is a crash-safe append-only WAL. On Open it replays the log to
// recover the live set, compacts the file to one line per live session, then
// appends put/del entries as sessions come and go. Safe for concurrent use.
type FileStore struct {
	mu   sync.Mutex
	path string
	f    *os.File
	lock *os.File // advisory flock holder (see Open)
	live map[string]Record
	Sync bool // fsync after each write (default true via Open)
}

// Open replays any existing WAL at path, compacts it, and returns a FileStore
// ready for appends. A missing file is treated as an empty store.
//
// An advisory lock on a sidecar .lock file stops two processes from interleaving
// appends into one WAL, which loses delete records.
func Open(path string) (*FileStore, error) {
	lf, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(lf.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lf.Close()
		return nil, fmt.Errorf("sessionstore: %s is locked by another process: %w", path, err)
	}
	live, err := replay(path)
	if err != nil {
		lf.Close()
		return nil, err
	}
	s := &FileStore{path: path, live: live, Sync: true, lock: lf}
	if err := s.compact(); err != nil {
		lf.Close()
		return nil, err
	}
	return s, nil
}

// replay reads the WAL and folds put/del entries into the live map. A truncated
// or corrupt trailing line (e.g. from a crash mid-write) is tolerated: replay
// stops at the first unparsable line and keeps everything before it.
func replay(path string) (map[string]Record, error) {
	live := map[string]Record{}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return live, nil
		}
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var e walEntry
		if err := json.Unmarshal(line, &e); err != nil {
			// crash-truncated tail: stop, keep prior entries.
			break
		}
		switch e.Op {
		case "put":
			if e.Rec != nil {
				live[e.SID] = *e.Rec
			}
		case "del":
			delete(live, e.SID)
		}
	}
	return live, nil
}

// compact rewrites the WAL to exactly one put per live session (atomic
// tmp+rename), then reopens it for appends. Bounds file growth to the live set
// rather than the full history.
func (s *FileStore) compact() error {
	if s.f != nil {
		s.f.Close()
		s.f = nil
	}
	tmp := s.path + ".tmp"
	tf, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	w := bufio.NewWriter(tf)
	for sid, r := range s.live {
		rec := r
		if err := writeLine(w, walEntry{Op: "put", SID: sid, Rec: &rec}); err != nil {
			tf.Close()
			return err
		}
	}
	if err := w.Flush(); err != nil {
		tf.Close()
		return err
	}
	if err := tf.Sync(); err != nil {
		tf.Close()
		return err
	}
	if err := tf.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return err
	}
	f, err := os.OpenFile(s.path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	s.f = f
	return nil
}

func writeLine(w *bufio.Writer, e walEntry) error {
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	if _, err := w.Write(b); err != nil {
		return err
	}
	return w.WriteByte('\n')
}

// Put records or updates a session.
func (s *FileStore) Put(sid string, r Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.append(walEntry{Op: "put", SID: sid, Rec: &r}); err != nil {
		return err
	}
	s.live[sid] = r
	return nil
}

// Delete removes a session.
func (s *FileStore) Delete(sid string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.live[sid]; !ok {
		return nil
	}
	if err := s.append(walEntry{Op: "del", SID: sid}); err != nil {
		return err
	}
	delete(s.live, sid)
	return nil
}

func (s *FileStore) append(e walEntry) error {
	if s.f == nil {
		return fmt.Errorf("sessionstore: closed")
	}
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if _, err := s.f.Write(b); err != nil {
		return err
	}
	if s.Sync {
		return s.f.Sync()
	}
	return nil
}

// Load returns a snapshot copy of the live session set.
func (s *FileStore) Load() (map[string]Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]Record, len(s.live))
	for k, v := range s.live {
		out[k] = v
	}
	return out, nil
}

// Close flushes and closes the underlying file.
func (s *FileStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.f == nil {
		return nil
	}
	err := s.f.Close()
	s.f = nil
	if s.lock != nil {
		s.lock.Close() // releases the flock
		s.lock = nil
	}
	return err
}
