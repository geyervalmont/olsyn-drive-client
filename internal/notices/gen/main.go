// Command gen writes internal/notices/NOTICE.txt and
// ui/frontend/src/licences.generated.ts from go.mod and
// ui/frontend/package.json. Run from the repository root:
//
//	go run ./internal/notices/gen
//
// It reads licence files from the Go module cache ("go mod download -json")
// and from ui/frontend/node_modules (run npm ci first).
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// WinFsp is not linked into this program (it is the system driver, installed
// by its own installer), but its licence asks every program that uses it
// under the FLOSS exception for an attribution. License.txt, condition 2 of
// the FLOSS exception, verbatim:
//
//  2. Includes the copyright notice "WinFsp - Windows File System Proxy,
//     Copyright (C) Bill Zissimopoulos" and a link to the WinFsp repository in
//     its user-interface and any user-facing documentation.
const (
	winfspNotice = "WinFsp - Windows File System Proxy, Copyright (C) Bill Zissimopoulos"
	winfspRepo   = "https://github.com/winfsp/winfsp"
)

type component struct {
	Name, Version, Licence, URL, Text string
	Kind                              string // "go" or "npm"
}

func main() {
	root := flag.String("root", ".", "repository root")
	flag.Parse()
	if err := run(*root); err != nil {
		fmt.Fprintln(os.Stderr, "gen:", err)
		os.Exit(1)
	}
}

func run(root string) error {
	var comps []component
	goStd, err := goStdlib()
	if err != nil {
		return err
	}
	comps = append(comps, goStd)
	seen := map[string]bool{}
	for _, dir := range []string{root} {
		mods, err := goRequires(filepath.Join(dir, "go.mod"))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		got, err := goModules(dir, mods)
		if err != nil {
			return err
		}
		for _, c := range got {
			if key := c.Name; !seen[key] {
				seen[key] = true
				comps = append(comps, c)
			}
		}
	}
	npm, err := npmPackages(filepath.Join(root, "ui", "frontend"))
	if err != nil {
		return err
	}
	comps = append(comps, npm...)
	slices.SortStableFunc(comps, func(a, b component) int {
		if a.Kind != b.Kind {
			return strings.Compare(a.Kind, b.Kind)
		}
		return strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name))
	})
	for _, c := range comps {
		if !permissive(c.Licence) {
			return fmt.Errorf("%s %s has licence %q: not on the permissive list", c.Name, c.Version, c.Licence)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "internal", "notices", "NOTICE.txt"), []byte(noticeText(comps)), 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(root, "ui", "frontend", "src", "licences.generated.ts"), []byte(tsText(comps)), 0o644)
}

func permissive(id string) bool {
	switch id {
	case "MIT", "BSD-2-Clause", "BSD-3-Clause", "Apache-2.0", "ISC", "OFL-1.1", "0BSD":
		return true
	}
	return false
}

// goRequires lists "path version" pairs required by a go.mod.
func goRequires(file string) ([][2]string, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out [][2]string
	in := false
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if i := strings.Index(line, "//"); i >= 0 {
			line = strings.TrimSpace(line[:i])
		}
		switch {
		case line == "require (":
			in = true
			continue
		case in && line == ")":
			in = false
			continue
		case strings.HasPrefix(line, "require "):
			line = strings.TrimPrefix(line, "require ")
		case !in:
			continue
		}
		if f := strings.Fields(line); len(f) == 2 {
			out = append(out, [2]string{f[0], f[1]})
		}
	}
	return out, sc.Err()
}

// goModules finds each module's licence in the module cache.
func goModules(dir string, mods [][2]string) ([]component, error) {
	if len(mods) == 0 {
		return nil, nil
	}
	args := []string{"mod", "download", "-json"}
	for _, m := range mods {
		args = append(args, m[0]+"@"+m[1])
	}
	cmd := exec.Command("go", args...)
	cmd.Dir = dir
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("go mod download in %s: %w", dir, err)
	}
	var comps []component
	dec := json.NewDecoder(bytes.NewReader(out))
	for {
		var m struct{ Path, Version, Dir, Error string }
		if err := dec.Decode(&m); err == io.EOF {
			break
		} else if err != nil {
			return nil, err
		}
		if m.Error != "" {
			return nil, fmt.Errorf("%s: %s", m.Path, m.Error)
		}
		text, err := licenceFile(m.Dir)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", m.Path, err)
		}
		comps = append(comps, component{Kind: "go", Name: m.Path, Version: m.Version, Licence: identify(text), URL: moduleURL(m.Path), Text: text})
	}
	return comps, nil
}

func goStdlib() (component, error) {
	out, err := exec.Command("go", "env", "GOROOT", "GOVERSION").Output()
	if err != nil {
		return component{}, fmt.Errorf("go env: %w", err)
	}
	f := strings.Fields(string(out))
	if len(f) != 2 {
		return component{}, errors.New("go env: unexpected output")
	}
	text, err := os.ReadFile(filepath.Join(f[0], "LICENSE"))
	if errors.Is(err, os.ErrNotExist) {
		// Some packaged toolchains drop GOROOT/LICENSE; golang.org/x/sys
		// carries the same Go Authors BSD licence.
		text, err = xsysLicence()
	}
	if err != nil {
		return component{}, err
	}
	return component{Kind: "go", Name: "Go standard library and runtime", Version: strings.TrimPrefix(f[1], "go"),
		Licence: identify(string(text)), URL: "https://go.dev", Text: string(text)}, nil
}

func xsysLicence() ([]byte, error) {
	out, err := exec.Command("go", "mod", "download", "-json", "golang.org/x/sys").Output()
	if err != nil {
		return nil, fmt.Errorf("find the Go licence: %w", err)
	}
	var m struct{ Dir string }
	if err := json.Unmarshal(out, &m); err != nil {
		return nil, err
	}
	return os.ReadFile(filepath.Join(m.Dir, "LICENSE"))
}

func moduleURL(path string) string {
	switch {
	case strings.HasPrefix(path, "golang.org/x/"):
		return "https://cs.opensource.google/go/x/" + strings.TrimPrefix(path, "golang.org/x/")
	case strings.HasPrefix(path, "go.etcd.io/bbolt"):
		return "https://github.com/etcd-io/bbolt"
	}
	p := regexp.MustCompile(`/v[0-9]+$`).ReplaceAllString(path, "")
	return "https://" + p
}

var licenceNames = []string{"LICENSE", "LICENSE.txt", "LICENSE.md", "LICENCE", "LICENSE-MIT", "COPYING", "license", "license.md", "LICENSE.MIT", "OFL.txt"}

func licenceFile(dir string) (string, error) {
	for _, n := range licenceNames {
		if b, err := os.ReadFile(filepath.Join(dir, n)); err == nil {
			return string(b), nil
		}
	}
	return "", fmt.Errorf("no licence file in %s", dir)
}

// identify names a licence text by its distinctive wording.
func identify(text string) string {
	t := strings.Join(strings.Fields(strings.ToLower(text)), " ")
	switch {
	case strings.Contains(t, "apache license") && strings.Contains(t, "version 2.0"):
		return "Apache-2.0"
	case strings.Contains(t, "sil open font license"):
		return "OFL-1.1"
	case strings.Contains(t, "permission is hereby granted, free of charge"):
		return "MIT"
	case (strings.Contains(t, "permission to use, copy, modify, and/or distribute this software for any purpose with or without fee is hereby granted") ||
		strings.Contains(t, "permission to use, copy, modify, and distribute this software for any purpose with or without fee is hereby granted")) &&
		strings.Contains(t, "copyright notice and this permission notice appear in all copies"):
		return "ISC"
	case strings.Contains(t, "permission to use, copy, modify, and/or distribute this software for any purpose with or without fee is hereby granted"):
		return "0BSD"
	case strings.Contains(t, "redistribution and use in source and binary forms") && strings.Contains(t, "neither the name"):
		return "BSD-3-Clause"
	case strings.Contains(t, "redistribution and use in source and binary forms"):
		return "BSD-2-Clause"
	}
	return "unknown"
}

// npmPackages walks the production dependencies of package.json.
func npmPackages(dir string) ([]component, error) {
	type pkg struct {
		Name         string            `json:"name"`
		Version      string            `json:"version"`
		License      any               `json:"license"`
		Homepage     string            `json:"homepage"`
		Dependencies map[string]string `json:"dependencies"`
	}
	read := func(p string) (pkg, error) {
		var out pkg
		b, err := os.ReadFile(p)
		if err != nil {
			return out, err
		}
		return out, json.Unmarshal(b, &out)
	}
	top, err := read(filepath.Join(dir, "package.json"))
	if err != nil {
		return nil, err
	}
	var comps []component
	seen := map[string]bool{}
	queue := slices.Sorted(func(yield func(string) bool) {
		for k := range top.Dependencies {
			if !yield(k) {
				return
			}
		}
	})
	for len(queue) > 0 {
		name := queue[0]
		queue = queue[1:]
		if seen[name] {
			continue
		}
		seen[name] = true
		pdir := filepath.Join(dir, "node_modules", filepath.FromSlash(name))
		p, err := read(filepath.Join(pdir, "package.json"))
		if err != nil {
			return nil, fmt.Errorf("%s: %w (run npm ci in ui/frontend)", name, err)
		}
		text, err := licenceFile(pdir)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		id, _ := p.License.(string)
		if id == "" {
			id = identify(text)
		}
		url := p.Homepage
		if url == "" {
			url = "https://www.npmjs.com/package/" + name
		}
		comps = append(comps, component{Kind: "npm", Name: name, Version: p.Version, Licence: id, URL: url, Text: text})
		for d := range p.Dependencies {
			queue = append(queue, d)
		}
	}
	return comps, nil
}

func noticeText(comps []component) string {
	var b strings.Builder
	b.WriteString("Olsyn Drive: open-source notices\n")
	b.WriteString("Generated by internal/notices/gen from go.mod and ui/frontend/package.json.\n\n")
	b.WriteString("Olsyn Drive uses WinFsp on Windows (installed unmodified by its own installer, not part of this program):\n\n")
	b.WriteString("    " + winfspNotice + "\n    " + winfspRepo + "\n\n")
	b.WriteString("WinFsp is licensed under the GPLv3 with the WinFsp FLOSS exception; see " + winfspRepo + "/blob/master/License.txt\n\n")
	b.WriteString("Components linked into Olsyn Drive:\n\n")
	for _, c := range comps {
		fmt.Fprintf(&b, "  %-48s %-16s %s\n", c.Name, c.Version, c.Licence)
	}
	for _, c := range comps {
		b.WriteString("\n" + strings.Repeat("=", 78) + "\n")
		fmt.Fprintf(&b, "%s %s (%s)\n%s\n\n", c.Name, c.Version, c.Licence, c.URL)
		b.WriteString(strings.TrimRight(strings.ReplaceAll(c.Text, "\r\n", "\n"), "\n") + "\n")
	}
	return b.String()
}

func tsText(comps []component) string {
	var b strings.Builder
	b.WriteString("// Generated by internal/notices/gen; do not edit. Run: go run ./internal/notices/gen\n\n")
	b.WriteString("import type { Licence } from './licences'\n\n")
	write := func(name, kind string) {
		fmt.Fprintf(&b, "export const %s: Licence[] = [\n", name)
		for _, c := range comps {
			if c.Kind == kind {
				fmt.Fprintf(&b, "  { name: %q, version: %q, licence: %q, url: %q },\n", c.Name, c.Version, c.Licence, c.URL)
			}
		}
		b.WriteString("]\n\n")
	}
	write("goLicences", "go")
	write("frontEndLicences", "npm")
	b.WriteString("// WinFsp License.txt, FLOSS exception condition 2: the copyright notice\n")
	b.WriteString("// \"WinFsp - Windows File System Proxy, Copyright (C) Bill Zissimopoulos\" and a\n")
	b.WriteString("// link to the WinFsp repository in the user interface.\n")
	fmt.Fprintf(&b, "export const winfspNotice = %q\n", winfspNotice)
	fmt.Fprintf(&b, "export const winfspRepo = %q\n", winfspRepo)
	return b.String()
}
