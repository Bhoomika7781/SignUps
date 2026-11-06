package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// child is one `kvdb serve` process.
type child struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stderr bytes.Buffer

	writeMu sync.Mutex // one command (incl. whole batch body) is written at a time

	mu      sync.Mutex
	waiters map[string]chan string // op id -> response line
	dead    bool

	ready chan string
	done  chan struct{} // closed when stdout reaches EOF (process gone)
}

var errChildDead = errors.New("serve process is dead")

func startServe(kvdb, db string, maxBytes int64) (*child, error) {
	args := []string{"serve", db}
	if maxBytes > 0 {
		args = append(args, "--max-bytes", fmt.Sprint(maxBytes))
	}
	c := &child{
		cmd:     exec.Command(kvdb, args...),
		waiters: map[string]chan string{},
		ready:   make(chan string, 1),
		done:    make(chan struct{}),
	}
	c.cmd.Stderr = &c.stderr
	var err error
	if c.stdin, err = c.cmd.StdinPipe(); err != nil {
		return nil, err
	}
	stdout, err := c.cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := c.cmd.Start(); err != nil {
		return nil, err
	}
	go c.readLoop(stdout)
	return c, nil
}

func (c *child) readLoop(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 8<<20)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "READY") {
			select {
			case c.ready <- line:
			default:
			}
			continue
		}
		// "ACK <id>" or "ERR <id> <msg>"
		fields := strings.SplitN(line, " ", 3)
		if len(fields) < 2 {
			continue
		}
		c.mu.Lock()
		ch, ok := c.waiters[fields[1]]
		delete(c.waiters, fields[1])
		c.mu.Unlock()
		if ok {
			ch <- line
		}
	}
	c.mu.Lock()
	c.dead = true
	for id, ch := range c.waiters {
		close(ch) // no response will ever come: the op is "in flight"
		delete(c.waiters, id)
	}
	c.mu.Unlock()
	close(c.done)
}

func (c *child) waitReady(timeout time.Duration) (string, error) {
	select {
	case line := <-c.ready:
		return line, nil
	case <-c.done:
		return "", fmt.Errorf("serve exited before READY: %s", strings.TrimSpace(c.stderr.String()))
	case <-time.After(timeout):
		return "", errors.New("serve did not print READY in time")
	}
}

// send writes one command and returns a channel that yields the response
// line, or is closed without a value if the process dies first.
func (c *child) send(id, text string) (<-chan string, error) {
	ch := make(chan string, 1)
	c.mu.Lock()
	if c.dead {
		c.mu.Unlock()
		return nil, errChildDead
	}
	c.waiters[id] = ch
	c.mu.Unlock()

	c.writeMu.Lock()
	_, err := io.WriteString(c.stdin, text)
	c.writeMu.Unlock()
	if err != nil {
		// The bytes may or may not have reached the server. The waiter stays
		// registered; it is closed when the process is gone.
		return ch, nil
	}
	return ch, nil
}

// kill sends SIGKILL (TerminateProcess on Windows) and reaps the process.
func (c *child) kill() {
	_ = c.cmd.Process.Kill()
	// Drain stdout first: ACK lines the process wrote before dying are still
	// in the pipe, and each of them is a promise we must record.
	<-c.done
	_ = c.cmd.Wait()
}
