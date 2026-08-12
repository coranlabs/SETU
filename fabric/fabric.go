// SPDX-FileCopyrightText: 2026 Coran Labs Private Limited
// SPDX-License-Identifier: Apache-2.0

// Package fabric keeps the mapping from adapter session id to core session
// reference, backed by a write-ahead log so a restart does not leave sessions
// stranded on the core.
//
// The in-memory map is authoritative for the running process and is updated
// synchronously. WAL writes fsync, so they happen on a single background writer
// and never block a signalling answer; a crash can therefore lose the last few
// queued entries. Per-session ordering is preserved by the channel, and Close
// drains it.
package fabric

import (
	"log"
	"sync"
	"time"

	api "github.com/coranlabs/SETU/api/v1"
	"github.com/coranlabs/SETU/fabric/sessionstore"
)

type walOp struct {
	del bool
	sid string
	rec sessionstore.Record
}

// Grants maps adapter session ids (Rx Session-Id) to core references, durably when
// a WAL path is configured, in memory otherwise.
type Grants struct {
	mu  sync.Mutex
	mem map[string]api.CoreRef

	wal    sessionstore.Store // nil = memory-only
	ch     chan walOp
	wg     sync.WaitGroup
	closed bool
}

// Open returns a Grants store. walPath == "" selects memory-only; otherwise the
// WAL at walPath is replayed and the surviving grants are restored.
func Open(walPath string) (*Grants, map[string]api.CoreRef, error) {
	g := &Grants{mem: map[string]api.CoreRef{}}
	if walPath == "" {
		return g, nil, nil
	}
	fs, err := sessionstore.Open(walPath)
	if err != nil {
		return nil, nil, err
	}
	g.wal = fs
	restored, err := fs.Load()
	if err != nil {
		return nil, nil, err
	}
	out := make(map[string]api.CoreRef, len(restored))
	for sid, rec := range restored {
		g.mem[sid] = api.CoreRef(rec.Location)
		out[sid] = api.CoreRef(rec.Location)
	}
	g.ch = make(chan walOp, 4096)
	g.wg.Add(1)
	go g.writer()
	return g, out, nil
}

// writer drains the queue into the WAL.
func (g *Grants) writer() {
	defer g.wg.Done()
	for op := range g.ch {
		var err error
		if op.del {
			err = g.wal.Delete(op.sid)
		} else {
			err = g.wal.Put(op.sid, op.rec)
		}
		if err != nil {
			log.Printf("fabric: WAL %s %s failed: %v", opName(op), op.sid, err)
		}
	}
}

func opName(op walOp) string {
	if op.del {
		return "del"
	}
	return "put"
}

// enqueue drops the entry rather than block if the writer falls far behind.
func (g *Grants) enqueue(op walOp) {
	if g.ch == nil {
		return
	}
	select {
	case g.ch <- op:
	default:
		log.Printf("fabric: WAL queue full, dropping %s %s (grant durable only in memory)", opName(op), op.sid)
	}
}

// Bind records sid -> ref.
func (g *Grants) Bind(sid string, ref api.CoreRef) error {
	g.mu.Lock()
	g.mem[sid] = ref
	g.mu.Unlock()
	g.enqueue(walOp{sid: sid, rec: sessionstore.Record{Location: string(ref), LastSeen: time.Now()}})
	return nil
}

// Take removes and returns the ref for sid.
func (g *Grants) Take(sid string) (api.CoreRef, bool) {
	g.mu.Lock()
	ref, ok := g.mem[sid]
	if ok {
		delete(g.mem, sid)
	}
	g.mu.Unlock()
	if ok {
		g.enqueue(walOp{del: true, sid: sid})
	}
	return ref, ok
}

// Len reports the number of live grants.
func (g *Grants) Len() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.mem)
}

// Close drains pending writes and releases the WAL.
func (g *Grants) Close() error {
	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		return nil
	}
	g.closed = true
	g.mu.Unlock()
	if g.ch != nil {
		close(g.ch)
		g.wg.Wait()
	}
	if g.wal != nil {
		return g.wal.Close()
	}
	return nil
}
