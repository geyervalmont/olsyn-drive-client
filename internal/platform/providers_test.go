package platform

import (
	"slices"
	"testing"
)

func TestOtherProvidersIn(t *testing.T) {
	cases := map[string][]string{
		"":                                  nil,
		"RDPNP,LanmanWorkstation,webclient": nil,
		"RDPNP,LanmanWorkstation,Dokan2,WinFsp.Np": {"dokany"},
		" dokan1 , dokan2":                         {"dokany"},
	}
	for in, want := range cases {
		if got := OtherProvidersIn(in); !slices.Equal(got, want) {
			t.Errorf("OtherProvidersIn(%q) = %v, want %v", in, got, want)
		}
	}
}
