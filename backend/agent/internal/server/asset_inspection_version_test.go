package server

import (
	"strings"
	"testing"
)

func TestInspectionDebianVersionOrder(t *testing.T) {
	const pkg = "pkg:deb/debian/libssl3?arch=amd64&distro=debian-12"
	for _, tc := range []struct {
		a, b string
		want int
	}{
		{"1:0.1-1", "99.0-1", 1},
		{"2.0~~rc1", "2.0~rc1", -1},
		{"2.0~rc10", "2.0", -1},
		{"1.0-1+deb12u9", "1.0-1+deb12u10", -1},
		{"1.0-2~bpo12+1", "1.0-2", -1},
		{"1.0-1ubuntu2.9", "1.0-1ubuntu2.10", -1},
		{"1.0a", "1.0+", -1},
		{"0:1.00", "1.0-00", 0},
		{"1-0~", "1", -1},
		{"1:2:3-1", "1:2:3-01", 0},
	} {
		for _, order := range []struct {
			a, b string
			want int
		}{{tc.a, tc.b, tc.want}, {tc.b, tc.a, -tc.want}} {
			cmp, ok := packageVersionCompare(pkg, order.a, order.b)
			if !ok || cmp < 0 != (order.want < 0) || cmp > 0 != (order.want > 0) {
				t.Errorf("compare(%q, %q) = (%d, %t), want sign %d", order.a, order.b, cmp, ok, order.want)
			}
		}
	}
	for _, v := range []string{"", "1-", " 1.0", "-1:2", "abc:1", "2147483648:1", "1.2_3", "1-2:3", strings.Repeat("9", 32)} {
		if _, ok := packageVersionCompare(pkg, v, "1.0"); ok {
			t.Errorf("accepted invalid or unsupported version %q", v)
		}
	}
	if _, ok := packageVersionCompare("pkg:generic/widget", "1.0-3", "1.0-4"); ok {
		t.Fatal("Debian ordering applied outside pkg:deb")
	}
}

func TestInspectionDebianRemediation(t *testing.T) {
	baseline := inspectionPackage{Component: "pkg:deb/debian/dpkg?arch=amd64&distro=debian-12", Advisory: "TEST-1", Version: "1:1.2-1+deb12u9", Introduced: "1:1.2-1", Fixed: "1:1.2-1+deb12u10"}
	for _, tc := range []struct{ check, version, status string }{
		{"V1", baseline.Introduced, "finding"}, {"V2", baseline.Version, "finding"},
		{"V1", baseline.Fixed, "pass"}, {"V2", "1:1.2~rc1", "pass"},
		{"V3", baseline.Version, "finding"}, {"V3", "1:1.2~rc1", "finding"},
		{"V3", baseline.Fixed, "pass"}, {"V3", "1:1.2-1+deb12u11", "pass"},
		{"V3", "unparseable", "insufficient"},
	} {
		p := baseline
		p.Version = tc.version
		e := inspectionEvidence{Source: "deployed-cyclonedx", SHA: strings.Repeat("a", 64), Bulletin: strings.Repeat("b", 64), Packages: []inspectionPackage{p}}
		var before []inspectionPackage
		if tc.check == "V3" {
			before = []inspectionPackage{baseline}
		}
		got, why, err := inspectionVerdict(tc.check, e, before)
		if err != nil || got != tc.status {
			t.Errorf("%s %s: got %s (%s), %v; want %s", tc.check, tc.version, got, why, err, tc.status)
		}
	}
	changed := baseline
	changed.Fixed = "1:1.3-1"
	e := inspectionEvidence{Source: "deployed-cyclonedx", SHA: strings.Repeat("a", 64), Bulletin: strings.Repeat("b", 64), Packages: []inspectionPackage{changed}}
	got, why, err := inspectionVerdict("V3", e, []inspectionPackage{baseline})
	if err != nil || got != "insufficient" || why != "baseline_advisory_not_comparable" {
		t.Fatalf("changed fix boundary: %s (%s), %v", got, why, err)
	}
}
