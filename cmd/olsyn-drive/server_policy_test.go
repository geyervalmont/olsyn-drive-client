package main

import (
	"testing"

	"github.com/geyervalmont/olsyn-drive-client/internal/settings"
)

func TestServerPolicy(t *testing.T) {
	t.Setenv(serverEnv, "")
	if devBuild {
		got, err := chooseServer("http://127.0.0.1:8080/")
		if err != nil || got != "http://127.0.0.1:8080" {
			t.Fatalf("dev build override: %q %v", got, err)
		}
		if err := allowServer("http://127.0.0.1:8080"); err != nil {
			t.Fatal(err)
		}
		return
	}
	got, err := chooseServer("")
	if err != nil || got != settings.DefaultServerURL {
		t.Fatalf("release default: %q %v", got, err)
	}
	if _, err := chooseServer("http://127.0.0.1:8080"); err == nil {
		t.Fatal("a release build accepted --server")
	}
	t.Setenv(serverEnv, "https://drive.example.org")
	if _, err := chooseServer(""); err == nil {
		t.Fatal("a release build accepted " + serverEnv)
	}
	if err := allowServer("https://drive.example.org"); err == nil {
		t.Fatal("a release build allowed another server")
	}
	if err := allowServer(settings.DefaultServerURL + "/"); err != nil {
		t.Fatal(err)
	}
}
