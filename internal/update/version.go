package update

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Version is a semantic version: major.minor.patch with an optional pre-release.
type Version struct {
	Nums []int
	Pre  string
}

// ParseVersion parses "1.4.2", "v1.4.2" or "1.5.0-beta.1" (build metadata after + is ignored).
func ParseVersion(s string) (Version, error) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	if i := strings.IndexByte(s, '+'); i >= 0 {
		s = s[:i]
	}
	var v Version
	if i := strings.IndexByte(s, '-'); i >= 0 {
		v.Pre = s[i+1:]
		s = s[:i]
		if v.Pre == "" {
			return v, errors.New("empty pre-release")
		}
	}
	nums, err := parseDotted(s)
	if err != nil {
		return v, err
	}
	if len(nums) != 3 {
		return v, fmt.Errorf("%q is not major.minor.patch", s)
	}
	v.Nums = nums
	return v, nil
}

func parseDotted(s string) ([]int, error) {
	if s == "" {
		return nil, errors.New("empty version")
	}
	parts := strings.Split(s, ".")
	out := make([]int, len(parts))
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || (len(p) > 1 && p[0] == '0') {
			return nil, fmt.Errorf("%q is not a dotted version", s)
		}
		out[i] = n
	}
	return out, nil
}

// Compare returns -1, 0 or 1. A pre-release sorts before its release.
func Compare(a, b Version) int {
	if c := compareNums(a.Nums, b.Nums); c != 0 {
		return c
	}
	switch {
	case a.Pre == b.Pre:
		return 0
	case a.Pre == "":
		return 1
	case b.Pre == "":
		return -1
	}
	return comparePre(a.Pre, b.Pre)
}

func comparePre(a, b string) int {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) && i < len(bs); i++ {
		an, aerr := strconv.Atoi(as[i])
		bn, berr := strconv.Atoi(bs[i])
		switch {
		case aerr == nil && berr == nil:
			if an != bn {
				return cmpInt(an, bn)
			}
		case aerr == nil:
			return -1
		case berr == nil:
			return 1
		default:
			if c := strings.Compare(as[i], bs[i]); c != 0 {
				return c
			}
		}
	}
	return cmpInt(len(as), len(bs))
}

func compareNums(a, b []int) int {
	for i := 0; i < max(len(a), len(b)); i++ {
		var x, y int
		if i < len(a) {
			x = a[i]
		}
		if i < len(b) {
			y = b[i]
		}
		if x != y {
			return cmpInt(x, y)
		}
	}
	return 0
}

func cmpInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// CompareStrings compares two semantic versions given as strings.
func CompareStrings(a, b string) (int, error) {
	va, err := ParseVersion(a)
	if err != nil {
		return 0, fmt.Errorf("version %q: %w", a, err)
	}
	vb, err := ParseVersion(b)
	if err != nil {
		return 0, fmt.Errorf("version %q: %w", b, err)
	}
	return Compare(va, vb), nil
}

// DriverAtLeast reports whether an installed dotted driver version ("2.2.25089")
// meets minVersion ("2.0"). An empty installed version never does.
func DriverAtLeast(installed, minVersion string) bool {
	if minVersion == "" {
		return true
	}
	have, err := parseDotted(installed)
	if err != nil {
		return false
	}
	want, err := parseDotted(minVersion)
	if err != nil {
		return false
	}
	return compareNums(have, want) >= 0
}
