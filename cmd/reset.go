package cmd

import (
	"fmt"
	"os"
	"time"

	"github.com/woofdoggo/resetti/cfg"
	"github.com/woofdoggo/resetti/internal/logger"
	"github.com/woofdoggo/resetti/manager"
	"github.com/woofdoggo/resetti/mc"
	"github.com/woofdoggo/resetti/obs"
	"github.com/woofdoggo/resetti/ui"
	"github.com/woofdoggo/resetti/x11"
)

func CmdReset(conf *cfg.Config) int {
	if conf == nil {
		confName, err := ui.ShowProfileMenu()
		if err != nil {
			fmt.Println("Failed to open menu:", err)
			os.Exit(1)
		}
		if confName == "" {
			os.Exit(1)
		}
		profile, err := cfg.GetProfile(confName)
		if err != nil {
			fmt.Println("Failed to get profile:", err)
			os.Exit(1)
		}
		conf = profile
	}
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		fmt.Println("Failed to get log path:", err)
		os.Exit(1)
	}
	logHandle, err := os.OpenFile(cacheDir+"/resetti.log", os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0644)
	if err != nil {
		fmt.Println("Failed to open log file:", err)
		os.Exit(1)
	}
	logger.SetWriter(logHandle)
	defer logHandle.Close()
	var mgr manager.Manager
	switch conf.General.Type {
	case "standard":
		mgr = &manager.StandardManager{}
	case "wall":
		mgr = &manager.WallManager{}
	default:
		fmt.Println("Unrecognized profile type:", conf.General.Type)
		return 1
	}
	var resetHandle *os.File
	if conf.General.CountResets {
		resetHandle, err = os.OpenFile(conf.General.CountPath, os.O_RDWR|os.O_CREATE, 0644)
		if err != nil {
			fmt.Println("Failed to open reset count:", err)
			os.Exit(1)
		}
		defer resetHandle.Close()
	}
	if conf.General.Type == "wall" && !conf.Obs.Enabled {
		fmt.Println("OBS integration must be enabled for wall.")
		fmt.Println("Please update your configuration.")
		os.Exit(1)
	}
	var obserr <-chan error
	if conf.Obs.Enabled {
		errch, err := obs.Initialize(conf.Obs.Port, conf.Obs.Password)
		obserr = errch
		if err != nil {
			fmt.Println("Failed to connect to OBS:", err)
			os.Exit(1)
		}
	}
	xerr := make(chan error, 32)
	x11.Subscribe(xerr, nil)
	err = x11.Initialize()
	if err != nil {
		fmt.Println("Failed to connect to X server:", err)
		os.Exit(1)
	}
	instances, err := mc.GetInstances()
	if err != nil {
		fmt.Println("Failed to get Minecraft instances:", err)
		os.Exit(1)
	}
	uierr := make(chan error, 32)
	ui.Subscribe(uierr)
	err = ui.Init(instances)
	if err != nil {
		fmt.Println("Failed to start UI:", err)
		os.Exit(1)
	}
	mgr.SetConfig(*conf)
	mgrErrors := make(chan error)
	err = mgr.Start(instances, mgrErrors)
	if err != nil {
		fmt.Println("Failed to start manager:", err)
		os.Exit(1)
	}
	logger.Log("Started up!")
	logger.Log("Session type: %s", conf.General.Type)
	for {
		select {
		case err := <-mgrErrors:
			logger.LogError("Fatal manager error: %s", err)
			mgr.Wait()
			logger.Log("Attempting to reboot manager...")
			time.Sleep(1 * time.Second)
			instances, err := mc.GetInstances()
			if err != nil {
				logger.LogError("Failed to get Minecraft instances: %s", err)
				ui.Fini()
				x11.Close()
				return 1
			}
			err = mgr.Start(instances, mgrErrors)
			if err != nil {
				logger.LogError("Failed to restart manager: %s", err)
				ui.Fini()
				x11.Close()
				return 1
			}
		case err := <-obserr:
			logger.LogError("OBS websocket error: %s", err)
		case err := <-xerr:
			if err == x11.ErrConnectionDied {
				logger.LogError("X connection died.")
				logger.Log("Shutting down.")
				ui.Fini()
				mgr.Stop()
				return 1
			} else {
				logger.LogError("Uncaught X error: %s", err)
			}
		case err := <-uierr:
			if err != ui.ErrShutdown {
				logger.LogError("UI error: %s", err)
				logger.Log("Shutting down.")
				fmt.Printf("UI error: %s\n", err)
				mgr.Stop()
				x11.Close()
				return 1
			}
			mgr.Stop()
			x11.Close()
			return 0
		}
	}
}