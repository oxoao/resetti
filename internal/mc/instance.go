package mc

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/woofdoggo/resetti/internal/cfg"
	"github.com/woofdoggo/resetti/internal/x11"
)

// TODO: Pre 1.14 support
// TODO: Process monitoring, handle instance death/restart
// TODO: Warmup (click, stretch, reset, etc)

// An instance contains all of the relevant information for an instance, such
// as its game directory and current state.
type instance struct {
	info   InstanceInfo
	reader stateReader
	state  State
}

// A Manager controls several Minecraft instances. It keeps track of each
// instance's state and communicates with a frontend to operate on the
// instances for the user.
type Manager struct {
	// The mutex is only needed to guard access to the active instance ID and
	// instance states.
	mu sync.Mutex

	active    int               // Active instance ID. -1 signals no active instance.
	instances []instance        // List of instances
	paths     map[string]int    // State file -> instance ID mapping
	watcher   *fsnotify.Watcher // State file watcher

	conf *cfg.Profile
	x    *x11.Client
}

// NewManager attempts to create a new Manager for the given instances.
func NewManager(infos []InstanceInfo, conf *cfg.Profile, x *x11.Client) (*Manager, error) {
	// Create instances.
	instances := make([]instance, 0, len(infos))
	for idx, info := range infos {
		var inst instance
		// TODO: Better state detection heuristic (WorldPreview jar version?)
		// TODO: Move out into separate function (for bench util)
		_, err := os.Stat(inst.info.Dir + "/wpstateout.txt")
		if err == nil {
			reader, state, err := newWpstateReader(info)
			if err != nil {
				return nil, fmt.Errorf("create wpstateReader %d: %w", idx, err)
			}
			inst = instance{info, &reader, state}
		} else if os.IsNotExist(err) {
			reader, state, err := newLogReader(info)
			if err != nil {
				return nil, fmt.Errorf("create logReader %d: %w", idx, err)
			}
			inst = instance{info, &reader, state}
		} else {
			return nil, fmt.Errorf("stat %d/wpstateout.txt: %w", idx, err)
		}
		instances = append(instances, inst)
	}

	// Setup state watcher.
	watcher, err := fsnotify.NewWatcher()
	paths := make(map[string]int)
	if err != nil {
		return nil, fmt.Errorf("open watcher: %w", err)
	}
	for idx, inst := range instances {
		path := inst.reader.Path()
		paths[path] = idx
		if err := watcher.Add(path); err != nil {
			_ = watcher.Close()
			return nil, fmt.Errorf("watch instance %d: %w", idx, err)
		}
	}
	m := Manager{
		sync.Mutex{},
		-1,
		instances,
		paths,
		watcher,
		conf,
		x,
	}
	return &m, nil
}

// Run starts managing instances in the background. Any non-fatal errors are
// logged, any fatal errors are returned via the provided error channel.
func (m *Manager) Run(ctx context.Context, evtch chan<- Update, errch chan<- error) {
	defer func() {
		_ = m.watcher.Close()
	}()
	for {
		select {
		case <-ctx.Done():
			log.Println("Manager: context cancelled")
			return
		case evt, ok := <-m.watcher.Events:
			if !ok {
				errch <- errors.New("watcher events closed")
				return
			}
			id := m.paths[evt.Name]
			switch evt.Op {
			case fsnotify.Write:
				// Process any updates to the state file.
				state, updated, err := m.instances[id].reader.Process()
				if err != nil {
					log.Printf("process log (%d) failed: %s", id, err)
					continue
				}
				if !updated {
					continue
				}

				// Only modify the fields that the state reader knows about.
				m.mu.Lock()
				lastType := m.instances[id].state.Type
				m.instances[id].state.Type = state.Type
				m.instances[id].state.Progress = state.Progress
				m.instances[id].state.Menu = state.Menu

				// The stWorld state should only ever be handled internally.
				// Update it to the appropriate public state before notifying
				// the frontend.
				switch state.Type {
				case stWorld:
					if m.active == id {
						m.instances[id].state.Type = StIngame
					} else {
						m.instances[id].state.Type = StIdle
						m.sendKeyDown(id, x11.KeyF3)
						m.sendKeyPress(id, x11.KeyEsc)
						m.sendKeyUp(id, x11.KeyF3)
					}
				case StPreview:
					if lastType != StPreview {
						m.instances[id].state.LastPreview = time.Now()
					}
				}
				evtch <- Update{m.instances[id].state, id}
				m.mu.Unlock()
			default:
				err := m.instances[id].reader.ProcessEvent(evt.Op)
				if err != nil {
					errch <- fmt.Errorf("process event (%d) failed: %w", id, err)
					return
				}
			}
		case err, ok := <-m.watcher.Errors:
			if !ok {
				errch <- fmt.Errorf("watcher died: %w", err)
			}
			log.Printf("Manager: watcher error: %s\n", err)
		}
	}
}

// Focus attempts to focus the window of the given instance. Any errors will
// be logged.
func (m *Manager) Focus(id int) {
	if err := m.x.FocusWindow(m.instances[id].info.Wid); err != nil {
		log.Printf("Focus %d failed: %s\n", id, err)
	}
}

// Play attempts to play the given instance.
//
// If there is a currently active instance, the given instance will supersede it.
// Any additional actions which should happen before playing (e.g. stretching,
// unpausing, F1) will be handled by this function. Any errors will be logged.
func (m *Manager) Play(id int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.active = id
	m.instances[id].state.Type = StIngame

	if m.conf.UnpauseFocus {
		m.sendKeyPress(id, x11.KeyEsc)
	}
	if m.conf.Wall.Enabled {
		m.setResolution(id, m.conf.Wall.UnstretchRes)
		if m.conf.UnpauseFocus && m.conf.Wall.UseF1 {
			m.sendKeyPress(id, x11.KeyF1)
		}

		// Wait for the instance to unstretch, then pause and unpause again
		// to update the cursor position for the next time a menu is opened.
		// TODO: Make sure GLFW doesn't discard the extra escape presses.
		m.sendKeyPress(id, x11.KeyEsc)
		m.sendKeyPress(id, x11.KeyEsc)
	}
	m.Focus(id)
}

// Reset attempts to reset the given instance. The return value will indicate
// whether or not the instance was in a legal state for resetting. If an actual
// error occurs, it will be logged.
func (m *Manager) Reset(id int) bool {
	// Check if the reset can occur.
	m.mu.Lock()
	defer m.mu.Unlock()
	state := m.instances[id].state
	if state.Type == StDirt {
		return false
	}
	if state.Type == StPreview && state.Progress > 85 {
		return false
	}
	if m.conf.Wall.Enabled {
		if time.Since(state.LastPreview) < time.Duration(m.conf.Wall.GracePeriod) {
			return false
		}
	}

	// Reset.
	if m.active == id {
		// TODO: Ghost pie fix
		m.active = -1
		m.setResolution(id, m.conf.Wall.StretchRes)
	}
	var key x11.Key
	if state.Type == StPreview {
		key = m.instances[id].info.PreviewKey
	} else {
		key = m.instances[id].info.ResetKey
	}
	m.sendKeyPress(id, key)
	return true
}

// sendKeyDown sends a key down event to the given instance.
func (m *Manager) sendKeyDown(id int, key x11.Key) {
	// XXX: 20 ms offset to block out any user inputs at the same time.
	// Refer to (*x11.Client).SendKeyDown for more information.
	m.x.SendKeyDown(key.Code, m.instances[id].info.Wid, m.x.GetCurrentTime()+20)
}

// sendKeyPress sends a key down and key up event to the given instance.
func (m *Manager) sendKeyPress(id int, key x11.Key) {
	// XXX: 20 ms offset to block out any user inputs at the same time.
	// Refer to (*x11.Client).SendKeyDown for more information.
	m.x.SendKeyPress(key.Code, m.instances[id].info.Wid, m.x.GetCurrentTime()+20)
}

// sendKeyUp sends a key up event to the given instance.
func (m *Manager) sendKeyUp(id int, key x11.Key) {
	// XXX: 20 ms offset to block out any user inputs at the same time.
	// Refer to (*x11.Client).SendKeyDown for more information.
	m.x.SendKeyUp(key.Code, m.instances[id].info.Wid, m.x.GetCurrentTime()+20)
}

// setResolution sets the window geometry of an instance.
func (m *Manager) setResolution(id int, rect *cfg.Rectangle) {
	err := m.x.MoveWindow(
		m.instances[id].info.Wid,
		rect.X, rect.Y, rect.W, rect.H,
	)
	if err != nil {
		log.Printf("setResolution %d failed: %s\n", id, err)
	}
}
