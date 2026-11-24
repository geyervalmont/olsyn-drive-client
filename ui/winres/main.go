// Command winres writes the Windows resources of olsyn-drive.exe (icon,
// manifest and version information) as .syso objects next to the main
// package, where go build picks them up for GOOS=windows:
//
//	go run ./ui/winres -version v1.2.3 -out cmd/olsyn-drive
//
// With -inspect it reads a built program instead and prints what a
// false-positive report to an antivirus vendor asks for (file name, size,
// SHA-256, version) and the version details Explorer shows; it fails when a
// required detail is missing or the program asks for administrator rights:
//
//	go run ./ui/winres -inspect dist/windows-amd64/olsyn-drive.exe
//
// It needs no cgo, no Windows and no third-party module. The Makefile runs it
// before every Windows build so the version is current.
package main

import (
	"flag"
	"fmt"
	"io"
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
	inspect := flag.String("inspect", "", "print the details of this built program instead")
	flag.Parse()
	if *inspect != "" {
		if err := printProgram(os.Stdout, *inspect); err != nil {
			fmt.Fprintln(os.Stderr, "winres:", err)
			os.Exit(1)
		}
		return
	}
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
	resources := append(icon, winres.ManifestResource(), winres.OlsynDrive(version).Resource())
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

// printProgram prints a built program's facts for a false-positive report.
func printProgram(w io.Writer, path string) error {
	p, err := winres.ReadProgram(path)
	if err != nil {
		return err
	}
	v := p.FileVersion
	fmt.Fprintf(w, "File name:        %s\n", p.FileName)
	fmt.Fprintf(w, "Size:             %d bytes\n", p.Size)
	fmt.Fprintf(w, "SHA-256:          %s\n", p.SHA256)
	fmt.Fprintf(w, "Version:          %s (file version %d.%d.%d.%d)\n", p.Strings["ProductVersion"], v[0], v[1], v[2], v[3])
	for _, k := range winres.RequiredStrings {
		if k == "FileVersion" || k == "ProductVersion" {
			continue
		}
		fmt.Fprintf(w, "%-17s %s\n", k+":", p.Strings[k])
	}
	fmt.Fprintf(w, "Execution level:  %s\n", p.ExecutionLevel)
	if m := p.Missing(); len(m) > 0 {
		return fmt.Errorf("%s lacks version details: %v", p.FileName, m)
	}
	if p.ExecutionLevel != "asInvoker" {
		return fmt.Errorf("%s asks for %q; it must run as the invoking user", p.FileName, p.ExecutionLevel)
	}
	return nil
}
