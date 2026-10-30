package main

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// Checking a package source for a newer release (docs/firmware-upgrades.md, "Package
// sources"). It only asks GitHub which release is the latest and works out the link a
// newer file would have. Nothing is downloaded or registered here: the administrator
// registers the link through the normal path, so the new file gets the same review and
// SHA-256 confirmation as any other. It runs only when an administrator presses the button.

type packageUpdateCheck struct {
	Status string `json:"status"` // "up_to_date" or "newer"
	Tag    string `json:"tag"`
	URL    string `json:"url,omitempty"`
}

var errNotGitHubRelease = errors.New("only a package registered from a GitHub release link can be checked; upload the new file instead")

// releaseParts splits a remembered origin into owner/repo, tag and file name.
func releaseParts(origin string) (repo, tag, file string, err error) {
	path, _, perr := parsePackageURL(origin)
	if perr != nil {
		return "", "", "", errNotGitHubRelease
	}
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/") // owner, repo, releases, download, tag, file
	return parts[0] + "/" + parts[1], parts[4], parts[5], nil
}

// newerReleaseFile names the file a newer release would carry: asset names usually embed
// the tag, so the old tag (with or without a leading v) is replaced by the new one.
func newerReleaseFile(file, oldTag, newTag string) string {
	oldBare, newBare := strings.TrimPrefix(oldTag, "v"), strings.TrimPrefix(newTag, "v")
	if strings.Contains(file, oldTag) {
		return strings.ReplaceAll(file, oldTag, newTag)
	}
	if oldBare != "" && strings.Contains(file, oldBare) {
		return strings.ReplaceAll(file, oldBare, newBare)
	}
	return file
}

// latestReleaseTag follows GitHub's /releases/latest redirect one step, without leaving
// github.com, and returns the tag it points to.
func latestReleaseTag(ctx context.Context, repo string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, packageFetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, githubBase+repo+"/releases/latest", nil)
	if err != nil {
		return "", errors.New("could not check for a newer release")
	}
	req.Header.Set("User-Agent", "OpenManage")
	client := *packageFetchClient
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		return "", errors.New("could not reach GitHub to check for a newer release")
	}
	defer resp.Body.Close()
	base, _ := url.Parse(githubBase)
	loc, err := base.Parse(resp.Header.Get("Location"))
	if err != nil || resp.StatusCode < 300 || resp.StatusCode > 399 || !packageRedirectAllowed(loc) {
		return "", errors.New("GitHub did not report a latest release for this repository")
	}
	const marker = "/releases/tag/"
	i := strings.Index(loc.Path, marker)
	if i < 0 || !strings.HasPrefix(loc.Path, "/"+repo+marker) {
		return "", errors.New("GitHub did not report a latest release for this repository")
	}
	tag, err := url.PathUnescape(loc.Path[i+len(marker):])
	if err != nil || tag == "" || strings.Contains(tag, "/") {
		return "", errors.New("GitHub reported a release tag that cannot be used")
	}
	return tag, nil
}

// checkPackageSourceUpdate answers whether a registered GitHub-release source has a newer release.
func (a *app) checkPackageSourceUpdate(w http.ResponseWriter, r *http.Request) {
	if !requireAnyRole(w, r, "admin") {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "invalid package source")
		return
	}
	var origin, status string
	err = a.db.QueryRow(r.Context(), `SELECT origin, status FROM package_sources WHERE id=$1`, id).Scan(&origin, &status)
	if err != nil {
		writeError(w, http.StatusNotFound, "package source not found")
		return
	}
	if status != "confirmed" {
		writeError(w, http.StatusConflict, "confirm this package source before checking it for updates")
		return
	}
	repo, tag, file, err := releaseParts(origin)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	latest, err := latestReleaseTag(r.Context(), repo)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	if latest == tag {
		writeJSON(w, http.StatusOK, packageUpdateCheck{Status: "up_to_date", Tag: latest})
		return
	}
	link := githubBase + repo + "/releases/download/" + latest + "/" + newerReleaseFile(file, tag, latest)
	if _, _, err := parsePackageURL(link); err != nil {
		writeError(w, http.StatusUnprocessableEntity, "the newer release's file name cannot be worked out; paste its link instead")
		return
	}
	writeJSON(w, http.StatusOK, packageUpdateCheck{Status: "newer", Tag: latest, URL: link})
}
