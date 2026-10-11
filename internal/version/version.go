/*
Copyright 2026 The cluster-api-ipam-provider-netbox Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package version exposes the build information of the manager.
package version

import (
	"fmt"
	"runtime"
)

// Set at build time with -ldflags "-X <package>.<name>=<value>".
var (
	gitVersion = "dev"
	gitCommit  = "unknown"
	buildDate  = "unknown"
)

// Info describes the running build.
type Info struct {
	GitVersion string
	GitCommit  string
	BuildDate  string
	GoVersion  string
	Platform   string
}

// Get returns the build information.
func Get() Info {
	return Info{
		GitVersion: gitVersion,
		GitCommit:  gitCommit,
		BuildDate:  buildDate,
		GoVersion:  runtime.Version(),
		Platform:   runtime.GOOS + "/" + runtime.GOARCH,
	}
}

// String returns info as a one-line version string.
func (i Info) String() string {
	return fmt.Sprintf("%s (git commit: %s, built: %s, %s, %s)", i.GitVersion, i.GitCommit, i.BuildDate, i.GoVersion, i.Platform)
}
