package main

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"
)

// Discord's local RPC: a socket (a named pipe on Windows) the desktop app
// listens on, carrying frames of a little-endian opcode and length, then
// JSON. The client says hello with its application id, then sets its
// activity.

const (
	opHandshake = 0
	opFrame     = 1
	opClose     = 2
	opPing      = 3
	opPong      = 4
)

// socketPaths are where the Discord app may listen, in order.
func socketPaths() []string {
	if runtime.GOOS == "windows" {
		var out []string
		for i := range 10 {
			out = append(out, fmt.Sprintf(`\\.\pipe\discord-ipc-%d`, i))
		}
		return out
	}
	var dirs []string
	for _, env := range []string{"XDG_RUNTIME_DIR", "TMPDIR", "TMP", "TEMP"} {
		if d := os.Getenv(env); d != "" {
			dirs = append(dirs, d,
				filepath.Join(d, "app", "com.discordapp.Discord"), // flatpak
				filepath.Join(d, "snap.discord"))                  // snap
		}
	}
	dirs = append(dirs, "/tmp")
	var out []string
	for i := range 10 {
		for _, d := range dirs {
			out = append(out, filepath.Join(d, fmt.Sprintf("discord-ipc-%d", i)))
		}
	}
	return out
}

func dial(path string) (io.ReadWriteCloser, error) {
	if runtime.GOOS == "windows" {
		return os.OpenFile(path, os.O_RDWR, 0)
	}
	return net.DialTimeout("unix", path, 2*time.Second)
}

// ipc is one connection to the Discord app.
type ipc struct {
	rw   io.ReadWriteCloser
	wmu  sync.Mutex
	done chan struct{} // closed when Discord closes it or a read fails
}

// connect finds the Discord app and says hello as clientID; it returns
// once Discord is ready.
func connect(clientID string) (*ipc, error) {
	var lastErr error = errors.New("Discord is not running")
	for _, p := range socketPaths() {
		if runtime.GOOS != "windows" {
			if _, err := os.Stat(p); err != nil {
				continue
			}
		}
		rw, err := dial(p)
		if err != nil {
			lastErr = err
			continue
		}
		c := &ipc{rw: rw, done: make(chan struct{})}
		if err := c.handshake(clientID); err != nil {
			rw.Close()
			lastErr = err
			continue
		}
		go c.readLoop()
		return c, nil
	}
	return nil, lastErr
}

func (c *ipc) write(op uint32, v any) error {
	payload, err := json.Marshal(v)
	if err != nil {
		return err
	}
	frame := make([]byte, 8+len(payload))
	binary.LittleEndian.PutUint32(frame[0:4], op)
	binary.LittleEndian.PutUint32(frame[4:8], uint32(len(payload)))
	copy(frame[8:], payload)
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_, err = c.rw.Write(frame)
	return err
}

func (c *ipc) read() (uint32, []byte, error) {
	var head [8]byte
	if _, err := io.ReadFull(c.rw, head[:]); err != nil {
		return 0, nil, err
	}
	n := binary.LittleEndian.Uint32(head[4:8])
	if n > 1<<20 {
		return 0, nil, fmt.Errorf("frame of %d bytes", n)
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(c.rw, body); err != nil {
		return 0, nil, err
	}
	return binary.LittleEndian.Uint32(head[0:4]), body, nil
}

func (c *ipc) handshake(clientID string) error {
	if err := c.write(opHandshake, map[string]any{"v": 1, "client_id": clientID}); err != nil {
		return err
	}
	op, body, err := c.read()
	if err != nil {
		return err
	}
	var m struct {
		Cmd  string `json:"cmd"`
		Evt  string `json:"evt"`
		Data struct {
			Message string `json:"message"`
		} `json:"data"`
		Message string `json:"message"`
	}
	_ = json.Unmarshal(body, &m)
	if op == opClose {
		msg := m.Message
		if msg == "" {
			msg = string(body)
		}
		return fmt.Errorf("Discord refused: %s", msg) // a wrong client id, say
	}
	if m.Evt != "READY" {
		return fmt.Errorf("Discord answered %s", body)
	}
	return nil
}

// readLoop answers pings and notices when Discord goes away; it drops the
// rest (the answers to SET_ACTIVITY).
func (c *ipc) readLoop() {
	defer close(c.done)
	for {
		op, body, err := c.read()
		if err != nil {
			return
		}
		switch op {
		case opPing:
			_ = c.write(opPong, json.RawMessage(body))
		case opClose:
			return
		}
	}
}

var nonce uint64

// setActivity shows a (nil clears it).
func (c *ipc) setActivity(a *activity) error {
	nonce++
	args := map[string]any{"pid": os.Getpid()}
	if a != nil {
		args["activity"] = a
	}
	return c.write(opFrame, map[string]any{"cmd": "SET_ACTIVITY", "args": args, "nonce": fmt.Sprint(nonce)})
}

func (c *ipc) alive() bool {
	select {
	case <-c.done:
		return false
	default:
		return true
	}
}

func (c *ipc) close() {
	_ = c.write(opClose, map[string]any{})
	_ = c.rw.Close()
}
