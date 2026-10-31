package main

import (
	"context"
	"errors"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
)

func (a *app) firmwarePlanForDevice(ctx context.Context, serverURL, deviceID string, manifest firmwareManifest, version string) (firmwarePlan, error) {
	plan, err := firmwarePlanFor(ctx, serverURL, manifest, version)
	if err != nil {
		return plan, err
	}
	set, err := a.loadDevicePackageSet(ctx, deviceID)
	if err != nil { return plan, err }
	if set.Current == nil {
		return plan, nil
	}
	plan.DesiredSources, err = a.resolveDesiredPackageSources(ctx, set.Current.Sources, &plan)
	if err != nil { return plan, err }
	applyDesiredPackageSet(&plan, *set.Current)
	return plan, nil
}

// Resolve latest-tracked sources once, at plan creation, and persist the selected id/version
// with the plan. A later upload cannot silently change the package chosen for this upgrade.
func (a *app) resolveDesiredPackageSources(ctx context.Context, sources []packageSetSource, plan *firmwarePlan) ([]packageSetSource, error) {
	out := make([]packageSetSource, 0, len(sources))
	for _, source := range sources {
		switch source.Mode {
		case "pinned":
			if source.SourceID == nil {
				plan.Warnings = append(plan.Warnings, "The pinned package source for "+source.Name+" has no source id.")
				out = append(out, source)
				continue
			}
			var version string
			err := a.db.QueryRow(ctx, `SELECT version FROM package_sources WHERE id=$1 AND name=$2 AND status='confirmed'`, *source.SourceID, source.Name).Scan(&version)
			if errors.Is(err, pgx.ErrNoRows) {
				plan.Warnings = append(plan.Warnings, "The pinned package source for "+source.Name+" is not currently confirmed.")
			} else if err != nil {
				return nil, err
			} else if source.Version != "" && version != source.Version {
				plan.Warnings = append(plan.Warnings, "The pinned package source version for "+source.Name+" no longer matches the desired revision.")
			} else {
				source.Version = version
			}
		case "latest":
			var id int64
			var version string
			err := a.db.QueryRow(ctx, `SELECT id, version FROM package_sources WHERE name=$1 AND status='confirmed' ORDER BY confirmed_at DESC NULLS LAST, id DESC LIMIT 1`, source.Name).Scan(&id, &version)
			if errors.Is(err, pgx.ErrNoRows) {
				plan.Warnings = append(plan.Warnings, "No confirmed package source is available for desired package "+source.Name+".")
			} else if err != nil { return nil, err
			} else {
				source.SourceID, source.Version = &id, version
			}
		default:
			plan.Warnings = append(plan.Warnings, "The desired package source tracking mode for "+source.Name+" is invalid.")
		}
		out = append(out, source)
	}
	return out, nil
}

func applyDesiredPackageSet(plan *firmwarePlan, revision packageSetRevision) {
	plan.DesiredSetRevision = revision.Revision
	plan.DesiredOfficialPackages = append([]string{}, revision.OfficialPackages...)
	plan.DesiredUnavailable = []string{}
	positive, negative := map[string]bool{}, map[string]bool{}
	for _, name := range plan.Packages {
		if strings.HasPrefix(name, "-") {
			negative[strings.TrimPrefix(name, "-")] = true
		} else {
			positive[name] = true
		}
	}
	stock := map[string]bool{}
	for _, name := range plan.stock {
		stock[name] = true
	}
	for _, name := range revision.RemovedDefaults {
		if !stock[name] {
			plan.Warnings = append(plan.Warnings, "Desired removal "+name+" is not a default package in the target release.")
			continue
		}
		delete(positive, name)
		plan.Kept = removePackageName(plan.Kept, name)
		if !negative[name] {
			negative[name] = true
			plan.RemovedDefaults = append(plan.RemovedDefaults, name)
		}
	}
	for _, name := range revision.OfficialPackages {
		if !plan.available[name] {
			plan.DesiredUnavailable = append(plan.DesiredUnavailable, name)
			continue
		}
		delete(negative, name)
		plan.RemovedDefaults = removePackageName(plan.RemovedDefaults, name)
		positive[name] = true
	}
	plan.Packages = plan.Packages[:0]
	for name := range positive {
		plan.Packages = append(plan.Packages, name)
	}
	for name := range negative {
		plan.Packages = append(plan.Packages, "-"+name)
	}
	sort.Strings(plan.Packages)
	sort.Strings(plan.RemovedDefaults)
	sort.Strings(plan.DesiredUnavailable)
	if len(plan.DesiredUnavailable) > 0 {
		plan.Warnings = append(plan.Warnings, "Some desired official packages are not available in the target release feeds and were left out of the image: "+strings.Join(plan.DesiredUnavailable, ", ")+".")
	}
}

func removePackageName(names []string, target string) []string {
	out := names[:0]
	for _, name := range names {
		if name != target {
			out = append(out, name)
		}
	}
	return out
}
