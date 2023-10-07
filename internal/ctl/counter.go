package ctl

import (
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tesselslate/resetti/internal/cfg"
	"github.com/tesselslate/resetti/internal/log"
)

// counter keeps track of the number of resets performed and writes them to a
// file on disk.
type counter struct {
	file      *os.File
	lastWrite time.Time
	count     int
	inc       chan bool
}

// newCounter creates a new counter with the given configuration profile. If
// the user has count_resets disabled, the counter will do nothing.
func newCounter(conf *cfg.Profile) (counter, error) {
	if conf.ResetCount == "" {
		return counter{}, nil
	}

	file, err := os.OpenFile(conf.ResetCount, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return counter{}, fmt.Errorf("open file: %w", err)
	}
	buf := make([]byte, 32)
	n, err := file.Read(buf)
	if err != nil && err != io.EOF {
		_ = file.Close()
		return counter{}, fmt.Errorf("read file: %w", err)
	}
	resets := 0
	if n != 0 {
		buf = buf[:n]
		resets, err = strconv.Atoi(strings.TrimSpace(string(buf)))
		if err != nil {
			_ = file.Close()
			return counter{}, fmt.Errorf("parse reset count: %w", err)
		}
	}

	return counter{file, time.Now(), resets, make(chan bool, 64)}, nil
}

// Increment increments the reset counter.
func (c *counter) Increment() {
	if c.inc != nil {
		c.inc <- true
	}
}

// increment adds 1 to the reset count and writes it to the count file.
func (c *counter) increment() {
	c.count += 1
	if time.Since(c.lastWrite) > time.Second {
		c.write()
	}
}

// write writes the counter.
func (c *counter) write() {
	buf := []byte(strconv.Itoa(c.count))
	_, err := c.file.Seek(0, 0)
	if err != nil {
		log.Error("Reset counter: seek failed: %s", err)
		return
	}
	n, err := c.file.Write(buf)
	if err != nil {
		log.Error("Reset counter: write failed: %s", err)
	} else if n != len(buf) {
		log.Error("Reset counter: write failed: not a full write (%d/%d)", n, len(buf))
	}
	c.lastWrite = time.Now()
}

// Run starts processing resets in the background.
func (c *counter) Run(ctx context.Context, wg *sync.WaitGroup) {
	// Return immediately if this is a noop counter.
	if c.inc == nil {
		return
	}
	wg.Add(1)
	defer func() {
		c.write()
		if err := c.file.Close(); err != nil {
			log.Warn("Reset counter: close failed: %s", err)
			log.Warn("Here's your reset count! Back it up: %d", c.count)
		} else {
			log.Info("Reset counter stopped (count: %d).", c.count)
		}
		wg.Done()
	}()
	for {
		select {
		case <-ctx.Done():
			// Drain the channel of any more reset increments.
			log.Info("Reset counter: waiting for last resets...")
			time.Sleep(50 * time.Millisecond)
		outer:
			for {
				select {
				case <-c.inc:
					c.increment()
				default:
					break outer
				}
			}
			return
		case <-c.inc:
			c.increment()
		}
	}
}
