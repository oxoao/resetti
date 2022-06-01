package manager

import (
	"bufio"
	"errors"
	"github.com/woofdoggo/resetti/cfg"
	"github.com/woofdoggo/resetti/mc"
	"github.com/woofdoggo/resetti/ui"
	"github.com/woofdoggo/resetti/x11"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/jezek/xgb/xproto"
	obs "github.com/woofdoggo/go-obs"
)

var (
	ErrCannotReset error = errors.New("invalid state for resetting")
)

type WorkerError struct {
	Err error
	Id  int
}

// Worker manages a single Minecraft instance and its state.
type Worker struct {
	sync.Mutex
	stop   chan struct{}
	active sync.Mutex

	conf cfg.ResetSettings
	x    *x11.Client
	o    *obs.Client

	reader   *bufio.Reader
	watcher  *fsnotify.Watcher
	instance mc.Instance
	lastTime xproto.Timestamp
}

// Start begins running the Worker's goroutine in the background.
func (w *Worker) Start(errch chan<- WorkerError) error {
	if !w.active.TryLock() {
		return errors.New("worker already running")
	}
	w.stop = make(chan struct{})
	path := w.instance.Dir + "/logs/latest.log"
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	w.reader = bufio.NewReader(file)
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	w.watcher = watcher
	err = watcher.Add(path)
	if err != nil {
		watcher.Close()
		return err
	}
	state, _ := w.readState()
	w.setState(state)
	go w.run(errch)
	return nil
}

// Stop stops the Worker's background goroutine. This function will hang
// permanently if the Worker is not running.
func (w *Worker) Stop() {
	// Send the stop signal. Since the channel is unbuffered, it cannot be
	// immediately received again here. Then, we wait for the Worker
	// goroutine to send another value back to signal that it is finished.
	w.stop <- struct{}{}
	<-w.stop
	ui.Log("Stopped worker %d!", w.instance.Id)
}

// SetConfig sets the worker's configuration.
func (w *Worker) SetConfig(c cfg.ResetSettings) {
	w.Lock()
	w.conf = c
	w.Unlock()
}

// SetDeps provides certain objects required for the Worker to function. This
// should be called once before Start and never again.
func (w *Worker) SetDeps(i mc.Instance, x *x11.Client, o *obs.Client) {
	w.x = x
	w.o = o
	w.instance = i
}

// Focus waits for the Worker to finish its current task before focusing the
// instance's window.
func (w *Worker) Focus(time xproto.Timestamp) error {
	w.Lock()
	defer w.Unlock()
	w.lastTime = time
	err := w.x.FocusWindow(w.instance.Window)
	if err != nil {
		return err
	}
	// If the instance is ready (generated, paused), then unpause it.
	if w.instance.State == mc.StateReady {
		w.x.SendKeyPress(x11.KeyEscape, w.instance.Window, &w.lastTime)
		w.setState(mc.StateIngame)
	}
	return nil
}

// Fullscreen toggles the instance's fullscreen state.
func (w *Worker) Fullscreen(timestamp xproto.Timestamp) {
	w.Lock()
	w.lastTime = timestamp
	w.x.SendKeyPress(x11.KeyF11, w.instance.Window, &w.lastTime)
	w.Unlock()
}

// Reset waits for the Worker to finish its current task before focusing the
// instance's window.
func (w *Worker) Reset(time xproto.Timestamp) error {
	w.Lock()
	defer w.Unlock()
	if w.instance.State == mc.StateGenerating {
		return ErrCannotReset
	}
	time, err := w.instance.Reset(&w.conf, w.x, time)
	w.lastTime = time
	return err
}

// Resize will resize the window of the Worker's instance.
func (w *Worker) Resize(width, height uint32) error {
	return w.x.MoveWindow(w.instance.Window, 0, 0, width, height)
}

func (w *Worker) run(errch chan<- WorkerError) {
	for {
		select {
		case err, ok := <-w.watcher.Errors:
			if !ok {
				errch <- WorkerError{
					err,
					w.instance.Id,
				}
				w.active.Unlock()
				return
			}
			ui.LogError("File watcher error: %s", err)
		case evt, ok := <-w.watcher.Events:
			if !ok {
				errch <- WorkerError{
					errors.New("log watcher closed"),
					w.instance.Id,
				}
				w.active.Unlock()
				return
			}
			switch evt.Op {
			case fsnotify.Write:
				w.updateState()
			case fsnotify.Remove, fsnotify.Rename:
				errch <- WorkerError{
					errors.New("log file no longer available"),
					w.instance.Id,
				}
				w.watcher.Close()
				w.active.Unlock()
				return
			}
		case <-w.stop:
			// Signal to the sender that this goroutine is finished.
			w.stop <- struct{}{}
			w.watcher.Close()
			w.active.Unlock()
			return
		}
	}
}

func (w *Worker) readState() (mc.InstanceState, bool) {
	state := mc.StateUnknown
	updated := false
	for {
		lineBytes, _, err := w.reader.ReadLine()
		if err != nil {
			break
		}
		line := string(lineBytes)
		if strings.Contains(line, "CHAT") {
			continue
		}
		if strings.Contains(line, "Resetting a random seed") {
			state = mc.StateGenerating
			updated = true
		}
		if strings.Contains(line, "Saving and pausing game...") {
			state = mc.StateReady
			updated = true
		}
		if strings.Contains(line, "Starting Preview at") {
			state = mc.StatePreview
			updated = true
		}
		if strings.Contains(line, "Leaving world generation") {
			state = mc.StateGenerating
			updated = true
		}
	}
	return state, updated
}

func (w *Worker) updateState() {
	state, updated := w.readState()
	// Preliminary checks:
	// If no state updates were logged, then no action is needed.
	if !updated {
		return
	}
	// If the state did not change, then no action is needed.
	if w.instance.State == state {
		return
	}
	// If the instance is already being played on, it cannot switch
	// directly to the Ready state. This condition should only be met
	// when playing on a LAN world.
	if w.instance.State == mc.StateIngame && state == mc.StateReady {
		return
	}
	w.Lock()
	defer w.Unlock()
	w.setState(state)
	activeWin, err := w.x.GetActiveWindow()
	if err != nil {
		ui.LogError("Failed to get active window: %s", err)
		return
	}
	isPreview := w.instance.State == mc.StatePreview
	isReady := w.instance.State == mc.StateReady
	isActive := activeWin == w.instance.Window
	// If the window is currently focused and enters the Ready state, then the
	// player wants to play it and it can be switched to Ingame.
	if isActive && isReady {
		w.setState(mc.StateIngame)
		return
	}
	// If the instance is not currently focused and it has either switched
	// to the WorldPreview menu or finished generating, press F3+Esc
	// to get the transparent pause menu.
	if !isActive && (isPreview || isReady) {
		time.Sleep(time.Duration(w.conf.Delay) * time.Millisecond)
		w.x.SendKeyDown(x11.KeyF3, w.instance.Window, &w.lastTime)
		w.x.SendKeyPress(x11.KeyEscape, w.instance.Window, &w.lastTime)
		w.x.SendKeyUp(x11.KeyF3, w.instance.Window, &w.lastTime)
	}
}

func (w *Worker) setState(s mc.InstanceState) {
	w.instance.State = s
	ui.UpdateInstance(w.instance)
}
