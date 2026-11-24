package main

import "testing"

func TestPlanLaunch(t *testing.T) {
	same := func() bool { return true }
	differ := func() bool { return false }
	cases := []struct {
		name            string
		isInstalledCopy bool
		installed       bool
		self, inst      string
		sameFile        func() bool
		want            launchAction
	}{
		{"installed copy runs", true, true, "1.2.0", "1.2.0", same, launchHere},
		{"first run installs", false, false, "1.2.0", "", differ, launchInstall},
		{"newer upgrades", false, true, "1.3.0", "1.2.0", differ, launchUpgrade},
		{"same opens installed", false, true, "1.2.0", "1.2.0", differ, launchInstalled},
		{"older opens installed", false, true, "1.1.0", "1.2.0", differ, launchInstalled},
		{"dev build replaces a different file", false, true, "c336d8b-dirty-dev", "c336d8b-dev", differ, launchUpgrade},
		{"dev build same file opens installed", false, true, "c336d8b-dev", "c336d8b-dev", same, launchInstalled},
		{"dev over release replaces", false, true, "v1.2.0-3-gabc-dev", "1.2.0", differ, launchUpgrade},
		{"unknown installed version replaces", false, true, "1.2.0", "", differ, launchUpgrade},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := planLaunch(c.isInstalledCopy, c.installed, c.self, c.inst, c.sameFile); got != c.want {
				t.Fatalf("got %d, want %d", got, c.want)
			}
		})
	}
}
