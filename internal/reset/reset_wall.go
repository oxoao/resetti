package reset

import (
	"context"
	"fmt"
	"log"
	"sync"
	"syscall"
	"time"

	"github.com/jezek/xgb/xproto"
	go_obs "github.com/woofdoggo/go-obs"
	"github.com/woofdoggo/resetti/internal/cfg"
	"github.com/woofdoggo/resetti/internal/x11"
	"golang.org/x/sys/unix"
)

type wallState struct {
	conf        cfg.Profile
	x           *x11.Client
	obs         *go_obs.Client
	instances   []Instance
	states      []InstanceState
	lastTime    []xproto.Timestamp
	locks       []bool
	frozen      []bool
	current     int
	onWall      bool
	lastMouseId int
	projector   xproto.Window

	forceFreeze     chan int
	toFreeze        chan int
	toUnfreeze      chan int
	stateUpdates    chan<- LogUpdate
	affinityUpdates chan<- affinityUpdate
	idleAffinity    unix.CPUSet
	lowAffinity     unix.CPUSet
	highAffinity    unix.CPUSet
	activeAffinity  unix.CPUSet
}

func ResetWall(conf cfg.Profile) error {
	// Start X connection.
	var x *x11.Client
	x, err := x11.NewClient()
	if err != nil {
		return err
	}
	xEvt, xErr, err := x.Poll()
	if err != nil {
		return err
	}
	go func() {
		for err := range xErr {
			log.Printf("X err: %s", err)
		}
	}()

	// Get instances.
	instances, err := findInstances(x)
	if err != nil {
		return err
	}

	// Start OBS connection.
	obs, obsErr, err := connectObs(conf, len(instances))
	if err != nil {
		return err
	}
	go func() {
		for err := range obsErr {
			log.Printf("OBS err: %s", err)
		}
	}()

	// Set OBS sources.
	err = setSources(obs, instances)
	if err != nil {
		return err
	}

	// Find OBS projector.
	projector, err := findProjector(x)
	if err != nil {
		return err
	}

	// Get wall and screen size.
	wallWidth, wallHeight, err := getWallSize(obs, len(instances))
	if err != nil {
		return err
	}
	screenWidth, screenHeight, err := x.GetScreenSize()
	if err != nil {
		return err
	}
	instanceWidth, instanceHeight := screenWidth/wallWidth, screenHeight/wallHeight

	// Grab global keys.
	x.GrabKey(conf.Keys.Focus, x.RootWindow())
	x.GrabKey(conf.Keys.Reset, x.RootWindow())
	defer x.UngrabKey(conf.Keys.Focus, x.RootWindow())
	defer x.UngrabKey(conf.Keys.Reset, x.RootWindow())

	// Turn off any lock indicators from the last time resetti was run
	// and switch to the wall scene.
	for i := 0; i < len(instances); i++ {
		setVisible(obs, "Wall", fmt.Sprintf("Lock %d", i+1), false)
	}
	setScene(obs, "Wall")

	// Set instance affinities if using simple affinity.
	if !conf.AdvancedWall.Affinity && conf.General.Affinity != "" {
		err := setSimpleAffinity(conf, instances)
		if err != nil {
			return err
		}
	}

	// Start log readers.
	logUpdates, stopLogReaders, err := startLogReaders(instances)
	if err != nil {
		return err
	}
	defer stopLogReaders()

	// Prepare to start main loop.
	wall := wallState{
		conf:        conf,
		x:           x,
		obs:         obs,
		instances:   instances,
		states:      make([]InstanceState, len(instances)),
		lastTime:    make([]xproto.Timestamp, len(instances)),
		locks:       make([]bool, len(instances)),
		frozen:      make([]bool, len(instances)),
		current:     0,
		onWall:      true,
		lastMouseId: -1,
		projector:   projector,
		forceFreeze: make(chan int, 128),
		toFreeze:    make(chan int, 128),
		toUnfreeze:  make(chan int, 128),
	}
	if conf.AdvancedWall.Affinity {
		wall.idleAffinity = makeCpuSet(conf.AdvancedWall.CpusIdle)
		wall.lowAffinity = makeCpuSet(conf.AdvancedWall.CpusLow)
		wall.highAffinity = makeCpuSet(conf.AdvancedWall.CpusHigh)
		wall.activeAffinity = makeCpuSet(conf.AdvancedWall.CpusActive)
	}

	// Unfreeze all instances before starting.
	if conf.AdvancedWall.Freeze {
		for _, v := range instances {
			syscall.Kill(int(v.Pid), syscall.SIGCONT)
		}
	}

	// Start UI.
	display := newResetDisplay(instances)
	uiStateUpdates, uiAffinityUpdates, uiStopped, err := display.Init()
	if err != nil {
		return err
	}
	ctx, cancelUi := context.WithCancel(context.Background())
	display.Run(ctx, conf.AdvancedWall.Affinity)
	wall.stateUpdates = uiStateUpdates
	wall.affinityUpdates = uiAffinityUpdates
	defer display.Fini()
	defer cancelUi()

	// Start main loop.
	wallGrabKeys(&wall)
	x.FocusWindow(projector)
	for {
		select {
		case <-uiStopped:
			return nil
		case id := <-wall.forceFreeze:
			wallFreeze(instances[id])
			uiAffinityUpdates <- affinityUpdate{
				Id:   id,
				Cpus: unix.CPUSet{},
			}
			wall.toUnfreeze <- id
			wall.frozen[id] = true
		case id := <-wall.toFreeze:
			if wall.states[id].State == StIdle {
				wallFreeze(instances[id])
				uiAffinityUpdates <- affinityUpdate{
					Id:   id,
					Cpus: unix.CPUSet{},
				}
			}
		case update := <-logUpdates:
			// If a log reader channel was closed, something went wrong.
			if update.Done {
				log.Println("ResetWall err: log reader closed")
				return nil
			}

			// If the instance finished generating or entered the preview
			// screen, pause it.
			prev := wall.states[update.Id]
			if prev.State != update.State.State {
				if update.State.State == StPreview || update.State.State == StIdle {
					x.SendKeyDown(x11.KeyF3, instances[update.Id].Wid, &wall.lastTime[update.Id])
					x.SendKeyPress(x11.KeyEscape, instances[update.Id].Wid, &wall.lastTime[update.Id])
					x.SendKeyUp(x11.KeyF3, instances[update.Id].Wid, &wall.lastTime[update.Id])
				}
			}

			// Freeze the instance if needed.
			if conf.AdvancedWall.Freeze && update.State.State == StIdle {
				go func() {
					time.Sleep(time.Millisecond * time.Duration(conf.AdvancedWall.FreezeDelay))
					wall.toFreeze <- update.Id
				}()
			}

			// Unfreeze the instance if needed.
			if conf.AdvancedWall.ConcResets > 0 {
				if update.State.State == StIdle {
					select {
					case id := <-wall.toUnfreeze:
						wallUnfreeze(instances[id])
						wall.frozen[id] = false
					default:
					}
				}
			}

			// Update state.
			wall.states[update.Id] = update.State
			uiStateUpdates <- update

			// Update the instance's affinity state if needed.
			if !conf.AdvancedWall.Affinity {
				continue
			}
			if wall.locks[update.Id] && update.State.State != StIdle {
				wallSetAffinity(&wall, instances[update.Id], wall.highAffinity)
				continue
			}
			switch update.State.State {
			case StGenerating:
				wallSetAffinity(&wall, instances[update.Id], wall.highAffinity)
			case StPreview:
				if update.State.Progress >= conf.AdvancedWall.LowThreshold {
					wallSetAffinity(&wall, instances[update.Id], wall.lowAffinity)
				}
			case StIdle:
				wallSetAffinity(&wall, instances[update.Id], wall.idleAffinity)
			}
		case evt := <-xEvt:
			switch evt := evt.(type) {
			case x11.KeyEvent:
				if evt.State == x11.KeyDown {
					switch evt.Key {
					case conf.Keys.Focus:
						if wall.onWall {
							x.FocusWindow(projector)
						} else {
							x.FocusWindow(instances[wall.current].Wid)
						}
					case conf.Keys.Reset:
						wallHandleResetKey(&wall, evt)
					default:
						if !wall.onWall {
							continue
						}
						id := int(evt.Key.Code - 10)
						if id < 0 || id > 8 || id > len(instances) {
							continue
						}
						wallHandleEvent(&wall, id, evt.Key.Mod, evt.Time)
					}
				}
			case x11.MoveEvent:
				if evt.State&xproto.ButtonMask1 != 0 {
					x := uint16(evt.X) / instanceWidth
					y := uint16(evt.Y) / instanceHeight
					id := int((y * wallWidth) + x)
					if id >= len(instances) {
						continue
					}
					if wall.lastMouseId == id {
						continue
					}
					wall.lastMouseId = id
					wallHandleEvent(&wall, id, x11.Keymod(evt.State)^xproto.ButtonMask1, evt.Time)
				}
			case x11.ButtonEvent:
				x := uint16(evt.X) / instanceWidth
				y := uint16(evt.Y) / instanceHeight
				id := int((y * wallWidth) + x)
				if id >= len(instances) {
					continue
				}
				wall.lastMouseId = id
				wallHandleEvent(&wall, id, x11.Keymod(evt.State), evt.Time)
			}
		}
	}
}

func wallGrabKeys(w *wallState) error {
	win := w.x.RootWindow()
	for i := 0; i < len(w.instances); i++ {
		key := x11.Key{
			Code: xproto.Keycode(i + 10),
		}
		key.Mod = w.conf.Keys.WallPlay
		err := w.x.GrabKey(key, win)
		if err != nil {
			return err
		}
		key.Mod = w.conf.Keys.WallReset
		err = w.x.GrabKey(key, win)
		if err != nil {
			return err
		}
		key.Mod = w.conf.Keys.WallResetOthers
		err = w.x.GrabKey(key, win)
		if err != nil {
			return err
		}
		key.Mod = w.conf.Keys.WallLock
		err = w.x.GrabKey(key, win)
		if err != nil {
			return err
		}
	}
	if w.conf.Wall.UseMouse {
		err := w.x.GrabPointer(w.x.RootWindow())
		if err != nil {
			return err
		}
	}
	return nil
}

func wallUngrabKeys(w *wallState) error {
	win := w.x.RootWindow()
	for i := 0; i < len(w.instances); i++ {
		key := x11.Key{
			Code: xproto.Keycode(i + 10),
		}
		key.Mod = w.conf.Keys.WallPlay
		err := w.x.UngrabKey(key, win)
		if err != nil {
			return err
		}
		key.Mod = w.conf.Keys.WallReset
		err = w.x.UngrabKey(key, win)
		if err != nil {
			return err
		}
		key.Mod = w.conf.Keys.WallResetOthers
		err = w.x.UngrabKey(key, win)
		if err != nil {
			return err
		}
		key.Mod = w.conf.Keys.WallLock
		err = w.x.UngrabKey(key, win)
		if err != nil {
			return err
		}
	}
	if w.conf.Wall.UseMouse {
		err := w.x.UngrabPointer()
		if err != nil {
			return err
		}
	}
	return nil
}

func wallUpdateLastTime(w *wallState, id int, timestamp xproto.Timestamp) {
	if w.lastTime[id] < timestamp {
		w.lastTime[id] = timestamp
	}
}

func wallSetLock(w *wallState, id int, state bool) {
	if w.locks[id] == state {
		return
	}
	w.locks[id] = state
	err := setVisible(w.obs, "Wall", fmt.Sprintf("Lock %d", id+1), state)
	if err != nil {
		log.Printf("ResetWall: setLock err: %s", err)
	}
}

func wallGotoWall(w *wallState) {
	w.onWall = true
	go setScene(w.obs, "Wall")
	err := w.x.FocusWindow(w.projector)
	if err != nil {
		log.Printf("ResetWall err: goToWall: %s", err)
	}
	wallGrabKeys(w)
}

func wallPlay(w *wallState, id int, timestamp xproto.Timestamp) {
	if w.states[id].State != StIdle {
		return
	}
	wallUnfreeze(w.instances[id])
	if w.conf.AdvancedWall.Affinity {
		wallSetAffinity(w, w.instances[id], w.activeAffinity)
	}
	w.states[id].State = StIngame
	w.stateUpdates <- LogUpdate{
		Id:    id,
		State: w.states[id],
	}
	go setScene(w.obs, fmt.Sprintf("Instance %d", id+1))
	err := wallUngrabKeys(w)
	if err != nil {
		log.Printf("ResetWall err: failed ungrab wall keys: %s", err)
	}
	w.x.FocusWindow(w.instances[id].Wid)
	wallUpdateLastTime(w, id, timestamp)
	if w.conf.Reset.UnpauseFocus {
		w.x.SendKeyPress(x11.KeyEscape, w.instances[id].Wid, &w.lastTime[id])
	}
	if w.conf.Reset.ClickFocus {
		time.Sleep(time.Millisecond * time.Duration(w.conf.Reset.Delay))
		w.x.Click(w.instances[id].Wid)
	}
	if w.conf.Wall.StretchWindows {
		err := w.x.MoveWindow(
			w.instances[id].Wid,
			0, 0,
			uint32(w.conf.Wall.UnstretchWidth),
			uint32(w.conf.Wall.UnstretchHeight),
		)
		if err != nil {
			log.Printf("ResetWall err: failed to unstretch window: %s", err)
		}
	}
	wallSetLock(w, id, false)
	w.onWall = false
	w.current = id
}

func wallResetOthers(w *wallState, id int, timestamp xproto.Timestamp) {
	if w.states[id].State != StIdle {
		return
	}
	wallPlay(w, id, timestamp)
	for i := 0; i < len(w.instances); i++ {
		if i != id {
			wallResetInstance(w, i, timestamp)
		}
	}
}

func wallLock(w *wallState, id int) {
	wallSetLock(w, id, !w.locks[id])
	if w.locks[id] {
		go runHook(w.conf.Hooks.Lock)
		if w.states[id].State == StPreview {
			wallSetAffinity(w, w.instances[id], w.highAffinity)
		}
	} else {
		go runHook(w.conf.Hooks.Unlock)
	}
}

func wallHandleEvent(w *wallState, id int, state x11.Keymod, timestamp xproto.Timestamp) {
	switch state {
	case w.conf.Keys.WallPlay:
		wallPlay(w, id, timestamp)
	case w.conf.Keys.WallReset:
		wallResetInstance(w, id, timestamp)
	case w.conf.Keys.WallResetOthers:
		wallResetOthers(w, id, timestamp)
	case w.conf.Keys.WallLock:
		wallLock(w, id)
	}
}

func wallHandleResetKey(w *wallState, evt x11.KeyEvent) {
	if w.onWall {
		wg := sync.WaitGroup{}
		for i, v := range w.instances {
			if w.locks[i] || w.states[i].State == StGenerating {
				continue
			}
			wg.Add(1)
			go func(inst Instance) {
				wallResetInstance(w, inst.Id, evt.Time)
				wg.Done()
			}(v)
		}
		wg.Wait()
	} else {
		wallUpdateLastTime(w, w.current, evt.Time)
		v14_reset(w.x, w.instances[w.current], &w.lastTime[w.current])
		w.states[w.current].State = StGenerating
		if w.conf.AdvancedWall.ConcResets != 0 &&
			wallGetResettingCount(w) > w.conf.AdvancedWall.ConcResets {
			go func() {
				log.Printf("Max resets, freeze %d\n", w.current)
				time.Sleep(time.Millisecond * 500)
				w.forceFreeze <- w.current
			}()
		}
		if w.conf.Wall.StretchWindows {
			err := w.x.MoveWindow(
				w.instances[w.current].Wid,
				0, 0,
				uint32(w.conf.Wall.StretchWidth),
				uint32(w.conf.Wall.StretchHeight),
			)
			if err != nil {
				log.Printf("ResetWall err: failed to unstretch window: %s", err)
			}
		}
		go runHook(w.conf.Hooks.Reset)
		time.Sleep(time.Duration(w.conf.Reset.Delay) * time.Millisecond)
		if !w.conf.Wall.GoToLocked {
			wallGotoWall(w)
		} else {
			for idx, locked := range w.locks {
				if locked {
					if w.states[idx].State != StIdle {
						continue
					}
					wallPlay(w, idx, evt.Time)
					return
				}
			}
			wallGotoWall(w)
		}
	}
}

func wallGetResettingCount(w *wallState) int {
	resetting := 0
	for _, v := range w.states {
		if v.State == StGenerating || v.State == StPreview {
			resetting += 1
		}
	}
	return resetting
}

func wallResetInstance(w *wallState, id int, timestamp xproto.Timestamp) {
	if w.locks[id] || w.frozen[id] || w.states[id].State == StGenerating {
		return
	}
	wallUpdateLastTime(w, id, timestamp)
	wallUnfreeze(w.instances[id])
	v14_reset(w.x, w.instances[id], &w.lastTime[id])
	w.states[id].State = StGenerating
	if w.conf.AdvancedWall.ConcResets != 0 &&
		wallGetResettingCount(w) > w.conf.AdvancedWall.ConcResets {
		go func() {
			log.Printf("Max resets, freeze %d\n", id)
			time.Sleep(time.Millisecond * 500)
			w.forceFreeze <- id
		}()
	}
	go runHook(w.conf.Hooks.WallReset)
}

func wallSetAffinity(w *wallState, inst Instance, affinity unix.CPUSet) {
	w.affinityUpdates <- affinityUpdate{
		Id:   inst.Id,
		Cpus: affinity,
	}
	unix.SchedSetaffinity(int(inst.Pid), &affinity)
}

func wallFreeze(inst Instance) {
	syscall.Kill(int(inst.Pid), syscall.SIGSTOP)
}

func wallUnfreeze(inst Instance) {
	syscall.Kill(int(inst.Pid), syscall.SIGCONT)
}