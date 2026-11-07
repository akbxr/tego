// Package peer lets tego sessions that are open at the same time on one
// machine find each other and exchange messages. Each tego process that
// takes messages listens on a unix socket and leaves a registry file
// beside it in <base>/live; the files of processes that are gone are
// removed by whoever lists them.
package peer

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Open is a session a process has open.
type Open struct {
	ID    string `json:"id"`
	Title string `json:"title,omitempty"`
	Cwd   string `json:"cwd,omitempty"`
	Busy  bool   `json:"busy,omitempty"` // a turn runs
	Model string `json:"model,omitempty"`
	Kind  string `json:"kind,omitempty"` // "terminal" or "editor" (ACP)
	PID   int    `json:"pid,omitempty"`  // set by List
}

// Message is a message from one session to another.
type Message struct {
	To        string    `json:"to"`
	From      string    `json:"from"`
	FromTitle string    `json:"from_title,omitempty"`
	FromCwd   string    `json:"from_cwd,omitempty"`
	Text      string    `json:"text"`
	Hops      int       `json:"hops"`      // messages in the chain that led to this one, this one included
	CanReply  bool      `json:"can_reply"` // the sender takes messages
	Sent      time.Time `json:"sent"`
}

// Outcome is what the receiving session did with a message.
type Outcome struct {
	// Status: "steered" (a turn runs; it reads it before its next request),
	// "started" (it was idle and started a turn to answer), "held" (it
	// waits for its user's next prompt), "queued" (the receiver was busy
	// with its screen; it is handled shortly) or "gone" (no such session
	// is open there any more).
	Status string `json:"status"`
	Note   string `json:"note,omitempty"`
}

// Handler is a process's side: the sessions it has open, and what it does
// with a message to one of them.
type Handler interface {
	Open() []Open
	Deliver(m Message) Outcome
}

// MaxText bounds a message.
const MaxText = 20_000

type request struct {
	Op      string   `json:"op"` // "open" | "send"
	Message *Message `json:"message,omitempty"`
}

type response struct {
	Open    []Open   `json:"open,omitempty"`
	Outcome *Outcome `json:"outcome,omitempty"`
	Error   string   `json:"error,omitempty"`
}

type record struct {
	PID    int    `json:"pid"`
	Socket string `json:"socket"`
}

func liveDir(base string) string { return filepath.Join(base, "live") }

// socketPath is where pid listens: beside its record, or in the temp
// folder when that path is too long for a unix socket.
func socketPath(base string, pid int) string {
	p := filepath.Join(liveDir(base), strconv.Itoa(pid)+".sock")
	if len(p) < 100 {
		return p
	}
	sum := sha256.Sum256([]byte(base))
	return filepath.Join(os.TempDir(), fmt.Sprintf("tego-%x-%d.sock", sum[:4], pid))
}

// Hub is a process's listener.
type Hub struct {
	ln     net.Listener
	record string
	socket string
	wg     sync.WaitGroup
	once   sync.Once
}

// Listen starts taking messages for h's sessions.
func Listen(base string, h Handler) (*Hub, error) {
	dir := liveDir(base)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	pid := os.Getpid()
	sock := socketPath(base, pid)
	_ = os.Remove(sock) // a process with this pid that is gone left it
	ln, err := net.Listen("unix", sock)
	if err != nil {
		return nil, err
	}
	_ = os.Chmod(sock, 0o600)
	hub := &Hub{ln: ln, socket: sock, record: filepath.Join(dir, strconv.Itoa(pid)+".json")}
	data, _ := json.Marshal(record{PID: pid, Socket: sock})
	if err := os.WriteFile(hub.record, data, 0o600); err != nil {
		ln.Close()
		return nil, err
	}
	hub.wg.Add(1)
	go func() {
		defer hub.wg.Done()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			hub.wg.Add(1)
			go func() {
				defer hub.wg.Done()
				serve(c, h)
			}()
		}
	}()
	return hub, nil
}

// Close stops taking messages and removes the registry file.
func (hub *Hub) Close() {
	if hub == nil {
		return
	}
	hub.once.Do(func() {
		hub.ln.Close()
		_ = os.Remove(hub.record)
		_ = os.Remove(hub.socket)
		hub.wg.Wait()
	})
}

func serve(c net.Conn, h Handler) {
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(30 * time.Second))
	var req request
	if err := json.NewDecoder(bufio.NewReader(c)).Decode(&req); err != nil {
		return
	}
	var resp response
	switch req.Op {
	case "open":
		resp.Open = h.Open()
	case "send":
		if req.Message == nil {
			resp.Error = "no message"
			break
		}
		out := h.Deliver(*req.Message)
		resp.Outcome = &out
	default:
		resp.Error = "unknown op " + req.Op
	}
	_ = json.NewEncoder(c).Encode(resp)
}

func call(ctx context.Context, sock string, req request) (response, error) {
	var d net.Dialer
	c, err := d.DialContext(ctx, "unix", sock)
	if err != nil {
		return response{}, err
	}
	defer c.Close()
	if dl, ok := ctx.Deadline(); ok {
		_ = c.SetDeadline(dl)
	}
	if err := json.NewEncoder(c).Encode(req); err != nil {
		return response{}, err
	}
	var resp response
	if err := json.NewDecoder(c).Decode(&resp); err != nil {
		return response{}, err
	}
	if resp.Error != "" {
		return resp, errors.New(resp.Error)
	}
	return resp, nil
}

func alive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func records(base string) []record {
	ents, err := os.ReadDir(liveDir(base))
	if err != nil {
		return nil
	}
	var out []record
	for _, e := range ents {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		path := filepath.Join(liveDir(base), e.Name())
		var r record
		data, err := os.ReadFile(path)
		if err != nil || json.Unmarshal(data, &r) != nil || r.PID <= 0 {
			continue
		}
		if !alive(r.PID) {
			_ = os.Remove(path)
			_ = os.Remove(r.Socket)
			continue
		}
		out = append(out, r)
	}
	return out
}

// List asks every live process which sessions it has open.
func List(ctx context.Context, base string) []Open {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	recs := records(base)
	var (
		mu  sync.Mutex
		out []Open
		wg  sync.WaitGroup
	)
	for _, r := range recs {
		wg.Add(1)
		go func(r record) {
			defer wg.Done()
			resp, err := call(ctx, r.Socket, request{Op: "open"})
			if err != nil {
				return
			}
			mu.Lock()
			for _, o := range resp.Open {
				o.PID = r.PID
				out = append(out, o)
			}
			mu.Unlock()
		}(r)
	}
	wg.Wait()
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	return out
}

// ErrNotOpen: no live process has the session open.
var ErrNotOpen = errors.New("not open in any running tego")

// Send delivers m to the process that has m.To open.
func Send(ctx context.Context, base string, m Message) (Outcome, error) {
	if len(m.Text) > MaxText {
		return Outcome{}, fmt.Errorf("the message is %d bytes; at most %d", len(m.Text), MaxText)
	}
	for _, o := range List(ctx, base) {
		if o.ID != m.To {
			continue
		}
		ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		for _, r := range records(base) {
			if r.PID != o.PID {
				continue
			}
			resp, err := call(ctx, r.Socket, request{Op: "send", Message: &m})
			if err != nil {
				return Outcome{}, err
			}
			if resp.Outcome == nil {
				return Outcome{}, errors.New("no answer")
			}
			return *resp.Outcome, nil
		}
	}
	return Outcome{}, ErrNotOpen
}
