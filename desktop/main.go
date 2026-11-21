package main

import (
	"embed"
	"io/fs"
	"log"
	"os"

	"github.com/egoist/mygo"
)

//go:embed all:frontend
var frontend embed.FS

//go:embed winres/icon.png
var appIcon []byte

func main() {
	web, err := fs.Sub(frontend, "frontend")
	if err != nil {
		log.Fatal(err)
	}
	mygo.SetFrontend(web)
	openStore()
	startAlertUI()
	if err := mygo.Protocol.Handle("ancapi", newAncAPIHandler()); err != nil {
		log.Printf("register ancapi protocol: %v", err)
	}
	windowURL := "/"
	if devURL := os.Getenv("MYGO_DEV_URL"); devURL != "" {
		windowURL = devURL
	}
	mygo.App.WhenReady(func() {
		win := mygo.NewWindow(mygo.WindowOptions{
			Title:     "CSN ANC · 航班与 ATIS",
			URL:       windowURL,
			Width:     1360,
			Height:    720,
			MinWidth:  1080,
			MinHeight: 640,
		})
		if err := win.SetIcon(appIcon); err != nil {
			log.Printf("set window icon: %v", err)
		}
	})
	if err := mygo.App.Run(); err != nil {
		log.Fatal(err)
	}
}
