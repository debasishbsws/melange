// Copyright 2022 Chainguard, Inc.
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

package build

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"text/template"

	apko_types "chainguard.dev/apko/pkg/build/types"

	"github.com/klauspost/compress/gzip"
	"github.com/klauspost/pgzip"

	"chainguard.dev/melange/pkg/config"
	"chainguard.dev/melange/pkg/sca"
	"chainguard.dev/melange/pkg/util"

	"github.com/chainguard-dev/clog"
	"github.com/chainguard-dev/go-apk/pkg/tarball"
	"github.com/psanford/memfs"
	"go.opentelemetry.io/otel"
)

// pgzip's default is GOMAXPROCS(0)
//
// This is fine for single builds, but we will starve CPU for larger builds.
// 8 is our max because modern laptops tend to have ~8 performance cores, and
// large CI machines tend to have ~64 cores.
//
// This gives us near 100% utility on workstations, allows us to do ~8
// concurrent builds on giant machines, and uses only 1 core on tiny machines.
var pgzipThreads = min(runtime.GOMAXPROCS(0), 8)

func min(l, r int) int {
	if l < r {
		return l
	}

	return r
}

type PackageBuild struct {
	Build         *Build
	Origin        *config.Package
	PackageName   string
	OriginName    string
	InstalledSize int64
	DataHash      string
	OutDir        string
	Dependencies  config.Dependencies
	Arch          string
	Options       config.PackageOption
	Scriptlets    config.Scriptlets
	Description   string
	URL           string
	Commit        string
}

func pkgFromSub(sub *config.Subpackage) *config.Package {
	return &config.Package{
		Name:         sub.Name,
		Dependencies: sub.Dependencies,
		Options:      sub.Options,
		Scriptlets:   sub.Scriptlets,
		Description:  sub.Description,
		URL:          sub.URL,
		Commit:       sub.Commit,
	}
}

func (pb *PipelineBuild) Emit(ctx context.Context, pkg *config.Package) error {
	pc := PackageBuild{
		Build:        pb.Build,
		Origin:       &pb.Build.Configuration.Package,
		PackageName:  pkg.Name,
		OriginName:   pkg.Name,
		OutDir:       filepath.Join(pb.Build.OutDir, pb.Build.Arch.ToAPK()),
		Dependencies: pkg.Dependencies,
		Arch:         pb.Build.Arch.ToAPK(),
		Options:      pkg.Options,
		Scriptlets:   pkg.Scriptlets,
		Description:  pkg.Description,
		URL:          pkg.URL,
		Commit:       pkg.Commit,
	}

	if !pb.Build.StripOriginName {
		pc.OriginName = pc.Origin.Name
	}

	return pc.EmitPackage(ctx)
}

// AppendBuildLog will create or append a list of packages that were built by melange build
func (pc *PackageBuild) AppendBuildLog(dir string) error {
	if !pc.Build.CreateBuildLog {
		return nil
	}

	f, err := os.OpenFile(filepath.Join(dir, "packages.log"),
		os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer f.Close()

	// separate with pipe so it is easy to parse
	_, err = f.WriteString(fmt.Sprintf("%s|%s|%s|%s-r%d\n", pc.Arch, pc.OriginName, pc.PackageName, pc.Origin.Version, pc.Origin.Epoch))
	return err
}

func (pc *PackageBuild) Identity() string {
	return fmt.Sprintf("%s-%s-r%d", pc.PackageName, pc.Origin.Version, pc.Origin.Epoch)
}

func (pc *PackageBuild) Filename() string {
	return fmt.Sprintf("%s/%s.apk", pc.OutDir, pc.Identity())
}

func (pc *PackageBuild) WorkspaceSubdir() string {
	return filepath.Join(pc.Build.WorkspaceDir, "melange-out", pc.PackageName)
}

var controlTemplate = `# Generated by melange.
pkgname = {{.PackageName}}
pkgver = {{.Origin.Version}}-r{{.Origin.Epoch}}
arch = {{.Arch}}
size = {{.InstalledSize}}
origin = {{.OriginName}}
pkgdesc = {{.Description}}
url = {{.URL}}
commit = {{.Commit}}
{{- if ne .Build.SourceDateEpoch.Unix 0 }}
builddate = {{ .Build.SourceDateEpoch.Unix }}
{{- end}}
{{- range $copyright := .Origin.Copyright }}
license = {{ $copyright.License }}
{{- end }}
{{- range $dep := .Dependencies.Runtime }}
depend = {{ $dep }}
{{- end }}
{{- range $dep := .Dependencies.Provides }}
provides = {{ $dep }}
{{- end }}
{{- range $dep := .Dependencies.Replaces }}
replaces = {{ $dep }}
{{- end }}
{{- range $dep := .Dependencies.Vendored }}
# vendored = {{ $dep }}
{{- end }}
{{- if .Dependencies.ProviderPriority }}
provider_priority = {{ .Dependencies.ProviderPriority }}
{{- end }}
{{- if .Scriptlets.Trigger.Paths }}
triggers = {{ range $item := .Scriptlets.Trigger.Paths }}{{ $item }} {{ end }}
{{- end }}
datahash = {{.DataHash}}
`

func (pc *PackageBuild) GenerateControlData(w io.Writer) error {
	tmpl := template.New("control")
	return template.Must(tmpl.Parse(controlTemplate)).Execute(w, pc)
}

func (pc *PackageBuild) generateControlSection(ctx context.Context) ([]byte, error) {
	tarctx, err := tarball.NewContext(
		tarball.WithSourceDateEpoch(pc.Build.SourceDateEpoch),
		tarball.WithOverrideUIDGID(0, 0),
		tarball.WithOverrideUname("root"),
		tarball.WithOverrideGname("root"),
		tarball.WithSkipClose(true),
	)
	if err != nil {
		return nil, fmt.Errorf("unable to build tarball context: %w", err)
	}

	var controlBuf bytes.Buffer
	if err := pc.GenerateControlData(&controlBuf); err != nil {
		return nil, fmt.Errorf("unable to process control template: %w", err)
	}

	fsys := memfs.New()
	if err := fsys.WriteFile(".PKGINFO", controlBuf.Bytes(), 0644); err != nil {
		return nil, fmt.Errorf("unable to build control FS: %w", err)
	}

	if pc.Scriptlets.Trigger.Script != "" {
		// #nosec G306 -- scriptlets must be executable
		if err := fsys.WriteFile(".trigger", []byte(pc.Scriptlets.Trigger.Script), 0755); err != nil {
			return nil, fmt.Errorf("unable to build control FS: %w", err)
		}
	}

	if pc.Scriptlets.PreInstall != "" {
		// #nosec G306 -- scriptlets must be executable
		if err := fsys.WriteFile(".pre-install", []byte(pc.Scriptlets.PreInstall), 0755); err != nil {
			return nil, fmt.Errorf("unable to build control FS: %w", err)
		}
	}

	if pc.Scriptlets.PostInstall != "" {
		// #nosec G306 -- scriptlets must be executable
		if err := fsys.WriteFile(".post-install", []byte(pc.Scriptlets.PostInstall), 0755); err != nil {
			return nil, fmt.Errorf("unable to build control FS: %w", err)
		}
	}

	if pc.Scriptlets.PreDeinstall != "" {
		// #nosec G306 -- scriptlets must be executable
		if err := fsys.WriteFile(".pre-deinstall", []byte(pc.Scriptlets.PreDeinstall), 0755); err != nil {
			return nil, fmt.Errorf("unable to build control FS: %w", err)
		}
	}

	if pc.Scriptlets.PostDeinstall != "" {
		// #nosec G306 -- scriptlets must be executable
		if err := fsys.WriteFile(".post-deinstall", []byte(pc.Scriptlets.PostDeinstall), 0755); err != nil {
			return nil, fmt.Errorf("unable to build control FS: %w", err)
		}
	}

	if pc.Scriptlets.PreUpgrade != "" {
		// #nosec G306 -- scriptlets must be executable
		if err := fsys.WriteFile(".pre-upgrade", []byte(pc.Scriptlets.PreUpgrade), 0755); err != nil {
			return nil, fmt.Errorf("unable to build control FS: %w", err)
		}
	}

	if pc.Scriptlets.PostUpgrade != "" {
		// #nosec G306 -- scriptlets must be executable
		if err := fsys.WriteFile(".post-upgrade", []byte(pc.Scriptlets.PostUpgrade), 0755); err != nil {
			return nil, fmt.Errorf("unable to build control FS: %w", err)
		}
	}

	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)

	if err := tarctx.WriteTar(ctx, zw, fsys, fsys); err != nil {
		return nil, fmt.Errorf("unable to write control tarball: %w", err)
	}
	if err := zw.Close(); err != nil {
		return nil, fmt.Errorf("flushing control section gzip: %w", err)
	}

	return buf.Bytes(), nil
}

func (pc *PackageBuild) SignatureName() string {
	return fmt.Sprintf(".SIGN.RSA.%s.pub", filepath.Base(pc.Build.SigningKey))
}

// removeSelfProvidedDeps removes dependencies which are provided by the package itself.
func removeSelfProvidedDeps(runtimeDeps, providedDeps []string) []string {
	providedDepsMap := map[string]bool{}

	for _, versionedDep := range providedDeps {
		dep := strings.Split(versionedDep, "=")[0]
		providedDepsMap[dep] = true
	}

	newRuntimeDeps := []string{}
	for _, dep := range runtimeDeps {
		_, ok := providedDepsMap[dep]
		if ok {
			continue
		}

		newRuntimeDeps = append(newRuntimeDeps, dep)
	}

	return newRuntimeDeps
}

func (pc *PackageBuild) GenerateDependencies(ctx context.Context) error {
	log := clog.FromContext(ctx)
	generated := config.Dependencies{}

	hdl := SCABuildInterface{
		PackageBuild: pc,
	}

	if err := sca.Analyze(ctx, &hdl, &generated); err != nil {
		return fmt.Errorf("analyzing package: %w", err)
	}

	if pc.Build.DependencyLog != "" {
		log.Info("writing dependency log")

		logFile, err := os.Create(fmt.Sprintf("%s.%s", pc.Build.DependencyLog, pc.Arch))
		if err != nil {
			log.Warnf("Unable to open dependency log: %v", err)
		}
		defer logFile.Close()

		je := json.NewEncoder(logFile)
		if err := je.Encode(&generated); err != nil {
			return err
		}
	}

	// Only consider vendored deps for self-provided generated runtime deps.
	// If a runtime dep is explicitly configured, assume we actually do need it.
	// This gives us an escape hatch in melange config in case there is a runtime
	// dep that we don't want to be satisfied by a vendored dep.
	unvendored := removeSelfProvidedDeps(generated.Runtime, generated.Vendored)

	newruntime := append(pc.Dependencies.Runtime, unvendored...)
	pc.Dependencies.Runtime = util.Dedup(newruntime)

	newprovides := append(pc.Dependencies.Provides, generated.Provides...)
	pc.Dependencies.Provides = util.Dedup(newprovides)

	pc.Dependencies.Runtime = removeSelfProvidedDeps(pc.Dependencies.Runtime, pc.Dependencies.Provides)

	// Sets .PKGINFO `# vendored = ...` comments; does not affect resolution.
	pc.Dependencies.Vendored = util.Dedup(generated.Vendored)

	pc.Dependencies.Summarize(ctx)

	return nil
}

func combine(out io.Writer, inputs ...io.Reader) error {
	for _, input := range inputs {
		if _, err := io.Copy(out, input); err != nil {
			return err
		}
	}

	return nil
}

// TODO(kaniini): generate APKv3 packages
func (pc *PackageBuild) calculateInstalledSize(fsys fs.FS) error {
	if err := fs.WalkDir(fsys, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		fi, err := d.Info()
		if err != nil {
			return err
		}

		pc.InstalledSize += fi.Size()
		return nil
	}); err != nil {
		return fmt.Errorf("unable to preprocess package data: %w", err)
	}

	return nil
}

func (pc *PackageBuild) emitDataSection(ctx context.Context, fsys fs.FS, userinfofs fs.FS, remapUIDs map[int]int, remapGIDs map[int]int, w io.WriteSeeker) error {
	log := clog.FromContext(ctx)
	tarctx, err := tarball.NewContext(
		tarball.WithSourceDateEpoch(pc.Build.SourceDateEpoch),
		tarball.WithRemapUIDs(remapUIDs),
		tarball.WithRemapGIDs(remapGIDs),
		tarball.WithUseChecksums(true),
	)
	if err != nil {
		return fmt.Errorf("unable to build tarball context: %w", err)
	}

	digest := sha256.New()
	mw := io.MultiWriter(digest, w)
	zw := pgzip.NewWriter(mw)
	if err := zw.SetConcurrency(1<<20, pgzipThreads); err != nil {
		return fmt.Errorf("tried to set pgzip concurrency to %d: %w", pgzipThreads, err)
	}

	if err := tarctx.WriteTar(ctx, zw, fsys, userinfofs); err != nil {
		return fmt.Errorf("unable to write data tarball: %w", err)
	}

	if err := zw.Close(); err != nil {
		return fmt.Errorf("flushing data section gzip: %w", err)
	}

	pc.DataHash = hex.EncodeToString(digest.Sum(nil))
	log.Infof("  data.tar.gz digest: %s", pc.DataHash)

	if _, err := w.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("unable to rewind data tarball: %w", err)
	}

	return nil
}

func (pc *PackageBuild) wantSignature() bool {
	return pc.Build.SigningKey != ""
}

func (pc *PackageBuild) EmitPackage(ctx context.Context) error {
	log := clog.FromContext(ctx)
	ctx, span := otel.Tracer("melange").Start(ctx, "EmitPackage")
	defer span.End()

	err := os.MkdirAll(pc.WorkspaceSubdir(), 0o755)
	if err != nil {
		return fmt.Errorf("unable to ensure workspace exists: %w", err)
	}

	log.Info("generating package " + pc.Identity())

	// filesystem for the data package
	fsys := readlinkFS(pc.WorkspaceSubdir())

	// provide the tar writer etc/passwd and etc/group of guest filesystem
	userinfofs := os.DirFS(pc.Build.GuestDir)

	// generate so:/cmd: virtuals for the filesystem
	if err := pc.GenerateDependencies(ctx); err != nil {
		return fmt.Errorf("unable to build final dependencies set: %w", err)
	}

	// walk the filesystem to calculate the installed-size
	if err := pc.calculateInstalledSize(fsys); err != nil {
		return err
	}

	log.Infof("  installed-size: %d", pc.InstalledSize)

	// prepare data.tar.gz
	dataTarGz, err := os.CreateTemp("", "melange-data-*.tar.gz")
	if err != nil {
		return fmt.Errorf("unable to open temporary file for writing: %w", err)
	}
	defer dataTarGz.Close()
	defer os.Remove(dataTarGz.Name())

	// why remap UIDs and GIDs of build?
	// the build user is not intended to be exposed as an owner of the contents of the package.
	// in most cases, when build is used, it is meant to refer to root.
	// in some previous versions of melange, the ownership of all files was root/root 0/0 but
	// this meant that permissions changed inside the environment were not preserved.
	// by remapping permissions here, we are ensuring that files owned by the build user
	// will be owned as the correct owner of root, while also ensuring that permissions
	// when writing the tar can be preserved for users other than root.
	remapUIDs := make(map[int]int)
	remapGIDs := make(map[int]int)

	// extract the build user and build group from the apko environment
	var buildUser apko_types.User
	var buildGroup apko_types.Group

	for _, user := range pc.Build.Configuration.Environment.Accounts.Users {
		if user.UserName == "build" {
			buildUser = user
		}
	}

	for _, group := range pc.Build.Configuration.Environment.Accounts.Groups {
		if group.GroupName == "build" {
			buildGroup = group
		}
	}

	// we can directly remap here since 0 is the default
	// for unspecified int fields and remapping 0 to 0 is okay
	remapUIDs[int(buildUser.UID)] = 0
	remapGIDs[int(buildGroup.GID)] = 0

	if err := pc.emitDataSection(ctx, fsys, userinfofs, remapUIDs, remapGIDs, dataTarGz); err != nil {
		return err
	}

	controlSectionData, err := pc.generateControlSection(ctx)
	if err != nil {
		return err
	}

	combinedParts := []io.Reader{bytes.NewReader(controlSectionData), dataTarGz}

	if pc.wantSignature() {
		signatureData, err := EmitSignature(ctx, pc.Signer(), controlSectionData, pc.Build.SourceDateEpoch)
		if err != nil {
			return fmt.Errorf("emitting signature: %w", err)
		}

		combinedParts = append([]io.Reader{bytes.NewReader(signatureData)}, combinedParts...)
	}

	// build the final tarball
	if err := os.MkdirAll(pc.OutDir, 0755); err != nil {
		return fmt.Errorf("unable to create output directory: %w", err)
	}

	outFile, err := os.Create(pc.Filename())
	if err != nil {
		return fmt.Errorf("unable to create apk file: %w", err)
	}
	defer outFile.Close()

	if err := combine(outFile, combinedParts...); err != nil {
		return fmt.Errorf("unable to write apk file: %w", err)
	}

	log.Infof("wrote %s", outFile.Name())

	// add the package to the build log if requested
	if err := pc.AppendBuildLog(""); err != nil {
		log.Warnf("unable to append package log: %s", err)
	}

	return nil
}

func (pc *PackageBuild) Signer() ApkSigner {
	return &KeyApkSigner{
		KeyFile:       pc.Build.SigningKey,
		KeyPassphrase: pc.Build.SigningPassphrase,
	}
}
