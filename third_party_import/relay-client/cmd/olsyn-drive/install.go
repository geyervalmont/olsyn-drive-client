package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// step is one thing install or uninstall does, shown before it's done.
type step struct {
	say  string
	file string // write this file …
	body string // … with this content
	cmd  []string
	rm   string
}

// installPlan is what `install --user` does on each system: start the
// drop-folder client when this person signs in to the computer, as them,
// with no administrator rights.
//
// Windows: a value under HKCU\…\Run (per user, no admin; listed under
// Task Manager > Startup apps, where the person can switch it off). The
// client re-launches itself without a console window (--background).
// macOS: a LaunchAgent. Linux: a systemd user unit.
func installPlan(goos, exe, home string, uninstall bool) []step {
	switch goos {
	case "windows":
		win := func(p ...string) string { return strings.Join(p, `\`) }
		local := os.Getenv("LOCALAPPDATA")
		if local == "" {
			local = win(home, "AppData", "Local")
		}
		dir := win(local, "Programs", "Olsyn Drive")
		target := win(dir, "olsyn-drive.exe")
		key := `HKCU\Software\Microsoft\Windows\CurrentVersion\Run`
		if uninstall {
			return []step{
				{say: "stop starting at sign-in", cmd: []string{"reg", "delete", key, "/v", "Olsyn Drive", "/f"}},
				{say: "stop the running client", cmd: []string{"taskkill", "/IM", "olsyn-drive.exe", "/F"}},
				{say: "the program in " + dir + " and your sign-in in %AppData%\\Olsyn Drive are left; delete them by hand if you want"},
			}
		}
		return []step{
			{say: "copy the program to " + target, cmd: []string{"copy-self", target}},
			{say: "start it when you sign in to Windows", cmd: []string{"reg", "add", key, "/v", "Olsyn Drive", "/t", "REG_SZ", "/d", `"` + target + `" relay --background`, "/f"}},
			{say: "start it now", cmd: []string{target, "relay", "--background"}},
		}
	case "darwin":
		plist := filepath.Join(home, "Library", "LaunchAgents", "com.olsyn.drive.plist")
		uid := fmt.Sprint(os.Getuid())
		if uninstall {
			return []step{
				{say: "stop it", cmd: []string{"launchctl", "bootout", "gui/" + uid + "/com.olsyn.drive"}},
				{say: "remove " + plist, rm: plist},
			}
		}
		logs := filepath.Join(home, "Library", "Logs", "olsyn-drive.log")
		return []step{
			{say: "write " + plist, file: plist, body: launchAgent(exe, logs)},
			{say: "start it now and at every sign-in", cmd: []string{"launchctl", "bootstrap", "gui/" + uid, plist}},
		}
	default:
		unit := filepath.Join(home, ".config", "systemd", "user", "olsyn-drive.service")
		if uninstall {
			return []step{
				{say: "stop it and stop starting it", cmd: []string{"systemctl", "--user", "disable", "--now", "olsyn-drive.service"}},
				{say: "remove " + unit, rm: unit},
			}
		}
		return []step{
			{say: "write " + unit, file: unit, body: systemdUnit(exe)},
			{say: "start it now and at every sign-in", cmd: []string{"systemctl", "--user", "enable", "--now", "olsyn-drive.service"}},
		}
	}
}

func launchAgent(exe, logs string) string {
	esc := func(s string) string {
		return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
	}
	return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>Label</key><string>com.olsyn.drive</string>
  <key>ProgramArguments</key><array><string>` + esc(exe) + `</string><string>relay</string></array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><dict><key>SuccessfulExit</key><false/></dict>
  <key>ProcessType</key><string>Background</string>
  <key>StandardErrorPath</key><string>` + esc(logs) + `</string>
</dict></plist>
`
}

func systemdUnit(exe string) string {
	return `[Unit]
Description=Olsyn Drive drop folders
After=network-online.target

[Service]
ExecStart="` + exe + `" relay
Restart=on-failure
RestartSec=30
NoNewPrivileges=yes

[Install]
WantedBy=default.target
`
}

func cmdInstall(which string, args []string, out io.Writer) error {
	fl := flag.NewFlagSet(which, flag.ContinueOnError)
	user := fl.Bool("user", false, "for this user only (the only kind there is)")
	yes := fl.Bool("yes", false, "do it (without --yes it only says what it would do)")
	if err := fl.Parse(args); err != nil {
		return err
	}
	if !*user {
		return fmt.Errorf("%s needs --user: the client runs as you, never as a system service", which)
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	exe, _ = filepath.EvalSymlinks(exe)
	home, _ := os.UserHomeDir()
	plan := installPlan(runtime.GOOS, exe, home, which == "uninstall")
	if !*yes {
		fmt.Fprintf(out, "%s would:\n", which)
	}
	for _, s := range plan {
		fmt.Fprintf(out, "  - %s\n", s.say)
		if len(s.cmd) > 0 && s.cmd[0] != "copy-self" {
			fmt.Fprintf(out, "      %s\n", strings.Join(quoteAll(s.cmd), " "))
		}
		if !*yes {
			continue
		}
		if err := do(s, exe); err != nil {
			return fmt.Errorf("%s: %w", s.say, err)
		}
	}
	if !*yes {
		fmt.Fprintf(out, "Run again with --yes to do it.\n")
	}
	return nil
}

func quoteAll(a []string) []string {
	out := make([]string, len(a))
	for i, s := range a {
		if strings.ContainsAny(s, ` "`) {
			s = `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
		}
		out[i] = s
	}
	return out
}

func do(s step, exe string) error {
	switch {
	case s.file != "":
		if err := os.MkdirAll(filepath.Dir(s.file), 0o755); err != nil {
			return err
		}
		return os.WriteFile(s.file, []byte(s.body), 0o644)
	case s.rm != "":
		err := os.Remove(s.rm)
		if os.IsNotExist(err) {
			return nil
		}
		return err
	case len(s.cmd) == 2 && s.cmd[0] == "copy-self":
		return copySelf(exe, s.cmd[1])
	case len(s.cmd) > 0:
		c := exec.Command(s.cmd[0], s.cmd[1:]...)
		c.Stdout, c.Stderr = os.Stdout, os.Stderr
		return c.Run()
	}
	return nil
}

func copySelf(from, to string) error {
	if from == to {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		return err
	}
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := to + ".new"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, to)
}
