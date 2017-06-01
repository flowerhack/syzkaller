// Copyright 2016 syzkaller project authors. All rights reserved.
// Use of this source code is governed by Apache 2 LICENSE that can be found in the LICENSE file.

//go:generate bash -c "echo -en '// AUTOGENERATED FILE\n\n' > generated.go"
//go:generate bash -c "echo -en 'package main\n\n' >> generated.go"
//go:generate bash -c "echo -en 'const syzconfig = `\n' >> generated.go"
//go:generate bash -c "cat kernel.config | grep -v '#' >> generated.go"
//go:generate bash -c "echo -en '`\n\n' >> generated.go"
//go:generate bash -c "echo -en 'const createImageScript = `#!/bin/bash\n' >> generated.go"
//go:generate bash -c "cat ../tools/create-gce-image.sh | grep -v '#' >> generated.go"
//go:generate bash -c "echo -en '`\n\n' >> generated.go"

// syz-gce runs syz-manager on GCE in a continous loop handling image/syzkaller updates.
// It downloads test image from GCS, downloads and builds syzkaller, then starts syz-manager
// and pulls for image/syzkaller source updates. If image/syzkaller changes,
// it stops syz-manager and starts from scratch.
package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/google/syzkaller/dashboard"
	"github.com/google/syzkaller/gce"
	. "github.com/google/syzkaller/log"
	pkgconfig "github.com/google/syzkaller/pkg/config"
	"github.com/google/syzkaller/pkg/gcs"
	"github.com/google/syzkaller/pkg/git"
	"github.com/google/syzkaller/syz-manager/config"
)

var (
	flagConfig = flag.String("config", "", "config file")

	cfg             *Config
	GCS             *gcs.Client
	GCE             *gce.Context
	managerHttpPort uint32
	patchesHash     string
	patches         []dashboard.Patch
)

type Config struct {
	Name                  string
	Hub_Addr              string
	Hub_Key               string
	Image_Archive         string
	Image_Path            string
	Image_Name            string
	Http_Port             int
	Machine_Type          string
	Machine_Count         int
	Sandbox               string
	Procs                 int
	Linux_Git             string
	Linux_Branch          string
	Linux_Config          string
	Linux_Compiler        string
	Linux_Userspace       string
	Enable_Syscalls       []string
	Disable_Syscalls      []string
	Dashboard_Addr        string
	Dashboard_Key         string
	Use_Dashboard_Patches bool
}

type Action interface {
	Name() string
	Poll() (string, error)
	Build() error
}

func main() {
	flag.Parse()
	cfg = &Config{
		Use_Dashboard_Patches: true,
	}
	if err := pkgconfig.Load(*flagConfig, cfg); err != nil {
		Fatalf("failed to load config file: %v", err)
	}
	EnableLogCaching(1000, 1<<20)
	initHttp(fmt.Sprintf(":%v", cfg.Http_Port))

	wd, err := os.Getwd()
	if err != nil {
		Fatalf("failed to get wd: %v", err)
	}
	gopath := abs(wd, "gopath")
	os.Setenv("GOPATH", gopath)

	if GCS, err = gcs.NewClient(); err != nil {
		Fatalf("failed to create cloud storage client: %v", err)
	}

	GCE, err = gce.NewContext()
	if err != nil {
		Fatalf("failed to init gce: %v", err)
	}
	Logf(0, "gce initialized: running on %v, internal IP, %v project %v, zone %v", GCE.Instance, GCE.InternalIP, GCE.ProjectID, GCE.ZoneID)

	sigC := make(chan os.Signal, 2)
	signal.Notify(sigC, syscall.SIGINT, syscall.SIGUSR1)

	var actions []Action
	actions = append(actions, new(SyzkallerAction))
	if cfg.Image_Archive == "local" {
		if syscall.Getuid() != 0 {
			Fatalf("building local image requires root")
		}
		if cfg.Use_Dashboard_Patches && cfg.Dashboard_Addr != "" {
			actions = append(actions, &DashboardAction{
				Dash: &dashboard.Dashboard{
					Addr:   cfg.Dashboard_Addr,
					Client: cfg.Name,
					Key:    cfg.Dashboard_Key,
				},
			})
		}
		actions = append(actions, &LocalBuildAction{
			Dir:          abs(wd, "build"),
			Repo:         cfg.Linux_Git,
			Branch:       cfg.Linux_Branch,
			Compiler:     cfg.Linux_Compiler,
			UserspaceDir: abs(wd, cfg.Linux_Userspace),
			ImagePath:    cfg.Image_Path,
			ImageName:    cfg.Image_Name,
		})
	} else {
		actions = append(actions, &GCSImageAction{
			ImageArchive: cfg.Image_Archive,
			ImagePath:    cfg.Image_Path,
			ImageName:    cfg.Image_Name,
		})
	}
	currHashes := make(map[string]string)
	nextHashes := make(map[string]string)

	var managerCmd *exec.Cmd
	managerStopped := make(chan error)
	stoppingManager := false
	alreadyPolled := false
	var delayDuration time.Duration
loop:
	for {
		if delayDuration != 0 {
			Logf(0, "sleep for %v", delayDuration)
			start := time.Now()
			select {
			case <-time.After(delayDuration):
			case err := <-managerStopped:
				if managerCmd == nil {
					Fatalf("spurious manager stop signal")
				}
				Logf(0, "syz-manager exited with %v", err)
				managerCmd = nil
				atomic.StoreUint32(&managerHttpPort, 0)
				minSleep := 5 * time.Minute
				if !stoppingManager && time.Since(start) < minSleep {
					Logf(0, "syz-manager exited too quickly, sleeping for %v", minSleep)
					time.Sleep(minSleep)
				}
			case s := <-sigC:
				switch s {
				case syscall.SIGUSR1:
					// just poll for updates
					Logf(0, "SIGUSR1")
				case syscall.SIGINT:
					Logf(0, "SIGINT")
					if managerCmd != nil {
						Logf(0, "shutting down syz-manager...")
						managerCmd.Process.Signal(syscall.SIGINT)
						select {
						case err := <-managerStopped:
							if managerCmd == nil {
								Fatalf("spurious manager stop signal")
							}
							Logf(0, "syz-manager exited with %v", err)
						case <-sigC:
							managerCmd.Process.Kill()
						case <-time.After(time.Minute):
							managerCmd.Process.Kill()
						}
					}
					os.Exit(0)
				}
			}
		}
		delayDuration = 15 * time.Minute // assume that an error happened

		if !alreadyPolled {
			Logf(0, "polling...")
			for _, a := range actions {
				hash, err := a.Poll()
				if err != nil {
					Logf(0, "failed to poll %v: %v", a.Name(), err)
					continue loop
				}
				nextHashes[a.Name()] = hash
			}
		}

		changed := managerCmd == nil
		for _, a := range actions {
			next := nextHashes[a.Name()]
			curr := currHashes[a.Name()]
			if curr != next {
				Logf(0, "%v changed %v -> %v", a.Name(), curr, next)
				changed = true
			}
		}
		if !changed {
			// Nothing has changed, sleep for another hour.
			delayDuration = time.Hour
			continue
		}

		// At this point we are starting an update. First, stop manager.
		if managerCmd != nil {
			if !stoppingManager {
				stoppingManager = true
				Logf(0, "stopping syz-manager...")
				managerCmd.Process.Signal(syscall.SIGINT)
			} else {
				Logf(0, "killing syz-manager...")
				managerCmd.Process.Kill()
			}
			delayDuration = time.Minute
			alreadyPolled = true
			continue
		}
		alreadyPolled = false

		for _, a := range actions {
			if currHashes[a.Name()] == nextHashes[a.Name()] {
				continue
			}
			Logf(0, "building %v...", a.Name())
			if err := a.Build(); err != nil {
				Logf(0, "building %v failed: %v", a.Name(), err)
				continue loop
			}
			currHashes[a.Name()] = nextHashes[a.Name()]
		}

		// Restart syz-manager.
		port, err := chooseUnusedPort()
		if err != nil {
			Logf(0, "failed to choose an unused port: %v", err)
			continue
		}
		if err := writeManagerConfig(cfg, port, "manager.cfg"); err != nil {
			Logf(0, "failed to write manager config: %v", err)
			continue
		}

		Logf(0, "starting syz-manager...")
		managerCmd = exec.Command("gopath/src/github.com/google/syzkaller/bin/syz-manager", "-config=manager.cfg")
		if err := managerCmd.Start(); err != nil {
			Logf(0, "failed to start syz-manager: %v", err)
			managerCmd = nil
			continue
		}
		stoppingManager = false
		atomic.StoreUint32(&managerHttpPort, uint32(port))
		go func() {
			managerStopped <- managerCmd.Wait()
		}()
		delayDuration = 6 * time.Hour
	}
}

type SyzkallerAction struct {
}

func (a *SyzkallerAction) Name() string {
	return "syzkaller"
}

// Poll executes 'git pull' on syzkaller and all depenent packages.
// Returns syzkaller HEAD hash.
func (a *SyzkallerAction) Poll() (string, error) {
	if _, err := runCmd("", "go", "get", "-u", "-d", "github.com/google/syzkaller/syz-manager"); err != nil {
		return "", err
	}
	return git.HeadCommit("gopath/src/github.com/google/syzkaller")
}

func (a *SyzkallerAction) Build() error {
	if _, err := runCmd("gopath/src/github.com/google/syzkaller", "make"); err != nil {
		return err
	}
	return nil
}

type DashboardAction struct {
	Dash *dashboard.Dashboard
}

func (a *DashboardAction) Name() string {
	return "dashboard"
}

func (a *DashboardAction) Poll() (hash string, err error) {
	patchesHash, err = a.Dash.PollPatches()
	return patchesHash, err
}

func (a *DashboardAction) Build() (err error) {
	patches, err = a.Dash.GetPatches()
	return
}

type LocalBuildAction struct {
	Dir          string
	Repo         string
	Branch       string
	Compiler     string
	UserspaceDir string
	ImagePath    string
	ImageName    string
}

func (a *LocalBuildAction) Name() string {
	return "kernel"
}

func (a *LocalBuildAction) Poll() (string, error) {
	dir := filepath.Join(a.Dir, "linux")
	rev, err := git.Poll(dir, a.Repo, a.Branch)
	if err != nil {
		return "", err
	}
	if patchesHash != "" {
		rev += "/" + patchesHash
	}
	return rev, nil
}

func (a *LocalBuildAction) Build() error {
	dir := filepath.Join(a.Dir, "linux")
	hash, err := git.HeadCommit(dir)
	if err != nil {
		return err
	}
	for _, p := range patches {
		if err := a.apply(p); err != nil {
			return err
		}
	}
	Logf(0, "building kernel on %v...", hash)
	if err := buildKernel(dir, a.Compiler); err != nil {
		return fmt.Errorf("build failed: %v", err)
	}
	scriptFile := filepath.Join(a.Dir, "create-gce-image.sh")
	if err := ioutil.WriteFile(scriptFile, []byte(createImageScript), 0700); err != nil {
		return fmt.Errorf("failed to write script file: %v", err)
	}
	Logf(0, "building image...")
	vmlinux := filepath.Join(dir, "vmlinux")
	bzImage := filepath.Join(dir, "arch/x86/boot/bzImage")
	if _, err := runCmd(a.Dir, scriptFile, a.UserspaceDir, bzImage, vmlinux, hash); err != nil {
		return fmt.Errorf("image build failed: %v", err)
	}
	os.Remove(filepath.Join(a.Dir, "disk.raw"))
	os.Remove(filepath.Join(a.Dir, "image.tar.gz"))
	os.MkdirAll("image/obj", 0700)
	if err := ioutil.WriteFile("image/tag", []byte(hash), 0600); err != nil {
		return fmt.Errorf("failed to write tag file: %v", err)
	}
	if err := os.Rename(filepath.Join(a.Dir, "key"), "image/key"); err != nil {
		return fmt.Errorf("failed to rename key file: %v", err)
	}
	if err := os.Rename(vmlinux, "image/obj/vmlinux"); err != nil {
		return fmt.Errorf("failed to rename vmlinux file: %v", err)
	}
	if err := createImage(filepath.Join(a.Dir, "disk.tar.gz"), a.ImagePath, a.ImageName); err != nil {
		return err
	}
	return nil
}

func (a *LocalBuildAction) apply(p dashboard.Patch) error {
	// Do --dry-run first to not mess with partially consistent state.
	cmd := exec.Command("patch", "-p1", "--force", "--ignore-whitespace", "--dry-run")
	cmd.Dir = filepath.Join(a.Dir, "linux")
	cmd.Stdin = bytes.NewReader(p.Diff)
	if output, err := cmd.CombinedOutput(); err != nil {
		// If it reverses clean, then it's already applied (seems to be the easiest way to detect it).
		cmd = exec.Command("patch", "-p1", "--force", "--ignore-whitespace", "--reverse", "--dry-run")
		cmd.Dir = filepath.Join(a.Dir, "linux")
		cmd.Stdin = bytes.NewReader(p.Diff)
		if _, err := cmd.CombinedOutput(); err == nil {
			Logf(0, "patch already present: %v", p.Title)
			return nil
		}
		Logf(0, "patch failed: %v\n%s", p.Title, output)
		return nil
	}
	// Now apply for real.
	cmd = exec.Command("patch", "-p1", "--force", "--ignore-whitespace")
	cmd.Dir = filepath.Join(a.Dir, "linux")
	cmd.Stdin = bytes.NewReader(p.Diff)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("patch '%v' failed after dry run:\n%s", p.Title, output)
	}
	Logf(0, "patch applied: %v", p.Title)
	return nil
}

type GCSImageAction struct {
	ImageArchive string
	ImagePath    string
	ImageName    string

	file *gcs.File
}

func (a *GCSImageAction) Name() string {
	return "GCS image"
}

func (a *GCSImageAction) Poll() (string, error) {
	f, err := GCS.Read(a.ImageArchive)
	if err != nil {
		return "", err
	}
	a.file = f
	return f.Updated.Format(time.RFC1123Z), nil
}

func (a *GCSImageAction) Build() error {
	Logf(0, "downloading image archive...")
	if err := os.RemoveAll("image"); err != nil {
		return fmt.Errorf("failed to remove image dir: %v", err)
	}
	if err := downloadAndExtract(a.file, "image"); err != nil {
		return fmt.Errorf("failed to download and extract %v: %v", a.ImageArchive, err)
	}
	if err := createImage("image/disk.tar.gz", a.ImagePath, a.ImageName); err != nil {
		return err
	}
	return nil
}

func writeManagerConfig(cfg *Config, httpPort int, file string) error {
	tag, err := ioutil.ReadFile("image/tag")
	if err != nil {
		return fmt.Errorf("failed to read tag file: %v", err)
	}
	if len(tag) != 0 && tag[len(tag)-1] == '\n' {
		tag = tag[:len(tag)-1]
	}
	managerCfg := &config.Config{
		Name:             cfg.Name,
		Hub_Addr:         cfg.Hub_Addr,
		Hub_Key:          cfg.Hub_Key,
		Dashboard_Addr:   cfg.Dashboard_Addr,
		Dashboard_Key:    cfg.Dashboard_Key,
		Http:             fmt.Sprintf(":%v", httpPort),
		Rpc:              ":0",
		Workdir:          "workdir",
		Vmlinux:          "image/obj/vmlinux",
		Tag:              string(tag),
		Syzkaller:        "gopath/src/github.com/google/syzkaller",
		Type:             "gce",
		Machine_Type:     cfg.Machine_Type,
		Count:            cfg.Machine_Count,
		Image:            cfg.Image_Name,
		Sandbox:          cfg.Sandbox,
		Procs:            cfg.Procs,
		Enable_Syscalls:  cfg.Enable_Syscalls,
		Disable_Syscalls: cfg.Disable_Syscalls,
		Cover:            true,
		Reproduce:        true,
	}
	if _, err := os.Stat("image/key"); err == nil {
		managerCfg.Sshkey = "image/key"
	}
	data, err := json.MarshalIndent(managerCfg, "", "\t")
	if err != nil {
		return err
	}
	if err := ioutil.WriteFile(file, data, 0600); err != nil {
		return err
	}
	return nil
}

func chooseUnusedPort() (int, error) {
	ln, err := net.Listen("tcp4", ":")
	if err != nil {
		return 0, err
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	return port, nil
}

func downloadAndExtract(f *gcs.File, dir string) error {
	r, err := f.Reader()
	if err != nil {
		return err
	}
	defer r.Close()
	gz, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	files := make(map[string]bool)
	ar := tar.NewReader(gz)
	for {
		hdr, err := ar.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		Logf(0, "extracting file: %v (%v bytes)", hdr.Name, hdr.Size)
		if len(hdr.Name) == 0 || hdr.Name[len(hdr.Name)-1] == '/' {
			continue
		}
		files[filepath.Clean(hdr.Name)] = true
		base, file := filepath.Split(hdr.Name)
		if err := os.MkdirAll(filepath.Join(dir, base), 0700); err != nil {
			return err
		}
		dst, err := os.OpenFile(filepath.Join(dir, base, file), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		_, err = io.Copy(dst, ar)
		dst.Close()
		if err != nil {
			return err
		}
	}
	for _, need := range []string{"disk.tar.gz", "tag", "obj/vmlinux"} {
		if !files[need] {
			return fmt.Errorf("archive misses required file '%v'", need)
		}
	}
	return nil
}

func createImage(localFile, gcsFile, imageName string) error {
	Logf(0, "uploading image...")
	if err := GCS.UploadFile(localFile, gcsFile); err != nil {
		return fmt.Errorf("failed to upload image: %v", err)
	}
	Logf(0, "creating gce image...")
	if err := GCE.DeleteImage(imageName); err != nil {
		return fmt.Errorf("failed to delete GCE image: %v", err)
	}
	if err := GCE.CreateImage(imageName, gcsFile); err != nil {
		return fmt.Errorf("failed to create GCE image: %v", err)
	}
	return nil
}

func buildKernel(dir, ccompiler string) error {
	os.Remove(filepath.Join(dir, ".config"))
	if _, err := runCmd(dir, "make", "defconfig"); err != nil {
		return err
	}
	if _, err := runCmd(dir, "make", "kvmconfig"); err != nil {
		return err
	}
	configFile := cfg.Linux_Config
	if configFile == "" {
		configFile = filepath.Join(dir, "syz.config")
		if err := ioutil.WriteFile(configFile, []byte(syzconfig), 0600); err != nil {
			return fmt.Errorf("failed to write config file: %v", err)
		}
	}
	if _, err := runCmd(dir, "scripts/kconfig/merge_config.sh", "-n", ".config", configFile); err != nil {
		return err
	}
	if _, err := runCmd(dir, "make", "olddefconfig"); err != nil {
		return err
	}
	if _, err := runCmd(dir, "make", "-j", strconv.Itoa(runtime.NumCPU()*2), "CC="+ccompiler); err != nil {
		return err
	}
	return nil
}

func runCmd(dir, bin string, args ...string) ([]byte, error) {
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("failed to run %v %+v: %v\n%s", bin, args, err, output)
	}
	return output, nil
}

func abs(wd, path string) string {
	if !filepath.IsAbs(path) {
		path = filepath.Join(wd, path)
	}
	return path
}
