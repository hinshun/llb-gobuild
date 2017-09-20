package loader

import (
	"fmt"
	"go/build"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/moby/buildkit/client/llb"
)

type GoOpt struct {
	Source     llb.State
	MountPath  string
	CgoEnabled bool
	BuildTags  []string
	GOARCH     string
	GOOS       string
	GOPATH     string
}

func New(opt GoOpt) *Loader {
	bctx := build.Context{
		GOARCH: opt.GOARCH,
		GOOS:   opt.GOOS,
		// GOROOT:      runtime.GOROOT(),
		GOPATH:      opt.GOPATH,
		Compiler:    "gc",
		CgoEnabled:  opt.CgoEnabled,
		BuildTags:   opt.BuildTags,
		ReleaseTags: []string{"go1.1", "go1.2", "go1.3", "go1.4", "go1.5", "go1.6", "go1.7", "go1.8"},
	}

	return &Loader{
		v:     newVendorDirs(opt.GOPATH),
		opt:   opt,
		cache: map[string]*pkg{},
		bctx:  bctx,
		local: opt.Source,
		base:  goBuildBase(),
		wd:    opt.MountPath,
	}
}

type Loader struct {
	opt   GoOpt
	v     *vendorDirs
	cache map[string]*pkg
	bctx  build.Context
	local llb.State
	base  llb.State
	wd    string
}

func (l *Loader) BuildExe(pkg string) (*llb.State, error) {
	p, err := l.loadDir(filepath.Join(l.opt.GOPATH, "src", pkg))
	if err != nil {
		return nil, err
	}

	cmd := llb.Shlexf("/usr/local/go/pkg/tool/linux_amd64/link -o /work/%s/_obj/exe/%s -linkmode=internal -L /work -extld=gcc -extldflags \"-static\" -buildmode=exe /work/%s.a", p.p.ImportPath, path.Base(p.p.ImportPath), p.p.ImportPath)

	st3 := l.base.Run(cmd)
	st3.AddMount(fmt.Sprintf("/work/%s.a", p.p.ImportPath), p.state, llb.SourcePath(path.Base(p.p.ImportPath)+".a"), llb.Readonly)
	out := st3.AddMount(fmt.Sprintf("/work/%s/_obj/exe", p.p.ImportPath), llb.Scratch())

	for s, d := range p.alldeps {
		st3.AddMount(fmt.Sprintf("/work/%s.a", s), d, llb.SourcePath(path.Base(s)+".a"), llb.Readonly)
	}

	return &out, nil
}

func (l *Loader) loadDir(dir string) (*pkg, error) {
	if p, ok := l.cache[dir]; ok {
		return p, nil
	}
	p, err := l.bctx.ImportDir(dir, 0)
	if err != nil {
		return nil, err
	}
	l.v.add(filepath.Dir(dir))

	pkg := &pkg{p: p, alldeps: map[string]llb.State{}}

	for _, imp := range p.Imports {
		for _, vd := range l.v.dirs {
			if strings.HasPrefix(dir, filepath.Dir(vd)) {
				d := filepath.Join(vd, imp)
				fi, err := os.Stat(d)
				if err == nil && fi.IsDir() {
					p, err := l.loadDir(d)
					if err != nil {
						return nil, err
					}
					pkg.AddDep(imp, p)
					break
				}
			}
		}
	}

	name := p.ImportPath
	if p.Name == "main" {
		name = p.Name
	}

	cmd := llb.Shlexf("/usr/local/go/pkg/tool/linux_amd64/compile -o /work/%s.a -trimpath /work -p %s -complete -I /work -pack %s", p.ImportPath, name, strings.Join(p.GoFiles, " "))

	if len(p.SFiles) > 0 {
		cmd = llb.Shlexf("/usr/local/go/pkg/tool/linux_amd64/compile -o /work/%s.a -trimpath /work -p %s -I /work -pack -asmhdr /work/%s/_obj/go_asm.h %s ", p.ImportPath, name, p.ImportPath, strings.Join(p.GoFiles, " "))

	}

	st := l.base.Run(cmd, llb.Dir("/root"))

	for _, mount := range pkg.mounts {
		st.AddMount(path.Join("/work", mount.target, mount.selector+".a"), mount.state, llb.SourcePath(mount.selector+".a"), llb.Readonly)
	}

	for _, f := range p.GoFiles {
		st.AddMount(path.Join("/root", f), l.local, llb.SourcePath(strings.TrimPrefix(path.Join(dir, f), l.wd)), llb.Readonly)
	}

	var asmheader llb.State
	if len(p.SFiles) > 0 {
		asmheader = st.AddMount(path.Join("/work", p.ImportPath, "_obj"), llb.Scratch())
	}

	out := st.AddMount(path.Join("/work", path.Dir(p.ImportPath)), llb.Scratch())

	if len(p.SFiles) > 0 {
		cmd := llb.Shlexf("/usr/local/go/pkg/tool/linux_amd64/asm -I /work/%s/_obj/ -I /usr/local/go/pkg/include -D GOOS_%s -D GOARCH_%s -o /work/%s/_obj/asm_%s_%s.o -trimpath /work %s", p.ImportPath, runtime.GOOS, runtime.GOARCH, p.ImportPath, runtime.GOOS, runtime.GOARCH, strings.Join(p.SFiles, " "))
		st := l.base.Run(cmd, llb.Dir("/root"))
		for _, f := range p.SFiles {
			st.AddMount(path.Join("/root", f), l.local, llb.SourcePath(strings.TrimPrefix(path.Join(dir, f), l.wd)), llb.Readonly)
		}
		st.AddMount(path.Join("/work", p.ImportPath, "_obj/go_asm.h"), asmheader, llb.SourcePath("go_asm.h"), llb.Readonly)
		asmp := st.AddMount(path.Join("/work", p.ImportPath, "_obj"), llb.Scratch())

		cmd = llb.Shlexf("/usr/local/go/pkg/tool/linux_amd64/pack r /work/%s.a /work/%s/_obj/asm_%s_%s.o", p.ImportPath, p.ImportPath, runtime.GOOS, runtime.GOARCH)
		st2 := l.base.Run(cmd)

		out = st2.AddMount(path.Join("/work", p.ImportPath+".a"), out, llb.SourcePath(path.Base(p.ImportPath)+".a"))
		st2.AddMount(path.Join("/work", p.ImportPath, fmt.Sprintf("_obj/asm_%s_%s.o", runtime.GOOS, runtime.GOARCH)), asmp, llb.SourcePath(fmt.Sprintf("asm_%s_%s.o", runtime.GOOS, runtime.GOARCH)), llb.Readonly)

	}

	pkg.state = out

	l.cache[dir] = pkg

	return pkg, err
}

type pkg struct {
	p       *build.Package
	mounts  []*mount
	state   llb.State
	alldeps map[string]llb.State
}

type mount struct {
	target   string
	selector string
	state    llb.State
}

func (p *pkg) AddDep(name string, dep *pkg) {
	p.mounts = append(p.mounts, &mount{
		target:   path.Dir(name),
		selector: path.Base(name),
		state:    dep.state,
	})
	for str, d := range dep.alldeps {
		p.alldeps[str] = d
	}
	p.alldeps[name] = dep.state
}

type vendorDirs struct {
	checkedDirs map[string]struct{}
	dirs        []string
	gopath      string
}

func newVendorDirs(gopath string) *vendorDirs {
	return &vendorDirs{
		checkedDirs: map[string]struct{}{},
		gopath:      gopath,
		dirs:        []string{filepath.Join(gopath, "src")},
	}
}

func (v *vendorDirs) add(d string) {
	if v.gopath == d {
		return
	}
	if _, ok := v.checkedDirs[d]; ok {
		return
	}
	v.checkedDirs[d] = struct{}{}
	vd := filepath.Join(d, "vendor")
	fi, err := os.Stat(vd)
	if err == nil {
		if fi.IsDir() {
			v.dirs = append(v.dirs, vd)
			sort.Slice(v.dirs, func(a, b int) bool {
				return v.dirs[a] > v.dirs[b]
			})
		}
	}
	v.add(filepath.Dir(d))
}

func goBuildBase() llb.State {
	goAlpine := llb.Image("docker.io/library/golang:1.8-alpine@sha256:2287e0e274c1d2e9076c1f81d04f1a63c86b73c73603b09caada5da307a8f86d")
	return goAlpine.
		AddEnv("CGO_ENABLED", "0").
		AddEnv("PATH", "/usr/local/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin") //.
	//AddEnv("GOPATH", "/go").Run(llb.Shlex("apk add --no-cache gcc libc-dev")).
	// Root()
}
