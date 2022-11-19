package cmd

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"runtime"

	"github.com/woofdoggo/resetti/internal/obs"
)

type obsSettings struct {
	instanceCount    int
	wallWidth        int
	wallHeight       int
	verificationPos  verifPos
	verificationSize int
	verification     bool
	lockImg          string
	lockWidth        int
	lockHeight       int
	obsPort          int
	obsPassword      string
}

type verifPos int

const (
	posUpleft verifPos = iota
	posLeft
	posDownleft
	posUpright
	posRight
	posDownright
)

var positions = map[string]verifPos{
	"upleft":    posUpleft,
	"left":      posLeft,
	"downleft":  posDownleft,
	"upright":   posUpright,
	"right":     posRight,
	"downright": posDownright,
}

func assert(err error) {
	if err != nil {
		_, _, line, _ := runtime.Caller(1)
		fmt.Printf("Failed (at line %d): %s\n", line, err)
		os.Exit(1)
	}
}

func obsGetFlags() (obsSettings, error) {
	res := obsSettings{}
	flag.IntVar(&res.obsPort, "obsPort", 4440, "the OBS websocket port")
	flag.StringVar(&res.obsPassword, "obsPassword", "", "the OBS websocket password, leave blank if there is none")
	flag.IntVar(&res.instanceCount, "instances", 1, "the number of instances for this scene collection")
	flag.IntVar(&res.wallWidth, "width", 1, "the width of the wall in instances")
	flag.IntVar(&res.wallHeight, "height", 1, "the height of the wall in instances")
	flag.StringVar(&res.lockImg, "lockImg", "", "the path to the lock image, leave blank to disable")
	flag.IntVar(&res.lockWidth, "lockWidth", 32, "the width of the lock image")
	flag.IntVar(&res.lockHeight, "lockHeight", 32, "the height of the lock image")

	flag.BoolVar(&res.verification, "verification", false, "whether or not to include verification instances")
	flag.IntVar(&res.verificationSize, "verifSize", 3, "the size of the verification instances")
	verifPos := flag.String("verifPos", "upleft", "the position of the verfication instances")
	flag.Parse()

	if _, ok := positions[*verifPos]; !ok {
		return res, errors.New("invalid verification instances position")
	}
	if res.verificationSize < 1 || res.verificationSize > 10 {
		return res, errors.New("invalid verification instances size")
	}
	if res.instanceCount < 0 {
		return res, errors.New("invalid instance count")
	}
	if res.wallWidth < 0 || res.wallHeight < 0 {
		return res, errors.New("invalid wall size")
	}
	if res.instanceCount > res.wallWidth*res.wallHeight {
		return res, errors.New("too many instances for wall size")
	}
	return res, nil
}

func obsPrintHelp() {
	fmt.Printf("  USAGE: resetti obs [...]\n\n")
	flag.PrintDefaults()
	fmt.Printf("\n  e.g.: resetti obs -instances=4 -width=2 -height=2\n")
}

func ObsSetup() {
	// Skip the "obs" argument when parsing flags.
	os.Args = os.Args[1:]
	settings, err := obsGetFlags()
	if len(os.Args) == 1 {
		obsPrintHelp()
		os.Exit(1)
	}
	if err != nil {
		obsPrintHelp()
		fmt.Printf("\nInvalid settings: %s\n", err)
		os.Exit(1)
	}
	client := &obs.Client{}
	_, err = client.Connect(
		context.Background(),
		fmt.Sprintf("localhost:%d", settings.obsPort),
		settings.obsPassword,
	)
	if err != nil {
		fmt.Println("OBS connection error:", err)
		os.Exit(1)
	}

	// TODO: Allow for modifying pre-existing scene collections?
	width, height, err := client.GetCanvasSize()
	assert(err)
	assert(client.CreateSceneCollection(fmt.Sprintf("resetti - %d multi", settings.instanceCount)))
	assert(client.CreateScene("Wall"))

	// Create the instance sources and scenes.
	for i := 1; i <= settings.instanceCount; i++ {
		scene := fmt.Sprintf("Instance %d", i)
		source := fmt.Sprintf("MC %d", i)
		assert(client.CreateScene(scene))
		assert(client.CreateSource(
			scene,
			source,
			"xcomposite_input",
			nil,
		))
		assert(client.SetSceneItemTransform(
			scene,
			source,
			obs.Transform{
				Width:  float64(width),
				Height: float64(height),
				Bounds: "OBS_BOUNDS_STRETCH",
			},
		))
		assert(client.SetSceneItemLocked(scene, source, true))

		// If necessary, create the verification items.
		if settings.verification {
			x, y, count := 0, 0, settings.instanceCount
			w, h := 16/settings.verificationSize, 36/settings.verificationSize
			switch settings.verificationPos {
			case posUpleft, posLeft, posDownleft:
				x = 0
			case posUpright, posRight, posDownright:
				x = width - w
			}
			switch settings.verificationPos {
			case posUpleft, posUpright:
				y = 0
			case posLeft, posRight:
				y = height/2 - (count*h)/2
			case posDownleft, posDownright:
				y = height - (count * h)
			}
			for j := 1; j <= count; j++ {
				source = fmt.Sprintf("MC %d", j)
				assert(client.AddSceneItem(scene, source))
				assert(client.SetSceneItemTransform(
					scene,
					source,
					obs.Transform{
						X:      float64(x),
						Y:      float64(y),
						Width:  float64(w),
						Height: float64(h),
						Bounds: "OBS_BOUNDS_STRETCH",
					},
				))
				assert(client.SetSceneItemLocked(scene, source, true))
				y += h
			}
		}
	}

	// Create the wall scene.
	w, h := width/settings.wallWidth, height/settings.wallHeight
	for x := 0; x < settings.wallWidth; x++ {
		for y := 0; y < settings.wallHeight; y++ {
			// Create the instance scene item.
			num := settings.wallWidth*y + x + 1
			if num > settings.instanceCount {
				// The user can have less instances than would fill the wall.
				// For example, a 4x2 wall with 7 instances is valid.
				break
			}
			source := fmt.Sprintf("MC %d", num)
			assert(client.AddSceneItem("Wall", source))
			assert(client.SetSceneItemTransform(
				"Wall",
				source,
				obs.Transform{
					X:      float64(x * w),
					Y:      float64(y * h),
					Width:  float64(w),
					Height: float64(h),
					Bounds: "OBS_BOUNDS_STRETCH",
				},
			))
			assert(client.SetSceneItemLocked("Wall", source, true))

			// Create the lock scene item.
			source = fmt.Sprintf("Lock %d", num)
			assert(client.CreateSource(
				"Wall",
				source,
				"image_source",
				obs.StringMap{"file": settings.lockImg},
			))
			assert(client.SetSceneItemTransform(
				"Wall",
				source,
				obs.Transform{
					X:      float64(x * w),
					Y:      float64(y * h),
					Width:  float64(settings.lockWidth),
					Height: float64(settings.lockHeight),
				},
			))
			assert(client.SetSceneItemLocked("Wall", source, true))
		}
	}

	// Remove the scene called "Scene" that gets created for every new scene collection.
	assert(client.DeleteScene("Scene"))
	fmt.Println("Finished!")
}