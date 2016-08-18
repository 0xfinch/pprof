// Copyright 2014 Google Inc. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package driver

import (
	"encoding/base64"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/google/pprof/internal/measurement"
	"github.com/google/pprof/internal/plugin"
	"github.com/google/pprof/profile"
)

// fetchProfiles fetches and symbolizes the profiles specified by s.
// It will merge all the profiles it is able to retrieve, even if
// there are some failures. It will return an error if it is unable to
// fetch any profiles.
func fetchProfiles(s *source, o *plugin.Options) (*profile.Profile, error) {
	if err := setTmpDir(o.UI); err != nil {
		return nil, err
	}

	sources := make([]profileSource, 0, len(s.Sources)+len(s.Base))
	for _, src := range s.Sources {
		sources = append(sources, profileSource{
			addr:   src,
			source: s,
			scale:  1,
		})
	}
	for _, src := range s.Base {
		sources = append(sources, profileSource{
			addr:   src,
			source: s,
			scale:  -1,
		})
	}
	p, msrcs, save, cnt, err := chunkedGrab(sources, o.Fetch, o.Obj, o.UI)
	if err != nil {
		return nil, err
	}
	if cnt == 0 {
		return nil, fmt.Errorf("failed to fetch any profiles")
	}
	if want, got := len(sources), cnt; want != got {
		o.UI.PrintErr(fmt.Sprintf("fetched %d profiles out of %d", got, want))
	}

	// Symbolize the merged profile.
	if err := o.Sym.Symbolize(s.Symbolize, msrcs, p); err != nil {
		return nil, err
	}
	p.RemoveUninteresting()

	// Save a copy of the merged profile if there is at least one remote source.
	if save {
		prefix := "pprof."
		if len(p.Mapping) > 0 && p.Mapping[0].File != "" {
			prefix += filepath.Base(p.Mapping[0].File) + "."
		}
		for _, s := range p.SampleType {
			prefix += s.Type + "."
		}

		dir := os.Getenv("PPROF_TMPDIR")
		tempFile, err := newTempFile(dir, prefix, ".pb.gz")
		if err == nil {
			if err = p.Write(tempFile); err == nil {
				o.UI.PrintErr("Saved profile in ", tempFile.Name())
			}
		}
		if err != nil {
			o.UI.PrintErr("Could not save profile: ", err)
		}
	}

	if err := p.CheckValid(); err != nil {
		return nil, err
	}

	return p, nil
}

// chunkedGrab fetches the profiles described in source and merges them into
// a single profile. It fetches a chunk of profiles concurrently, with a maximum
// chunk size to limit its memory usage.
func chunkedGrab(sources []profileSource, fetch plugin.Fetcher, obj plugin.ObjTool, ui plugin.UI) (*profile.Profile, plugin.MappingSources, bool, int, error) {
	const chunkSize = 64

	var p *profile.Profile
	var msrc plugin.MappingSources
	var save bool
	var count int

	for start := 0; start < len(sources); start += chunkSize {
		end := start + chunkSize
		if end > len(sources) {
			end = len(sources)
		}
		chunkP, chunkMsrc, chunkSave, chunkCount, chunkErr := concurrentGrab(sources[start:end], fetch, obj, ui)
		switch {
		case chunkErr != nil:
			return nil, nil, false, 0, chunkErr
		case chunkP == nil:
			continue
		case p == nil:
			p, msrc, save, count = chunkP, chunkMsrc, chunkSave, chunkCount
		default:
			p, msrc, chunkErr = combineProfiles([]*profile.Profile{p, chunkP}, []plugin.MappingSources{msrc, chunkMsrc})
			if chunkErr != nil {
				return nil, nil, false, 0, chunkErr
			}
			if chunkSave {
				save = true
			}
			count += chunkCount
		}
	}
	return p, msrc, save, count, nil
}

// concurrentGrab fetches multiple profiles concurrently
func concurrentGrab(sources []profileSource, fetch plugin.Fetcher, obj plugin.ObjTool, ui plugin.UI) (*profile.Profile, plugin.MappingSources, bool, int, error) {
	wg := sync.WaitGroup{}
	wg.Add(len(sources))
	for i := range sources {
		go func(s *profileSource) {
			defer wg.Done()
			s.p, s.msrc, s.remote, s.err = grabProfile(s.source, s.addr, s.scale, fetch, obj, ui)
		}(&sources[i])
	}
	wg.Wait()

	var save bool
	profiles := make([]*profile.Profile, 0, len(sources))
	msrcs := make([]plugin.MappingSources, 0, len(sources))
	for i := range sources {
		s := &sources[i]
		if err := s.err; err != nil {
			ui.PrintErr(s.addr + ": " + err.Error())
			continue
		}
		save = save || s.remote
		profiles = append(profiles, s.p)
		msrcs = append(msrcs, s.msrc)
		*s = profileSource{}
	}

	if len(profiles) == 0 {
		return nil, nil, false, 0, nil
	}

	p, msrc, err := combineProfiles(profiles, msrcs)
	if err != nil {
		return nil, nil, false, 0, err
	}
	return p, msrc, save, len(profiles), nil
}

func combineProfiles(profiles []*profile.Profile, msrcs []plugin.MappingSources) (*profile.Profile, plugin.MappingSources, error) {
	// Merge profiles.
	if err := measurement.ScaleProfiles(profiles); err != nil {
		return nil, nil, err
	}

	p, err := profile.Merge(profiles)
	if err != nil {
		return nil, nil, err
	}

	// Combine mapping sources.
	msrc := make(plugin.MappingSources)
	for _, ms := range msrcs {
		for m, s := range ms {
			msrc[m] = append(msrc[m], s...)
		}
	}
	return p, msrc, nil
}

type profileSource struct {
	addr   string
	source *source
	scale  float64

	p      *profile.Profile
	msrc   plugin.MappingSources
	remote bool
	err    error
}

// setTmpDir sets the PPROF_TMPDIR environment variable with a new
// temp directory, if not already set.
func setTmpDir(ui plugin.UI) error {
	if profileDir := os.Getenv("PPROF_TMPDIR"); profileDir != "" {
		return nil
	}
	for _, tmpDir := range []string{os.Getenv("HOME") + "/pprof", "/tmp"} {
		if err := os.MkdirAll(tmpDir, 0755); err != nil {
			ui.PrintErr("Could not use temp dir ", tmpDir, ": ", err.Error())
			continue
		}
		os.Setenv("PPROF_TMPDIR", tmpDir)
		return nil
	}
	return fmt.Errorf("failed to identify temp dir")
}

// grabProfile fetches a profile. Returns the profile, sources for the
// profile mappings, a bool indicating if the profile was fetched
// remotely, and an error.
func grabProfile(s *source, source string, scale float64, fetcher plugin.Fetcher, obj plugin.ObjTool, ui plugin.UI) (p *profile.Profile, msrc plugin.MappingSources, remote bool, err error) {
	var src string
	duration, timeout := time.Duration(s.Seconds)*time.Second, time.Duration(s.Timeout)*time.Second
	if fetcher != nil {
		p, src, err = fetcher.Fetch(source, duration, timeout)
		if err != nil {
			return
		}
	}
	if err != nil || p == nil {
		// Fetch the profile over HTTP or from a file.
		p, src, err = fetch(source, duration, timeout, ui)
		if err != nil {
			return
		}
	}

	if err = p.CheckValid(); err != nil {
		return
	}

	// Apply local changes to the profile.
	p.Scale(scale)

	// Update the binary locations from command line and paths.
	locateBinaries(p, s, obj, ui)

	// Collect the source URL for all mappings.
	if src != "" {
		msrc = collectMappingSources(p, src)
		remote = true
	}
	return
}

// collectMappingSources saves the mapping sources of a profile.
func collectMappingSources(p *profile.Profile, source string) plugin.MappingSources {
	ms := plugin.MappingSources{}
	for _, m := range p.Mapping {
		src := struct {
			Source string
			Start  uint64
		}{
			source, m.Start,
		}
		key := m.BuildID
		if key == "" {
			key = m.File
		}
		if key == "" {
			// If there is no build id or source file, use the source as the
			// mapping file. This will enable remote symbolization for this
			// mapping, in particular for Go profiles on the legacy format.
			m.File = source
			key = source
		}
		ms[key] = append(ms[key], src)
	}
	return ms
}

// locateBinaries searches for binary files listed in the profile and, if found,
// updates the profile accordingly.
func locateBinaries(p *profile.Profile, s *source, obj plugin.ObjTool, ui plugin.UI) {
	// Construct search path to examine
	searchPath := os.Getenv("PPROF_BINARY_PATH")
	if searchPath == "" {
		// Use $HOME/pprof/binaries as default directory for local symbolization binaries
		searchPath = filepath.Join(os.Getenv("HOME"), "pprof", "binaries")
	}

mapping:
	for i, m := range p.Mapping {
		var baseName string
		// Replace executable filename/buildID with the overrides from source.
		// Assumes the executable is the first Mapping entry.
		if i == 0 {
			if s.ExecName != "" {
				m.File = s.ExecName
			}
			if s.BuildID != "" {
				m.BuildID = s.BuildID
			}
		}
		if m.File != "" {
			baseName = filepath.Base(m.File)
		}

		for _, path := range filepath.SplitList(searchPath) {
			var fileNames []string
			if m.BuildID != "" {
				fileNames = []string{filepath.Join(path, m.BuildID, baseName)}
				if matches, err := filepath.Glob(filepath.Join(path, m.BuildID, "*")); err == nil {
					fileNames = append(fileNames, matches...)
				}
			}
			if baseName != "" {
				fileNames = append(fileNames, filepath.Join(path, baseName))
			}
			for _, name := range fileNames {
				if f, err := obj.Open(name, m.Start, m.Limit, m.Offset); err == nil {
					defer f.Close()
					fileBuildID := f.BuildID()
					if m.BuildID != "" && m.BuildID != fileBuildID {
						ui.PrintErr("Ignoring local file " + name + ": build-id mismatch (" + m.BuildID + " != " + fileBuildID + ")")
					} else {
						m.File = name
						continue mapping
					}
				}
			}
		}
	}
}

// fetch fetches a profile from source, within the timeout specified,
// producing messages through the ui. It returns the profile and the
// url of the actual source of the profile for remote profiles.
func fetch(source string, duration, timeout time.Duration, ui plugin.UI) (p *profile.Profile, src string, err error) {
	var f io.ReadCloser

	if sourceURL, timeout := adjustURL(source, duration, timeout); sourceURL != "" {
		ui.Print("Fetching profile over HTTP from " + sourceURL)
		if duration > 0 {
			ui.Print(fmt.Sprintf("Please wait... (%v)", duration))
		}
		f, err = fetchURL(sourceURL, timeout)
		src = sourceURL
	} else {
		f, err = profileProtoReader(source)
	}
	if err == nil {
		defer f.Close()
		p, err = profile.Parse(f)
	}
	return
}

// fetchURL fetches a profile from a URL using HTTP.
func fetchURL(source string, timeout time.Duration) (io.ReadCloser, error) {
	resp, err := httpGet(source, timeout)
	if err != nil {
		return nil, fmt.Errorf("http fetch %s: %v", source, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("server response: %s", resp.Status)
	}

	return resp.Body, nil
}

// profileProtoReader takes a path, and using heuristics, will try to convert
// the file to profile.proto format. It returns a ReadCloser to the
// profile.proto data; however, if the file contents were unknown or conversion
// failed, it may still not be a valid profile.proto.
func profileProtoReader(path string) (io.ReadCloser, error) {
	sourceFile, openErr := os.Open(path)
	if openErr != nil {
		return nil, openErr
	}

	// If the file is the output of a perf record command, it should begin
	// with the string PERFILE2.
	perfHeader := []byte("PERFILE2")
	actualHeader := make([]byte, len(perfHeader))
	_, readErr := sourceFile.Read(actualHeader)
	_, seekErr := sourceFile.Seek(0, 0)
	if seekErr != nil {
		return nil, seekErr
	} else if readErr != nil && readErr != io.EOF{
		return nil, readErr
	} else if string(actualHeader) == string(perfHeader) {
		sourceFile.Close()
		profileFile, convertErr := convertPerfData(path)
		if convertErr != nil {
			return nil, convertErr
		}
		return os.Open(profileFile)
	}
	return sourceFile, nil
}

// convertPerfData converts the file at path which should be in perf.data format
// using the perf_to_profile tool. It prints the stderr and stdout of
// perf_to_profile to the stderr and stdout of this process, then returns the
// path to a file containing the profile.proto formatted data.
func convertPerfData(perfPath string) (string, error) {
	randomBytes := make([]byte, 32)
	_, randErr := rand.Read(randomBytes)
	if randErr != nil {
		return "", randErr
	}
	randomFileName := "/tmp/pprof_" +
		base64.StdEncoding.EncodeToString(randomBytes)
	cmd := exec.Command("perf_to_profile", perfPath, randomFileName)

	stdout, stdoutErr := cmd.StdoutPipe()
	if stdoutErr != nil {
		return "", stdoutErr
	}
	go func() {
		io.Copy(os.Stdout, stdout)
	}()

	stderr, stderrErr := cmd.StderrPipe()
	if stderrErr != nil {
		return "", stderrErr
	}
	go func() {
		io.Copy(os.Stderr, stderr)
	}()

	if err := cmd.Run(); err != nil {
		return "", err
	}

	return randomFileName, nil
}

// adjustURL validates if a profile source is a URL and returns an
// cleaned up URL and the timeout to use for retrieval over HTTP.
// If the source cannot be recognized as a URL it returns an empty string.
func adjustURL(source string, duration, timeout time.Duration) (string, time.Duration) {
	u, err := url.Parse(source)
	if err != nil || (u.Host == "" && u.Scheme != "" && u.Scheme != "file") {
		// Try adding http:// to catch sources of the form hostname:port/path.
		// url.Parse treats "hostname" as the scheme.
		u, err = url.Parse("http://" + source)
	}
	if err != nil || u.Host == "" {
		return "", 0
	}

	// Apply duration/timeout overrides to URL.
	values := u.Query()
	if duration > 0 {
		values.Set("seconds", fmt.Sprint(int(duration.Seconds())))
	} else {
		if urlSeconds := values.Get("seconds"); urlSeconds != "" {
			if us, err := strconv.ParseInt(urlSeconds, 10, 32); err == nil {
				duration = time.Duration(us) * time.Second
			}
		}
	}
	if timeout <= 0 {
		if duration > 0 {
			timeout = duration + duration/2
		} else {
			timeout = 60 * time.Second
		}
	}
	u.RawQuery = values.Encode()
	return u.String(), timeout
}

// httpGet is a wrapper around http.Get; it is defined as a variable
// so it can be redefined during for testing.
var httpGet = func(url string, timeout time.Duration) (*http.Response, error) {
	client := &http.Client{
		Transport: &http.Transport{
			ResponseHeaderTimeout: timeout + 5*time.Second,
		},
	}
	return client.Get(url)
}
