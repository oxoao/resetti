// Package ctl implements the main controller used for all of the available
// resetting schemes (e.g. multi, wall.)
package ctl

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jezek/xgb/xproto"
	"github.com/woofdoggo/resetti/internal/cfg"
	"github.com/woofdoggo/resetti/internal/mc"
	"github.com/woofdoggo/resetti/internal/obs"
	"github.com/woofdoggo/resetti/internal/x11"
	"golang.org/x/exp/slices"
)

// bufferSize is the capacity a buffered channel that processes per-instance
// state should have for each instance.
const bufferSize = 16

// Hook types
const (
	HookReset int = iota
	HookLock
	HookUnlock
	HookWallPlay
	HookWallReset
)

// Controller manages all of the components necessary for resetti to run and
// handles communication between them.
type Controller struct {
	conf *cfg.Profile
	obs  *obs.Client
	x    *x11.Client

	counter  counter
	manager  *mc.Manager
	cpu      CpuManager
	frontend Frontend

	binds    map[cfg.Bind]cfg.ActionList
	inputMgr inputManager
	inputs   <-chan Input
	hooks    map[int]string

	obsErrors    <-chan error
	mgrErrors    <-chan error
	x11Errors    <-chan error
	mgrEvents    <-chan mc.Update
	signals      <-chan os.Signal
	focusChanges <-chan xproto.Window
}

// A Frontend handles user-facing I/O (input handling, instance actions, OBS
// output) and communicates with a Controller.
type Frontend interface {
	// FocusChange processes a single window focus change.
	FocusChange(xproto.Window)

	// Input processes a single user input.
	Input(Input)

	// Setup takes in all of the potentially needed dependencies and prepares
	// the Frontend to handle user input.
	Setup(frontendDependencies) error

	// Update processes a single instance state update.
	Update(mc.Update)
}

// An Input represents a single user input.
type Input struct {
	Bind cfg.Bind
	Held bool
	X, Y int
}

// frontendDependencies contains all of the dependencies that a Frontend might
// need to setup and run.
type frontendDependencies struct {
	conf      *cfg.Profile
	obs       *obs.Client
	x         *x11.Client
	states    []mc.State
	instances []mc.InstanceInfo
	host      *Controller
}

// inputManager checks the state of the user's input devices to determine if
// they are pressing any hotkeys.
type inputManager struct {
	conf *cfg.Profile
	x    *x11.Client

	lastBinds []cfg.Bind
}

// Run creates a new controller with the given configuration profile and runs it.
func Run(conf *cfg.Profile) error {
	defer log.Println("Done!")
	wg := sync.WaitGroup{}
	defer wg.Wait()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c := Controller{}
	c.conf = conf
	c.binds = make(map[cfg.Bind]cfg.ActionList)
	c.hooks = map[int]string{
		HookReset:     c.conf.Hooks.Reset,
		HookLock:      c.conf.Hooks.WallLock,
		HookUnlock:    c.conf.Hooks.WallUnlock,
		HookWallPlay:  c.conf.Hooks.WallPlay,
		HookWallReset: c.conf.Hooks.WallReset,
	}

	signals := make(chan os.Signal, 8)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM, syscall.SIGUSR1)
	c.signals = signals

	x, err := x11.NewClient()
	if err != nil {
		return fmt.Errorf("(init) create X client: %w", err)
	}
	c.x = &x

	c.obs = &obs.Client{}
	if conf.Obs.Enabled {
		obsErrors, err := c.obs.Connect(ctx, conf.Obs.Port, conf.Obs.Password)
		if err != nil {
			return fmt.Errorf("(init) create OBS client: %w", err)
		}
		c.obsErrors = obsErrors
	}

	c.counter, err = newCounter(conf)
	if err != nil {
		return fmt.Errorf("(init) create counter: %w", err)
	}

	instances, err := mc.FindInstances(&x)
	if err != nil {
		return fmt.Errorf("(init) find instances: %w", err)
	}
	c.manager, err = mc.NewManager(instances, conf, &x)
	if err != nil {
		return fmt.Errorf("(init) create manager: %w", err)
	}

	if c.conf.Wall.Enabled {
		if c.conf.Wall.Perf.Affinity != "" {
			states := c.manager.GetStates()
			c.cpu, err = NewCpuManager(instances, states, conf)
			if err != nil {
				return fmt.Errorf("(init) create cpuManager: %w", err)
			}
		}
	}

	if c.conf.Wall.Enabled {
		c.frontend = &Wall{}
	} else {
		c.frontend = &Multi{}
	}

	// Start various components
	err = c.frontend.Setup(frontendDependencies{
		conf:      c.conf,
		obs:       c.obs,
		x:         c.x,
		states:    c.manager.GetStates(),
		instances: instances,
		host:      &c,
	})
	if err != nil {
		return fmt.Errorf("(init) setup frontend: %w", err)
	}
	go c.counter.Run(ctx, &wg)
	if c.cpu != nil {
		go c.cpu.Run(ctx, &wg)
	}
	evtch := make(chan mc.Update, bufferSize*len(instances))
	errch := make(chan error, 1)
	c.mgrEvents = evtch
	c.mgrErrors = errch
	go c.manager.Run(ctx, evtch, errch)
	if c.conf.Wall.Enabled {
		c.focusChanges, c.x11Errors, err = c.x.Poll(ctx)
	}
	if err != nil {
		return fmt.Errorf("(init) X poll: %w", err)
	}
	inputs := make(chan Input, 256)
	c.inputMgr = inputManager{c.conf, c.x, nil}
	c.inputs = inputs
	go c.inputMgr.Run(inputs)

	err = c.run(ctx)
	if err != nil {
		fmt.Println("Failed to run:", err)
	}
	return nil
}

// FocusInstance switches focus to the given instance.
func (c *Controller) FocusInstance(id int) {
	c.manager.Focus(id)
}

// PlayInstance switches focus to the given instance, marks it as the active
// instance, and starts playing it.
func (c *Controller) PlayInstance(id int) {
	c.manager.Play(id)
	if c.cpu != nil {
		c.cpu.Update(mc.Update{
			State: mc.State{Type: mc.StIngame},
			Id:    id,
		})
	}
}

// ResetInstance attempts to reset the given instance and returns whether or
// not the reset was successful.
func (c *Controller) ResetInstance(id int) bool {
	ok := c.manager.Reset(id)
	if ok {
		c.counter.Increment()
	}
	return ok
}

// RunHook runs the hook of the given type if it exists.
func (c *Controller) RunHook(hook int) {
	cmdStr := c.hooks[hook]
	if cmdStr == "" {
		return
	}
	go func() {
		bin, rawArgs, ok := strings.Cut(cmdStr, " ")
		var args []string
		if ok {
			args = strings.Split(rawArgs, " ")
		}
		cmd := exec.Command(bin, args...)
		err := cmd.Run()
		if err != nil {
			log.Printf("RunHook (%d) failed: %s\n", hook, err)
		}
	}()
}

// SetPriority sets the priority of the instance in the CPU manager.
func (c *Controller) SetPriority(id int, prio bool) {
	if c.cpu != nil {
		c.cpu.SetPriority(id, prio)
	}
}

// debug prints debug information.
func (c *Controller) debug() {
	mem := runtime.MemStats{}
	runtime.ReadMemStats(&mem)
	memStats := strings.Builder{}
	memStats.WriteString(fmt.Sprintf("\nLive objects: %d\n", mem.HeapObjects))
	memStats.WriteString(fmt.Sprintf("Malloc count: %d\n", mem.Mallocs))
	memStats.WriteString(fmt.Sprintf("Total allocation: %.2f mb\n", float64(mem.TotalAlloc)/1000000))
	memStats.WriteString(fmt.Sprintf("Current allocation: %.2f mb\n", float64(mem.HeapAlloc)/1000000))
	memStats.WriteString(fmt.Sprintf("GC time: %.2f%%\n", mem.GCCPUFraction))
	memStats.WriteString(fmt.Sprintf("GC cycles: %d\n", mem.NumGC))
	memStats.WriteString(fmt.Sprintf("Total STW: %.4f ms", float64(mem.PauseTotalNs)/1000000))
	log.Printf(
		"Received SIGUSR1\n---- Debug info\nGoroutine count: %d\nMemory:%s\nInstances:\n%s",
		runtime.NumGoroutine(),
		memStats.String(),
		c.manager.Debug(),
	)
}

// run runs the main loop for the controller.
func (c *Controller) run(ctx context.Context) error {
	for {
		select {
		case sig := <-c.signals:
			switch sig {
			case syscall.SIGINT, syscall.SIGTERM:
				log.Println("Shutting down.")
				return nil
			case syscall.SIGUSR1:
				c.debug()
			}
		case err := <-c.mgrErrors:
			// All manager errors are fatal.
			return fmt.Errorf("manager: %w", err)
		case err, ok := <-c.obsErrors:
			if !ok {
				return fmt.Errorf("fatal OBS error: %w", err)
			}
			log.Printf("OBS error: %s\n", err)
		case err, ok := <-c.x11Errors:
			if !ok {
				return fmt.Errorf("fatal X error: %w", err)
			}
			log.Printf("X error: %s\n", err)
		case evt := <-c.mgrEvents:
			c.frontend.Update(evt)
			if c.cpu != nil {
				c.cpu.Update(evt)
			}
		case win := <-c.focusChanges:
			c.frontend.FocusChange(win)
		case input := <-c.inputs:
			c.frontend.Input(input)
		}
	}
}

func (i *inputManager) Run(inputs chan<- Input) {
	for {
		// Sleep for this polling iteration and query the input devices' state.
		time.Sleep(time.Second / time.Duration(i.conf.PollRate))
		keymap, err := i.x.QueryKeymap()
		if err != nil {
			log.Printf("inputManager: Query keymap failed: %s\n", err)
			continue
		}
		pointer, err := i.x.QueryPointer()
		if err != nil {
			log.Printf("inputManager: Query pointer failed: %s\n", err)
			continue
		}

		// XXX: This is kind of bad and can probably be optimized
		var pressed []cfg.Bind
		for bind := range i.conf.Keybinds {
			var mask [32]byte
			for _, key := range bind.Keys[:bind.KeyCount] {
				mask[key/8] |= (1 << (key % 8))
			}
			if keymap.HasPressed(mask) && pointer.HasPressed(bind.Buttons[:bind.ButtonCount]) {
				pressed = append(pressed, bind)
			}
		}
		if len(pressed) == 0 {
			i.lastBinds = pressed
			continue
		}

		// XXX: This is kinda jank but it works (mostly).
		// (thanks boyenn for the suggestion)
		//
		// TODO: This should probably be made better (or more config restrictions
		// should be put in place.) Right now, you can trigger essentially random
		// keybind behavior by having e.g. (A+B, B+C, C+D) and holding down all
		// of A, B, C, and D at once. This is caused by Go's non-deterministic
		// map iteration order.
		slices.SortFunc(pressed, func(a, b cfg.Bind) bool {
			if b.KeyCount < a.KeyCount {
				return true
			}
			return b.ButtonCount < a.ButtonCount
		})
		bind := pressed[0]
		inputs <- Input{
			bind,
			slices.Contains(i.lastBinds, bind),
			pointer.EventX, pointer.EventY,
		}
		i.lastBinds = pressed
	}
}
