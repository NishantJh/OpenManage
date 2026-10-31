package main

import (
	"slices"
	"strings"
	"testing"
)

func TestApplyDesiredPackageSet(t *testing.T) {
	plan := firmwarePlan{
		Packages:        []string{"curl", "-ppp"},
		RemovedDefaults: []string{"ppp"},
		stock:     []string{"ppp", "dnsmasq"},
		available: map[string]bool{"curl": true, "ppp": true, "luci-app-demo": true},
		Warnings:  []string{},
	}
	revision := packageSetRevision{
		Revision: 7, OfficialPackages: []string{"luci-app-demo", "missing-feed-package", "ppp"}, RemovedDefaults: []string{"dnsmasq"},
	}
	applyDesiredPackageSet(&plan, revision)
	if plan.DesiredSetRevision != 7 || !slices.Equal(plan.DesiredOfficialPackages, revision.OfficialPackages) {
		t.Fatalf("desired revision snapshot: %+v", plan)
	}
	if !slices.Equal(plan.Packages, []string{"-dnsmasq", "curl", "luci-app-demo", "ppp"}) {
		t.Fatalf("merged package request = %v", plan.Packages)
	}
	if !slices.Equal(plan.RemovedDefaults, []string{"dnsmasq"}) {
		t.Fatalf("removed defaults = %v", plan.RemovedDefaults)
	}
	if !slices.Equal(plan.DesiredUnavailable, []string{"missing-feed-package"}) {
		t.Fatalf("unavailable desired packages = %v", plan.DesiredUnavailable)
	}
	if len(plan.Warnings) != 1 || !strings.Contains(plan.Warnings[0], "missing-feed-package") {
		t.Fatalf("warnings = %v", plan.Warnings)
	}
}

func TestApplyDesiredPackageSetOnlyRemovesTargetDefaults(t *testing.T) {
	plan := firmwarePlan{Packages: []string{"busybox"}, stock: []string{"busybox"}, available: map[string]bool{"busybox": true}}
	applyDesiredPackageSet(&plan, packageSetRevision{Revision: 2, RemovedDefaults: []string{"not-a-default"}})
	if !slices.Equal(plan.Packages, []string{"busybox"}) || len(plan.RemovedDefaults) != 0 {
		t.Fatalf("a non-default package must not be removed: %+v", plan)
	}
	if len(plan.Warnings) != 1 || !strings.Contains(plan.Warnings[0], "not-a-default") {
		t.Fatalf("warning = %v", plan.Warnings)
	}
}
