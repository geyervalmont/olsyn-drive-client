// Command winres writes the Windows resources of olsyn-drive.exe (icon,
// manifest and version information) as .syso objects next to the main
// package, where go build picks them up for GOOS=windows:
//
//	go run ./ui/winres -version v1.2.3 -out cmd/olsyn-drive
//
// It needs no cgo, no Windows and no third-party module. The Makefile runs it
// before every Windows build so the version is current.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/geyervalmont/olsyn-drive-client/ui/internal/tray"
	"github.com/geyervalmont/olsyn-drive-client/ui/internal/winres"
)

// iconSizes are the images in the icon: Explorer and the taskbar pick the
// nearest for the display's scaling.
var iconSizes = []int{16, 20, 24, 32, 40, 48, 64, 128, 256}

func main() {
	version := flag.String("version", "dev", "version string shown in the file's details")
	archs := flag.String("arch", "amd64,arm64", "comma-separated GOARCH values")
	out := flag.String("out", "cmd/olsyn-drive", "folder for rsrc_windows_<arch>.syso")
	ico := flag.String("ico", "", "also write the icon as a .ico file here")
	flag.Parse()
	if err := run(*version, strings.Split(*archs, ","), *out, *ico); err != nil {
		fmt.Fprintln(os.Stderr, "winres:", err)
		os.Exit(1)
	}
}

func run(version string, archs []string, outDir, icoPath string) error {
	var images []winres.IconImage
	for _, size := range iconSizes {
		png, err := tray.AppIcon(size)
		if err != nil {
			return err
		}
		images = append(images, winres.IconImage{PNG: png})
	}
	icon, err := winres.Icon(images)
	if err != nil {
		return err
	}
	resources := append(icon, winres.ManifestResource(), winres.VersionInfo{
		Version:     version,
		ProductName: "Olsyn Drive",
		Description: "Olsyn Drive",
		Company:     "Olsyn",
		Copyright:   "Copyright Olsyn. Licensed under the Apache License 2.0.",
		FileName:    "olsyn-drive.exe",
	}.Resource())
	for _, arch := range archs {
		m, err := winres.MachineFor(strings.TrimSpace(arch))
		if err != nil {
			return err
		}
		obj, err := winres.Object(m, resources)
		if err != nil {
			return fmt.Errorf("%s: %w", arch, err)
		}
		path := filepath.Join(outDir, "rsrc_windows_"+strings.TrimSpace(arch)+".syso")
		if err := os.WriteFile(path, obj, 0o644); err != nil {
			return fmt.Errorf("write %s: %w", path, err)
		}
	}
	if icoPath != "" {
		data, err := winres.ICO(images)
		if err != nil {
			return err
		}
		if err := os.WriteFile(icoPath, data, 0o644); err != nil {
			return fmt.Errorf("write %s: %w", icoPath, err)
		}
	}
	return nil
}
