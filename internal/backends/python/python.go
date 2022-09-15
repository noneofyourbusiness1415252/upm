// Package python provides backends for Python 2 and 3 using Poetry.
package python

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/BurntSushi/toml"
	"github.com/replit/upm/internal/api"
	"github.com/replit/upm/internal/util"
)

// this generates a mapping of pypi packages <-> modules
// moduleToPypiPackage pypiPackageToModules are provided
//go:generate go run ./gen_pypi_map -from pypi_packages.json -pkg python -out pypi_map.gen.go

// pypiEntry represents one element of the response we get from
// the PyPI API search results.
type pypiEntry struct {
	Name    string `json:"name"`
	Summary string `json:"summary"`
	Version string `json:"version"`
}

// pypiEntryInfoResponse is a wrapper around pypiEntryInfo
// that matches the format of the REST API
type pypiEntryInfoResponse struct {
	Info pypiEntryInfo `json:"info"`
}

// pypiEntryInfo represents the response we get from the
// PyPI API on doing a single-package lookup.
type pypiEntryInfo struct {
	Author        string   `json:"author"`
	AuthorEmail   string   `json:"author_email"`
	HomePage      string   `json:"home_page"`
	License       string   `json:"license"`
	Name          string   `json:"name"`
	ProjectURL    string   `json:"project_url"`
	PackageURL    string   `json:"package_url"`
	BugTrackerURL string   `json:"bugtrack_url"`
	DocsURL       string   `json:"docs_url"`
	RequiresDist  []string `json:"requires_dist"`
	Summary       string   `json:"summary"`
	Version       string   `json:"version"`
}

// pyprojectTOML represents the relevant parts of a pyproject.toml
// file.
type pyprojectTOML struct {
	Tool struct {
		Poetry struct {
			Name string `json:"name"`
			// interface{} because they can be either
			// strings or maps (why?? good lord).
			Dependencies    map[string]interface{} `json:"dependencies"`
			DevDependencies map[string]interface{} `json:"dev-dependencies"`
		} `json:"poetry"`
	} `json:"tool"`
}

// poetryLock represents the relevant parts of a poetry.lock file, in
// TOML format.
type poetryLock struct {
	Package []struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	} `json:"package"`
}

// moduleMetadata represents the information that could be associated with
// a module using a #upm pragma
type modulePragmas struct {
	Package string `json:"package"`
}

// normalizeSpec returns the version string from a Poetry spec, or the
// empty string. The Poetry spec may be either a string or a
// map[string]interface{} with a "version" key that is a string. If
// neither, then the empty string is returned.
func normalizeSpec(spec interface{}) string {
	switch spec := spec.(type) {
	case string:
		return spec
	case map[string]interface{}:
		switch spec := spec["version"].(type) {
		case string:
			return spec
		}
	}
	return ""
}

// normalizePackageName implements NormalizePackageName for the Python
// backends.
func normalizePackageName(name api.PkgName) api.PkgName {
	nameStr := string(name)
	nameStr = strings.ToLower(nameStr)
	nameStr = strings.Replace(nameStr, "_", "-", -1)
	return api.PkgName(nameStr)
}

// pythonMakeBackend returns a language backend for a given version of
// Python. name is either "python2" or "python3", and python is the
// name of an executable (either a full path or just a name like
// "python3") to use when invoking Python. (This is used to implement
// UPM_POETRY)
func pythonMakeBackend(name string, poetry string) api.LanguageBackend {
	info_func := func(name api.PkgName) api.PkgInfo {
		res, err := http.Get(fmt.Sprintf("https://pypi.org/pypi/%s/json", string(name)))

		if err != nil {
			util.Die("HTTP Request failed with error: %s", err)
		}

		defer res.Body.Close()

		if res.StatusCode == 404 {
			return api.PkgInfo{}
		}

		if res.StatusCode != 200 {
			util.Die("Received status code: %d", res.StatusCode)
		}

		body, err := ioutil.ReadAll(res.Body)
		if err != nil {
			util.Die("Res body read failed with error: %s", err)
		}

		var output pypiEntryInfoResponse
		if err := json.Unmarshal(body, &output); err != nil {
			util.Die("PyPI response: %s", err)
		}

		info := api.PkgInfo{
			Name:             output.Info.Name,
			Description:      output.Info.Summary,
			Version:          output.Info.Version,
			HomepageURL:      output.Info.HomePage,
			DocumentationURL: output.Info.DocsURL,
			BugTrackerURL:    output.Info.BugTrackerURL,
			Author: util.AuthorInfo{
				Name:  output.Info.Author,
				Email: output.Info.AuthorEmail,
			}.String(),
			License: output.Info.License,
		}

		deps := []string{}
		for _, line := range output.Info.RequiresDist {
			if strings.Contains(line, "extra ==") {
				continue
			}

			deps = append(deps, strings.Fields(line)[0])
		}
		info.Dependencies = deps

		return info
	}

	return api.LanguageBackend{
		Name:             "python-" + name + "-poetry",
		Specfile:         "pyproject.toml",
		Lockfile:         "poetry.lock",
		FilenamePatterns: []string{"*.py"},
		Quirks: api.QuirksAddRemoveAlsoLocks |
			api.QuirksAddRemoveAlsoInstalls,
		NormalizePackageName: normalizePackageName,
		GetPackageDir: func() string {
			// Check if we're already inside an activated
			// virtualenv. If so, just use it.
			if venv := os.Getenv("VIRTUAL_ENV"); venv != "" {
				return venv
			}

			// Ideally Poetry would provide some way of
			// actually checking where the virtualenv will
			// go. But it doesn't. So we have to
			// reimplement the logic ourselves, which is
			// totally fragile and disgusting. (No, we
			// can't use 'poetry run which python' because
			// that will *create* a virtualenv if one
			// doesn't exist, and there's no workaround
			// for that without mutating the global config
			// file.)
			//
			// Note, we don't yet support Poetry's
			// settings.virtualenvs.in-project. That would
			// be a pretty easy fix, though. (Why is this
			// so complicated??)

			outputB := util.GetCmdOutput([]string{
				poetry, "config", "settings.virtualenvs.path",
			})
			var path string
			if err := json.Unmarshal(outputB, &path); err != nil {
				util.Die("parsing output from Poetry: %s", err)
			}

			base := ""
			if util.Exists("pyproject.toml") {
				var cfg pyprojectTOML
				if _, err := toml.DecodeFile("pyproject.toml", &cfg); err != nil {
					util.Die("%s", err.Error())
				}
				base = cfg.Tool.Poetry.Name
			}

			if base == "" {
				cwd, err := os.Getwd()
				if err != nil {
					util.Die("%s", err)
				}
				base = strings.ToLower(filepath.Base(cwd))
			}

			version := strings.TrimSpace(string(util.GetCmdOutput([]string{
				poetry, "-c",
				`import sys; print(".".join(map(str, sys.version_info[:2])))`,
			})))

			return filepath.Join(path, base+"-py"+version)
		},
		Search: func(query string) []api.PkgInfo {
			// Do a search on pypiPackageToModules
			var packages []string
			for p, _ := range pypiPackageToModules() {
				if strings.Contains(p, query) {
					packages = append(packages, p)
				}
			}

			// Lookup the package info for each result
			var barrier sync.WaitGroup
			packageQueries := make(chan api.PkgInfo, len(packages))
			for _, p := range packages {
				barrier.Add(1)
				go func(name api.PkgName) {
					packageQueries <- info_func(name)
					barrier.Done()
				}(api.PkgName(p))
			}
			barrier.Wait()
			close(packageQueries)

			results := []api.PkgInfo{}
			for pkg := range packageQueries {
				results = append(results, pkg)
			}

			sort.Slice(results, func(i, j int) bool {
				return pypiPackageToDownloads()[results[i].Name] > pypiPackageToDownloads()[results[j].Name]
			})

			return results
		},
		Info: info_func,
		Add: func(pkgs map[api.PkgName]api.PkgSpec, projectName string) {
			// Initalize the specfile if it doesnt exist
			if !util.Exists("pyproject.toml") {
				cmd := []string{poetry, "init", "--no-interaction"}

				if projectName != "" {
					cmd = append(cmd, "--name", projectName)
				}

				util.RunCmd(cmd)
			}

			cmd := []string{poetry, "add"}
			for name, spec := range pkgs {
				name := string(name)
				spec := string(spec)

				// NB: this doesn't work if spec has
				// spaces in it, because of a bug in
				// Poetry that can't be worked around.
				// It looks like that bug might be
				// fixed in the 1.0 release though :/
				if spec != "" {
					cmd = append(cmd, name+" "+spec)
				} else {
					cmd = append(cmd, name)
				}
			}
			util.RunCmd(cmd)
		},
		Remove: func(pkgs map[api.PkgName]bool) {
			cmd := []string{poetry, "remove"}
			for name, _ := range pkgs {
				cmd = append(cmd, string(name))
			}
			util.RunCmd(cmd)
		},
		Lock: func() {
			util.RunCmd([]string{poetry, "lock", "--no-update"})
		},
		Install: func() {
			// Unfortunately, this doesn't necessarily uninstall
			// packages that have been removed from the lockfile,
			// which happens for example if 'poetry remove' is
			// interrupted. See
			// <https://github.com/sdispater/poetry/issues/648>.
			util.RunCmd([]string{poetry, "-m", "poetry", "install"})
		},
		ListSpecfile: func() map[api.PkgName]api.PkgSpec {
			pkgs, err := listSpecfile()
			if err != nil {
				util.Die("%s", err.Error())
			}

			return pkgs
		},
		ListLockfile: func() map[api.PkgName]api.PkgVersion {
			var cfg poetryLock
			if _, err := toml.DecodeFile("poetry.lock", &cfg); err != nil {
				util.Die("%s", err.Error())
			}
			pkgs := map[api.PkgName]api.PkgVersion{}
			for _, pkgObj := range cfg.Package {
				name := api.PkgName(pkgObj.Name)
				version := api.PkgVersion(pkgObj.Version)
				pkgs[name] = version
			}
			return pkgs
		},
		GuessRegexps: util.Regexps([]string{
			// The (?:.|\\\n) subexpression allows us to
			// match match multiple lines if
			// backslash-escapes are used on the newlines.
			`from (?:.|\\\n) import`,
			`import ((?:.|\\\n)*) as`,
			`import ((?:.|\\\n)*)`,
		}),
		Guess: func() (map[api.PkgName]bool, bool) { return guess(poetry) },
	}
}

func listSpecfile() (map[api.PkgName]api.PkgSpec, error) {
	var cfg pyprojectTOML
	if _, err := toml.DecodeFile("pyproject.toml", &cfg); err != nil {
		return nil, err
	}
	pkgs := map[api.PkgName]api.PkgSpec{}
	for nameStr, spec := range cfg.Tool.Poetry.Dependencies {
		if nameStr == "python" {
			continue
		}

		specStr := normalizeSpec(spec)
		if specStr == "" {
			continue
		}
		pkgs[api.PkgName(nameStr)] = api.PkgSpec(specStr)
	}
	for nameStr, spec := range cfg.Tool.Poetry.DevDependencies {
		if nameStr == "python" {
			continue
		}

		specStr := normalizeSpec(spec)
		if specStr == "" {
			continue
		}
		pkgs[api.PkgName(nameStr)] = api.PkgSpec(specStr)
	}

	return pkgs, nil
}

func guess(python string) (map[api.PkgName]bool, bool) {
	tempdir := util.TempDir()
	defer os.RemoveAll(tempdir)

	util.WriteResource("/python/pipreqs.py", tempdir)
	script := util.WriteResource("/python/bare-imports.py", tempdir)

	outputB := util.GetCmdOutput([]string{
		python, script, strings.Join(util.IgnoredPaths, " "),
	})

	var output struct {
		Imports map[string]modulePragmas `json:"imports"`
		Success bool                     `json:"success"`
	}

	if err := json.Unmarshal(outputB, &output); err != nil {
		util.Die("pipreqs: %s", err)
	}

	availMods := map[string]bool{}

	if knownPkgs, err := listSpecfile(); err == nil {
		for pkgName := range knownPkgs {
			mods, ok := pypiPackageToModules()[string(pkgName)]
			if ok {
				for _, mod := range strings.Split(mods, ",") {
					availMods[mod] = true
				}
			}
		}
	}

	pkgs := map[api.PkgName]bool{}

	for modname, pragmas := range output.Imports {
		// provided by an existing package or perhaps by the system
		if availMods[modname] {
			continue
		}

		// If this module has a package pragma, use that
		if pragmas.Package != "" {
			name := api.PkgName(pragmas.Package)
			pkgs[normalizePackageName(name)] = true

		} else {
			// Otherwise, try and look it up in Pypi
			pkg, ok := moduleToPypiPackage()[modname]
			if ok {
				name := api.PkgName(pkg)
				pkgs[normalizePackageName(name)] = true
			}
		}
	}

	return pkgs, output.Success
}

// getPython2 returns either "poetry" or the value of the env var 'UPM_POETRY'
func getPoetry() string {
	poetry := os.Getenv("UPM_POETRY")
	if poetry != "" {
		return poetry
	} else {
		return "poetry"
	}
}

// PythonBackend is a UPM backend for Python that uses Poetry.
var PythonBackend = pythonMakeBackend("python", getPoetry())
