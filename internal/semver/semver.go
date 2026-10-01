// Package semver parses and compares semantic versions (SemVer 2.0.0).
//
// The update checker orders releases with it: prereleases order before the
// release they precede (0.1.0-alpha.2 < 0.1.0-rc.1 < 0.1.0) and build
// metadata after "+" is ignored. A leading "v" is accepted.
package semver

import (
	"fmt"
	"strconv"
	"strings"
)

// Version is a parsed semantic version.
type Version struct {
	Major, Minor, Patch int
	// Prerelease identifiers, empty for a stable version.
	Prerelease []string
}

// Parse parses "MAJOR.MINOR.PATCH[-pre][+build]" with an optional leading "v".
func Parse(s string) (Version, error) {
	var v Version
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	if i := strings.IndexByte(s, '+'); i >= 0 {
		s = s[:i]
	}
	core, pre, hasPre := strings.Cut(s, "-")
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return v, fmt.Errorf("%q is not MAJOR.MINOR.PATCH", s)
	}
	nums := [3]int{}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || (len(p) > 1 && p[0] == '0') {
			return v, fmt.Errorf("%q has an invalid numeric component %q", s, p)
		}
		nums[i] = n
	}
	v.Major, v.Minor, v.Patch = nums[0], nums[1], nums[2]
	if hasPre {
		if pre == "" {
			return v, fmt.Errorf("%q has an empty prerelease", s)
		}
		v.Prerelease = strings.Split(pre, ".")
		for _, id := range v.Prerelease {
			if id == "" {
				return v, fmt.Errorf("%q has an empty prerelease identifier", s)
			}
		}
	}
	return v, nil
}

// IsPrerelease reports whether the version carries prerelease identifiers.
func (v Version) IsPrerelease() bool { return len(v.Prerelease) > 0 }

// String renders the version without a "v" prefix.
func (v Version) String() string {
	s := fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
	if len(v.Prerelease) > 0 {
		s += "-" + strings.Join(v.Prerelease, ".")
	}
	return s
}

// Compare returns -1, 0 or 1 as a is lower than, equal to or higher than b.
func Compare(a, b Version) int {
	for _, pair := range [][2]int{{a.Major, b.Major}, {a.Minor, b.Minor}, {a.Patch, b.Patch}} {
		if pair[0] != pair[1] {
			if pair[0] < pair[1] {
				return -1
			}
			return 1
		}
	}
	switch {
	case len(a.Prerelease) == 0 && len(b.Prerelease) == 0:
		return 0
	case len(a.Prerelease) == 0:
		return 1
	case len(b.Prerelease) == 0:
		return -1
	}
	for i := 0; i < len(a.Prerelease) && i < len(b.Prerelease); i++ {
		if c := compareIdent(a.Prerelease[i], b.Prerelease[i]); c != 0 {
			return c
		}
	}
	switch {
	case len(a.Prerelease) < len(b.Prerelease):
		return -1
	case len(a.Prerelease) > len(b.Prerelease):
		return 1
	}
	return 0
}

// CompareStrings parses both inputs and compares them.
func CompareStrings(a, b string) (int, error) {
	va, err := Parse(a)
	if err != nil {
		return 0, err
	}
	vb, err := Parse(b)
	if err != nil {
		return 0, err
	}
	return Compare(va, vb), nil
}

func compareIdent(a, b string) int {
	na, errA := strconv.Atoi(a)
	nb, errB := strconv.Atoi(b)
	switch {
	case errA == nil && errB == nil:
		if na < nb {
			return -1
		} else if na > nb {
			return 1
		}
		return 0
	case errA == nil:
		return -1 // numeric identifiers order before alphanumeric ones
	case errB == nil:
		return 1
	}
	return strings.Compare(a, b)
}
