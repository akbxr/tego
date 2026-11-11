package session

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"time"

	"github.com/akbxr/tego/internal/llm"
)

// Subagent records keep the history of a task subagent that ended, so that
// the session can resume it after tego restarts. A record holds what was
// added to the subagent's history since its previous record (all of it
// when there is none, or when its history was rewritten), gzipped: the
// histories of many subagents together can be larger than the session's
// own. Media goes in blob entries like the session's.
//
// A record is anchored at the node the session's path was at when the
// subagent ended: only the records on the active path count, so a rewind
// to before a subagent ran forgets it, and coming back to that branch
// brings it back.

// subRecord is a subagent record without its history.
type subRecord struct {
	id, key, prev, anchor string
	from, count           int // its messages start at index from; count: the history's length
	meta                  json.RawMessage
	time                  time.Time
	off                   int64
	size                  int
}

// SubagentRecord is the latest record of one subagent.
type SubagentRecord struct {
	ID    string          // the record, for SubagentHistory and as the next one's prev
	Key   string          // the subagent: the same in all its records
	Meta  json.RawMessage // what the agent keeps about it
	Count int             // messages in its history
	Time  time.Time
}

// SaveSubagent records the subagent key's history: msgs are its messages
// from index from on, following record prev ("" with from 0: the whole
// history). It returns the record's id; "" when the session has no file.
func (s *Session) SaveSubagent(key, prev string, from int, meta json.RawMessage, msgs []llm.Message) (string, error) {
	if s == nil || key == "" || (prev == "") != (from == 0) {
		return "", errors.New("session: bad subagent record")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.f == nil {
		return "", nil
	}
	msgs = slices.Clone(msgs)
	for i := range msgs {
		var err error
		if msgs[i].Blocks, err = s.refBlocks(msgs[i].Blocks); err != nil {
			return "", err
		}
	}
	data, err := json.Marshal(msgs)
	if err != nil {
		return "", err
	}
	var z bytes.Buffer
	w := gzip.NewWriter(&z)
	_, _ = w.Write(data)
	if err := w.Close(); err != nil {
		return "", err
	}
	e := entry{Type: "subagent", ID: randHex(4), Parent: s.leaf, Time: time.Now().Unix(), Key: key, Prev: prev,
		From: from, Count: from + len(msgs), Meta: meta, Gzip: base64.StdEncoding.EncodeToString(z.Bytes())}
	line, err := json.Marshal(e)
	if err != nil {
		return "", err
	}
	off, size, err := s.writeLine(line)
	if err != nil {
		return "", err
	}
	if off >= 0 {
		s.subs = append(s.subs, &subRecord{id: e.ID, key: key, prev: prev, anchor: e.Parent, from: from,
			count: e.Count, meta: meta, time: time.Unix(e.Time, 0), off: off, size: size})
	}
	return e.ID, nil
}

// Subagents are the latest record of each subagent on the active path,
// oldest first.
func (s *Session) Subagents() []SubagentRecord {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	recs, leaf := slices.Clone(s.subs), s.leaf
	s.mu.Unlock()
	if len(recs) == 0 {
		return nil
	}
	onPath := map[string]bool{"": true}
	for n := s.nodes[leaf]; n != nil; n = s.nodes[n.Parent] {
		onPath[n.ID] = true
	}
	latest := map[string]*subRecord{}
	for _, r := range recs {
		if onPath[r.anchor] {
			latest[r.key] = r // file order: a later one replaces it
		}
	}
	var out []SubagentRecord
	for _, r := range recs {
		if latest[r.key] == r {
			out = append(out, SubagentRecord{ID: r.id, Key: r.key, Meta: r.meta, Count: r.count, Time: r.time})
		}
	}
	return out
}

// SubagentHistory reads the history that record id holds, with the records
// before it that it builds on.
func (s *Session) SubagentHistory(id string) ([]llm.Message, error) {
	if s == nil {
		return nil, errors.New("no session")
	}
	s.mu.Lock()
	byID := make(map[string]*subRecord, len(s.subs))
	for _, r := range s.subs {
		byID[r.id] = r
	}
	s.mu.Unlock()
	var chain []*subRecord
	for at := id; ; {
		r := byID[at]
		if r == nil {
			return nil, fmt.Errorf("record %s is missing from the session file", at)
		}
		chain = append(chain, r)
		if r.prev == "" {
			break
		}
		if len(chain) > len(byID) {
			return nil, errors.New("the records go in a circle")
		}
		at = r.prev
	}
	rd := s.reader()
	defer rd.close()
	var hist []llm.Message
	for i := len(chain) - 1; i >= 0; i-- {
		r := chain[i]
		msgs, err := rd.subagent(r)
		if err != nil {
			return nil, fmt.Errorf("record %s: %w", r.id, err)
		}
		if r.from > len(hist) {
			return nil, fmt.Errorf("record %s starts at message %d of %d", r.id, r.from, len(hist))
		}
		hist = append(hist[:r.from:r.from], msgs...)
	}
	return hist, nil
}

// subagent reads the messages of record r back from the file.
func (rd *fileReader) subagent(r *subRecord) ([]llm.Message, error) {
	buf := make([]byte, r.size)
	if err := rd.readAt(buf, r.off); err != nil {
		return nil, err
	}
	var e struct {
		ID   string `json:"id"`
		Gzip string `json:"gzip"`
	}
	if err := json.Unmarshal(buf, &e); err != nil || e.ID != r.id {
		return nil, errors.New("not where it was")
	}
	z, err := base64.StdEncoding.DecodeString(e.Gzip)
	if err != nil {
		return nil, err
	}
	zr, err := gzip.NewReader(bytes.NewReader(z))
	if err != nil {
		return nil, err
	}
	data, err := io.ReadAll(zr)
	if err != nil {
		return nil, err
	}
	var msgs []llm.Message
	if err := json.Unmarshal(data, &msgs); err != nil {
		return nil, err
	}
	for i := range msgs {
		rd.resolve(msgs[i].Blocks)
	}
	return msgs, nil
}
